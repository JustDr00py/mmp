package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
// packets then ends, "eof-then-503" does that once and then refuses, "503"
// refuses, "429" refuses for lack of connections.
type fakeProvider struct {
	*httptest.Server
	hits atomic.Int32
	open atomic.Int32 // requests currently being served
}

func newFakeProvider(t *testing.T, tag byte, mode string, n int) *fakeProvider {
	t.Helper()
	fp := &fakeProvider{}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := fp.hits.Add(1)
		fp.open.Add(1)
		defer fp.open.Add(-1)
		if mode == "503" || (mode == "eof-then-503" && hit > 1) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if mode == "429" {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "max connections", http.StatusTooManyRequests)
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
				if (mode == "stall" || mode == "eof" || mode == "eof-then-503") && sent >= n {
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
		wantWhy     string // in the event's message
	}{
		{"stall", "stall", false, true, EventFailover, true, "no data for "},
		{"eof", "eof", false, true, EventFailover, true, "the provider ended the stream"},
		{"503", "503", false, false, EventOpenFailed, true, "503"},
		{"slot full", "ok", true, false, "", false, ""},
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
				} else if !strings.Contains(evs[0].Message, tt.wantWhy) {
					t.Errorf("%s message = %q, want it to say %q", tt.wantFailEvt, evs[0].Message, tt.wantWhy)
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
	if evs := h.eventsOf(EventExhausted); len(evs) != 1 || !strings.Contains(evs[0].Message, "no stream played for ") ||
		!strings.Contains(evs[0].Message, "last problem: ") {
		t.Errorf("exhausted events = %+v", evs)
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

// TestFailoverTiming (PLAN verification): the viewer keeps receiving, and
// B takes over within stall_timeout + ε of A stalling, or within ε when A
// fails outright or has no free slot.
func TestFailoverTiming(t *testing.T) {
	const stall = 300 * time.Millisecond
	const eps = 700 * time.Millisecond
	for _, tc := range []struct {
		name, modeA string
		holdSlot    bool
		minGap      time.Duration // lower bound on the A→B gap (stall must actually be detected)
		maxGap      time.Duration
	}{
		{"stall", "stall", false, stall * 8 / 10, stall + eps},
		{"eof", "eof", false, 0, eps},
		{"503", "503", false, 0, eps},
		{"slot full", "ok", true, 0, eps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newFakeProvider(t, 'A', tc.modeA, 400)
			b := newFakeProvider(t, 'B', "ok", 0)
			start := time.Now()
			h := startSession(t, []Candidate{
				{StreamID: 1, ProviderID: 1, MaxConnections: 1, URL: a.URL + "/1.ts"},
				{StreamID: 2, ProviderID: 2, MaxConnections: 1, URL: b.URL + "/2.ts"},
			}, func(cfg *SessionConfig) {
				cfg.Thresholds.StallTimeout = stall
				if tc.holdSlot {
					cfg.Capacity.TryAcquire(1, 1)
				}
			})
			var lastA, firstB time.Time
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			buf := make([]byte, TSPacketSize)
			for firstB.IsZero() {
				if _, err := io.ReadFull(readerCtx{ctx, h.viewer}, buf); err != nil {
					t.Fatalf("viewer stream broke: %v", err)
				}
				switch tagOf(buf) {
				case 'A':
					lastA = time.Now()
				case 'B':
					firstB = time.Now()
				}
			}
			from := lastA
			if from.IsZero() { // A never played: measure from session start
				from = start
			}
			if gap := firstB.Sub(from); gap < tc.minGap || gap > tc.maxGap {
				t.Errorf("A→B gap %v, want within [%v, %v]", gap, tc.minGap, tc.maxGap)
			}
			// And the viewer keeps receiving B afterwards on the same reader.
			if _, err := h.watch(t, 3*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 100 }); err != nil {
				t.Errorf("viewer stopped after the switch: %v", err)
			}
		})
	}
}

// readerCtx adapts a ring Reader to io.Reader for io.ReadFull.
type readerCtx struct {
	ctx context.Context
	rd  *Reader
}

func (r readerCtx) Read(p []byte) (int, error) { return r.rd.Read(r.ctx, p) }

// TestSessionSwitch: an admin's switch moves the session to the chosen
// stream without counting a failure or cooling the old one, and the
// session reports bytes and bitrate as it goes.
func TestSessionSwitch(t *testing.T) {
	a := newFakeProvider(t, 'A', "ok", 0)
	b := newFakeProvider(t, 'B', "ok", 0)
	h := startSession(t, []Candidate{
		{StreamID: 1, ProviderID: 1, MaxConnections: 1, URL: a.URL},
		{StreamID: 2, ProviderID: 2, MaxConnections: 1, URL: b.URL},
	}, nil)
	if _, err := h.watch(t, 5*time.Second, func(p [][]byte) bool { return countTag(p, 'A') >= 100 }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // a second of rate samples
	st := h.sess.Status()
	if st.StreamID != 1 || st.Bytes == 0 || st.Kbps <= 0 || st.AvgKbps <= 0 || st.Started.IsZero() {
		t.Fatalf("status = %+v, want stream 1 with bytes and bitrate", st)
	}

	h.sess.Switch(1) // already playing: nothing happens
	h.sess.Switch(2)
	if _, err := h.watch(t, 5*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 100 }); err != nil {
		t.Fatalf("no B after switch: %v", err)
	}
	st = h.sess.Status()
	if st.StreamID != 2 || st.State != "streaming" || st.Failovers != 0 {
		t.Errorf("after switch: %+v, want streaming stream 2 with no failovers", st)
	}
	if h.cool.Active(1, time.Now()) {
		t.Error("switching away cooled stream 1 down")
	}
	if ev := h.eventsOf(EventSwitch); len(ev) != 1 || ev[0].FromStreamID != 1 || ev[0].ToStreamID != 2 {
		t.Errorf("switch events = %+v", ev)
	}
	if ev := h.eventsOf(EventFailover); len(ev) != 0 {
		t.Errorf("a switch logged failovers: %+v", ev)
	}

	// A cooling stream can still be picked by hand.
	h.cool.Fail(1, time.Now())
	h.sess.Switch(1)
	if _, err := h.watch(t, 5*time.Second, func(p [][]byte) bool { return countTag(p, 'A') >= 100 }); err != nil {
		t.Fatalf("could not switch to a cooling stream: %v", err)
	}
}

func TestShortDur(t *testing.T) {
	for d, want := range map[time.Duration]string{12 * time.Second: "12s", 90 * time.Second: "1m30s",
		2 * time.Minute: "2m", time.Hour: "1h", 1500 * time.Millisecond: "2s"} {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestSessionReconnectsAfterDrop: a stream that played well and was then
// closed by the provider is reopened before any other source is tried,
// without a cooldown or a failover.
func TestSessionReconnectsAfterDrop(t *testing.T) {
	a := newFakeProvider(t, 'A', "eof", 3000) // about 0.6s per connection
	b := newFakeProvider(t, 'B', "ok", 0)
	h := startSession(t, []Candidate{
		{StreamID: 1, ProviderID: 1, MaxConnections: 1, URL: a.URL + "/1.ts"},
		{StreamID: 2, ProviderID: 2, MaxConnections: 1, URL: b.URL + "/2.ts"},
	}, func(cfg *SessionConfig) { cfg.StableAfter = 100 * time.Millisecond })
	pkts, err := h.watch(t, 10*time.Second, func([][]byte) bool { return a.hits.Load() >= 3 })
	if err != nil {
		t.Fatalf("viewer stream broke: %v", err)
	}
	if n := countTag(pkts, 'B'); n != 0 || b.hits.Load() != 0 {
		t.Errorf("failed over to B (%d packets, %d requests) instead of reconnecting", n, b.hits.Load())
	}
	if st := h.sess.Status(); st.Failovers != 0 || st.StreamID != 1 {
		t.Errorf("status = %+v, want stream 1 with no failovers", st)
	}
	if ev := h.eventsOf(EventFailover); len(ev) != 0 {
		t.Errorf("a reconnect logged failovers: %+v", ev)
	}
}

// TestSessionReconnectFailsOver: when the reopen fails, the session fails
// over as usual, naming the original drop.
func TestSessionReconnectFailsOver(t *testing.T) {
	a := newFakeProvider(t, 'A', "eof-then-503", 3000)
	b := newFakeProvider(t, 'B', "ok", 0)
	h := startSession(t, []Candidate{
		{StreamID: 1, ProviderID: 1, MaxConnections: 1, URL: a.URL + "/1.ts"},
		{StreamID: 2, ProviderID: 2, MaxConnections: 1, URL: b.URL + "/2.ts"},
	}, func(cfg *SessionConfig) { cfg.StableAfter = 100 * time.Millisecond })
	if _, err := h.watch(t, 10*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 100 }); err != nil {
		t.Fatalf("no B after the failed reconnect: %v", err)
	}
	if n := a.hits.Load(); n != 2 {
		t.Errorf("A requests = %d, want 2 (play, one reconnect)", n)
	}
	ev := h.eventsOf(EventFailover)
	if len(ev) != 1 || ev[0].FromStreamID != 1 || ev[0].ToStreamID != 2 || ev[0].Message != "the provider ended the stream" {
		t.Errorf("failover events = %+v", ev)
	}
}

// TestSessionProviderOutOfConnections: a 429 skips the provider for its
// Retry-After without cooling the stream down.
func TestSessionProviderOutOfConnections(t *testing.T) {
	a := newFakeProvider(t, 'A', "429", 0)
	b := newFakeProvider(t, 'B', "ok", 0)
	h := startSession(t, []Candidate{
		{StreamID: 1, ProviderID: 1, MaxConnections: 5, URL: a.URL + "/1.ts"},
		{StreamID: 3, ProviderID: 1, MaxConnections: 5, URL: a.URL + "/3.ts"},
		{StreamID: 2, ProviderID: 2, MaxConnections: 1, URL: b.URL + "/2.ts"},
	}, nil)
	if _, err := h.watch(t, 5*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 100 }); err != nil {
		t.Fatalf("no B after the 429: %v", err)
	}
	if h.cool.Active(1, time.Now()) {
		t.Error("a 429 put stream 1 in cooldown")
	}
	if n := a.hits.Load(); n != 1 {
		t.Errorf("provider 1 requests = %d, want 1 (its other stream skipped while busy)", n)
	}
	if _, ok := h.cap.TryAcquire(1, 5); ok {
		t.Error("provider 1 not marked busy")
	}
	if ev := h.eventsOf(EventOpenFailed); len(ev) != 1 || !strings.Contains(ev[0].Message, "no free connection") {
		t.Errorf("open_failed events = %+v", ev)
	}
}

func TestProviderBusy(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want time.Duration
		ok   bool
	}{
		{&StatusError{Code: 429}, busyDefault, true},
		{fmt.Errorf("open x: %w", &StatusError{Code: 429, RetryAfter: 3 * time.Second}), 3 * time.Second, true},
		{&StatusError{Code: 429, RetryAfter: time.Hour}, busyMax, true},
		{&StatusError{Code: 503}, 0, false},
		{errors.New("eof"), 0, false},
		{nil, 0, false},
	} {
		if d, ok := ProviderBusy(tc.err); d != tc.want || ok != tc.ok {
			t.Errorf("ProviderBusy(%v) = %v %v, want %v %v", tc.err, d, ok, tc.want, tc.ok)
		}
	}
}

