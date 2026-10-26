// Package health runs the background stream prober: it samples channel
// streams off the playback path, feeds failures into the proxy's shared
// cooldowns (so sessions skip dead streams before a viewer hits them),
// clears cooldowns once a stream recovers, and keeps health_score current.
package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

const (
	// sampleMax caps the bytes kept from one probe (enough for ffprobe to
	// find codec parameters in a few seconds of HD).
	sampleMax = 1 << 20
	// minPackets aligned TS packets make a stream look alive.
	minPackets = 50
	// scoreWeight is the weight of the newest probe in health_score.
	scoreWeight = 0.25
	// ffprobeTimeout bounds one ffprobe run over a sample.
	ffprobeTimeout = 10 * time.Second
)

// Store is the persistence the prober needs (satisfied by *db.Queries).
type Store interface {
	ListProbeTargets(ctx context.Context) ([]db.ListProbeTargetsRow, error)
	RecordProbe(ctx context.Context, arg db.RecordProbeParams) error
}

// Config tunes the prober.
type Config struct {
	Interval    time.Duration // every stream is probed this often
	Recheck     time.Duration // streams in cooldown are re-probed this often
	Sample      time.Duration // how long one probe reads
	Concurrency int           // probes at once (each holds a provider slot)
	FFprobePath string        // "" skips the ffprobe check
}

// Prober probes channel streams in the background.
type Prober struct {
	Log       *slog.Logger
	Store     Store
	Opener    proxy.Opener // the direct (HTTP-TS/HLS) opener: profiles are not applied
	Capacity  *proxy.Capacity
	Cooldowns *proxy.Cooldowns
	// Busy returns streams a session is playing right now; they are
	// evidently up and must not cost a second provider slot. Optional.
	Busy func() map[int64]bool
	// Outcomes, optional, counts each probe result for the provider health scores.
	Outcomes *proxy.Outcomes
	Cfg      Config
	Now      func() time.Time
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Run probes due streams every Recheck until ctx ends.
func (p *Prober) Run(ctx context.Context) {
	t := time.NewTicker(p.Cfg.Recheck)
	defer t.Stop()
	for {
		if _, err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			p.Log.Warn("prober: round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Result is one probe's outcome.
type Result struct {
	StreamID int64
	OK       bool
	Skipped  bool   // provider full: not probed, nothing recorded
	Err      string // why it failed (URLs redacted)
}

// RunOnce probes every due stream, cooling ones first, and returns the
// results of those it got to.
func (p *Prober) RunOnce(ctx context.Context) ([]Result, error) {
	targets, err := p.Store.ListProbeTargets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list probe targets: %w", err)
	}
	now := p.now()
	var busy map[int64]bool
	if p.Busy != nil {
		busy = p.Busy()
	}
	var due []db.ListProbeTargetsRow
	for _, t := range targets {
		if !busy[t.StreamID] && p.due(t, now) {
			due = append(due, t)
		}
	}
	slices.SortStableFunc(due, func(a, b db.ListProbeTargetsRow) int {
		ca, cb := p.cooling(a, now), p.cooling(b, now)
		switch {
		case ca && !cb:
			return -1
		case cb && !ca:
			return 1
		}
		return 0
	})

	results := make([]Result, len(due))
	sem := make(chan struct{}, max(p.Cfg.Concurrency, 1))
	var wg sync.WaitGroup
	for i, t := range due {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return results[:i], nil
		}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = p.probe(ctx, t)
		})
	}
	wg.Wait()
	return results, nil
}

// cooling: in cooldown in memory (a session or probe failed it) or in the
// database (a probe before a restart did).
func (p *Prober) cooling(t db.ListProbeTargetsRow, now time.Time) bool {
	return p.Cooldowns.Active(t.StreamID, now) || (t.CooldownUntil != nil && now.Before(*t.CooldownUntil))
}

func (p *Prober) due(t db.ListProbeTargetsRow, now time.Time) bool {
	if t.LastProbedAt == nil {
		return true
	}
	every := p.Cfg.Interval
	if p.cooling(t, now) {
		every = p.Cfg.Recheck
	}
	return now.Sub(*t.LastProbedAt) >= every
}

