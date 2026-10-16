package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ErrExhausted means every candidate kept failing for MaxRetryWindow.
var ErrExhausted = errors.New("all upstream candidates exhausted")

// CandidateSource returns a channel's failover list in priority order. It
// is re-read on every (re)start so admin reorders apply to the next pick.
type CandidateSource func(ctx context.Context) ([]Candidate, error)

// Event types, matching the events table.
const (
	EventOpenFailed = "open_failed" // a candidate could not be opened
	EventFailover   = "failover"    // a playing upstream failed and another took over
	EventExhausted  = "exhausted"   // no candidate worked within the retry window
	EventSwitch     = "switch"      // an admin moved the channel to another stream
)

// reasonSwitched ends an upstream that an admin switched away from: not a
// failure, so no cooldown.
const reasonSwitched = "switched"

// Event is a session state change worth recording (see events table).
type Event struct {
	Type         string
	At           time.Time
	ChannelID    int64
	ProviderID   int64
	FromStreamID int64 // the stream that failed
	ToStreamID   int64 // the stream now playing (failover only)
	Message      string
}

// SessionConfig wires a session. Ring, Source, Opener, Capacity and
// Cooldowns are required.
type SessionConfig struct {
	ChannelID      int64
	Ring           *Ring
	Source         CandidateSource
	Opener         Opener
	Capacity       *Capacity
	Cooldowns      *Cooldowns
	Thresholds     Thresholds
	MaxRetryWindow time.Duration // give up after failing this long with nothing playing
	RetryBackoff   time.Duration // pause when no candidate is usable; default 500ms
	StableAfter    time.Duration // healthy streaming this long clears a stream's backoff; default 30s
	Log            *slog.Logger
	OnEvent        func(Event) // optional; called from the session goroutine
	Metrics        *Metrics    // optional
	Probe          MediaProbe  // optional: fills in resolution, frame rate, audio layout
}

// SessionStatus is a snapshot for the dashboard.
type SessionStatus struct {
	ChannelID  int64     `json:"channel_id"`
	State      string    `json:"state"` // connecting | streaming | waiting | closed
	StreamID   int64     `json:"stream_id,omitempty"`
	ProviderID int64     `json:"provider_id,omitempty"`
	Since      time.Time `json:"since"`   // in the current state
	Started    time.Time `json:"started"` // the session
	Failovers  int       `json:"failovers"`
	Bytes      uint64    `json:"bytes"`    // delivered to the ring, all upstreams
	Kbps       int       `json:"kbps"`     // over the last few seconds
	AvgKbps    int       `json:"avg_kbps"` // since the first byte
	Media      MediaInfo `json:"media"`    // of the current upstream
}

// rateWindow is how far back the current bitrate looks.
const rateWindow = 5 * time.Second

type rateSample struct {
	at    time.Time
	bytes uint64
}

// Session runs one channel's upstream: it keeps exactly one provider
// connection open, feeds the shared ring, and fails over between candidates
// without closing the ring until the channel is torn down or every
// candidate is exhausted.
type Session struct {
	cfg SessionConfig
	log *slog.Logger

	mu        sync.Mutex
	status    SessionStatus
	firstByte time.Time    // first byte of the session, for the average
	rate      []rateSample // about one per second over rateWindow
	preferred int64        // stream an admin asked for, tried next
	switchReq chan struct{}
}

// NewSession fills defaults.
func NewSession(cfg SessionConfig) *Session {
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 500 * time.Millisecond
	}
	if cfg.StableAfter <= 0 {
		cfg.StableAfter = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	now := time.Now()
	return &Session{
		cfg:       cfg,
		log:       cfg.Log.With("channel_id", cfg.ChannelID),
		status:    SessionStatus{ChannelID: cfg.ChannelID, State: "connecting", Since: now, Started: now},
		switchReq: make(chan struct{}, 1),
	}
}

