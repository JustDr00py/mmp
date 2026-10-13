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

	Log     *slog.Logger
	OnEvent func(Event)
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
		rd := run.ring.NewReader(m.cfg.JoinBacklog)
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
	run.idle = time.AfterFunc(m.cfg.IdleGrace, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		// A viewer may have attached while this timer was firing.
		if run.viewers == 0 && !run.closing && m.channels[run.id] == run {
			m.teardown(run)
		}
	})
}

// teardown cancels run's session; it stays listed (closing) until its
// upstream slot is released. Callers hold m.mu.
func (m *Manager) teardown(run *channelRun) {
	run.closing = true
	run.cancel()
	m.log.Info("proxy: teardown", "channel_id", run.id)
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
