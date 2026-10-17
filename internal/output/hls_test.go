package output

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/proxy"
)

// realTS serves ffmpeg-made h264+aac in real time and counts upstream
// requests. Skips the test without ffmpeg. The clip (15s) outlasts every
// test, so its loop (an unflagged timestamp restart) is never reached.
func realTS(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	data, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine",
		"-t", strconv.Itoa(clipSecs), "-c:v", "libx264", "-preset", "ultrafast", "-x264-params", "bframes=2", "-g", "25", "-c:a", "aac",
		"-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		const chunk = 50 * proxy.TSPacketSize
		per := clipSecs * time.Second * chunk / time.Duration(len(data))
		for {
			for off := 0; off < len(data); off += chunk {
				if _, err := w.Write(data[off:min(off+chunk, len(data))]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(per):
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newHLSServer is newLiveServer over real video with the HLS output on.
// BaseURL is the test server, so redirects are followable.
func newHLSServer(t *testing.T, idle time.Duration) (*httptest.Server, *Handlers, *atomic.Int32) {
	t.Helper()
	up, hits := realTS(t)
	hs, _ := newTestHandlers(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := proxy.NewManager(ctx, proxy.ManagerConfig{
		Source: func(_ context.Context, id int64) ([]proxy.Candidate, error) {
			return []proxy.Candidate{{StreamID: id, ProviderID: 1, URL: up.URL + "/live/u/secretpw/1.ts"}}, nil
		},
		Opener:         proxy.NewHTTPOpener(time.Second),
		Capacity:       proxy.NewCapacity(),
		Cooldowns:      proxy.NewCooldowns(time.Second, time.Minute),
		RingSize:       4 << 20,
		IdleGrace:      100 * time.Millisecond,
		Thresholds:     proxy.Thresholds{StallTimeout: 2 * time.Second, Window: time.Second},
		MaxRetryWindow: 2 * time.Second,
		RetryBackoff:   50 * time.Millisecond,
		Log:            slog.New(slog.DiscardHandler),
	})
	hs.Streams = m
	hs.HLS = NewHLS(ctx, HLSConfig{Segment: time.Second, Window: 4, Idle: idle}, m, hs.Access, slog.New(slog.DiscardHandler))
	srv := httptest.NewUnstartedServer(nil)
	hs.BaseURL = "http://" + srv.Listener.Addr().String()
	srv.Config.Handler = fromAddr(chiRouter(hs))
	srv.Start()
	t.Cleanup(func() { srv.Close(); cancel(); m.Shutdown() })
	return srv, hs, hits
}

// fromAddr lets a test request come from another client address
// (X-Test-Addr: host:port).
func fromAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("X-Test-Addr"); a != "" {
			r.RemoteAddr = a
		}
		next.ServeHTTP(w, r)
	})
}

// getFrom is an un-followed GET from client address addr.
func getFrom(t *testing.T, addr, u string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("X-Test-Addr", addr)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

const clipSecs = 15

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func httpGet(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var segURI = regexp.MustCompile(`(?m)^(\d+)\.ts$`)

func TestHLSOutputPlaylistAndSegments(t *testing.T) {
	srv, hs, hits := newHLSServer(t, 5*time.Second)
	resp, _ := httpGet(t, noRedirect, srv.URL+"/live/alice/alice-pw1/10.m3u8")
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, srv.URL+"/hls/") || !strings.HasSuffix(loc, "/index.m3u8") {
		t.Fatalf("start = %d %q, want a redirect to the lease playlist on MMP", resp.StatusCode, loc)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("redirect lacks CORS for browser players")
	}
	if strings.Contains(loc, "alice-pw1") {
		t.Errorf("lease URL carries the password: %s", loc)
	}
	// The same user and channel again (a player reopening): same lease, no
	// second slot (alice may hold one).
	resp2, _ := httpGet(t, noRedirect, srv.URL+"/t/tok-alice/10.m3u8")
	if resp2.Header.Get("Location") != loc || hs.HLS.Leases() != 1 {
		t.Errorf("reopen = %d %q (leases %d), want the same lease", resp2.StatusCode, resp2.Header.Get("Location"), hs.HLS.Leases())
	}

	resp, pl := httpGet(t, http.DefaultClient, loc)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("playlist = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	segs := segURI.FindAllStringSubmatch(pl, -1)
	if len(segs) < 2 || !strings.Contains(pl, "#EXT-X-TARGETDURATION:") || strings.Contains(pl, "http") {
		t.Fatalf("playlist (want >= 2 relative segments):\n%s", pl)
	}
	base := strings.TrimSuffix(loc, "index.m3u8")
	resp, body := httpGet(t, http.DefaultClient, base+segs[0][1]+".ts")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp2t" || len(body) == 0 || body[0] != 0x47 {
		t.Errorf("segment = %d %s, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	for path, want := range map[string]int{
		base + "999999.ts":                   404,
		srv.URL + "/hls/deadbeef/index.m3u8": 404,
		srv.URL + "/hls/deadbeef/1.ts":       404,
	} {
		if resp, _ := httpGet(t, http.DefaultClient, path); resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream requests = %d, want 1", n)
	}
}

// TestHLSLeaseBoundToNetworks: a lease link answers the network that
// opened it and one more (IPv4/IPv6), not a third it was shared with; a
// start from that third network needs its own slot.
func TestHLSLeaseBoundToNetworks(t *testing.T) {
	srv, hs, _ := newHLSServer(t, 5*time.Second)
	resp := getFrom(t, "203.0.113.7:4000", srv.URL+"/live/alice/alice-pw1/10.m3u8")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	for _, tt := range []struct {
		addr string
		want int
	}{
		{"203.0.113.7:4001", 200},            // the opener
		{"[2001:db8:5:6::1]:4000", 200},      // its IPv6 side
		{"[2001:db8:5:6:ffff::2]:4000", 200}, // same /64
		{"198.51.100.20:4000", 404},          // shared elsewhere
		{"203.0.113.7:4002", 200},            // still fine for the opener
	} {
		if resp := getFrom(t, tt.addr, loc); resp.StatusCode != tt.want {
			t.Errorf("playlist from %s = %d, want %d", tt.addr, resp.StatusCode, tt.want)
		}
	}
	// Reopening from the other network asks for a lease of its own, and
	// alice's only slot is taken.
	if resp := getFrom(t, "198.51.100.20:4000", srv.URL+"/live/alice/alice-pw1/10.m3u8"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("start from another network = %d, want 403 (connection limit)", resp.StatusCode)
	}
	if n := hs.HLS.Leases(); n != 1 {
		t.Errorf("leases = %d, want 1", n)
	}
}

// TestHLSOutputSlotLifecycle: an HLS client holds one of the user's
// connection slots until it goes quiet, and then the channel stops.
func TestHLSOutputSlotLifecycle(t *testing.T) {
	srv, hs, _ := newHLSServer(t, 600*time.Millisecond)
	resp, _ := httpGet(t, noRedirect, srv.URL+"/live/alice/alice-pw1/10.m3u8")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	if code, _ := play(t, srv.URL, "/live/alice/alice-pw1/10.ts"); code != http.StatusForbidden {
		t.Errorf("TS while HLS holds alice's only slot = %d, want 403", code)
	}
	// alice stops fetching: the lease expires and the slot frees.
	deadline := time.Now().Add(3 * time.Second)
	for hs.HLS.Leases() != 0 || hs.Access.ActiveConnections(1) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("lease not reaped: leases %d, slots %d", hs.HLS.Leases(), hs.Access.ActiveConnections(1))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resp, _ := httpGet(t, http.DefaultClient, resp.Header.Get("Location")); resp.StatusCode != 404 {
		t.Errorf("expired lease playlist = %d, want 404", resp.StatusCode)
	}
	code, stop := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	if code != 200 {
		t.Errorf("TS after the lease expired = %d", code)
	}
	stop()
	// And at the limit, a new HLS start is refused.
	_, stop = play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	defer stop()
	if resp, _ := httpGet(t, noRedirect, srv.URL+"/live/alice/alice-pw1/10.m3u8"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("HLS at the limit = %d, want 403", resp.StatusCode)
	}
}

// TestHLSOutputPlaysInFFmpeg is the closest automated stand-in for a
// player: ffmpeg's HLS demuxer follows the live playlist for 6s and must
// decode without errors.
func TestHLSOutputPlaysInFFmpeg(t *testing.T) {
	srv, _, _ := newHLSServer(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-v", "error",
		"-i", srv.URL+"/live/alice/alice-pw1/10.m3u8", "-t", "6", "-f", "null", "-").CombinedOutput()
	if err != nil || len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("ffmpeg: %v\n%s", err, out)
	}
}

func TestHLSAdvertised(t *testing.T) {
	srv, hs, _ := newHLSServer(t, 5*time.Second)
	if f := hs.userInfo(db.User{ID: 1}, "alice", "x").AllowedOutputFormats; strings.Join(f, ",") != "m3u8,ts" {
		t.Errorf("allowed_output_formats = %v", f)
	}
	_, body := httpGet(t, http.DefaultClient, srv.URL+"/get.php?username=alice&password=alice-pw1&output=m3u8")
	if !strings.Contains(body, srv.URL+"/live/alice/alice-pw1/10.m3u8\n") {
		t.Errorf("get.php output=m3u8 lacks .m3u8 URLs:\n%s", body)
	}
	_, body = httpGet(t, http.DefaultClient, srv.URL+"/get.php?username=alice&password=alice-pw1")
	if !strings.Contains(body, "/10.ts\n") {
		t.Errorf("get.php default lacks .ts URLs:\n%s", body)
	}
	hs.HLS = nil
	if ext := hs.streamExt("m3u8"); ext != ".ts" {
		t.Errorf("HLS off, output=m3u8 -> %q", ext)
	}
}
