package proxy

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// ErrManagerClosed is returned by Attach after Shutdown.
var ErrManagerClosed = errors.New("proxy manager closed")

// ChannelSource loads a channel's failover list in priority order.
type ChannelSource func(ctx context.Context, channelID int64) ([]Candidate, error)

// ManagerConfig wires the manager. Source, Opener, Capacity and Cooldowns
// are required.
type ManagerConfig struct {
	Source    ChannelSource
	Opener    Opener
	Capacity  *Capacity
	Cooldowns *Cooldowns

	RingSize       int           // bytes per channel ring
	JoinBacklog    int           // bytes of buffered stream a new viewer starts with
	IdleGrace      time.Duration // keep a session this long after its last viewer leaves
	Thresholds     Thresholds
	MaxRetryWindow time.Duration
	RetryBackoff   time.Duration
	StableAfter    time.Duration

	Log      *slog.Logger
	OnEvent  func(Event)
	Metrics  *Metrics   // optional
	Outcomes *Outcomes  // optional: per-provider results for the health scores
	Probe    MediaProbe // optional: resolution, frame rate and audio layout for the dashboard
}

// Manager owns one Session per active channel and fans its ring out to
// every viewer of that channel: N viewers, one upstream connection.
type Manager struct {
	cfg ManagerConfig
	log *slog.Logger
	ctx context.Context // parent of every session

	mu       sync.Mutex
	closed   bool
	channels map[int64]*channelRun
	wg       sync.WaitGroup
}

// channelRun is one live (or closing) session and its audience.
type channelRun struct {
	id      int64
	sess    *Session
	ring    *Ring
	cancel  context.CancelFunc
	viewers int
	idle    *time.Timer
	idleAt  time.Time     // when the last viewer left, while idle is set
	closing bool          // teardown started; new viewers wait for done
	done    chan struct{} // closed once Run has returned and slots are released
}

// NewManager builds a manager whose sessions live at most as long as ctx.
func NewManager(ctx context.Context, cfg ManagerConfig) *Manager {
	if cfg.RingSize <= 0 {
		cfg.RingSize = 32 << 20
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg, log: cfg.Log, ctx: ctx, channels: map[int64]*channelRun{}}
}

// Attach joins a viewer to channelID, starting its session if needed. The
// caller must Close the viewer when the client goes away.
func (m *Manager) Attach(channelID int64) (*Viewer, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrManagerClosed
		}
		run, ok := m.channels[channelID]
		if ok && run.closing {
			// Wait for the old upstream to be released before opening a new
			// one: never two provider connections for one channel.
			done := run.done
			m.mu.Unlock()
			<-done
			continue
		}
		if !ok {
			run = m.start(channelID)
		}
		run.viewers++
		if run.idle != nil {
			run.idle.Stop()
			run.idle = nil
		}
		rd := run.ring.NewReaderAtKeyframe(m.cfg.JoinBacklog)
		m.mu.Unlock()
		return &Viewer{Reader: rd, m: m, run: run}, nil
	}
}

// start launches a session for channelID. Callers hold m.mu.
func (m *Manager) start(channelID int64) *channelRun {
	ctx, cancel := context.WithCancel(m.ctx)
	ring := NewRing(m.cfg.RingSize)
	run := &channelRun{id: channelID, ring: ring, cancel: cancel, done: make(chan struct{})}
	run.sess = NewSession(SessionConfig{
		ChannelID: channelID,
		Ring:      ring,
		Source: func(ctx context.Context) ([]Candidate, error) {
			return m.cfg.Source(ctx, channelID)
		},
		Opener:         m.cfg.Opener,
		Capacity:       m.cfg.Capacity,
		Cooldowns:      m.cfg.Cooldowns,
		Thresholds:     m.cfg.Thresholds,
		MaxRetryWindow: m.cfg.MaxRetryWindow,
		RetryBackoff:   m.cfg.RetryBackoff,
		StableAfter:    m.cfg.StableAfter,
		Log:            m.log,
		OnEvent:        m.cfg.OnEvent,
		Metrics:        m.cfg.Metrics,
		Outcomes:       m.cfg.Outcomes,
		Probe:          m.cfg.Probe,
		Reclaim:        m.reclaimIdle,
	})
	m.channels[channelID] = run
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := run.sess.Run(ctx)
		cancel()
		m.mu.Lock()
		if m.channels[channelID] == run {
			delete(m.channels, channelID)
		}
		if run.idle != nil {
			run.idle.Stop()
		}
		m.mu.Unlock()
		close(run.done)
		m.log.Info("proxy: session ended", "channel_id", channelID, "err", err)
	}()
	m.log.Info("proxy: session started", "channel_id", channelID)
	return run
}

