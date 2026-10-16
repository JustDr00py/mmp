package proxy

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

func newTestManager(t *testing.T, srcs map[int64][]Candidate, tune func(*ManagerConfig)) *Manager {
	t.Helper()
	cfg := ManagerConfig{
		Source: func(_ context.Context, id int64) ([]Candidate, error) {
			return srcs[id], nil
		},
		Opener:         NewHTTPOpener(time.Second),
		Capacity:       NewCapacity(),
		Cooldowns:      NewCooldowns(5*time.Second, time.Minute),
		RingSize:       1 << 20,
		IdleGrace:      300 * time.Millisecond,
		Thresholds:     Thresholds{StallTimeout: 300 * time.Millisecond, Window: time.Second},
		MaxRetryWindow: 5 * time.Second,
		RetryBackoff:   50 * time.Millisecond,
	}
	if tune != nil {
		tune(&cfg)
	}
	m := NewManager(context.Background(), cfg)
	t.Cleanup(m.Shutdown)
	return m
}

// readTag reads from v until n packets carrying tag arrive.
func readTag(t *testing.T, v *Viewer, tag byte, n int, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	buf := make([]byte, 32*TSPacketSize)
	var pending []byte
	got := 0
	for got < n {
		k, err := v.Read(ctx, buf)
		pending = append(pending, buf[:k]...)
		for len(pending) >= TSPacketSize {
			if tagOf(pending[:TSPacketSize]) == tag {
				got++
			}
			pending = pending[TSPacketSize:]
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestManagerSharesOneUpstream is the 4.6 AC: N viewers -> exactly one
// upstream request, and the slot is released after idle grace.
func TestManagerSharesOneUpstream(t *testing.T) {
	p := newFakeProvider(t, 'A', "ok", 0)
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 10, ProviderID: 1, MaxConnections: 1, URL: p.URL}},
	}, nil)

	const n = 10
	viewers := make([]*Viewer, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			v, err := m.Attach(1)
			if err != nil {
				t.Error(err)
				return
			}
			viewers[i] = v
		})
	}
	wg.Wait()
	for i, v := range viewers {
		wg.Go(func() {
			if err := readTag(t, v, 'A', 100, 5*time.Second); err != nil {
				t.Errorf("viewer %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	if h := p.hits.Load(); h != 1 {
		t.Fatalf("upstream requests = %d, want 1 for %d viewers", h, n)
	}
	if got := m.cfg.Capacity.InUse(1); got != 1 {
		t.Errorf("slots in use = %d, want 1", got)
	}
	chs := m.Channels()
	if len(chs) != 1 || chs[0].Viewers != n || chs[0].State != "streaming" || chs[0].StreamID != 10 {
		t.Errorf("channels = %+v", chs)
	}

	// All leave: the session survives the grace period, then releases.
	for _, v := range viewers {
		v.Close()
		v.Close() // idempotent
	}
	time.Sleep(100 * time.Millisecond)
	if got := m.cfg.Capacity.InUse(1); got != 1 {
		t.Errorf("slot released before idle grace (in use %d)", got)
	}
	eventually(t, 3*time.Second, func() bool {
		return m.cfg.Capacity.InUse(1) == 0 && p.open.Load() == 0 && len(m.Channels()) == 0
	}, "slot/upstream not released after idle grace")
}

func TestManagerReattachWithinGraceReusesSession(t *testing.T) {
	p := newFakeProvider(t, 'A', "ok", 0)
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 10, ProviderID: 1, URL: p.URL}},
	}, nil)

	v, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := readTag(t, v, 'A', 20, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	v.Close()
	time.Sleep(100 * time.Millisecond) // < 300ms grace

	v2, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	time.Sleep(400 * time.Millisecond) // past the original grace deadline
	if err := readTag(t, v2, 'A', 20, 5*time.Second); err != nil {
		t.Fatalf("reattached viewer: %v", err)
	}
	if h := p.hits.Load(); h != 1 {
		t.Errorf("upstream requests = %d, want 1 (grace reattach)", h)
	}
}