// Status returns a snapshot of the session state.
func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	now := time.Now()
	if len(s.rate) > 0 {
		// From the oldest sample in the window to now: a stalled upstream
		// shows its rate falling instead of the last good value.
		first := s.rate[0]
		if d := now.Sub(first.at); d > 0 {
			st.Kbps = int(float64(st.Bytes-first.bytes) * 8 / d.Seconds() / 1000)
		}
	}
	if !s.firstByte.IsZero() {
		if d := now.Sub(s.firstByte); d >= time.Second {
			st.AvgKbps = int(float64(st.Bytes) * 8 / d.Seconds() / 1000)
		} else {
			st.AvgKbps = st.Kbps
		}
	}
	return st
}

// addBytes counts n bytes delivered at now.
func (s *Session) addBytes(now time.Time, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstByte.IsZero() {
		s.firstByte = now
		s.rate = append(s.rate, rateSample{now, 0})
	}
	s.status.Bytes += uint64(n)
	if now.Sub(s.rate[len(s.rate)-1].at) >= time.Second {
		s.rate = append(s.rate, rateSample{now, s.status.Bytes})
	}
	cut := 0
	for cut < len(s.rate)-1 && now.Sub(s.rate[cut].at) > rateWindow {
		cut++
	}
	s.rate = s.rate[cut:]
}

// setMedia records what stream streamID carries, unless the session moved on.
func (s *Session) setMedia(streamID int64, m MediaInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.StreamID == streamID {
		s.status.Media = s.status.Media.merge(m)
	}
}

// Switch asks the session to move to streamID now, even if it is cooling
// down. The current upstream is closed without counting as a failure. If
// the stream cannot be opened the session fails over as usual.
func (s *Session) Switch(streamID int64) {
	s.mu.Lock()
	if s.status.StreamID == streamID && s.status.State == "streaming" {
		s.mu.Unlock()
		return // already playing it
	}
	s.preferred = streamID
	s.mu.Unlock()
	select {
	case s.switchReq <- struct{}{}:
	default: // one pending request is enough
	}
}

// takePreferred returns cands with the requested stream first (once).
func (s *Session) takePreferred(cands []Candidate) ([]Candidate, int64) {
	s.mu.Lock()
	id := s.preferred
	s.preferred = 0
	s.mu.Unlock()
	if id == 0 {
		return cands, 0
	}
	i := slices.IndexFunc(cands, func(c Candidate) bool { return c.StreamID == id })
	if i < 0 {
		return cands, 0
	}
	out := append([]Candidate{cands[i]}, cands[:i]...)
	return append(out, cands[i+1:]...), id
}

func (s *Session) setState(state string, c *Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = state
	s.status.Since = time.Now()
	if c != nil {
		if c.StreamID != s.status.StreamID {
			s.status.Media = MediaInfo{}
		}
		s.status.StreamID, s.status.ProviderID = c.StreamID, c.ProviderID
	}
}

func (s *Session) emit(e Event) {
	e.ChannelID = s.cfg.ChannelID
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(e)
	}
}

