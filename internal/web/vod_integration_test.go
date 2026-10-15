//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sysadmin/mmp/internal/db"
)

func TestVODPages(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	xt, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel", Type: "xtream", Url: "http://panel.example/", Username: "u",
		Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	m3u, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "List", Type: "m3u", Url: "http://list.example/x.m3u",
		MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})

	// Provider form: the checkbox turns VOD on for Xtream, never for M3U.
	edit := func(p db.Provider, typ string) {
		f := url.Values{"name": {p.Name}, "type": {typ}, "url": {p.Url}, "max_connections": {"1"}, "refresh_minutes": {"60"},
			"enabled": {"on"}, "vod_enabled": {"on"}}
		if resp, body := b.post("/admin/providers/"+strconv.FormatInt(p.ID, 10)+"/edit", f, b.base); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("edit %s = %d\n%s", p.Name, resp.StatusCode, body)
		}
	}
	edit(xt, "xtream")
	edit(m3u, "m3u")
	if p, _ := q.GetProvider(ctx, xt.ID); !p.VodEnabled {
		t.Error("xtream provider: VOD not enabled")
	}
	if p, _ := q.GetProvider(ctx, m3u.ID); p.VodEnabled {
		t.Error("m3u provider got VOD enabled")
	}
	_, body := b.get("/admin/providers/" + strconv.FormatInt(xt.ID, 10) + "/edit")
	if !strings.Contains(body, `name="vod_enabled" checked`) {
		t.Error("form does not show VOD on")
	}

	// Import button on the row (only for VOD providers) and its result.
	_, body = b.get("/admin/providers")
	if strings.Count(body, "Import catalog") != 1 {
		t.Errorf("Import catalog buttons = %d, want 1", strings.Count(body, "Import catalog"))
	}
	resp, body := b.htmx(http.MethodPost, "/admin/providers/"+strconv.FormatInt(xt.ID, 10)+"/vod", nil, "provider-1")
	if resp.StatusCode != 200 || !strings.Contains(body, "321 movies, 12 series") || !strings.HasPrefix(strings.TrimSpace(body), "<tr id=") {
		t.Errorf("import = %d\n%s", resp.StatusCode, body)
	}

	// Seed a catalog as an import would.
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: xt.ID, Kind: "movie", ExternalIds: []string{"1", "2"}, Names: []string{"Action", "Kids"}}))
	must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: xt.ID, Kind: "series", ExternalIds: []string{"5"}, Names: []string{"Drama"}}))
	must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: xt.ID, ExternalIds: []string{"11", "12", "21"}, Names: []string{"Die Hard", "Speed", "Cars"},
		CategoryIds: []string{"1", "1", "2"}, Icons: []string{"", "", ""}, Ratings: []float32{7.5, -1, 6}, ContainerExtensions: []string{"mkv", "mp4", "mp4"},
		TmdbIds: []string{"", "", ""}, Added: []int64{0, 0, 0}}))
	must(q.UpsertSeries(ctx, db.UpsertSeriesParams{ProviderID: xt.ID, ExternalIds: []string{"7"}, Names: []string{"The Show"}, CategoryIds: []string{"5"},
		Covers: []string{""}, Plots: []string{"A plot."}, Casts: []string{""}, Directors: []string{""}, Genres: []string{""},
		ReleaseDates: []string{"2020-01-01"}, Ratings: []float32{8}, LastModified: []int64{0}}))
	var showID int64
	shows, _ := q.ListSeriesAdmin(ctx, db.ListSeriesAdminParams{Limit: 10})
	showID = shows[0].ID
	must(q.UpsertEpisodes(ctx, db.UpsertEpisodesParams{SeriesID: showID, ExternalIds: []string{"701", "702", "801"}, Seasons: []int32{1, 1, 2},
		EpisodeNums: []int32{1, 2, 1}, Titles: []string{"Pilot", "Second", "Return"}, ContainerExtensions: []string{"mkv", "mkv", "mkv"},
		Plots: []string{"", "", ""}, Durations: []int32{2700, 0, 0}, Images: []string{"", "", ""}, Added: []int64{0, 0, 0}}))

	// Catalog page.
	_, body = b.get("/admin/vod")
	if !strings.Contains(body, "Die Hard") || !strings.Contains(body, "Cars") || !strings.Contains(body, "Action (2) · Panel") || !strings.Contains(body, `aria-current="page">Movies &amp; Series`) {
		t.Errorf("movies page:\n%s", body)
	}
	_, body = b.htmx(http.MethodGet, "/admin/vod?kind=movie&q=car", nil, "vod-list")
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="vod-list">`) || !strings.Contains(body, "Cars") || strings.Contains(body, "Die Hard") {
		t.Errorf("search fragment:\n%s", body)
	}
	_, body = b.get("/admin/vod?kind=series")
	if !strings.Contains(body, `<a href="/admin/vod/series/`+strconv.FormatInt(showID, 10)+`">The Show</a>`) {
		t.Errorf("series page:\n%s", body)
	}
	_, body = b.get("/admin/vod/series/" + strconv.FormatInt(showID, 10))
	if !strings.Contains(body, "Season 1") || !strings.Contains(body, "Season 2") || !strings.Contains(body, "Pilot") || !strings.Contains(body, "2700s") {
		t.Errorf("series detail:\n%s", body)
	}
	if resp, _ := b.get("/admin/vod/series/999999"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown series = %d", resp.StatusCode)
	}

	// Bouquet categories.
	bq, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "Movies"})
	base := "/admin/bouquets/" + strconv.FormatInt(bq.ID, 10)
	_, body = b.get(base)
	if !strings.Contains(body, "No movie or series categories") || !strings.Contains(body, "Action <span class=\"sub\">movies · 2 titles · Panel") {
		t.Fatalf("bouquet vod section:\n%s", body)
	}
	cats, _ := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: "movie"})
	var action int64
	for _, c := range cats {
		if c.Name == "Action" {
			action = c.ID
		}
	}
	if resp, _ := b.post(base+"/vod-categories", url.Values{"category": {strconv.FormatInt(action, 10)}}, b.base); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add = %d", resp.StatusCode)
	}
	if got, _ := q.ListBouquetVODCategories(ctx, bq.ID); len(got) != 1 || got[0].Name != "Action" {
		t.Fatalf("granted = %+v", got)
	}
	b.post(base+"/vod-categories/"+strconv.FormatInt(action, 10)+"/remove", url.Values{}, b.base)
	if got, _ := q.ListBouquetVODCategories(ctx, bq.ID); len(got) != 0 {
		t.Errorf("after remove = %+v", got)
	}
}