// probe samples one stream and records the outcome.
func (p *Prober) probe(ctx context.Context, t db.ListProbeTargetsRow) Result {
	res := Result{StreamID: t.StreamID}
	slot, ok := p.Capacity.TryAcquire(t.ProviderID, int(t.MaxConnections))
	if !ok {
		res.Skipped = true
		return res
	}
	sample, err := p.sample(ctx, t)
	slot.Release()
	if ctx.Err() != nil {
		res.Skipped = true // shutting down: an interrupted probe proves nothing
		return res
	}
	if busy, ok := proxy.ProviderBusy(err); ok {
		// No free connection at the provider says nothing about the stream.
		p.Capacity.MarkBusy(t.ProviderID, time.Now().Add(busy))
		res.Skipped = true
		return res
	}
	if err == nil {
		err = sniffTS(sample)
	}
	if err == nil && p.Cfg.FFprobePath != "" {
		err = p.ffprobe(ctx, sample)
	}

	now := p.now()
	score := float32((1 - scoreWeight) * float64(t.HealthScore))
	rec := db.RecordProbeParams{ID: t.StreamID, LastProbedAt: &now}
	if err == nil {
		res.OK = true
		score += scoreWeight
		p.Cooldowns.Reset(t.StreamID)
		p.Outcomes.Record(t.ProviderID, t.Backbone, proxy.OutcomeProbeOK)
	} else {
		res.Err = err.Error()
		until := now.Add(p.Cooldowns.Fail(t.StreamID, now))
		p.Outcomes.Record(t.ProviderID, t.Backbone, proxy.OutcomeProbeFail)
		rec.CooldownUntil, rec.LastError = &until, res.Err
		p.Log.Info("prober: stream failed", "stream_id", t.StreamID, "provider_id", t.ProviderID,
			"cooldown_until", until, "err", res.Err)
	}
	rec.HealthScore = score
	if err := p.Store.RecordProbe(ctx, rec); err != nil && ctx.Err() == nil {
		p.Log.Warn("prober: record", "stream_id", t.StreamID, "err", err)
	}
	return res
}

// sample reads the stream for Cfg.Sample (or until sampleMax bytes).
func (p *Prober) sample(ctx context.Context, t db.ListProbeTargetsRow) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc, err := p.Opener.Open(ctx, proxy.Candidate{
		StreamID: t.StreamID, ProviderID: t.ProviderID, URL: t.StreamUrl, UserAgent: t.UserAgent,
	})
	if err != nil {
		return nil, err // openers redact URLs
	}
	defer rc.Close()
	// The read deadline starts once the upstream answered, so a slow
	// connect does not eat the sample.
	timer := time.AfterFunc(p.Cfg.Sample, cancel)
	defer timer.Stop()
	buf := make([]byte, 0, 64*1024)
	chunk := make([]byte, 32*1024)
	for len(buf) < sampleMax {
		n, err := rc.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				break // sample time is up, or a short (VOD-like) stream ended
			}
			if len(buf) == 0 {
				return nil, fmt.Errorf("read: %w", err)
			}
			break
		}
	}
	return buf, nil
}

// sniffTS checks that b holds at least minPackets aligned MPEG-TS packets
// and a PAT.
func sniffTS(b []byte) error {
	const pkt = proxy.TSPacketSize
	if len(b) == 0 {
		return errors.New("no data")
	}
	start := -1
	for i := 0; i < pkt && i < len(b); i++ {
		if b[i] == 0x47 && (i+pkt >= len(b) || b[i+pkt] == 0x47) {
			start = i
			break
		}
	}
	if start < 0 {
		return errors.New("not MPEG-TS")
	}
	aligned, pat := 0, false
	for i := start; i+pkt <= len(b); i += pkt {
		if b[i] != 0x47 {
			return fmt.Errorf("lost TS sync after %d packets", aligned)
		}
		aligned++
		if proxy.PID(b[i:i+pkt]) == 0 {
			pat = true
		}
	}
	switch {
	case aligned < minPackets:
		return fmt.Errorf("too little data (%d packets)", aligned)
	case !pat:
		return errors.New("no PAT")
	}
	return nil
}

// ffprobe asks ffprobe for the sample's streams over stdin, so no URL is
// ever on a command line.
func (p *Prober) ffprobe(ctx context.Context, sample []byte) error {
	ctx, cancel := context.WithTimeout(ctx, ffprobeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Cfg.FFprobePath, "-v", "error",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", "-f", "mpegts", "-i", "pipe:0")
	cmd.Stdin = bytes.NewReader(sample)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.Join(strings.Fields(stderr.String()), " ")
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("ffprobe: %v %s", err, msg)
	}
	types := string(out)
	if !strings.Contains(types, "video") && !strings.Contains(types, "audio") {
		return errors.New("ffprobe: no audio or video stream")
	}
	return nil
}
