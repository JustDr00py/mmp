//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestProviderVODCategories: the per-provider category choice hides the
// titles and categories left out, from the catalog page, bouquets and
// customers, and "all" lifts it.
func TestProviderVODCategories(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	xt, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel", Type: "xtream", Url: "http://panel.example/", Username: "u",
		Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.SetProviderVODEnabled(ctx, db.SetProviderVODEnabledParams{ID: xt.ID, VodEnabled: true}))
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: xt.ID, Kind: "movie",
		ExternalIds: []string{"1", "2"}, Names: []string{"EN | Action", "|FR| Films"}}))
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: xt.ID, Kind: "series",
		ExternalIds: []string{"5"}, Names: []string{"EN | Drama"}}))
	must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: xt.ID, ExternalIds: []string{"11", "12"},
		Names: []string{"Die Hard", "Amelie"}, CategoryIds: []string{"1", "2"}, Icons: []string{"", ""}, Ratings: []float32{7, 7},
		ContainerExtensions: []string{"mp4", "mp4"}, TmdbIds: []string{"", ""}, Added: []int64{0, 0}}))
	ids := map[string]int64{}
	for _, kind := range []string{"movie", "series"} {
		cats, _ := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: kind})
		for _, c := range cats {
			ids[c.Name] = c.ID
		}
	}
	// The French category is already granted to a bouquet.
	bq, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "All"})
	if _, err := q.AddVODCategoriesToBouquet(ctx, db.AddVODCategoriesToBouquetParams{BouquetID: bq.ID, CategoryIds: []int64{ids["|FR| Films"]}}); err != nil {
		t.Fatal(err)
	}

	page := "/admin/providers/" + strconv.FormatInt(xt.ID, 10) + "/vod-categories"
	_, body := b.get(page)
	for _, want := range []string{`value="all" checked`, "EN | Action", "movies · English · 1 titles", "series · English",
		`<option value="French">`, "0 of 3 ticked"} {
		if !strings.Contains(body, want) {
			t.Errorf("categories page lacks %q:\n%s", want, body)
		}
	}
	if _, body = b.get("/admin/providers"); !strings.Contains(body, page) {
		t.Error("providers row lacks the VOD categories link")
	}

	// Keep English movies only.
	resp, _ := b.post(page, url.Values{"mode": {"selected"}, "category": {strconv.FormatInt(ids["EN | Action"], 10)}}, b.base)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "msg=saved") || !strings.Contains(loc, "disabled=1") {
		t.Fatalf("save = %d %s", resp.StatusCode, loc)
	}
	_, body = b.get(resp.Header.Get("Location"))
	if !strings.Contains(body, "1 titles outside the chosen categories are hidden now") || !strings.Contains(body, "1 of 3 ticked") {
		t.Errorf("after save:\n%s", body)
	}
	var amelieOff bool
	items, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 10})
	for _, i := range items {
		if i.Name == "Amelie" {
			amelieOff = i.Disabled
		} else if i.Disabled {
			t.Errorf("%s disabled", i.Name)
		}
	}
	if !amelieOff {
		t.Error("Amelie (unticked category) still active")
	}
	cats, _ := q.ListUserVODCategories(ctx, db.ListUserVODCategoriesParams{Kind: "movie", IsAdmin: true})
	if len(cats) != 1 || cats[0].Name != "EN | Action" {
		t.Errorf("customer movie categories = %+v", cats)
	}
	if _, body = b.get("/admin/vod?kind=movie"); strings.Contains(body, "|FR| Films") {
		t.Error("catalog page still offers the unticked category")
	}
	_, body = b.get("/admin/bouquets/" + strconv.FormatInt(bq.ID, 10))
	if !strings.Contains(body, "not chosen on the provider, hidden") || strings.Contains(body, `> EN | Drama <span`) {
		t.Errorf("bouquet page:\n%s", body)
	}

	// All categories again: offered at once (titles return on import).
	resp, _ = b.post(page, url.Values{"mode": {"all"}, "category": {strconv.FormatInt(ids["EN | Action"], 10)}}, b.base)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "mode=all") {
		t.Fatalf("save all = %s", loc)
	}
	if cats, _ := q.ListUserVODCategories(ctx, db.ListUserVODCategoriesParams{Kind: "movie", IsAdmin: true}); len(cats) != 2 {
		t.Errorf("after all: %d movie categories", len(cats))
	}
	if p, _ := q.GetProvider(ctx, xt.ID); p.VodCategoryFilter {
		t.Error("filter still on")
	}
	// The selection is kept for later.
	if _, body = b.get(page); !strings.Contains(body, "1 of 3 ticked") {
		t.Error("selection lost when switching to all")
	}

	// Save and import runs the import; load fetches the categories only.
	resp, _ = b.post(page, url.Values{"mode": {"all"}, "action": {"import"}}, b.base)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "msg=imported") {
		t.Errorf("save and import = %s", loc)
	}
	resp, _ = b.post(page, url.Values{"action": {"load"}}, b.base)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "msg=loaded") {
		t.Errorf("load = %s", loc)
	}
}
