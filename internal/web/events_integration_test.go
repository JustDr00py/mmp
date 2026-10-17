//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/events"
)

func TestEventsPage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	mkProv := func(name string) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "m3u", Url: "http://" + name + "/",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pa, pb := mkProv("Alpha"), mkProv("Beta")
	mkStream := func(p db.Provider, name string) int64 {
		id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: name, Name: name,
			StreamUrl: "http://x/" + name})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	espnA, espnB := mkStream(pa, "ESPN HD"), mkStream(pb, "ESPN")
	ch, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "ESPN"})
	other, _ := q.CreateChannel(ctx, db.CreateChannelParams{Name: "CNN"})
	now := time.Now()
	ins := func(at time.Time, typ string, chID, prov, from, to int64, msg string) {
		t.Helper()
		nz := func(v int64) *int64 {
			if v == 0 {
				return nil
			}
			return &v
		}
		if err := q.InsertEvent(ctx, db.InsertEventParams{Ts: at, Type: typ, ChannelID: nz(chID), ProviderID: nz(prov),
			FromStreamID: nz(from), ToStreamID: nz(to), Message: msg}); err != nil {
			t.Fatal(err)
		}
	}
	ins(now.Add(-time.Minute), "failover", ch.ID, pb.ID, espnA, espnB, "no data for 12s")
	ins(now.Add(-2*time.Minute), "open_failed", ch.ID, pa.ID, espnA, 0, "upstream status 503")
	ins(now.Add(-3*time.Minute), "exhausted", other.ID, 0, 0, 0, "no stream played for 30s")
	ins(now.AddDate(0, 0, -3), "failover", ch.ID, pb.ID, espnA, espnB, "switched upstream") // old wording

	_, body := b.get("/admin/events")
	for _, want := range []string{
		"ESPN HD · Alpha → ESPN · Beta", "no data for 12s", // why, and which providers
		"ESPN HD · Alpha</span>", "upstream status 503", // the stream that would not open
		"no stream played for 30s",
		now.AddDate(0, 0, -3).Local().Format("Jan 2 15:04"), // older events carry their date
	} {
		if !strings.Contains(body, want) {
			t.Errorf("events page lacks %q", want)
		}
	}
	if strings.Contains(body, "switched upstream") {
		t.Error("the old placeholder message is shown")
	}

	count := func(path string) int {
		t.Helper()
		_, body := b.get(path)
		return strings.Count(body, "<li><span class=\"mono muted\"")
	}
	for path, want := range map[string]int{
		"/admin/events":                                            4,
		"/admin/events?type=failover":                              2,
		"/admin/events?type=exhausted":                             1,
		"/admin/events?channel=" + strconv.FormatInt(other.ID, 10): 1,
		"/admin/events?provider=" + strconv.FormatInt(pa.ID, 10):   3, // its own open failure and the failovers away from it
		"/admin/events?provider=" + strconv.FormatInt(pb.ID, 10):   2,
	} {
		if got := count(path); got != want {
			t.Errorf("%s lists %d events, want %d", path, got, want)
		}
	}
	if _, body := b.get("/admin/events?channel=" + strconv.FormatInt(ch.ID, 10)); !strings.Contains(body, "Events: ESPN") {
		t.Error("channel filter not named in the heading")
	}
	if _, body := b.get("/admin/"); !strings.Contains(body, "no data for 12s") || !strings.Contains(body, `href="/admin/events"`) {
		t.Error("dashboard lacks the reason or the link to all events")
	}

	// Retention: saved on the Settings page, applied by the pruner.
	resp, _ := b.post("/admin/settings/events", url.Values{"keep_days": {"2"}}, b.base)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save retention = %d", resp.StatusCode)
	}
	if resp, body := b.post("/admin/settings/events", url.Values{"keep_days": {"0"}}, b.base); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "between 1 and 3650 days") {
		t.Errorf("0 days = %d", resp.StatusCode)
	}
	if _, body := b.get("/admin/settings"); !strings.Contains(body, `name="keep_days" min="1" max="3650" value="2"`) {
		t.Error("settings page lacks the saved retention")
	}
	if n, err := events.Prune(ctx, q, now); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want the 3-day-old event deleted", n, err)
	}
	if got := count("/admin/events"); got != 3 {
		t.Errorf("after pruning: %d events, want 3", got)
	}
}
