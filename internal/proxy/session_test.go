package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- synthetic TS with provider tags -------------------------------------

const (
	pidPCR  = 0x100
	pidData = 0x101
)

func patPkt(cc uint8) []byte {
	b := tsPkt(0, cc)
	b[1] |= 0x40 // payload_unit_start
	return b
}

// pcrPkt carries an adaptation field (as PCR packets do) and tag at byte 12.
func pcrPkt(tag byte, cc uint8) []byte {
	b := tsPkt(pidPCR, cc)
	b[3] = 0x30 | cc&0x0F // adaptation + payload
	b[4] = 7              // adaptation_field_length
	b[5] = 0x10           // PCR flag
	b[12] = tag
	return b
}

func dataPkt(tag byte, cc uint8) []byte {
	b := tsPkt(pidData, cc)
	b[4] = tag
	return b
}

// tagOf returns the provider tag of a PCR/data packet, 0 for others.
func tagOf(p []byte) byte {
	switch PID(p) {
	case pidPCR:
		return p[12]
	case pidData:
		return p[4]
	}
	return 0
}

// fakeProvider serves a live TS tagged with tag. mode: "ok" streams until
// the client leaves, "stall" sends n packets then goes silent, "eof" sends n
// packets then ends, "503" refuses.
type fakeProvider struct {
	*httptest.Server
	hits atomic.Int32
	open atomic.Int32 // requests currently being served
}