// TestManagerReattachAfterTeardownStartsFresh: each attach after a
// teardown starts a new session, and never while the old one still holds
// the provider's only slot.
func TestManagerReattachAfterTeardownStartsFresh(t *testing.T) {
	p := newFakeProvider(t, 'A', "ok", 0)
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 10, ProviderID: 1, MaxConnections: 1, URL: p.URL}},
	}, func(c *ManagerConfig) { c.IdleGrace = 0 }) // tear down immediately

	for round := range 5 {
		v, err := m.Attach(1)
		if err != nil {
			t.Fatal(err)
		}
		if err := readTag(t, v, 'A', 10, 5*time.Second); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if n := m.cfg.Capacity.InUse(1); n != 1 {
			t.Fatalf("round %d: %d slots held", round, n)
		}
		v.Close()
	}
	if h := p.hits.Load(); h != 5 {
		t.Errorf("upstream requests = %d, want 5 (one per round)", h)
	}
	eventually(t, 3*time.Second, func() bool { return m.cfg.Capacity.InUse(1) == 0 }, "slot leaked")
}

func TestManagerChannelsIndependent(t *testing.T) {
	a := newFakeProvider(t, 'A', "ok", 0)
	b := newFakeProvider(t, 'B', "ok", 0)
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 10, ProviderID: 1, URL: a.URL}},
		2: {{StreamID: 20, ProviderID: 2, URL: b.URL}},
	}, nil)
	va, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	defer va.Close()
	vb, err := m.Attach(2)
	if err != nil {
		t.Fatal(err)
	}
	defer vb.Close()
	if err := readTag(t, va, 'A', 30, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := readTag(t, vb, 'B', 30, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if va.ChannelID() != 1 || vb.ChannelID() != 2 || len(m.Channels()) != 2 {
		t.Errorf("channels = %+v", m.Channels())
	}
}

// TestManagerExhaustedSessionEndsViewers: when a channel runs out of
// candidates its viewers get EOF and the next Attach starts a new session.
func TestManagerExhaustedSessionEndsViewers(t *testing.T) {
	bad := newFakeProvider(t, 'X', "503", 0)
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 10, ProviderID: 1, URL: bad.URL}},
	}, func(c *ManagerConfig) {
		c.MaxRetryWindow = 300 * time.Millisecond
		c.Cooldowns = NewCooldowns(20*time.Millisecond, 50*time.Millisecond)
	})
	v, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := readTag(t, v, 'X', 1, 5*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("viewer = %v, want EOF after exhaustion", err)
	}
	v.Close() // after the session is gone: harmless
	eventually(t, 2*time.Second, func() bool { return len(m.Channels()) == 0 }, "exhausted session still listed")

	before := bad.hits.Load()
	v2, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	eventually(t, 2*time.Second, func() bool { return bad.hits.Load() > before }, "no new session after exhaustion")
}

