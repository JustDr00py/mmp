package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
)

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
}

// SessionStatus is a snapshot for the dashboard.
type SessionStatus struct {
	ChannelID  int64     `json:"channel_id"`
	State      string    `json:"state"` // connecting | streaming | waiting | closed
	StreamID   int64     `json:"stream_id,omitempty"`
	ProviderID int64     `json:"provider_id,omitempty"`
	Since      time.Time `json:"since"`
	Failovers  int       `json:"failovers"`
}

// Session runs one channel's upstream: it keeps exactly one provider
// connection open, feeds the shared ring, and fails over between candidates
// without closing the ring until the channel is torn down or every
// candidate is exhausted.
type Session struct {
	cfg SessionConfig
	log *slog.Logger

	mu     sync.Mutex
	status SessionStatus
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
	return &Session{
		cfg:    cfg,
		log:    cfg.Log.With("channel_id", cfg.ChannelID),
		status: SessionStatus{ChannelID: cfg.ChannelID, State: "connecting", Since: time.Now()},
	}
}

// Status returns a snapshot of the session state.
func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Session) setState(state string, c *Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = state
	s.status.Since = time.Now()
	if c != nil {
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

		played := false
		for _, c := range cands {
			if s.cfg.Cooldowns.Active(c.StreamID, time.Now()) {
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
				d := s.cfg.Cooldowns.Fail(c.StreamID, time.Now())
				// Opener errors carry redacted URLs only.
				s.log.Warn("session: open failed", "stream_id", c.StreamID, "provider_id", c.ProviderID,
					"cooldown", d, "err", err)
				s.emit(Event{Type: EventOpenFailed, ProviderID: c.ProviderID, FromStreamID: c.StreamID, Message: err.Error()})
				continue
			}

			res := s.stream(ctx, c, rc, spliced, lastFailed)
			rc.Close()
			slot.Release()
			if res.delivered {
				spliced = true
				failingSince = time.Now()
			}
			if ctx.Err() != nil {
				return nil
			}
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
			s.emit(Event{Type: EventExhausted, FromStreamID: lastFailed, Message: ErrExhausted.Error()})
			return ErrExhausted
		}
		if !played {
			s.setState("waiting", nil)
			select {
			case <-ctx.Done():
				return nil
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
// watchdog closes rc when tswatch judges the upstream unhealthy, which
// unblocks the read.
func (s *Session) stream(ctx context.Context, c Candidate, rc io.ReadCloser, spliced bool, failedID int64) streamResult {
	w := NewWatcher(s.cfg.Thresholds, time.Now())
	var verdict atomic.Int32
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
	buf := make([]byte, 64*1024)
	var out []byte
	var res streamResult
	var firstAt time.Time
	stableCleared := false
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			now := time.Now()
			out = sp.process(w.Feed(now, out[:0], buf[:n]))
			if len(out) > 0 {
				if _, werr := s.cfg.Ring.Write(out); werr != nil {
					res.reason = "ring closed"
					return res
				}
				if !res.delivered {
					res.delivered, firstAt = true, now
					s.setState("streaming", &c)
					s.log.Info("session: streaming", "stream_id", c.StreamID, "provider_id", c.ProviderID)
					if failedID != 0 {
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
