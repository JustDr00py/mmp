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
	return srv, hs.Store.(*fakeStore), hits
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
	srv, _, _ := newLiveServer(t)
	code, stop := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	if code != 200 {
		t.Fatalf("first = %d", code)
	}
	if code, _ := play(t, srv.URL, "/t/tok-alice/10.ts"); code != http.StatusForbidden {
		t.Errorf("second connection (max 1) = %d, want 403", code)
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