func newFakeProvider(t *testing.T, tag byte, mode string, n int) *fakeProvider {
	t.Helper()
	fp := &fakeProvider{}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.hits.Add(1)
		fp.open.Add(1)
		defer fp.open.Add(-1)
		if mode == "503" {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		var cc [3]uint8
		sent := 0
		for {
			// One "GOP": PAT, PCR, 8 data packets.
			group := [][]byte{patPkt(cc[0]), pcrPkt(tag, cc[1])}
			cc[0]++
			cc[1]++
			for range 8 {
				group = append(group, dataPkt(tag, cc[2]))
				cc[2]++
			}
			for _, p := range group {
				if (mode == "stall" || mode == "eof") && sent >= n {
					w.(http.Flusher).Flush()
					if mode == "stall" {
						<-r.Context().Done()
					}
					return
				}
				if _, err := w.Write(p); err != nil {
					return
				}
				sent++
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(fp.Close)
	return fp
}

// --- harness ---------------------------------------------------------------

type harness struct {
	ring   *Ring
	cap    *Capacity
	cool   *Cooldowns
	sess   *Session
	viewer *Reader
	runErr chan error
	cancel context.CancelFunc

	mu     sync.Mutex
	events []Event
}

func startSession(t *testing.T, cands []Candidate, tune func(*SessionConfig)) *harness {
	t.Helper()
	h := &harness{ring: NewRing(1 << 20), cap: NewCapacity(), cool: NewCooldowns(5*time.Second, time.Minute), runErr: make(chan error, 1)}
	h.viewer = h.ring.NewReader(0) // attached before the session starts
	cfg := SessionConfig{
		ChannelID: 42, Ring: h.ring, Capacity: h.cap, Cooldowns: h.cool,
		Source:         func(context.Context) ([]Candidate, error) { return cands, nil },
		Opener:         NewHTTPOpener(time.Second),
		Thresholds:     Thresholds{StallTimeout: 300 * time.Millisecond, Window: time.Second},
		MaxRetryWindow: 5 * time.Second,
		RetryBackoff:   50 * time.Millisecond,
		OnEvent: func(e Event) {
			h.mu.Lock()
			h.events = append(h.events, e)
			h.mu.Unlock()
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	h.cool = cfg.Cooldowns
	h.sess = NewSession(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.runErr <- h.sess.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.runErr:
		case <-time.After(5 * time.Second):
			t.Error("session did not stop")
		}
	})
	return h
}

// stop cancels the session and returns Run's result.
func (h *harness) stop(t *testing.T) error {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.runErr:
		h.runErr <- err // leave it for Cleanup
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
		return nil
	}
}

func (h *harness) eventsOf(typ string) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Event
	for _, e := range h.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// watch reads viewer packets until stop returns true, the reader ends, or
// the deadline passes. It returns copies of every packet read.
func (h *harness) watch(t *testing.T, timeout time.Duration, stop func(pkts [][]byte) bool) ([][]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var pkts [][]byte
	buf := make([]byte, 16*TSPacketSize)
	var pending []byte
	for {
		n, err := h.viewer.Read(ctx, buf)
		pending = append(pending, buf[:n]...)
		for len(pending) >= TSPacketSize {
			pkts = append(pkts, append([]byte(nil), pending[:TSPacketSize]...))
			pending = pending[TSPacketSize:]
		}
		if stop(pkts) {
			return pkts, nil
		}
		if err != nil {
			return pkts, err
		}
	}
}

func countTag(pkts [][]byte, tag byte) int {
	n := 0
	for _, p := range pkts {
		if tagOf(p) == tag {
			n++
		}
	}
	return n
}

// --- tests -----------------------------------------------------------------

// TestSessionFailoverKeepsClientContinuous is the 4.5 AC: whatever kills
// the preferred upstream (stall, EOF, 503, slot-full), the viewer's stream
// keeps going on the next candidate without ever seeing EOF.
func TestSessionFailoverKeepsClientContinuous(t *testing.T) {
	tests := []struct {
		name        string
		modeA       string
		holdSlotA   bool
		wantA       bool   // viewer sees A's packets first
		wantFailEvt string // event type naming stream 1 as the failure
		wantCoolA   bool
	}{
		{"stall", "stall", false, true, EventFailover, true},
		{"eof", "eof", false, true, EventFailover, true},
		{"503", "503", false, false, EventOpenFailed, true},
		{"slot full", "ok", true, false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFakeProvider(t, 'A', tt.modeA, 400)
			b := newFakeProvider(t, 'B', "ok", 0)
			cands := []Candidate{
				{StreamID: 1, ProviderID: 1, MaxConnections: 1, URL: a.URL + "/live/u/pw/1.ts"},
				{StreamID: 2, ProviderID: 2, MaxConnections: 1, URL: b.URL + "/live/u/pw/2.ts"},
			}
			var held *Slot
			h := startSession(t, cands, func(cfg *SessionConfig) {
				if tt.holdSlotA {
					held, _ = cfg.Capacity.TryAcquire(1, 1) // another session owns A's only slot
				}
			})

			pkts, err := h.watch(t, 10*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 200 })
			if err != nil {
				t.Fatalf("viewer stream broke: %v (A=%d B=%d packets)", err, countTag(pkts, 'A'), countTag(pkts, 'B'))
			}
			if PID(pkts[0]) != 0 {
				t.Errorf("stream does not start at a PAT (pid %#x)", PID(pkts[0]))
			}
			if gotA := countTag(pkts, 'A') > 0; gotA != tt.wantA {
				t.Errorf("saw A packets = %v, want %v", gotA, tt.wantA)
			}

			// Splice: after the last A packet the next packet is a PAT, and
			// B's first PCR packet carries discontinuity_indicator.
			lastA := -1
			for i, p := range pkts {
				switch tagOf(p) {
				case 'A':
					lastA = i
					if PID(p) == pidPCR && p[5]&0x80 != 0 {
						t.Fatalf("A packet %d flagged discontinuity", i)
					}
				case 'B':
					if i < lastA {
						t.Fatal("A and B interleaved")
					}
				}
			}
			if lastA >= 0 {
				if PID(pkts[lastA+1]) != 0 {
					t.Errorf("switch does not start at a PAT: pid %#x", PID(pkts[lastA+1]))
				}
				for _, p := range pkts[lastA+1:] {
					if PID(p) == pidPCR {
						if p[5]&0x80 == 0 {
							t.Error("first PCR after switch lacks discontinuity_indicator")
						}
						break
					}
				}
			}

			st := h.sess.Status()
			if st.State != "streaming" || st.StreamID != 2 || st.ProviderID != 2 {
				t.Errorf("status = %+v", st)
			}
			if tt.wantFailEvt != "" {
				evs := h.eventsOf(tt.wantFailEvt)
				if len(evs) == 0 || evs[0].FromStreamID != 1 || evs[0].ChannelID != 42 {
					t.Errorf("%s events = %+v", tt.wantFailEvt, evs)
				} else if tt.wantFailEvt == EventFailover && evs[0].ToStreamID != 2 {
					t.Errorf("failover to = %d, want 2", evs[0].ToStreamID)
				}
			}
			if got := h.cool.Active(1, time.Now()); got != tt.wantCoolA {
				t.Errorf("A cooldown active = %v, want %v", got, tt.wantCoolA)
			}
			if tt.holdSlotA && a.hits.Load() != 0 {
				t.Errorf("full provider A was contacted %d times", a.hits.Load())
			}
			// One upstream per channel: B opened exactly once, holding 1 slot.
			if b.hits.Load() != 1 {
				t.Errorf("B opened %d times", b.hits.Load())
			}
			if snap := h.cap.Snapshot(); snap[2] != 1 || (!tt.holdSlotA && snap[1] != 0) {
				t.Errorf("slots = %v", snap)
			}

			// Teardown: Run returns nil, slots released, viewer gets EOF.
			if err := h.stop(t); err != nil {
				t.Errorf("Run = %v, want nil on cancel", err)
			}
			if n := h.cap.InUse(2); n != 0 {
				t.Errorf("slot leaked after teardown: %d", n)
			}
			if held != nil {
				held.Release()
			}
			if _, err := h.watch(t, 5*time.Second, func([][]byte) bool { return false }); !errors.Is(err, io.EOF) {
				t.Errorf("viewer after teardown: %v, want EOF", err)
			}
		})
	}
}