// Run streams until ctx is cancelled (returns nil) or every candidate has
// failed for MaxRetryWindow (returns ErrExhausted). Either way the ring is
// closed and all provider slots are released on return.
func (s *Session) Run(ctx context.Context) error {
	defer s.cfg.Ring.Close()
	defer s.setState("closed", nil)

	failingSince := time.Now() // nothing is playing yet
	var lastFailed int64       // stream that failed most recently, for the failover event
	spliced := false           // true once any upstream has fed the ring

	for {
		if ctx.Err() != nil {
			return nil
		}
		cands, err := s.cfg.Source(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Warn("session: load candidates", "err", err)
		}

		cands, preferred := s.takePreferred(cands)
		played := false
		for _, c := range cands {
			if c.StreamID != preferred && s.cfg.Cooldowns.Active(c.StreamID, time.Now()) {
				continue
			}
			slot, ok := s.cfg.Capacity.TryAcquire(c.ProviderID, c.MaxConnections)
			if !ok {
				continue // provider full: not the stream's fault, no cooldown
			}
			s.setState("connecting", &c)
			rc, err := s.cfg.Opener.Open(ctx, c)
			if err != nil {
				slot.Release()
				if ctx.Err() != nil {
					return nil
				}
				s.cfg.Metrics.openFailure()
				d := s.cfg.Cooldowns.Fail(c.StreamID, time.Now())
				// Opener errors carry redacted URLs only.
				s.log.Warn("session: open failed", "stream_id", c.StreamID, "provider_id", c.ProviderID,
					"cooldown", d, "err", err)
				s.emit(Event{Type: EventOpenFailed, ProviderID: c.ProviderID, FromStreamID: c.StreamID, Message: err.Error()})
				continue
			}

			res := s.stream(ctx, c, rc, spliced, lastFailed, c.StreamID == preferred)
			rc.Close()
			slot.Release()
			if res.delivered {
				spliced = true
				failingSince = time.Now()
			}
			if ctx.Err() != nil {
				return nil
			}
			if res.reason == reasonSwitched {
				s.log.Info("session: switching stream", "from_stream_id", c.StreamID)
				lastFailed = c.StreamID // the switch event names it
				played = true
				break
			}
			s.cfg.Metrics.upstreamFailure(res.reason)
			d := s.cfg.Cooldowns.Fail(c.StreamID, time.Now())
			s.log.Warn("session: upstream failed", "stream_id", c.StreamID, "provider_id", c.ProviderID,
				"reason", res.reason, "cooldown", d)
			lastFailed = c.StreamID
			s.mu.Lock()
			s.status.Failovers++
			s.mu.Unlock()
			played = true
			break // rescan from the top: the failed stream is now cooling down
		}

		if time.Since(failingSince) > s.cfg.MaxRetryWindow {
			s.log.Error("session: candidates exhausted", "window", s.cfg.MaxRetryWindow)
			s.cfg.Metrics.exhaust()
			s.emit(Event{Type: EventExhausted, FromStreamID: lastFailed, Message: ErrExhausted.Error()})
			return ErrExhausted
		}
		if !played {
			s.setState("waiting", nil)
			select {
			case <-ctx.Done():
				return nil
			case <-s.switchReq: // an admin picked a stream: try it now
			case <-time.After(s.cfg.RetryBackoff):
			}
		}
	}
}

type streamResult struct {
	delivered bool   // at least one packet reached the ring
	reason    string // why the upstream ended
}

