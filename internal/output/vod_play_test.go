package output

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
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

	"github.com/JustDr00py/mmp/internal/activity"
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
	// The playback is on the dashboard, titled.
	if vs := hs.Access.Viewers(); len(vs) != 1 || vs[0].Output != "movie" || vs[0].ItemID != 101 || vs[0].Title == "" || vs[0].ProviderID != 1 {
		t.Errorf("viewers = %+v", vs)
	}
	// Another user cannot have the provider's only slot.
	if r, _ := fetch(t, srv.URL+"/movie/root/root-pw1/101.mkv", nil); r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" {
		t.Errorf("provider full = %d, want 503", r.StatusCode)
	}
	if hs.Access.ActiveConnections(2) != 0 {
		t.Error("refused playback kept root's slot")
	}
	// The player leaves: both slots come back.
	cancel()
	resp.Body.Close()
	waitVODIdle(t, hs, slots)
	if vs := hs.Access.Viewers(); len(vs) != 0 {
		t.Errorf("viewers after the player left = %+v", vs)
	}
	if r, _ := fetch(t, srv.URL+"/movie/root/root-pw1/101.mkv", map[string]string{"Range": "bytes=0-0"}); r.StatusCode != 206 {
		t.Errorf("after release = %d", r.StatusCode)
	}
}

func waitVODIdle(t *testing.T, hs *Handlers, slots *proxy.Capacity) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for slots.InUse(1) != 0 || hs.Access.ActiveConnections(1) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("slots not released: provider %d, alice %d", slots.InUse(1), hs.Access.ActiveConnections(1))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// openPlayback starts a GET that stays open (the body is not read).
func openPlayback(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return resp, cancel
}

// TestVODPlaybackTakeover: a player that starts the next title without
// closing the last one (alice's only slot, the provider's only slot) ends
// the old playback instead of being refused.
func TestVODPlaybackTakeover(t *testing.T) {
	movie := make([]byte, 16<<20)
	srv, hs, panel, slots := newPlaybackServer(t, movie, 1)
	panel.episode = make([]byte, 16<<20) // still playing when checked
	old, _ := openPlayback(t, srv.URL+"/movie/alice/alice-pw1/101.mkv")
	if old.StatusCode != 200 {
		t.Fatalf("first = %d", old.StatusCode)
	}
	next, _ := openPlayback(t, srv.URL+"/series/alice/alice-pw1/701.mkv")
	if next.StatusCode != 200 {
		t.Fatalf("next title = %d, want 200 (take over)", next.StatusCode)
	}
	waitBodyEnds(t, old) // the old response is cut off
	if n := hs.Access.ActiveConnections(1); n != 1 || slots.InUse(1) != 1 {
		t.Errorf("alice %d, provider %d slots; want 1 and 1", n, slots.InUse(1))
	}
	if vs := hs.Access.Viewers(); len(vs) != 1 || vs[0].Output != "series" || vs[0].ItemID != 701 {
		t.Errorf("viewers = %+v", vs)
	}
}

// TestVODPlaybackKick: the dashboard's disconnect ends a playback and frees
// its slots at once.
func TestVODPlaybackKick(t *testing.T) {
	movie := make([]byte, 16<<20)
	srv, hs, _, slots := newPlaybackServer(t, movie, 1)
	resp, _ := openPlayback(t, srv.URL+"/movie/alice/alice-pw1/101.mkv")
	if resp.StatusCode != 200 {
		t.Fatalf("play = %d", resp.StatusCode)
	}
	vs := hs.Access.Viewers()
	if len(vs) != 1 || !hs.Access.Kick(vs[0].ID) {
		t.Fatalf("kick %+v", vs)
	}
	if hs.Access.ActiveConnections(1) != 0 || slots.InUse(1) != 0 || len(hs.Access.Viewers()) != 0 {
		t.Error("kick left slots or the viewer behind")
	}
	waitBodyEnds(t, resp)
}

