package output

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// fakeTS serves an endless paced TS (PAT + 9 payload packets per group)
// and counts upstream requests.
func fakeTS(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	pkt := func(pid uint16, cc byte) []byte {
		b := make([]byte, proxy.TSPacketSize)
		b[0], b[1], b[2], b[3] = 0x47, byte(pid>>8)&0x1F|0x40, byte(pid), 0x10|cc&0x0F
		return b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		for cc := byte(0); ; cc++ {
			group := pkt(0, cc)
			for range 9 {
				group = append(group, pkt(0x100, cc)...)
			}
			if _, err := w.Write(group); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newLiveServer serves the outputs over real HTTP, with a real proxy
// manager whose every channel is backed by one fake upstream.
func newLiveServer(t *testing.T) (*httptest.Server, *fakeStore, *atomic.Int32) {
	t.Helper()
	srv, hs, hits := newLiveHandlers(t)
	return srv, hs.Store.(*fakeStore), hits
}

func newLiveHandlers(t *testing.T) (*httptest.Server, *Handlers, *atomic.Int32) {
	t.Helper()
	up, hits := fakeTS(t)
	hs, _ := newTestHandlers(t)
	m := proxy.NewManager(context.Background(), proxy.ManagerConfig{
		Source: func(_ context.Context, id int64) ([]proxy.Candidate, error) {
			return []proxy.Candidate{{StreamID: id, ProviderID: 1, URL: up.URL}}, nil
		},
		Opener:         proxy.NewHTTPOpener(time.Second),
		Capacity:       proxy.NewCapacity(),
		Cooldowns:      proxy.NewCooldowns(time.Second, time.Minute),
		RingSize:       1 << 20,
		IdleGrace:      100 * time.Millisecond,
		Thresholds:     proxy.Thresholds{StallTimeout: time.Second, Window: time.Second},
		MaxRetryWindow: 2 * time.Second,
		RetryBackoff:   50 * time.Millisecond,
		Log:            slog.New(slog.DiscardHandler),
	})
	t.Cleanup(m.Shutdown)
	hs.Streams = m
	srv := httptest.NewServer(chiRouter(hs))
	t.Cleanup(srv.Close)
	return srv, hs, hits
}

// play opens a stream and, on 200, reads the first TS packet. The returned
// stop closes the connection.
func play(t *testing.T, base, path string) (int, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("%s: %v", path, err)
	}
	stop := func() { cancel(); resp.Body.Close() }
	if resp.StatusCode == http.StatusOK {
		if ct := resp.Header.Get("Content-Type"); ct != "video/mp2t" {
			t.Errorf("%s content-type = %q", path, ct)
		}
		pkt := make([]byte, proxy.TSPacketSize)
		if _, err := io.ReadFull(resp.Body, pkt); err != nil || pkt[0] != 0x47 {
			t.Errorf("%s: first packet %x, %v", path, pkt[:4], err)
		}
	} else {
		stop()
	}
	return resp.StatusCode, stop
}

func TestLivePlaysAndSharesUpstream(t *testing.T) {
	srv, _, hits := newLiveServer(t)
	code, stopA := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	if code != 200 {
		t.Fatalf("xtream play = %d", code)
	}
	defer stopA()
	code, stopB := play(t, srv.URL, "/t/tok-root/10") // token form, no extension
	if code != 200 {
		t.Fatalf("token play = %d", code)
	}
	defer stopB()
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream requests = %d, want 1 shared session", n)
	}
}

func TestLiveConnectionLimit(t *testing.T) {
	srv, store, _ := newLiveServer(t)
	store.byUser[1] = append(store.byUser[1], db.ListUserChannelsRow{ID: 12, Name: "Sports"})
	code, stop := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	if code != 200 {
		t.Fatalf("first = %d", code)
	}
	if code, _ := play(t, srv.URL, "/t/tok-alice/12.ts"); code != http.StatusForbidden {
		t.Errorf("second channel (max 1) = %d, want 403", code)
	}
	stop()
	// The slot frees once the server notices the client left.
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, stop2 := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
		if code == 200 {
			stop2()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never released: %d", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveRejections(t *testing.T) {
	srv, store, hits := newLiveServer(t)
	store.nostream = map[int64]bool{11: true}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/live/alice/wrong-pw1/10.ts", http.StatusUnauthorized},
		{"/live/old/old-pw1/10.ts", http.StatusUnauthorized}, // expired
		{"/live/off/off-pw1/10.ts", http.StatusUnauthorized}, // disabled
		{"/t/nope/10.ts", http.StatusUnauthorized},
		{"/live/alice/alice-pw1/11.ts", http.StatusNotFound}, // not in her bouquets
		{"/live/alice/alice-pw1/999.ts", http.StatusNotFound},
		{"/live/alice/alice-pw1/abc.ts", http.StatusNotFound},
		{"/live/alice/alice-pw1/10.m3u8", http.StatusNotFound},       // HLS output disabled
		{"/live/root/root-pw1/11.ts", http.StatusServiceUnavailable}, // no streams
	} {
		if code, _ := play(t, srv.URL, tc.path); code != tc.want {
			t.Errorf("%s = %d, want %d", tc.path, code, tc.want)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("rejected requests opened %d upstream connections", n)
	}
}

func chiRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

// A player reconnecting to the channel it is already playing (its read
// timeout fired during a stall) takes over its old connection instead of
// being refused: the old one is ended and its slot handed over.
func TestLiveReconnectTakesOver(t *testing.T) {
	srv, hs, hits := newLiveHandlers(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/live/alice/alice-pw1/10.ts", nil)
	old, err := http.DefaultClient.Do(req)
	if err != nil || old.StatusCode != 200 {
		t.Fatalf("first = %v %v", old, err)
	}
	defer old.Body.Close()
	// The old player stops reading but keeps its connection open, as after
	// a stall: the server still counts it.
	code, stop := play(t, srv.URL, "/t/tok-alice/10.ts")
	if code != 200 {
		t.Fatalf("reconnect to the same channel = %d, want 200", code)
	}
	defer stop()
	// The old connection was ended by the server.
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, old.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old connection still open after take-over")
	}
	if n := hs.Access.ActiveConnections(1); n != 1 {
		t.Errorf("alice connections = %d, want 1", n)
	}
	if vs := hs.Access.Viewers(); len(vs) != 1 || vs[0].ChannelID != 10 {
		t.Errorf("viewers = %+v, want the new connection only", vs)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream requests = %d, want the session kept", n)
	}
}

func TestTakeOverMatchesUserChannelNetwork(t *testing.T) {
	var vs viewers
	var kicked []string
	add := func(name string, user, ch int64, out, network string, since time.Time) {
		vs.add(Viewer{UserID: user, ChannelID: ch, Output: out, Network: network, Since: since},
			func() { kicked = append(kicked, name) }, nil)
	}
	t0 := time.Now()
	add("other-user", 2, 10, "ts", "1.2.3.4", t0)
	add("other-channel", 1, 11, "ts", "1.2.3.4", t0)
	add("other-network", 1, 10, "ts", "5.6.7.8", t0)
	add("hls", 1, 10, "hls", "1.2.3.4", t0)
	add("newer", 1, 10, "ts", "1.2.3.4", t0.Add(time.Second))
	add("oldest", 1, 10, "ts", "1.2.3.4", t0)
	if _, ok := vs.takeOver(1, 10, "1.2.3.4"); !ok || len(kicked) != 1 || kicked[0] != "oldest" {
		t.Errorf("kicked = %v, want [oldest]", kicked)
	}
	kicked = nil
	if _, ok := vs.takeOver(1, 12, "1.2.3.4"); ok || len(kicked) != 0 {
		t.Errorf("no match: kicked = %v", kicked)
	}
}
