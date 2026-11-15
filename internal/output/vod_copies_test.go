package output

import (
	"net/http"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// copiesOf101 makes movie 101 a title with one copy per given provider and
// panel stream id (11 plays; anything else is a 404 on the fake panel).
func copiesOf101(hs *Handlers, copies map[int64]string, order ...int64) {
	var rows []db.ListUserVODItemCopiesRow
	for _, p := range order {
		rows = append(rows, db.ListUserVODItemCopiesRow{ID: 101, ProviderID: p, ExternalID: copies[p], Name: "Heat",
			ContainerExtension: "mkv", CategoryID: 1})
	}
	hs.VOD.(*fakeVOD).copies101 = rows
}

// TestVODPlaybackCopyFallback: a fresh start whose first copy is missing
// upstream plays the next copy, and the failed provider's slot is freed.
func TestVODPlaybackCopyFallback(t *testing.T) {
	srv, hs, _, slots := newPlaybackServer(t, make([]byte, 1<<20), 1)
	copiesOf101(hs, map[int64]string{1: "404", 2: "11"}, 1, 2)
	resp, cancel := openPlayback(t, srv.URL+"/movie/alice/alice-pw1/101.mkv")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback = %d, want 200 from the second copy", resp.StatusCode)
	}
	if slots.InUse(1) != 0 || slots.InUse(2) != 1 {
		t.Errorf("slots: provider 1 %d, provider 2 %d; want 0 and 1", slots.InUse(1), slots.InUse(2))
	}
	cancel()
	// Every copy missing: the last copy's answer stands.
	copiesOf101(hs, map[int64]string{1: "404", 2: "405"}, 1, 2)
	waitVODIdle(t, hs, slots)
	if r, _ := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("all copies missing = %d, want 404", r.StatusCode)
	}
	// A single copy keeps its own answer.
	copiesOf101(hs, map[int64]string{1: "404"}, 1)
	if r, _ := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("one missing copy = %d, want 404", r.StatusCode)
	}
}

// TestVODPlaybackResumeSticks: a resume (a range past the start) after the
// playback ended goes back to the copy it started on, even when another
// panel's copy is free; a fresh start may use any copy.
func TestVODPlaybackResumeSticks(t *testing.T) {
	srv, hs, _, slots := newPlaybackServer(t, make([]byte, 1<<20), 1)
	copiesOf101(hs, map[int64]string{1: "11", 2: "11"}, 1, 2)
	if r, _ := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("alice = %d", r.StatusCode)
	}
	waitVODIdle(t, hs, slots)
	// root now holds provider 1, alice's copy.
	if r, _ := openPlayback(t, srv.URL+"/movie/root/root-pw1/101.mkv"); r.StatusCode != http.StatusOK {
		t.Fatalf("root = %d", r.StatusCode)
	}
	if slots.InUse(1) != 1 {
		t.Fatalf("root not on provider 1 (in use: %d, %d)", slots.InUse(1), slots.InUse(2))
	}
	if r, _ := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", map[string]string{"Range": "bytes=5000-"}); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resume with its copy busy = %d, want 503 (never another panel's file)", r.StatusCode)
	}
	if r, _ := fetch(t, srv.URL+"/movie/alice/alice-pw1/101.mkv", nil); r.StatusCode != http.StatusOK {
		t.Errorf("fresh start = %d, want 200 from provider 2", r.StatusCode)
	}
}

// TestVODPlaybackSpread: providers of equal priority share playbacks
// instead of the first filling up.
func TestVODPlaybackSpread(t *testing.T) {
	srv, hs, _, slots := newPlaybackServer(t, make([]byte, 16<<20), 2)
	copiesOf101(hs, map[int64]string{1: "11", 2: "11"}, 1, 2)
	for _, u := range []string{"alice/alice-pw1", "root/root-pw1"} {
		if r, _ := openPlayback(t, srv.URL+"/movie/"+u+"/101.mkv"); r.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", u, r.StatusCode)
		}
	}
	if slots.InUse(1) != 1 || slots.InUse(2) != 1 {
		t.Errorf("slots: provider 1 %d, provider 2 %d; want one each", slots.InUse(1), slots.InUse(2))
	}
}
