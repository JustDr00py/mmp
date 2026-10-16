package health

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// tsPackets builds n aligned packets: a PAT first (if pat), then payload
// packets on PID 0x100.
func tsPackets(n int, pat bool) []byte {
	var out []byte
	for i := range n {
		p := make([]byte, proxy.TSPacketSize)
		p[0], p[3] = 0x47, 0x10|byte(i)&0x0F
		if i > 0 || !pat {
			p[1], p[2] = 0x01, 0x00
		}
		out = append(out, p...)
	}
	return out
}

// origin is a fake provider whose behaviour can be switched mid-test.
type origin struct {
	mode    atomic.Value // "ts" | "503" | "html" | "nopat"
	active  atomic.Int32
	maxSeen atomic.Int32
	srv     *httptest.Server
}

func newOrigin(t *testing.T, mode string) *origin {
	o := &origin{}
	o.mode.Store(mode)
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := o.active.Add(1)
		defer o.active.Add(-1)
		for {
			m := o.maxSeen.Load()
			if n <= m || o.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		switch o.mode.Load().(string) {
		case "503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "html":
			io.WriteString(w, strings.Repeat("<html>not a stream</html>", 2000))
		case "nopat":
			w.Write(tsPackets(400, false))
		default:
			for {
				if _, err := w.Write(tsPackets(100, true)); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

type fakeStore struct {
	mu   sync.Mutex
	rows map[int64]*db.ListProbeTargetsRow
	recs []db.RecordProbeParams
}

func (f *fakeStore) ListProbeTargets(context.Context) ([]db.ListProbeTargetsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.ListProbeTargetsRow
	for id := range int64(len(f.rows) + 100) { // stable order by id
		if r, ok := f.rows[id]; ok {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) RecordProbe(_ context.Context, a db.RecordProbeParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, a)
	r := f.rows[a.ID]
	r.HealthScore, r.CooldownUntil, r.LastProbedAt = a.HealthScore, a.CooldownUntil, a.LastProbedAt
	return nil
}

func (f *fakeStore) last(id int64) db.RecordProbeParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.recs) - 1; i >= 0; i-- {
		if f.recs[i].ID == id {
			return f.recs[i]
		}
	}
	return db.RecordProbeParams{}
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newProber(store *fakeStore, clk *clock) *Prober {
	return &Prober{
		Log:       slog.New(slog.DiscardHandler),
		Store:     store,
		Opener:    proxy.NewProtocolOpener(time.Second, proxy.VariantLimit{}),
		Capacity:  proxy.NewCapacity(),
		Cooldowns: proxy.NewCooldowns(5*time.Second, time.Minute),
		Cfg:       Config{Interval: time.Hour, Recheck: 30 * time.Second, Sample: 200 * time.Millisecond, Concurrency: 2},
		Now:       clk.now,
	}
}

func target(id int64, url string) *db.ListProbeTargetsRow {
	return &db.ListProbeTargetsRow{StreamID: id, StreamUrl: url, HealthScore: 1, ProviderID: 1, MaxConnections: 5}
}

// TestDeadStreamCooldownAndRecovery is the 6.3 AC.
func TestDeadStreamCooldownAndRecovery(t *testing.T) {
	o := newOrigin(t, "503")
	clk := &clock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	store := &fakeStore{rows: map[int64]*db.ListProbeTargetsRow{1: target(1, o.srv.URL+"/live/user/secretpw/1.ts")}}
	p := newProber(store, clk)
	p.Cooldowns = proxy.NewCooldowns(time.Minute, time.Hour) // outlasts the recheck below
	ctx := context.Background()

	res, err := p.RunOnce(ctx)
	if err != nil || len(res) != 1 || res[0].OK {
		t.Fatalf("dead probe = %+v %v", res, err)
	}
	if !p.Cooldowns.Active(1, clk.now()) {
		t.Error("dead stream not cooling down")
	}
	rec := store.last(1)
	if rec.CooldownUntil == nil || !rec.CooldownUntil.After(clk.now()) || !strings.Contains(rec.LastError, "503") {
		t.Errorf("recorded %+v", rec)
	}
	if strings.Contains(rec.LastError, "secretpw") {
		t.Errorf("last_error leaks credentials: %q", rec.LastError)
	}
	if rec.HealthScore >= 1 {
		t.Errorf("score = %v, want lowered", rec.HealthScore)
	}

	// Not due again until Recheck has passed.
	clk.add(10 * time.Second)
	if res, _ := p.RunOnce(ctx); len(res) != 0 {
		t.Errorf("re-probed before recheck: %+v", res)
	}

	// The provider recovers: the next recheck clears the cooldown, even
	// though it would otherwise still run.
	o.mode.Store("ts")
	clk.add(25 * time.Second)
	if !p.Cooldowns.Active(1, clk.now()) {
		t.Fatal("cooldown expired by itself: the probe below would prove nothing")
	}
	res, _ = p.RunOnce(ctx)
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("recovery probe = %+v", res)
	}
	if p.Cooldowns.Active(1, clk.now()) {
		t.Error("cooldown not cleared after recovery")
	}
	rec = store.last(1)
	if rec.CooldownUntil != nil || rec.LastError != "" || rec.HealthScore <= 0.75 {
		t.Errorf("recorded %+v", rec)
	}
	// Healthy again: next probe only after the full interval.
	clk.add(time.Minute)
	if res, _ := p.RunOnce(ctx); len(res) != 0 {
		t.Errorf("healthy stream re-probed early: %+v", res)
	}
	clk.add(time.Hour)
	if res, _ := p.RunOnce(ctx); len(res) != 1 {
		t.Errorf("healthy stream not re-probed after interval: %+v", res)
	}
}

func TestProbeRejectsNonTS(t *testing.T) {
	for mode, want := range map[string]string{"html": "not MPEG-TS", "nopat": "no PAT"} {
		o := newOrigin(t, mode)
		store := &fakeStore{rows: map[int64]*db.ListProbeTargetsRow{1: target(1, o.srv.URL+"/1.ts")}}
		p := newProber(store, &clock{t: time.Now()})
		res, _ := p.RunOnce(context.Background())
		if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Err, want) {
			t.Errorf("%s: %+v, want %q", mode, res, want)
		}
	}
}

func TestProbeSkipsBusyAndFullProviders(t *testing.T) {
	o := newOrigin(t, "ts")
	store := &fakeStore{rows: map[int64]*db.ListProbeTargetsRow{
		1: target(1, o.srv.URL+"/1.ts"),
		2: target(2, o.srv.URL+"/2.ts"),
		3: {StreamID: 3, StreamUrl: o.srv.URL + "/3.ts", HealthScore: 1, ProviderID: 9, MaxConnections: 1},
	}}
	p := newProber(store, &clock{t: time.Now()})
	p.Busy = func() map[int64]bool { return map[int64]bool{2: true} }
	held, _ := p.Capacity.TryAcquire(9, 1) // provider 9's only slot is in use
	defer held.Release()

	res, _ := p.RunOnce(context.Background())
	got := map[int64]Result{}
	for _, r := range res {
		got[r.StreamID] = r
	}
	if !got[1].OK || len(res) != 2 || !got[3].Skipped {
		t.Errorf("results = %+v; want 1 probed, 2 (busy) left out, 3 skipped", res)
	}
	if rec := store.last(3); rec.ID != 0 {
		t.Errorf("skipped stream recorded: %+v", rec)
	}
	if n := p.Capacity.InUse(1); n != 0 {
		t.Errorf("%d slots still held at provider 1", n)
	}
}

func TestProbeConcurrencyAndCoolingFirst(t *testing.T) {
	o := newOrigin(t, "ts")
	clk := &clock{t: time.Now()}
	store := &fakeStore{rows: map[int64]*db.ListProbeTargetsRow{}}
	for id := int64(1); id <= 6; id++ {
		store.rows[id] = target(id, o.srv.URL+"/x.ts")
	}
	p := newProber(store, clk)
	p.Cooldowns.Fail(5, clk.now())
	res, _ := p.RunOnce(context.Background())
	if len(res) != 6 || res[0].StreamID != 5 {
		t.Errorf("results = %+v; want all 6, cooling stream 5 first", res)
	}
	if m := o.maxSeen.Load(); m > 2 {
		t.Errorf("%d probes ran at once, want <= 2", m)
	}
}

func TestProbeFFprobe(t *testing.T) {
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	real, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-t", "1", "-c:v", "libx264", "-preset", "ultrafast", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	p := &Prober{Cfg: Config{FFprobePath: ffprobe}}
	if err := p.ffprobe(context.Background(), real); err != nil {
		t.Errorf("real TS: %v", err)
	}
	// Aligned, with a PAT, but no programme: sniffing passes, ffprobe does not.
	fake := tsPackets(400, true)
	if err := sniffTS(fake); err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if err := p.ffprobe(context.Background(), fake); err == nil {
		t.Error("ffprobe accepted a stream without audio or video")
	}
}

func TestSniffTS(t *testing.T) {
	good := tsPackets(60, true)
	for name, tc := range map[string]struct {
		b    []byte
		want string
	}{
		"ok":        {good, ""},
		"offset":    {append([]byte{1, 2, 3}, good...), ""},
		"torn tail": {good[:len(good)-50], ""},
		"empty":     {nil, "no data"},
		"short":     {tsPackets(10, true), "too little"},
		"lost sync": {append(append(bytes.Clone(good[:188*55]), 0, 0), good...), "lost TS sync"},
		"not ts":    {bytes.Repeat([]byte("x"), 20000), "not MPEG-TS"},
		"no pat":    {tsPackets(60, false), "no PAT"},
	} {
		err := sniffTS(tc.b)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