// waitBodyEnds fails unless resp's body is cut off within a few seconds.
func waitBodyEnds(t *testing.T, resp *http.Response) {
	t.Helper()
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("playback still streaming")
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
	ctx := context.Background()
	key := vodKey{1, "movie", 101}
	_, _, rel1, _ := l.acquire(ctx, vodAcquire{key: key, providerID: 2, take: take})
	_, _, rel2, _ := l.acquire(ctx, vodAcquire{key: key, providerID: 2, take: take}) // a seek while playing
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
	if _, _, _, err := l.acquire(ctx, vodAcquire{key: vodKey{1, "movie", 102}, providerID: 2, take: take}); err != nil || takes != 2 {
		t.Errorf("another item: takes %d, %v", takes, err)
	}
}

// TestVODLeaseTakeover: at the user's limit the user's oldest lease is
// ended; with the provider full, the user's oldest lease on that provider.
// Other users' leases are never taken.
func TestVODLeaseTakeover(t *testing.T) {
	var l vodLeases
	users, provs := proxy.NewCapacity(), proxy.NewCapacity()
	take := func(user int64, userMax int, prov int64, provMax int) func() (*proxy.Slot, *proxy.Slot, error) {
		return func() (*proxy.Slot, *proxy.Slot, error) {
			u, ok := users.TryAcquire(user, userMax)
			if !ok {
				return nil, nil, ErrTooManyConnections
			}
			p, ok := provs.TryAcquire(prov, provMax)
			if !ok {
				u.Release()
				return nil, nil, errProviderFull
			}
			return u, p, nil
		}
	}
	ctx := context.Background()
	var taken []vodKey
	play := func(user, id, prov int64, userMax, provMax int) (context.Context, error) {
		c, _, _, err := l.acquire(ctx, vodAcquire{key: vodKey{user, "movie", id}, providerID: prov,
			take: take(user, userMax, prov, provMax), tookOver: func(o *vodLease) { taken = append(taken, o.key) }})
		return c, err
	}
	// User 1 may have 2: movies 1 (provider 10) then 2 (provider 20).
	c1, _ := play(1, 1, 10, 2, 0)
	time.Sleep(time.Millisecond)
	c2, _ := play(1, 2, 20, 2, 0)
	// A third title ends the oldest (movie 1) only.
	if _, err := play(1, 3, 30, 2, 0); err != nil || len(taken) != 1 || taken[0].id != 1 || c1.Err() == nil || c2.Err() != nil {
		t.Fatalf("user limit: err %v, taken %v, c1 %v, c2 %v", err, taken, c1.Err(), c2.Err())
	}
	// Provider 20 allows 1: user 1 starting movie 4 there ends movie 2
	// (movie 3, older-than-nothing on provider 30, stays).
	taken = nil
	if _, err := play(1, 4, 20, 3, 1); err != nil || len(taken) != 1 || taken[0].id != 2 {
		t.Fatalf("provider full: err %v, taken %v", err, taken)
	}
	// User 2 cannot take user 1's provider slot.
	taken = nil
	if _, err := play(2, 5, 20, 3, 1); !errors.Is(err, errProviderFull) || len(taken) != 0 {
		t.Fatalf("other user: err %v, taken %v", err, taken)
	}
	if users.InUse(1) != 2 || provs.InUse(20) != 1 || users.InUse(2) != 0 {
		t.Errorf("slots: user1 %d, prov20 %d, user2 %d", users.InUse(1), provs.InUse(20), users.InUse(2))
	}
}

