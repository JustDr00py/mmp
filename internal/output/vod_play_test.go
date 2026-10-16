package output

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/proxy"
)

// fakeFilePanel serves movie 11 and episode 7001 for puser/ppass with
// real Range support, plus a redirecting copy of the movie.
type fakeFilePanel struct {
	movie, episode []byte
	hits           atomic.Int32
	down           atomic.Bool
	srv            *httptest.Server
}

func newFakeFilePanel(t *testing.T, movie []byte) *fakeFilePanel {
	f := &fakeFilePanel{movie: movie, episode: []byte(strings.Repeat("E", 5000))}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.down.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/movie/puser/ppass/11.mkv":
			http.Redirect(w, r, "/storage/movie-11.mkv", http.StatusFound) // panels often redirect to a CDN
		case "/storage/movie-11.mkv":
			http.ServeContent(w, r, "m.mkv", time.Unix(1700000000, 0), bytes.NewReader(f.movie))
		case "/series/puser/ppass/7001.mkv":
			http.ServeContent(w, r, "e.mkv", time.Unix(1700000000, 0), bytes.NewReader(f.episode))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newPlaybackServer(t *testing.T, movie []byte, maxConns int32) (*httptest.Server, *Handlers, *fakeFilePanel, *proxy.Capacity) {
	t.Helper()
	panel := newFakeFilePanel(t, movie)
	hs, _ := newTestHandlers(t)
	slots := proxy.NewCapacity()
	hs.VOD = &fakeVOD{panelURL: panel.srv.URL, maxConns: maxConns}
	hs.Slots = slots
	srv := httptest.NewServer(chiRouter(hs))
	t.Cleanup(srv.Close)
	return srv, hs, panel, slots
}

func fetch(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestVODPlaybackRanges(t *testing.T) {
	movie := make([]byte, 300_000)
	rand.Read(movie)
	srv, _, panel, _ := newPlaybackServer(t, movie, 2)
	u := srv.URL + "/movie/alice/alice-pw1/101.mkv"

	resp, body := fetch(t, u, nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, movie) || resp.Header.Get("Content-Type") != "video/x-matroska" ||
		resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Location") != "" {
		t.Fatalf("full GET = %d, %d bytes, %v", resp.StatusCode, len(body), resp.Header)
	}
	// Seeking: ranged requests are passed through.
	for _, rg := range []struct {
		hdr      string
		from, to int
	}{{"bytes=100-199", 100, 199}, {"bytes=250000-", 250000, len(movie) - 1}} {
		resp, body = fetch(t, u, map[string]string{"Range": rg.hdr})
		want := "bytes " + itoa(rg.from) + "-" + itoa(rg.to) + "/" + itoa(len(movie))
		if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != want || !bytes.Equal(body, movie[rg.from:rg.to+1]) {
			t.Errorf("%s = %d %q, %d bytes", rg.hdr, resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
		}
	}
	// Token form, an episode, and HEAD.
	if resp, body := fetch(t, srv.URL+"/t/tok-alice/movie/101.mkv", map[string]string{"Range": "bytes=0-9"}); resp.StatusCode != 206 || !bytes.Equal(body, movie[:10]) {
		t.Errorf("token form = %d", resp.StatusCode)
	}
	if resp, body := fetch(t, srv.URL+"/series/alice/alice-pw1/701.mkv", nil); resp.StatusCode != 200 || len(body) != 5000 {
		t.Errorf("episode = %d, %d bytes", resp.StatusCode, len(body))
	}
	head, _ := http.Head(u)
	if head.StatusCode != 200 || head.Header.Get("Content-Length") != itoa(len(movie)) {
		t.Errorf("HEAD = %d %v", head.StatusCode, head.Header)
	}
	if panel.hits.Load() == 0 {
		t.Fatal("panel never asked")
	}

	// Access: Kids (102) is not alice's; unknown ids; wrong password.
	for path, want := range map[string]int{
		"/movie/alice/alice-pw1/102.mp4":  404,
		"/movie/alice/alice-pw1/999.mkv":  404,
		"/series/alice/alice-pw1/702.mkv": 404,
		"/movie/alice/wrong-pw1/101.mkv":  401,
		"/movie/root/root-pw1/102.mp4":    404, // admin may, but the panel has no file for it
	} {
		if resp, _ := fetch(t, srv.URL+path, nil); resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestVODPlaybackSlots(t *testing.T) {
	movie := make([]byte, 16<<20) // big enough that an unread response stays open
	srv, hs, _, slots := newPlaybackServer(t, movie, 1)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("first = %v %v", resp, err)
	}
	// Seeking the same movie while the first request is open (what players
	// do) shares alice's only slot and the provider's only slot.
	if r, body := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", map[string]string{"Range": "bytes=1000-1999"}); r.StatusCode != 206 || len(body) != 1000 {
		t.Errorf("seek while playing = %d (%d bytes), want 206", r.StatusCode, len(body))
	}
	// Another item is another connection: alice is at her limit.
	if r, _ := fetch(t, srv.URL+"/series/alice/alice-pw1/701.mkv", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("alice second item = %d, want 403", r.StatusCode)
	}
	if r, _ := fetch(t, srv.URL+"/movie/root/root-pw1/101.mkv", nil); r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" {
		t.Errorf("provider full = %d, want 503", r.StatusCode)
	}
	// The player leaves: both slots come back.
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for slots.InUse(1) != 0 || hs.Access.ActiveConnections(1) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("slots not released: provider %d, alice %d", slots.InUse(1), hs.Access.ActiveConnections(1))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r, _ := fetch(t, srv.URL+"/movie/root/root-pw1/101.mkv", map[string]string{"Range": "bytes=0-0"}); r.StatusCode != 206 {
		t.Errorf("after release = %d", r.StatusCode)
	}
}

func TestVODPlaybackUpstreamErrors(t *testing.T) {
	srv, _, panel, slots := newPlaybackServer(t, []byte("x"), 2)
	panel.down.Store(true)
	resp, body := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "ppass") || strings.Contains(string(body), panel.srv.URL) {
		t.Errorf("panel 500 = %d %q", resp.StatusCode, body)
	}
	panel.srv.Close() // connection refused
	resp, body = fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "ppass") {
		t.Errorf("panel down = %d %q", resp.StatusCode, body)
	}
	if slots.InUse(1) != 0 {
		t.Error("failed playback kept a provider slot")
	}
}

