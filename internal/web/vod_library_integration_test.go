//go:build integration

package web

import (
	"context"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/vodmeta"
)

// TestVODLibrary: copies of one title on unpooled providers of different
// panels list once (by TMDB id, lent between copies by title and year),
// while other languages, 4K and yearless titles stay apart; playback
// lookups return every copy the user may see, in priority order, and a
// series' episodes match across providers by season and number.
func TestVODLibrary(t *testing.T) {
	_, q := pgUI(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	type movie struct{ id, name, cat, tmdb string }
	cats := map[string]string{"1": "EN | Action", "2": "FR | Films"}
	movies := func(p db.Provider, ms ...movie) {
		t.Helper()
		arg := db.UpsertVODItemsParams{ProviderID: p.ID}
		for _, m := range ms {
			pm := vodmeta.Parse(m.name, cats[m.cat])
			arg.ExternalIds, arg.Names, arg.CategoryIds = append(arg.ExternalIds, m.id), append(arg.Names, m.name), append(arg.CategoryIds, m.cat)
			arg.Icons, arg.Ratings, arg.ContainerExtensions = append(arg.Icons, ""), append(arg.Ratings, -1), append(arg.ContainerExtensions, "mkv")
			arg.TmdbIds, arg.Added = append(arg.TmdbIds, m.tmdb), append(arg.Added, 0)
			arg.Titles, arg.TitleNorms, arg.Years = append(arg.Titles, pm.Title), append(arg.TitleNorms, pm.Key()), append(arg.Years, int32(pm.Year))
			arg.Qualities, arg.Langs = append(arg.Qualities, pm.Quality), append(arg.Langs, pm.Lang)
		}
		must(q.UpsertVODItems(ctx, arg))
	}
	mk := func(name, backbone string, priority int32) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + strings.ToLower(name) + ".example/",
			Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Priority: priority, Backbone: backbone})
		must(err)
		must(q.SetProviderVODEnabled(ctx, db.SetProviderVODEnabledParams{ID: p.ID, VodEnabled: true}))
		must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: "movie", ExternalIds: []string{"1", "2"}, Names: []string{cats["1"], cats["2"]}}))
		must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: "series", ExternalIds: []string{"5"}, Names: []string{"EN | Drama"}}))
		return p
	}
	// a's rows are older; b has the better priority.
	a := mk("Alpha", "ALPHA", 1)
	b := mk("Bravo", "BRAVO", 0)
	movies(a,
		movie{"11", "EN | The Matrix (1999)", "1", "603"},
		movie{"12", "Heat (1995)", "1", ""},
		movie{"13", "The Matrix (1999)", "2", "603"}, // French category: another title
		movie{"14", "The Matrix 4K (1999)", "1", "603"},
		movie{"15", "Untitled Thing", "1", ""},
	)
	movies(b,
		movie{"21", "The Matrix - 1999", "1", ""}, // no id: lent a's by title and year
		movie{"22", "Heat 1995", "1", ""},         // neither has an id: merged by title and year
		movie{"25", "Untitled Thing", "1", ""},    // no year: never merged across panels
	)
	if _, err := q.InheritVODItemTmdbIDs(ctx); err != nil {
		t.Fatal(err)
	}

	byKey := map[string]int64{} // provider/external id -> row id
	items, err := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 50})
	must(err)
	for _, i := range items {
		byKey[map[int64]string{a.ID: "a", b.ID: "b"}[i.ProviderID]+i.ExternalID] = i.ID
	}

	list, err := q.ListUserVODItems(ctx, db.ListUserVODItemsParams{IsAdmin: true})
	must(err)
	listed := map[int64]bool{}
	for _, m := range list {
		listed[m.ID] = true
	}
	want := []string{"a11", "a12", "a13", "a14", "a15", "b25"}
	if len(list) != len(want) {
		t.Errorf("listed %d titles %+v, want %v", len(list), list, want)
	}
	for _, k := range want {
		if !listed[byKey[k]] {
			t.Errorf("%s not listed (listed: %+v)", k, list)
		}
	}

	// Copies of the Matrix: b first (priority 0), then a; never a's French
	// or 4K copy.
	copies, err := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: byKey["a11"], IsAdmin: true})
	must(err)
	if len(copies) != 2 || copies[0].ID != byKey["b21"] || copies[1].ID != byKey["a11"] {
		t.Errorf("matrix copies = %+v", copies)
	}
	// Any copy's id finds them all.
	if c, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: byKey["b22"], IsAdmin: true}); len(c) != 2 {
		t.Errorf("heat copies from b's id = %+v", c)
	}
	if c, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: byKey["a15"], IsAdmin: true}); len(c) != 1 {
		t.Errorf("yearless copies = %+v, want only itself", c)
	}

	// A customer granted only a's Action sees a's copies only, and plays
	// them from a alone.
	var aAction, bAction int64
	adm, _ := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: "movie"})
	for _, c := range adm {
		if c.ExternalID == "1" && c.ProviderID == a.ID {
			aAction = c.ID
		}
		if c.ExternalID == "1" && c.ProviderID == b.ID {
			bAction = c.ID
		}
	}
	u, err := q.CreateUser(ctx, db.CreateUserParams{Username: "cust", PasswordHash: "x", Role: "customer", Enabled: true, StreamToken: "tok-cust", MaxConnections: 1})
	must(err)
	bq, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "A action"})
	_, err = q.AddVODCategoriesToBouquet(ctx, db.AddVODCategoriesToBouquetParams{BouquetID: bq.ID, CategoryIds: []int64{aAction}})
	must(err)
	must(q.AddUserBouquets(ctx, db.AddUserBouquetsParams{UserID: u.ID, BouquetIds: []int64{bq.ID}}))
	if c, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: byKey["a11"], UserID: u.ID}); len(c) != 1 || c[0].ProviderID != a.ID {
		t.Errorf("customer matrix copies = %+v, want a's only", c)
	}
	if c, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: byKey["b21"], UserID: u.ID}); len(c) != 1 || c[0].ProviderID != a.ID {
		t.Errorf("customer copies by b's id = %+v, want a's only", c)
	}

	// Filtering by b's Action lists the merged titles under b's category,
	// with the same ids as the full listing.
	inB, err := q.ListUserVODItems(ctx, db.ListUserVODItemsParams{IsAdmin: true, CategoryID: &bAction})
	must(err)
	if len(inB) != 3 {
		t.Errorf("in b's Action: %+v, want Matrix, Heat and b's Untitled", inB)
	}
	for _, m := range inB {
		if !listed[m.ID] || m.CategoryID != bAction {
			t.Errorf("in b's Action: %+v (id listed: %v, want category %d)", m, listed[m.ID], bAction)
		}
	}

	// An admin's fix is locked: imports keep it, and '' splits the title.
	must(q.SetVODItemMatch(ctx, db.SetVODItemMatchParams{ID: byKey["b21"], TmdbID: ""}))
	movies(b, movie{"21", "The Matrix - 1999", "1", "603"})
	if _, err := q.InheritVODItemTmdbIDs(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := q.GetVODItem(ctx, byKey["b21"]); it.TmdbID != "" || !it.MatchLocked || it.MatchSource != "manual" {
		t.Errorf("locked item after import = tmdb %q locked %v source %q", it.TmdbID, it.MatchLocked, it.MatchSource)
	}
	if c, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: byKey["a11"], IsAdmin: true}); len(c) != 1 {
		t.Errorf("after split: matrix copies = %+v, want a's only", c)
	}

	// Series: a sends an id, b a release date; episodes match by number.
	must(q.UpsertSeries(ctx, db.UpsertSeriesParams{ProviderID: a.ID, ExternalIds: []string{"7"}, Names: []string{"Breaking Bad (2008)"}, CategoryIds: []string{"5"},
		Covers: []string{""}, Plots: []string{""}, Casts: []string{""}, Directors: []string{""}, Genres: []string{""},
		ReleaseDates: []string{""}, Ratings: []float32{-1}, LastModified: []int64{0}, TmdbIds: []string{"1396"},
		Titles: []string{"Breaking Bad"}, TitleNorms: []string{"breaking bad"}, Years: []int32{2008}, Qualities: []string{""}, Langs: []string{"English"}}))
	must(q.UpsertSeries(ctx, db.UpsertSeriesParams{ProviderID: b.ID, ExternalIds: []string{"8"}, Names: []string{"Breaking Bad"}, CategoryIds: []string{"5"},
		Covers: []string{""}, Plots: []string{""}, Casts: []string{""}, Directors: []string{""}, Genres: []string{""},
		ReleaseDates: []string{"2008-01-20"}, Ratings: []float32{-1}, LastModified: []int64{0}, TmdbIds: []string{""},
		Titles: []string{"Breaking Bad"}, TitleNorms: []string{"breaking bad"}, Years: []int32{2008}, Qualities: []string{""}, Langs: []string{"English"}}))
	if _, err := q.InheritSeriesTmdbIDs(ctx); err != nil {
		t.Fatal(err)
	}
	shows, err := q.ListUserSeries(ctx, db.ListUserSeriesParams{IsAdmin: true})
	must(err)
	if len(shows) != 1 {
		t.Fatalf("series = %+v, want one", shows)
	}
	var aShow, bShow int64
	all, _ := q.ListSeriesAdmin(ctx, db.ListSeriesAdminParams{Limit: 10})
	for _, s := range all {
		if s.ProviderID == a.ID {
			aShow = s.ID
		} else {
			bShow = s.ID
		}
	}
	ep := func(seriesID int64, extID string) {
		must(q.UpsertEpisodes(ctx, db.UpsertEpisodesParams{SeriesID: seriesID, ExternalIds: []string{extID}, Seasons: []int32{1}, EpisodeNums: []int32{1},
			Titles: []string{"Pilot"}, ContainerExtensions: []string{"mp4"}, Plots: []string{""}, Durations: []int32{0}, Images: []string{""}, Added: []int64{0}}))
		must(q.MarkSeriesEpisodesFetched(ctx, db.MarkSeriesEpisodesFetchedParams{ID: seriesID}))
	}
	ep(aShow, "9001")
	// Before b's episodes are loaded only a can play it.
	cached, err := q.ListUserCachedEpisodes(ctx, db.ListUserCachedEpisodesParams{IsAdmin: true})
	must(err)
	if len(cached) != 1 {
		t.Fatalf("cached episodes = %+v", cached)
	}
	if eps, _ := q.ListUserEpisodeCopies(ctx, db.ListUserEpisodeCopiesParams{ID: cached[0].ID, IsAdmin: true}); len(eps) != 1 || eps[0].ProviderID != a.ID {
		t.Errorf("episode copies before b loaded = %+v", eps)
	}
	ep(bShow, "5001")
	if cached, _ := q.ListUserCachedEpisodes(ctx, db.ListUserCachedEpisodesParams{IsAdmin: true}); len(cached) != 1 {
		t.Errorf("cached episodes after b loaded = %+v, want one", cached)
	}
	eps, err := q.ListUserEpisodeCopies(ctx, db.ListUserEpisodeCopiesParams{ID: cached[0].ID, IsAdmin: true})
	must(err)
	if len(eps) != 2 || eps[0].ProviderID != b.ID || eps[0].ExternalID != "5001" || eps[1].ProviderID != a.ID || eps[1].ExternalID != "9001" {
		t.Errorf("episode copies = %+v, want b's 5001 then a's 9001", eps)
	}
}