func TestManagerShutdown(t *testing.T) {
	p := newFakeProvider(t, 'A', "ok", 0)
	m := newTestManager(t, map[int64][]Candidate{
		1: {{StreamID: 10, ProviderID: 1, URL: p.URL}},
	}, nil)
	v, err := m.Attach(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := readTag(t, v, 'A', 10, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	m.Shutdown()
	if err := readTag(t, v, 'Z', 1<<20, 5*time.Second); !errors.Is(err, io.EOF) {
		t.Errorf("viewer after shutdown = %v, want EOF", err)
	}
	if m.cfg.Capacity.InUse(1) != 0 {
		t.Error("slot held after shutdown")
	}
	eventually(t, 2*time.Second, func() bool { return p.open.Load() == 0 }, "upstream open after shutdown")
	if _, err := m.Attach(1); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("attach after shutdown = %v", err)
	}
	v.Close()
}

// TestManagerAttachDetachStress races attaches, detaches and idle teardowns
// on a few channels; at most one slot is ever held per channel's provider.
func TestManagerAttachDetachStress(t *testing.T) {
	srcs := map[int64][]Candidate{}
	for id := int64(1); id <= 3; id++ {
		p := newFakeProvider(t, byte('A'+id), "ok", 0)
		srcs[id] = []Candidate{{StreamID: id * 10, ProviderID: id, MaxConnections: 0, URL: p.URL}}
	}
	m := newTestManager(t, srcs, func(c *ManagerConfig) { c.IdleGrace = 20 * time.Millisecond })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 12 {
		wg.Go(func() {
			id := int64(w%3 + 1)
			for {
				select {
				case <-stop:
					return
				default:
				}
				v, err := m.Attach(id)
				if err != nil {
					t.Error(err)
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				v.Read(ctx, make([]byte, TSPacketSize)) //nolint:errcheck // timing only
				cancel()
				v.Close()
				time.Sleep(time.Duration(w) * 3 * time.Millisecond)
			}
		})
	}
	// Limits are unlimited (0) so the capacity table itself never refuses:
	// any second session per channel would show up as 2 slots held.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for id := range srcs {
			if n := m.cfg.Capacity.InUse(id); n > 1 {
				t.Fatalf("channel %d holds %d upstream slots", id, n)
			}
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	m.Shutdown()
	for id := range srcs {
		if n := m.cfg.Capacity.InUse(id); n != 0 {
			t.Errorf("provider %d slots leaked: %d", id, n)
		}
	}
}

func TestSortedByHealth(t *testing.T) {
	cands := []Candidate{
		{StreamID: 1, Health: 0.42}, // first by priority, but failing
		{StreamID: 2, Health: 0.97}, // rounds to the same tier as 3
		{StreamID: 3, Health: 1},
		{StreamID: 4, Health: 0.75},
	}
	src := func(context.Context, int64) ([]Candidate, error) { return slices.Clone(cands), nil }
	on := true
	sorted := SortedByHealth(src, func() bool { return on })
	ids := func() []int64 {
		got, err := sorted(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, c := range got {
			out = append(out, c.StreamID)
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []int64{2, 3, 4, 1}) {
		t.Errorf("sorted = %v, want [2 3 4 1] (2 and 3 tie: priority order kept)", got)
	}
	on = false // the setting is read per load
	if got := ids(); !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Errorf("unsorted = %v, want priority order", got)
	}
	failing := SortedByHealth(func(context.Context, int64) ([]Candidate, error) { return nil, errors.New("db down") }, func() bool { return true })
	if _, err := failing(context.Background(), 1); err == nil {
		t.Error("source error swallowed")
	}
}

// TestSortByHealthPicksHealthierStream: with the setting on, a session
// opens the healthier stream even though it is second by priority.
func TestSortByHealthPicksHealthierStream(t *testing.T) {
	for _, on := range []bool{false, true} {
		var mu sync.Mutex
		var opened []int64
		cands := map[int64][]Candidate{1: {{StreamID: 10, ProviderID: 1, Health: 0.3}, {StreamID: 20, ProviderID: 2, Health: 1}}}
		m := newTestManager(t, cands, func(cfg *ManagerConfig) {
			cfg.Source = SortedByHealth(cfg.Source, func() bool { return on })
			cfg.Opener = openerFunc(func(ctx context.Context, c Candidate) (io.ReadCloser, error) {
				mu.Lock()
				opened = append(opened, c.StreamID)
				mu.Unlock()
				pr, pw := io.Pipe()
				go func() {
					pkt := make([]byte, TSPacketSize)
					pkt[0] = 0x47
					for ctx.Err() == nil {
						if _, err := pw.Write(pkt); err != nil {
							return
						}
						time.Sleep(time.Millisecond)
					}
					pw.Close()
				}()
				return pr, nil
			})
		})
		v, err := m.Attach(1)
		if err != nil {
			t.Fatal(err)
		}
		var first int64
		for deadline := time.Now().Add(2 * time.Second); first == 0; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("no upstream opened")
			}
			mu.Lock()
			if len(opened) > 0 {
				first = opened[0]
			}
			mu.Unlock()
		}
		v.Close()
		want := int64(10)
		if on {
			want = 20
		}
		if first != want {
			t.Errorf("sort_by_health=%v: opened stream %d first, want %d", on, first, want)
		}
	}
}

// TestManagerStopAndSwitch: Stop ends a channel's session and its viewers;
// Switch reaches the session; both report unknown channels.
func TestManagerStopAndSwitch(t *testing.T) {
	a := newFakeProvider(t, 'A', "ok", 0)
	b := newFakeProvider(t, 'B', "ok", 0)
	m := newTestManager(t, map[int64][]Candidate{
		7: {{StreamID: 1, ProviderID: 1, URL: a.URL}, {StreamID: 2, ProviderID: 2, URL: b.URL}},
	}, nil)
	v, err := m.Attach(7)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := readTag(t, v, 'A', 50, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if m.Switch(99, 1) || m.Stop(99) {
		t.Error("unknown channel reported as running")
	}
	if !m.Switch(7, 2) {
		t.Fatal("Switch on a running channel = false")
	}
	if err := readTag(t, v, 'B', 50, 5*time.Second); err != nil {
		t.Fatalf("no B after Switch: %v", err)
	}
	if !m.Stop(7) {
		t.Fatal("Stop on a running channel = false")
	}
	if err := readTag(t, v, 'B', 1<<30, 5*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("viewer after Stop: %v, want EOF", err)
	}
	eventually(t, 5*time.Second, func() bool { return len(m.Channels()) == 0 }, "channel still listed after Stop")
	eventually(t, 5*time.Second, func() bool { return a.open.Load()+b.open.Load() == 0 }, "upstream still open after Stop")
}
