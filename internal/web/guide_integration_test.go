//go:build integration

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

func TestGuidePage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Lists", Type: "m3u", Url: "http://lists/",
		EpgUrl: "http://guide.example/epg.xml", RefreshIntervalSecs: 3600, Priority: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	src, err := q.UpsertEpgSource(ctx, db.UpsertEpgSourceParams{Name: "Lists", Url: p.EpgUrl, ProviderID: &p.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := q.InsertEpgPrograms(ctx, []db.InsertEpgProgramsParams{
		{EpgSourceID: src.ID, ChannelTvgID: "BBC1.uk", Title: "The News", Description: "Headlines.", StartTime: now.Add(-10 * time.Minute), StopTime: now.Add(20 * time.Minute)},
		{EpgSourceID: src.ID, ChannelTvgID: "BBC1.uk", Title: "Late Film", StartTime: now.Add(30 * time.Hour), StopTime: now.Add(32 * time.Hour)},
		{EpgSourceID: src.ID, ChannelTvgID: "old.uk", Title: "Yesterday", StartTime: now.Add(-30 * time.Hour), StopTime: now.Add(-29 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct{ name, tvg string }{{"BBC One", "bbc1.uk"}, {"Old Channel", "old.uk"}, {"No Tvg", ""}} {
		n := int32(101 + i)
		if _, err := q.CreateChannel(ctx, db.CreateChannelParams{Number: &n, Name: c.name, TvgID: c.tvg}); err != nil {
			t.Fatal(err)
		}
	}

	_, body := b.get("/admin/guide")
	// Case-insensitive tvg-id match, with its details in a popover.
	if !strings.Contains(body, `<span class="gp-title">The News</span>`) || !strings.Contains(body, "<p>Headlines.</p>") {
		t.Errorf("BBC One's program missing:\n%s", body)
	}
	if strings.Contains(body, "Late Film") || strings.Contains(body, "Yesterday") {
		t.Error("programs outside the window shown")
	}
	// The default stream-name guide fills the others with their names.
	if strings.Count(body, "· from stream name") != 2 {
		t.Errorf("stream-name rows: %d", strings.Count(body, "· from stream name"))
	}
	if !strings.Contains(body, `class="guide-now"`) || !strings.Contains(body, `href="/admin/epg"`) || !strings.Contains(body, ">EPG<") {
		t.Error("now line or EPG link missing")
	}

	_, body = b.htmx(http.MethodGet, "/admin/guide?missing=1", nil, "guide-grid")
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="guide-grid">`) || strings.Contains(body, "BBC One") ||
		!strings.Contains(body, "Old Channel") || !strings.Contains(body, "No Tvg") {
		t.Errorf("missing-only fragment:\n%s", body)
	}

	_, body = b.get("/admin/guide?q=BBC1")
	if !strings.Contains(body, "BBC One") || strings.Contains(body, "Old Channel") {
		t.Error("tvg-id search")
	}

	// A day ahead: the late film; a start before the served window is
	// clamped to its beginning, with no earlier window.
	_, body = b.get("/admin/guide?start=" + strconv.FormatInt(now.Add(29*time.Hour+30*time.Minute).Unix(), 10))
	if !strings.Contains(body, "Late Film") || strings.Contains(body, "The News") {
		t.Error("later window")
	}
	_, body = b.get("/admin/guide?start=0")
	if !strings.Contains(body, `<span class="button secondary small" aria-disabled="true">← Earlier</span>`) {
		t.Error("window not clamped to the served range")
	}
}