// stream pumps one upstream into the ring until it fails or ctx ends. A
// watchdog closes rc when tswatch judges the upstream unhealthy, or when an
// admin switches streams, which unblocks the read. manual marks an upstream
// an admin picked.
func (s *Session) stream(ctx context.Context, c Candidate, rc io.ReadCloser, spliced bool, failedID int64, manual bool) streamResult {
	w := NewWatcher(s.cfg.Thresholds, time.Now())
	var verdict atomic.Int32
	var switched atomic.Bool
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := min(s.cfg.Thresholds.StallTimeout/4, 250*time.Millisecond)
		if tick <= 0 {
			tick = 250 * time.Millisecond
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				rc.Close()
				return
			case <-s.switchReq:
				switched.Store(true)
				rc.Close()
				return
			case now := <-t.C:
				if v := w.Check(now); v != Healthy {
					verdict.Store(int32(v))
					rc.Close()
					return
				}
			}
		}
	}()

	sp := newSplicer(spliced)
	keys := newKeyTracker()
	buf := make([]byte, 64*1024)
	var out []byte
	var res streamResult
	var firstAt time.Time
	stableCleared := false
	var sample []byte // the start of this upstream, for MediaInfo
	sampled := false
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			now := time.Now()
			out = sp.process(w.Feed(now, out[:0], buf[:n]))
			if len(out) > 0 {
				// Note the newest keyframe before writing: this goroutine is
				// the ring's only writer, so Head is where out will land.
				if idx, prefix := keys.scan(out); idx >= 0 {
					s.cfg.Ring.SetJoinPoint(s.cfg.Ring.Head()+uint64(idx*TSPacketSize), prefix)
				}
				if _, werr := s.cfg.Ring.Write(out); werr != nil {
					res.reason = "ring closed"
					return res
				}
				s.cfg.Metrics.addUpstreamBytes(len(out))
				s.addBytes(now, len(out))
				if !sampled {
					sample = append(sample, out...)
					if len(sample) >= mediaSampleSize || (res.delivered && now.Sub(firstAt) >= 3*time.Second) {
						sampled = true
						go s.probeMedia(ctx, c.StreamID, sample)
						sample = nil
					}
				}
				if !res.delivered {
					res.delivered, firstAt = true, now
					s.setState("streaming", &c)
					s.log.Info("session: streaming", "stream_id", c.StreamID, "provider_id", c.ProviderID)
					switch {
					case manual:
						s.emit(Event{Type: EventSwitch, ProviderID: c.ProviderID, FromStreamID: failedID,
							ToStreamID: c.StreamID, Message: "switched by an admin"})
					case failedID != 0:
						s.cfg.Metrics.failover()
						s.emit(Event{Type: EventFailover, ProviderID: c.ProviderID, FromStreamID: failedID,
							ToStreamID: c.StreamID, Message: "switched upstream"})
					}
				} else if !stableCleared && now.Sub(firstAt) >= s.cfg.StableAfter {
					s.cfg.Cooldowns.Reset(c.StreamID)
					stableCleared = true
				}
			}
		}
		if err != nil {
			switch v := Verdict(verdict.Load()); {
			case switched.Load():
				res.reason = reasonSwitched
			case v != Healthy:
				res.reason = v.String()
			case errors.Is(err, io.EOF):
				res.reason = "eof"
			default:
				res.reason = fmt.Sprintf("read: %v", err)
			}
			return res
		}
	}
}

// probeMedia works out what streamID carries from the start of its data:
// codecs from the PMT, the rest from the configured probe.
func (s *Session) probeMedia(ctx context.Context, streamID int64, sample []byte) {
	m := tsMediaInfo(sample)
	s.setMedia(streamID, m)
	if s.cfg.Probe == nil {
		return
	}
	pm, err := s.cfg.Probe(ctx, sample)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Debug("session: media probe", "stream_id", streamID, "err", err)
		}
		return
	}
	s.setMedia(streamID, pm.merge(m))
}

// maxPATWait bounds how much of a new upstream is discarded while waiting
// for a PAT; a provider that never sends one is passed through as is.
const maxPATWait = 2 << 20

// splicer makes upstream switches clean for decoders: output (re)starts at a
// PAT, and after a switch the first adaptation-field packet of each PID gets
// discontinuity_indicator so clocks reset. Packets without an adaptation
// field cannot be flagged without re-packetizing; decoders tolerate their
// continuity-counter jump.
type splicer struct {
	waitPAT bool
	markDis bool
	marked  map[uint16]bool
	dropped int
}

func newSplicer(afterSwitch bool) *splicer {
	return &splicer{waitPAT: true, markDis: afterSwitch, marked: map[uint16]bool{}}
}

// process filters aligned packets in place and returns the kept ones.
func (sp *splicer) process(pkts []byte) []byte {
	out := pkts[:0]
	for i := 0; i+TSPacketSize <= len(pkts); i += TSPacketSize {
		p := pkts[i : i+TSPacketSize]
		pid := PID(p)
		if sp.waitPAT {
			isPAT := pid == 0 && p[1]&0x40 != 0 // PAT with payload_unit_start
			sp.dropped += TSPacketSize
			if !isPAT && sp.dropped < maxPATWait {
				continue
			}
			sp.waitPAT = false
		}
		if sp.markDis && !sp.marked[pid] && p[3]&0x20 != 0 && p[4] > 0 {
			p[5] |= 0x80
			sp.marked[pid] = true
		}
		out = append(out, p...)
	}
	return out
}
