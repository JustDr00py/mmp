package web

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/output"
)

func TestSSEEvent(t *testing.T) {
	got := string(sseEvent("live", []byte("<p>a</p>\r\n<p>b</p>\n")))
	if want := "event: live\ndata: <p>a</p>\ndata: <p>b</p>\n\n"; got != want {
		t.Errorf("sseEvent = %q, want %q", got, want)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		4 * time.Second: "4s", 75 * time.Second: "1m15s", 3*time.Hour + 5*time.Minute: "3h05m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v = %q, want %q", d, got, want)
		}
	}
}

// readEvent reads one SSE event (until a blank line) and returns its
// name and joined data.
func readEvent(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var name string
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if name != "" || len(data) > 0 {
				return name, strings.Join(data, "\n")
			}
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
}

func TestLiveStream(t *testing.T) {
	srv, _, ui := newUIWith(t)
	b := newBrowser(t, srv.URL)
	b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/live", nil)
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("live = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(resp.Body)
	name, data := readEvent(t, rd)
	if name != "live" || !strings.Contains(data, "Channel 1") || strings.Contains(data, "<html") {
		t.Fatalf("first event %q:\n%s", name, data)
	}
	// The card: what plays, the picker on the current stream, details and
	// figures from the session.
	for _, want := range []string{`<option value="7" selected>Stream 7 [Acme]</option>`, `<option value="8">Stream 8 [Acme]</option>`,
		"FFmpeg · Remux", "Now playing: <span class=\"muted\">Storage Wars", "1280×720", "29.97 FPS", "H264", "AAC", "STEREO",
		"2.48 Mbps", "13.66 Mbps", "17.71 MB", `action="/admin/live/channels/1/stop"`, `action="/admin/live/channels/1/switch"`} {
		if !strings.Contains(data, want) {
			t.Errorf("live card lacks %q", want)
		}
	}

	// A new event (a failover) appears in the next pushed snapshot.
	from, to, ch := int64(7), int64(8), int64(1)
	store := ui.Store.(*fakeStore)
	store.mu.Lock()
	store.events = []db.Event{{Ts: time.Now(), Type: "failover", ChannelID: &ch, FromStreamID: &from, ToStreamID: &to}}
	store.mu.Unlock()
	for {
		_, data = readEvent(t, rd)
		if strings.Contains(data, "Stream 7 · Acme → Stream 8 · Acme") {
			break
		}
	}

	// Signed out: no stream (redirect to login).
	anon := newBrowser(t, srv.URL)
	if resp, _ := anon.get("/admin/live"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous live = %d", resp.StatusCode)
	}
}

func TestLiveStreamStopsOnShutdown(t *testing.T) {
	srv, _, ui := newUIWith(t)
	ctx, stop := context.WithCancel(context.Background())
	ui.Ctx = ctx
	b := newBrowser(t, srv.URL)
	b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/live", nil)
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	readEvent(t, rd)
	stop()
	done := make(chan struct{})
	go func() {
		for {
			if _, err := rd.ReadString('\n'); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream still open after shutdown")
	}
}

func TestLiveControls(t *testing.T) {
	srv, _, ui := newUIWith(t)
	viewers := &fakeViewers{list: []output.Viewer{
		{ID: 5, ChannelID: 1, UserID: 9, Username: "sysadmin", Addr: "10.89.0.2", Output: "ts", Since: time.Now().Add(-90 * time.Second)},
		{ID: 6, ChannelID: 99, Username: "elsewhere", Addr: "10.0.0.9", Output: "hls", Since: time.Now()}, // not live: not shown
	}}
	ui.Viewers = viewers
	live := ui.Live.(*fakeLive)
	b := newBrowser(t, srv.URL)
	b.post("/admin/login", url.Values{"username": {"boss"}, "password": {"boss-pass-1"}}, srv.URL)

	_, body := b.get("/admin/")
	for _, want := range []string{"10.89.0.2", "sysadmin", "1m30s", `action="/admin/live/viewers/5/kick"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(body, "elsewhere") {
		t.Error("a viewer of a channel that is not live is listed")
	}

	post := func(path string, f url.Values) (int, string) {
		resp, _ := b.post(path, f, srv.URL)
		return resp.StatusCode, resp.Header.Get("Location")
	}
	if code, loc := post("/admin/live/channels/1/stop", nil); code != http.StatusSeeOther || loc != "/admin/?msg=stopped" {
		t.Errorf("stop = %d %s", code, loc)
	}
	if code, loc := post("/admin/live/channels/3/stop", nil); loc != "/admin/?msg=gone" {
		t.Errorf("stop of an idle channel = %d %s", code, loc)
	}
	if code, loc := post("/admin/live/channels/1/switch", url.Values{"stream": {"8"}}); code != http.StatusSeeOther || loc != "/admin/?msg=switched" {
		t.Errorf("switch = %d %s", code, loc)
	}
	if code, _ := post("/admin/live/channels/1/switch", url.Values{"stream": {"77"}}); code != http.StatusConflict {
		t.Errorf("switch to a stream not on the channel = %d, want 409", code)
	}
	if code, _ := post("/admin/live/channels/1/switch", url.Values{"stream": {"x"}}); code != http.StatusBadRequest {
		t.Errorf("switch without a stream = %d, want 400", code)
	}
	if code, loc := post("/admin/live/viewers/5/kick", nil); code != http.StatusSeeOther || loc != "/admin/?msg=kicked" {
		t.Errorf("kick = %d %s", code, loc)
	}
	if _, loc := post("/admin/live/viewers/404/kick", nil); loc != "/admin/?msg=gone" {
		t.Errorf("kick of an unknown viewer → %s", loc)
	}
	live.mu.Lock()
	if !slices.Equal(live.stopped, []int64{1, 3}) || len(live.switched) != 1 || live.switched[0] != [2]int64{1, 8} {
		t.Errorf("stopped %v, switched %v", live.stopped, live.switched)
	}
	live.mu.Unlock()
	if !slices.Equal(viewers.kicked, []uint64{5, 404}) {
		t.Errorf("kicked %v", viewers.kicked)
	}
	if _, body := b.get("/admin/?msg=kicked"); !strings.Contains(body, "Viewer disconnected.") {
		t.Error("kick flash missing")
	}

	// Cross-site posts are refused like every other admin form.
	if resp, _ := b.post("/admin/live/channels/1/stop", nil, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site stop = %d, want 403", resp.StatusCode)
	}
}
