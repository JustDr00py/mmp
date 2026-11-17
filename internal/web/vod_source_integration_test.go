//go:build integration

package web

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestVODSourceFilter: the catalog page narrows to a backbone or one
// provider, and counts each title's copies on the picked backbone.
func TestVODSourceFilter(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name, backbone string, names, tmdb []string) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + strings.ToLower(name) + ".example/",
			Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Backbone: backbone})
		must(err)
		must(q.SetProviderVODEnabled(ctx, db.SetProviderVODEnabledParams{ID: p.ID, VodEnabled: true}))
		must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: "movie", ExternalIds: []string{"1"}, Names: []string{name + " Action"}}))
		n := len(names)
		ext, cats, icons, exts := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		for i := range names {
			ext[i], cats[i], exts[i] = strconv.Itoa(i+1), "1", "mkv"
		}
		must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: p.ID, ExternalIds: ext, Names: names, CategoryIds: cats,
			Icons: icons, Ratings: make([]float32, n), ContainerExtensions: exts, TmdbIds: tmdb, Added: make([]int64, n)}))
		return p
	}
	ta := mk("TrexA", "TREX", []string{"Heat on TrexA", "Ronin on TrexA"}, []string{"949", "8195"})
	tb := mk("TrexB", "TREX", []string{"Heat on TrexB"}, []string{"949"})
	gold := mk("Gold", "GOLD", []string{"Heat on Gold"}, []string{"949"})

	rows, err := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{ProviderIds: []int64{ta.ID, tb.ID}, Limit: 50})
	must(err)
	if len(rows) != 3 {
		t.Fatalf("TREX rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		want := map[string][2]int64{"Heat on TrexA": {3, 2}, "Heat on TrexB": {3, 2}, "Ronin on TrexA": {1, 1}}[r.Name]
		if r.Copies != want[0] || r.SourceCopies != want[1] {
			t.Errorf("%s: copies %d, on TREX %d, want %v", r.Name, r.Copies, r.SourceCopies, want)
		}
	}

	_, body := b.get("/admin/vod?kind=movie&source=b:TREX")
	if !strings.Contains(body, "Heat on TrexB") || strings.Contains(body, "Heat on Gold") {
		t.Errorf("backbone page lists the wrong providers")
	}
	if !strings.Contains(body, "2 on TREX") || !strings.Contains(body, "All of TREX (2 providers)") {
		t.Errorf("backbone page lacks its copies count or picker option")
	}
	if strings.Contains(body, "Gold Action") {
		t.Errorf("backbone page offers another provider's category")
	}

	_, body = b.get("/admin/vod?kind=movie&source=p:" + strconv.FormatInt(gold.ID, 10))
	if !strings.Contains(body, "Heat on Gold") || strings.Contains(body, "Heat on TrexA") || strings.Contains(body, " on GOLD<") {
		t.Errorf("provider page lists the wrong titles")
	}

	// A category of another source is dropped, not applied.
	cats, err := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: "movie", ProviderID: &gold.ID})
	must(err)
	_, body = b.get("/admin/vod?kind=movie&source=b:TREX&category=" + strconv.FormatInt(cats[0].ID, 10))
	if !strings.Contains(body, "Heat on TrexA") {
		t.Errorf("other source's category was applied")
	}

	// An unknown source shows everything.
	if _, body = b.get("/admin/vod?kind=movie&source=b:NOPE"); !strings.Contains(body, "Heat on Gold") || !strings.Contains(body, "Heat on TrexA") {
		t.Errorf("unknown source filtered the page")
	}
}
