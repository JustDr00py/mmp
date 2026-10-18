package output

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func waitViewers(t *testing.T, a *Access, n int) []Viewer {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		vs := a.Viewers()
		if len(vs) == n {
			return vs
		}
		if time.Now().After(deadline) {
			t.Fatalf("viewers = %+v, want %d", vs, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestViewersListAndKickTS: a TS viewer is listed with user and address,
// and a kick ends its stream and frees the user's slot.
func TestViewersListAndKickTS(t *testing.T) {
	srv, hs, _ := newLiveHandlers(t)
	code, stop := play(t, srv.URL, "/live/alice/alice-pw1/10.ts")
	if code != 200 {
		t.Fatalf("play = %d", code)
	}
	defer stop()
	access := hs.Access
	vs := waitViewers(t, access, 1)
	if v := vs[0]; v.ChannelID != 10 || v.Username != "alice" || v.Addr != "127.0.0.1" || v.Output != "ts" || v.Since.IsZero() {
		t.Errorf("viewer = %+v", v)
	}
	if access.Kick(999) {
		t.Error("kicked an unknown viewer")
	}
	if !access.Kick(vs[0].ID) {
		t.Fatal("kick = false")
	}
	waitViewers(t, access, 0)
	// The handler unlists the viewer before its deferred slot release
	// runs, so the slot frees a moment after the list empties.
	deadline := time.Now().Add(3 * time.Second)
	for access.ActiveConnections(1) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("alice still holds %d slots after the kick", access.ActiveConnections(1))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestViewersKickHLS: kicking an HLS viewer ends its lease at once.
func TestViewersKickHLS(t *testing.T) {
	srv, hs, _ := newHLSServer(t, 5*time.Second)
	resp, _ := httpGet(t, noRedirect, srv.URL+"/live/alice/alice-pw1/10.m3u8")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	vs := waitViewers(t, hs.Access, 1)
	if vs[0].Output != "hls" || vs[0].Username != "alice" || vs[0].ChannelID != 10 {
		t.Errorf("viewer = %+v", vs[0])
	}
	if !hs.Access.Kick(vs[0].ID) {
		t.Fatal("kick = false")
	}
	waitViewers(t, hs.Access, 0)
	if hs.HLS.Leases() != 0 || hs.Access.ActiveConnections(1) != 0 {
		t.Errorf("after kick: leases %d, slots %d", hs.HLS.Leases(), hs.Access.ActiveConnections(1))
	}
	r, err := http.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("kicked lease playlist = %d, want 404", r.StatusCode)
	}
}