func TestCapacityBusy(t *testing.T) {
	c := NewCapacity()
	c.MarkBusy(1, time.Now().Add(150*time.Millisecond))
	if _, ok := c.TryAcquire(1, 0); ok {
		t.Fatal("busy provider gave a slot")
	}
	if s, ok := c.TryAcquire(2, 0); !ok {
		t.Fatal("other provider refused")
	} else {
		s.Release()
	}
	time.Sleep(200 * time.Millisecond)
	s, ok := c.TryAcquire(1, 0)
	if !ok {
		t.Fatal("still busy after its time")
	}
	s.Release()
}

// TestSessionFailoverSkipsBackbone: after a stream fails while playing, the
// other resellers of its backbone go behind the streams on other backbones.
func TestSessionFailoverSkipsBackbone(t *testing.T) {
	a := newFakeProvider(t, 'A', "stall", 400)
	a2 := newFakeProvider(t, 'C', "ok", 0)
	b := newFakeProvider(t, 'B', "ok", 0)
	h := startSession(t, []Candidate{
		{StreamID: 1, ProviderID: 1, Backbone: "Strong", MaxConnections: 1, URL: a.URL + "/1.ts"},
		{StreamID: 3, ProviderID: 3, Backbone: "Strong", MaxConnections: 1, URL: a2.URL + "/3.ts"},
		{StreamID: 2, ProviderID: 2, Backbone: "Trex", MaxConnections: 1, URL: b.URL + "/2.ts"},
	}, nil)
	if _, err := h.watch(t, 10*time.Second, func(p [][]byte) bool { return countTag(p, 'B') >= 100 }); err != nil {
		t.Fatalf("no B after A stalled: %v", err)
	}
	if n := a2.hits.Load(); n != 0 {
		t.Errorf("tried the failed backbone's other stream (%d requests) before another backbone", n)
	}
	ev := h.eventsOf(EventFailover)
	if len(ev) != 1 || ev[0].ToStreamID != 2 || !strings.Contains(ev[0].Message, "skipped the other streams on backbone Strong") {
		t.Errorf("failover events = %+v", ev)
	}
}

func TestAvoidBackbones(t *testing.T) {
	now := time.Now()
	cands := []Candidate{{StreamID: 1, Backbone: "X"}, {StreamID: 2}, {StreamID: 3, Backbone: "X"}, {StreamID: 4, Backbone: "Y"}}
	ids := func(cs []Candidate) (out []int64) {
		for _, c := range cs {
			out = append(out, c.StreamID)
		}
		return out
	}
	avoid := map[string]time.Time{"X": now.Add(time.Minute), "Z": now.Add(-time.Second)}
	out, jumped := avoidBackbones(slices.Clone(cands), avoid, now)
	if got := ids(out); !slices.Equal(got, []int64{2, 4, 1, 3}) {
		t.Errorf("order = %v", got)
	}
	if len(jumped) != 2 || jumped[2] != "X" || jumped[4] != "X" {
		t.Errorf("jumped = %v", jumped)
	}
	if _, ok := avoid["Z"]; ok {
		t.Error("expired backbone kept")
	}
	if out, jumped := avoidBackbones(cands, map[string]time.Time{"X": now}, now); !slices.Equal(ids(out), []int64{1, 2, 3, 4}) || jumped != nil {
		t.Errorf("expired avoid reordered: %v %v", ids(out), jumped)
	}
}
