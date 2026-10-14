package web

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/mmp/internal/db"
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
	if name != "live" || !strings.Contains(data, "Channel 1") || !strings.Contains(data, "Stream 7 · Acme") || strings.Contains(data, "<html") {
		t.Fatalf("first event %q:\n%s", name, data)
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