func TestSessionExhausted(t *testing.T) {
	a := newFakeProvider(t, 'A', "503", 0)
	b := newFakeProvider(t, 'B', "stall", 0) // headers, then nothing
	cands := []Candidate{
		{StreamID: 1, ProviderID: 1, URL: a.URL},
		{StreamID: 2, ProviderID: 2, URL: b.URL},
	}
	h := startSession(t, cands, func(cfg *SessionConfig) {
		cfg.MaxRetryWindow = 800 * time.Millisecond
		cfg.Cooldowns = NewCooldowns(50*time.Millisecond, 200*time.Millisecond)
		cfg.Thresholds.StallTimeout = 200 * time.Millisecond
	})
	start := time.Now()
	select {
	case err := <-h.runErr:
		h.runErr <- err
		if !errors.Is(err, ErrExhausted) {
			t.Fatalf("Run = %v, want ErrExhausted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session never gave up")
	}
	if d := time.Since(start); d < 800*time.Millisecond {
		t.Errorf("gave up after %v, before the retry window", d)
	}
	if _, err := h.watch(t, time.Second, func([][]byte) bool { return false }); !errors.Is(err, io.EOF) {
		t.Errorf("viewer = %v, want EOF once exhausted", err)
	}
	if n := len(h.eventsOf(EventExhausted)); n != 1 {
		t.Errorf("exhausted events = %d", n)
	}
	if a.hits.Load() < 2 {
		t.Errorf("A tried %d times; expected retries after cooldown", a.hits.Load())
	}
	if snap := h.cap.Snapshot(); len(snap) != 0 {
		t.Errorf("slots leaked: %v", snap)
	}
}

// TestSessionNoCandidateWaitsThenStarts: with every provider full the
// session waits (no upstream contact) and starts as soon as a slot frees.
func TestSessionNoCandidateWaitsThenStarts(t *testing.T) {
	a := newFakeProvider(t, 'A', "ok", 0)
	var held *Slot
	h := startSession(t, []Candidate{{StreamID: 1, ProviderID: 1, MaxConnections: 1, URL: a.URL}},
		func(cfg *SessionConfig) { held, _ = cfg.Capacity.TryAcquire(1, 1) })

	time.Sleep(300 * time.Millisecond)
	if st := h.sess.Status(); st.State != "waiting" || a.hits.Load() != 0 {
		t.Fatalf("status %+v hits %d, want waiting with no contact", st, a.hits.Load())
	}
	held.Release()
	if _, err := h.watch(t, 5*time.Second, func(p [][]byte) bool { return countTag(p, 'A') >= 50 }); err != nil {
		t.Fatalf("did not start after slot freed: %v", err)
	}
}

func TestSplicer(t *testing.T) {
	cat := func(ps ...[]byte) []byte {
		var b []byte
		for _, p := range ps {
			b = append(b, p...)
		}
		return b
	}
	// Initial start: drop until PAT, no discontinuity marking.
	sp := newSplicer(false)
	out := sp.process(cat(dataPkt('X', 0), pcrPkt('X', 0), patPkt(0), pcrPkt('X', 1), dataPkt('X', 1)))
	if len(out) != 3*TSPacketSize || PID(out) != 0 {
		t.Fatalf("initial: kept %d packets starting pid %#x", len(out)/TSPacketSize, PID(out))
	}
	if out[TSPacketSize+5]&0x80 != 0 {
		t.Error("initial start must not mark discontinuity")
	}

	// After a switch: first AF packet per PID marked, later ones untouched.
	sp = newSplicer(true)
	out = sp.process(cat(patPkt(0), dataPkt('Y', 0), pcrPkt('Y', 5), pcrPkt('Y', 6)))
	if out[2*TSPacketSize+5]&0x80 == 0 {
		t.Error("first PCR after switch not marked")
	}
	if out[3*TSPacketSize+5]&0x80 != 0 {
		t.Error("second PCR wrongly marked")
	}

	// A provider that never sends a PAT is passed through after maxPATWait.
	sp = newSplicer(false)
	kept := 0
	for range maxPATWait/TSPacketSize + 10 {
		kept += len(sp.process(dataPkt('Z', 0))) / TSPacketSize
	}
	if kept == 0 || kept > 11 {
		t.Errorf("passthrough kept %d packets", kept)
	}
}

func TestCooldownsBackoff(t *testing.T) {
	c := NewCooldowns(5*time.Second, 30*time.Second)
	now := t0
	var got []time.Duration
	for range 5 {
		got = append(got, c.Fail(1, now))
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("failure %d cooldown = %v, want %v", i+1, got[i], want[i])
		}
	}
	if !c.Active(1, now.Add(29*time.Second)) || c.Active(1, now.Add(30*time.Second)) {
		t.Error("active window wrong")
	}
	if c.Active(2, now) {
		t.Error("unknown stream active")
	}
	c.Reset(1)
	if c.Active(1, now) || c.Fail(1, now) != 5*time.Second {
		t.Error("reset did not clear history")
	}
}