// TestVODWatchHistory: each playback (all its seeks together) lands in the
// watch history once, with why it ended; a bare HEAD probe does not.
func TestVODWatchHistory(t *testing.T) {
	movie := make([]byte, 16<<20) // still playing when the seek arrives
	srv, hs, panel, slots := newPlaybackServer(t, movie, 1)
	panel.episode = make([]byte, 16<<20) // still playing when kicked
	st := &activityStore{}
	tr := activity.New(st, nil, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	hs.Activity = tr
	hs.vodLeases.resume = 10 * time.Millisecond
	u := srv.URL + "/movie/alice/alice-pw1/101.mkv"

	// Played to the end with one seek while the first request was open.
	first, stop := openPlayback(t, u)
	if r, _ := fetch(t, u, map[string]string{"Range": "bytes=0-999"}); r.StatusCode != 206 {
		t.Fatalf("seek = %d", r.StatusCode)
	}
	io.Copy(io.Discard, first.Body)
	stop()
	waitVODIdle(t, hs, slots)
	waitWatched(t, st, 1) // once the resume window passes
	// A HEAD probe: no session.
	if head, _ := http.Head(u); head.StatusCode != 200 {
		t.Fatalf("HEAD = %d", head.StatusCode)
	}
	waitVODIdle(t, hs, slots)
	// Kicked from the dashboard.
	ep, _ := openPlayback(t, srv.URL+"/series/alice/alice-pw1/701.mkv")
	deadline := time.Now().Add(3 * time.Second)
	for {
		vs := hs.Access.Viewers()
		if len(vs) == 1 && vs[0].Bytes > 0 {
			hs.Access.Kick(vs[0].ID)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("episode never played: %+v", vs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitBodyEnds(t, ep)
	waitVODIdle(t, hs, slots)
	// The provider fails.
	panel.down.Store(true)
	if r, _ := fetch(t, u, nil); r.StatusCode != http.StatusBadGateway {
		t.Fatalf("panel down = %d", r.StatusCode)
	}
	waitVODIdle(t, hs, slots)
	hs.FlushVOD()
	cancel()
	<-done

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.watched) != 3 {
		t.Fatalf("watched = %+v, want 3 sessions", st.watched)
	}
	for i, want := range []struct {
		output, reason string
		item           int64
		bytes          int64 // -1: some
	}{{"movie", vodStopped, 101, int64(len(movie)) + 1000}, {"series", vodKicked, 701, -1}, {"movie", vodFailed, 101, 0}} {
		w := st.watched[i]
		if want.output == "series" && w.Title != "Show · S01E02" {
			t.Errorf("episode title = %q", w.Title)
		}
		if w.UserID != 1 || w.ChannelID != 0 || w.Output != want.output || w.ItemID != want.item || w.Title == "" ||
			w.EndReason != want.reason || w.Ip != "127.0.0.1" || w.EndedAt.Before(w.StartedAt) {
			t.Errorf("session %d = %+v, want %s %d %q", i, w, want.output, want.item, want.reason)
		}
		if (want.bytes < 0 && w.Bytes <= 0) || (want.bytes >= 0 && w.Bytes != want.bytes) {
			t.Errorf("session %d bytes = %d, want %d", i, w.Bytes, want.bytes)
		}
	}
}

// TestVODResume: a player that closes its request before seeking (or
// reconnects) within the resume window continues the same session.
func TestVODResume(t *testing.T) {
	movie := make([]byte, 300_000)
	srv, hs, _, slots := newPlaybackServer(t, movie, 1)
	st := &activityStore{}
	tr := activity.New(st, nil, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	hs.Activity = tr
	hs.vodLeases.resume = time.Hour
	u := srv.URL + "/movie/alice/alice-pw1/101.mkv"

	start := time.Now()
	fetch(t, u, nil)
	waitVODIdle(t, hs, slots) // slots are free while the session waits
	fetch(t, u, map[string]string{"Range": "bytes=0-999"})
	waitVODIdle(t, hs, slots)
	time.Sleep(20 * time.Millisecond)
	fetch(t, u, map[string]string{"Range": "bytes=1000-1999"})
	waitVODIdle(t, hs, slots)
	st.mu.Lock()
	n := len(st.watched)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("reported before the resume window passed: %d", n)
	}

	hs.FlushVOD() // shutdown: reported at once
	cancel()
	<-done
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.watched) != 1 {
		t.Fatalf("watched = %+v, want 1 session", st.watched)
	}
	w := st.watched[0]
	if w.Bytes != int64(len(movie))+2000 || w.EndReason != vodStopped || w.StartedAt.Before(start) ||
		w.EndedAt.Sub(w.StartedAt) < 20*time.Millisecond {
		t.Errorf("session = %+v", w)
	}
}

// waitWatched waits for n sessions in st.
func waitWatched(t *testing.T, st *activityStore, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st.mu.Lock()
		got := len(st.watched)
		st.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watched %d sessions, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
