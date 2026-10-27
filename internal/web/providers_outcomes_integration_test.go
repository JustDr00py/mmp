//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestProviderHealthOnPage: a provider's recent outcomes score it on the
// providers page, and its backbone gets a row in the backbone health table.
func TestProviderHealthOnPage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "STRONG1", Type: "m3u", Url: "http://strong.example/",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Backbone: "Strong8K"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertOutcomeBucket(ctx, db.UpsertOutcomeBucketParams{BucketStart: time.Now().UTC(), ProviderID: p.ID,
		Backbone: "Strong8K", Outcome: "open_ok", Count: 20}); err != nil {
		t.Fatal(err)
	}

	_, body := b.get("/admin/providers")
	for _, want := range []string{"Backbone health", "Strong8K", `pill ok">100</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("providers page lacks %q", want)
		}
	}
}

// TestBackbonePage: a backbone's name links to its details: its providers
// (including one that moved off it since), attempts by result and day,
// and the streams that fail most, from outcomes recorded under it only.
func TestBackbonePage(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	const name = "Big Net / EU" // a label with a space and a slash
	mk := func(pname, backbone string) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: pname, Type: "m3u", Url: "http://" + strings.ToLower(pname) + ".example/",
			MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Backbone: backbone})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	good, flaky, moved, other := mk("GoodLine", name), mk("FlakyLine", name), mk("MovedLine", "Elsewhere"), mk("OtherNet", "Elsewhere")
	stream := func(p db.Provider, sname string) int64 {
		id, err := q.UpsertProviderStream(ctx, db.UpsertProviderStreamParams{ProviderID: p.ID, ExternalID: sname, Name: sname, StreamUrl: "http://p/" + sname})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	bad, fine := stream(flaky, "Bad Sports HD"), stream(good, "Fine News HD")
	now := time.Now().UTC()
	for _, o := range []struct {
		p        db.Provider
		stream   int64
		backbone string
		outcome  string
		count    int32
	}{
		{good, fine, name, "open_ok", 30},
		{flaky, bad, name, "open_ok", 5},
		{flaky, bad, name, "open_failed", 8},
		{flaky, bad, name, "mid_play_failure", 4},
		{moved, 0, name, "open_ok", 12}, // before it moved to Elsewhere
		{other, 0, "Elsewhere", "probe_fail", 99},
	} {
		if err := q.UpsertOutcomeBucket(ctx, db.UpsertOutcomeBucketParams{BucketStart: now, ProviderID: o.p.ID, StreamID: o.stream,
			Backbone: o.backbone, Outcome: o.outcome, Count: o.count}); err != nil {
			t.Fatal(err)
		}
	}
	link := "/admin/providers/backbone?name=" + url.QueryEscape(name)

	_, body := b.get("/admin/providers")
	if !strings.Contains(body, `href="/admin/providers/backbone?name=Big%20Net%20%2f%20EU"`) {
		t.Errorf("backbone table does not link to the details:\n%s", body)
	}
	resp, body := b.get(link)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backbone page = %d", resp.StatusCode)
	}
	flakyAt, goodAt := strings.Index(body, ">FlakyLine</a>"), strings.Index(body, ">GoodLine</a>")
	if flakyAt < 0 || goodAt < flakyAt {
		t.Errorf("providers, worst first:\n%s", body)
	}
	for _, want := range []string{">MovedLine</a>", "now on Elsewhere", "Failed to open", "Failed while playing", ">Bad Sports HD</a>",
		"59 stream attempts", "12 failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("backbone page lacks %q", want)
		}
	}
	for _, unwanted := range []string{"OtherNet", "Probe failed", "Fine News HD"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("backbone page shows %q", unwanted)
		}
	}
	if resp, _ := b.get("/admin/providers/backbone?name=Nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown backbone = %d, want 404", resp.StatusCode)
	}
}