// TestVODPlaybackFFprobe: a real MP4 with its index at the end, which
// players can only open by seeking (ranged requests), probes through MMP.
func TestVODPlaybackFFprobe(t *testing.T) {
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	path := filepath.Join(t.TempDir(), "m.mp4")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-f", "lavfi", "-i", "sine", "-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-y", path).CombinedOutput(); err != nil {
		t.Fatalf("make mp4: %v %s", err, out)
	}
	movie, _ := os.ReadFile(path)
	srv, _, panel, _ := newPlaybackServer(t, movie, 2)
	before := panel.hits.Load()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration:stream=codec_name", "-of", "csv=p=0",
		srv.URL+"/movie/alice/alice-pw1/101.mkv").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "h264") || !strings.Contains(string(out), "aac") || !strings.Contains(string(out), "6.0") {
		t.Fatalf("ffprobe through MMP: %v\n%s", err, out)
	}
	if panel.hits.Load()-before < 2 {
		t.Errorf("ffprobe made %d upstream requests; expected it to seek (moov at the end)", panel.hits.Load()-before)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestVODLeaseSharing: overlapping requests for one item share one slot
// pair, released when the last one ends; another item takes its own.
func TestVODLeaseSharing(t *testing.T) {
	var l vodLeases
	takes := 0
	slots := proxy.NewCapacity()
	take := func() (*proxy.Slot, *proxy.Slot, error) {
		takes++
		s, _ := slots.TryAcquire(1, 0)
		p, _ := slots.TryAcquire(2, 0)
		return s, p, nil
	}
	key := vodKey{1, "movie", 101}
	rel1, _ := l.acquire(key, take)
	rel2, _ := l.acquire(key, take) // a seek while playing
	if takes != 1 || slots.InUse(1) != 1 {
		t.Fatalf("takes %d, slots %d; want one shared pair", takes, slots.InUse(1))
	}
	rel1()
	rel1() // idempotent
	if slots.InUse(1) != 1 {
		t.Fatal("released while a request still runs")
	}
	rel2()
	if slots.InUse(1) != 0 || slots.InUse(2) != 0 {
		t.Fatal("slots kept after the last request")
	}
	if _, err := l.acquire(vodKey{1, "movie", 102}, take); err != nil || takes != 2 {
		t.Errorf("another item: takes %d, %v", takes, err)
	}
}