// detach drops one viewer and schedules teardown when none remain.
func (m *Manager) detach(run *channelRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run.viewers--
	if run.viewers > 0 || run.closing || m.channels[run.id] != run {
		return // still watched, already closing, or the session already ended
	}
	if m.cfg.IdleGrace <= 0 {
		m.teardown(run)
		return
	}
	run.idleAt = time.Now()
	run.idle = time.AfterFunc(m.cfg.IdleGrace, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		// A viewer may have attached while this timer was firing.
		if run.viewers == 0 && !run.closing && m.channels[run.id] == run {
			m.teardown(run)
		}
	})
}

// reclaimWait bounds how long a reclaim waits for an idle session's slot.
const reclaimWait = 5 * time.Second

// reclaimIdle frees a slot at providerID held by a session nobody watches
// (one waiting out its idle grace), so that a channel someone tunes to is
// not refused for slots kept warm for channels they flipped past. It tears
// down the longest-idle such session and waits until its slot is released,
// reporting whether one was.
func (m *Manager) reclaimIdle(ctx context.Context, providerID int64) bool {
	m.mu.Lock()
	var idle []*channelRun
	for _, run := range m.channels {
		if run.viewers == 0 && run.idle != nil && !run.closing {
			idle = append(idle, run)
		}
	}
	m.mu.Unlock()
	// Session.Status takes its own lock, so filter outside m.mu.
	idle = slices.DeleteFunc(idle, func(run *channelRun) bool {
		st := run.sess.Status()
		return st.ProviderID != providerID || (st.State != "streaming" && st.State != "connecting")
	})
	slices.SortFunc(idle, func(a, b *channelRun) int { return a.idleAt.Compare(b.idleAt) })

	m.mu.Lock()
	var victim *channelRun
	for _, run := range idle {
		if run.viewers == 0 && run.idle != nil && !run.closing { // still idle
			victim = run
			break
		}
	}
	if victim == nil {
		m.mu.Unlock()
		return false
	}
	victim.idle.Stop()
	victim.idle = nil
	m.teardown(victim)
	m.mu.Unlock()
	m.log.Info("proxy: reclaimed an idle session's slot", "channel_id", victim.id, "provider_id", providerID)

	t := time.NewTimer(reclaimWait)
	defer t.Stop()
	select {
	case <-victim.done:
		return true
	case <-ctx.Done():
	case <-t.C:
	}
	return false
}

// teardown cancels run's session; it stays listed (closing) until its
// upstream slot is released. Callers hold m.mu.
func (m *Manager) teardown(run *channelRun) {
	run.closing = true
	run.cancel()
	m.log.Info("proxy: teardown", "channel_id", run.id)
}

// Stop ends channelID's session now: its upstream is closed and its viewers
// get EOF. Players that reconnect start a fresh session. It reports whether
// the channel was running.
func (m *Manager) Stop(channelID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.channels[channelID]
	if !ok || run.closing {
		return false
	}
	if run.idle != nil {
		run.idle.Stop()
		run.idle = nil
	}
	m.teardown(run)
	return true
}

// Switch moves channelID's session to streamID (see Session.Switch). It
// reports whether the channel was running.
func (m *Manager) Switch(channelID, streamID int64) bool {
	m.mu.Lock()
	run, ok := m.channels[channelID]
	m.mu.Unlock()
	if !ok || run.closing {
		return false
	}
	run.sess.Switch(streamID)
	return true
}

// ChannelStatus is one active channel for the dashboard.
type ChannelStatus struct {
	SessionStatus
	Viewers int  `json:"viewers"`
	Closing bool `json:"closing"`
}

// Channels lists active channels ordered by channel id.
func (m *Manager) Channels() []ChannelStatus {
	m.mu.Lock()
	out := make([]ChannelStatus, 0, len(m.channels))
	sessions := make([]*Session, 0, len(m.channels))
	for _, r := range m.channels {
		out = append(out, ChannelStatus{Viewers: r.viewers, Closing: r.closing})
		sessions = append(sessions, r.sess)
	}
	m.mu.Unlock()
	for i, s := range sessions { // Session.Status takes its own lock
		out[i].SessionStatus = s.Status()
	}
	slices.SortFunc(out, func(a, b ChannelStatus) int { return cmp.Compare(a.ChannelID, b.ChannelID) })
	return out
}

// BusyStreams returns the streams sessions are playing right now.
func (m *Manager) BusyStreams() map[int64]bool {
	out := map[int64]bool{}
	for _, c := range m.Channels() {
		if c.State == "streaming" && c.StreamID != 0 {
			out[c.StreamID] = true
		}
	}
	return out
}

// Shutdown stops every session and waits until their upstreams are closed
// and slots released. Viewers receive EOF; Attach fails afterwards.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	for _, run := range m.channels {
		if !run.closing {
			m.teardown(run)
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Viewer is one client's view of a channel. Read it until EOF or until the
// client leaves, then Close it.
type Viewer struct {
	*Reader
	m    *Manager
	run  *channelRun
	once sync.Once
}

// ChannelID is the channel this viewer watches.
func (v *Viewer) ChannelID() int64 { return v.run.id }

// Close detaches the viewer. It is idempotent.
func (v *Viewer) Close() {
	v.once.Do(func() { v.m.detach(v.run) })
}
