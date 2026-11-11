//go:build integration

package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/JustDr00py/mmp/internal/db"
)

// TestVODPooling: two accounts on one panel (backbone TREX) list each
// shared title once when both are pooled, a grant of either copy's
// category grants it, and lookups return every copy in priority order.
// A third account on another backbone is never pooled with them.
func TestVODPooling(t *testing.T) {
	b, q := pgUI(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name, backbone string, priority int32) db.Provider {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + strings.ToLower(name) + ".example/",
			Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Priority: priority, Backbone: backbone})
		must(err)
		must(q.SetProviderVODEnabled(ctx, db.SetProviderVODEnabledParams{ID: p.ID, VodEnabled: true}))
		must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: "movie", ExternalIds: []string{"1"}, Names: []string{"EN | Action"}}))
		must(q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: "series", ExternalIds: []string{"5"}, Names: []string{"EN | Drama"}}))
		must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: p.ID, ExternalIds: []string{"11", "12"}, Names: []string{"Die Hard", "Heat"},
			CategoryIds: []string{"1", "1"}, Icons: []string{"", ""}, Ratings: []float32{7, 7}, ContainerExtensions: []string{"mkv", "mkv"},
			TmdbIds: []string{"", ""}, Added: []int64{0, 0}}))
		must(q.UpsertSeries(ctx, db.UpsertSeriesParams{ProviderID: p.ID, ExternalIds: []string{"7"}, Names: []string{"The Show"}, CategoryIds: []string{"5"},
			Covers: []string{""}, Plots: []string{""}, Casts: []string{""}, Directors: []string{""}, Genres: []string{""},
			ReleaseDates: []string{""}, Ratings: []float32{8}, LastModified: []int64{0}}))
		return p
	}
	// a is imported first (its rows are older), but b has the better priority.
	a := mk("TREXA", "TREX", 2)
	bp := mk("TREXB", "TREX", 0)
	other := mk("Other", "GOLD", 1)
	pool := func(p db.Provider, on bool) {
		must(q.SetProviderVODPooled(ctx, db.SetProviderVODPooledParams{ID: p.ID, VodPooled: on}))
	}

	movies := func(userID int64, admin bool, cat *int64) []db.ListUserVODItemsRow {
		t.Helper()
		rows, err := q.ListUserVODItems(ctx, db.ListUserVODItemsParams{UserID: userID, IsAdmin: admin, CategoryID: cat})
		must(err)
		return rows
	}
	if n := len(movies(0, true, nil)); n != 6 {
		t.Fatalf("unpooled: %d movies, want 6 (two per account)", n)
	}
	pool(a, true)
	if n := len(movies(0, true, nil)); n != 6 {
		t.Fatalf("one pooled account alone: %d movies, want 6", n)
	}
	pool(bp, true)
	got := movies(0, true, nil)
	if len(got) != 4 {
		t.Fatalf("pooled: %d movies, want 4 (Die Hard and Heat once for TREX, once for Other)", len(got))
	}
	cats, err := q.ListUserVODCategories(ctx, db.ListUserVODCategoriesParams{Kind: "movie", IsAdmin: true})
	must(err)
	if len(cats) != 2 {
		t.Fatalf("pooled categories = %+v, want TREX's Action once and Other's", cats)
	}
	// The listed ids are the oldest rows: account a's.
	var aDieHard, bDieHard, aAction, bAction int64
	items, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 50})
	for _, i := range items {
		if i.Name == "Die Hard" && i.ProviderID == a.ID {
			aDieHard = i.ID
		}
		if i.Name == "Die Hard" && i.ProviderID == bp.ID {
			bDieHard = i.ID
		}
	}
	adm, _ := q.ListVODCategoriesAdmin(ctx, db.ListVODCategoriesAdminParams{Kind: "movie"})
	for _, c := range adm {
		switch c.ProviderID {
		case a.ID:
			aAction = c.ID
		case bp.ID:
			bAction = c.ID
		}
	}
	listed := map[int64]bool{}
	for _, m := range got {
		listed[m.ID] = true
		if m.Name == "Die Hard" && m.ID != aDieHard && m.CategoryID == aAction {
			t.Errorf("TREX Die Hard listed as %d, want %d", m.ID, aDieHard)
		}
	}
	if !listed[aDieHard] || listed[bDieHard] {
		t.Errorf("listed %v; want a's Die Hard (%d), not b's (%d)", listed, aDieHard, bDieHard)
	}
	// Either copy's category id selects the pooled category.
	for _, cid := range []int64{aAction, bAction} {
		if n := len(movies(0, true, &cid)); n != 2 {
			t.Errorf("category %d: %d movies, want 2", cid, n)
		}
	}

	// A customer granted only b's Action sees the pooled titles, and may
	// play them through either account, b first (priority 0).
	hash := "x"
	u, err := q.CreateUser(ctx, db.CreateUserParams{Username: "cust", PasswordHash: hash, Role: "customer", Enabled: true, StreamToken: "tok-cust", MaxConnections: 1})
	must(err)
	bq, _ := q.CreateBouquet(ctx, db.CreateBouquetParams{Name: "B only"})
	_, err = q.AddVODCategoriesToBouquet(ctx, db.AddVODCategoriesToBouquetParams{BouquetID: bq.ID, CategoryIds: []int64{bAction}})
	must(err)
	must(q.AddUserBouquets(ctx, db.AddUserBouquetsParams{UserID: u.ID, BouquetIds: []int64{bq.ID}}))
	if n := len(movies(u.ID, false, nil)); n != 2 {
		t.Errorf("customer sees %d movies, want the 2 pooled ones", n)
	}
	copies, err := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: aDieHard, UserID: u.ID})
	must(err)
	if len(copies) != 2 || copies[0].ProviderID != bp.ID || copies[1].ProviderID != a.ID || copies[0].CategoryID != aAction {
		t.Errorf("copies = %+v; want b then a, category %d", copies, aAction)
	}
	if copies, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: bDieHard, UserID: u.ID}); len(copies) != 2 {
		t.Errorf("b's id: %d copies, want 2", len(copies))
	}

	// Series and episodes pool the same way.
	series, err := q.ListUserSeries(ctx, db.ListUserSeriesParams{UserID: 0, IsAdmin: true})
	must(err)
	if len(series) != 2 {
		t.Errorf("pooled series = %d, want 2 (TREX once, Other)", len(series))
	}
	var aShow, bShow int64
	all, _ := q.ListSeriesAdmin(ctx, db.ListSeriesAdminParams{Limit: 10})
	for _, s := range all {
		switch s.ProviderID {
		case a.ID:
			aShow = s.ID
		case bp.ID:
			bShow = s.ID
		}
	}
	ep := func(seriesID int64) {
		must(q.UpsertEpisodes(ctx, db.UpsertEpisodesParams{SeriesID: seriesID, ExternalIds: []string{"701"}, Seasons: []int32{1}, EpisodeNums: []int32{1},
			Titles: []string{"Pilot"}, ContainerExtensions: []string{"mkv"}, Plots: []string{""}, Durations: []int32{0}, Images: []string{""}, Added: []int64{0}}))
		must(q.MarkSeriesEpisodesFetched(ctx, db.MarkSeriesEpisodesFetchedParams{ID: seriesID}))
	}
	ep(aShow)
	ep(bShow)
	cached, err := q.ListUserCachedEpisodes(ctx, db.ListUserCachedEpisodesParams{IsAdmin: true})
	must(err)
	if len(cached) != 1 || cached[0].SeriesID != aShow {
		t.Errorf("cached episodes = %+v, want one, of series %d", cached, aShow)
	}
	eps, err := q.ListUserEpisodeCopies(ctx, db.ListUserEpisodeCopiesParams{ID: cached[0].ID, IsAdmin: true})
	must(err)
	if len(eps) != 2 || eps[0].ProviderID != bp.ID || eps[0].ExternalID != "701" || eps[1].ProviderID != a.ID {
		t.Errorf("episode copies = %+v", eps)
	}
	// The customer has no series category: no copies.
	if eps, _ := q.ListUserEpisodeCopies(ctx, db.ListUserEpisodeCopiesParams{ID: cached[0].ID, UserID: u.ID}); len(eps) != 0 {
		t.Errorf("customer episode copies = %+v", eps)
	}

	// A disabled account drops out of playback; its titles stay listed
	// through the other.
	must(q.SetProviderEnabled(ctx, db.SetProviderEnabledParams{ID: bp.ID, Enabled: false}))
	if copies, _ := q.ListUserVODItemCopies(ctx, db.ListUserVODItemCopiesParams{ID: aDieHard, IsAdmin: true}); len(copies) != 1 || copies[0].ProviderID != a.ID {
		t.Errorf("with b disabled: %+v", copies)
	}
	must(q.SetProviderEnabled(ctx, db.SetProviderEnabledParams{ID: bp.ID, Enabled: true}))

	// The page shows the overlap and toggles pooling.
	page := "/admin/providers/" + strconv.FormatInt(a.ID, 10) + "/vod-categories"
	_, body := b.get(page)
	for _, want := range []string{"TREXB", "2 of this account", "s 2 movies match", "· pooled", `name="pooled" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Other") {
		t.Error("page lists a provider on another backbone")
	}
	if resp, _ := b.post(page, url.Values{"action": {"pool"}}, b.base); !strings.Contains(resp.Header.Get("Location"), "msg=pooled") {
		t.Fatalf("unpool = %s", resp.Header.Get("Location"))
	}
	if p, _ := q.GetProvider(ctx, a.ID); p.VodPooled {
		t.Error("still pooled")
	}
	if n := len(movies(0, true, nil)); n != 6 {
		t.Errorf("after unpooling: %d movies, want 6", n)
	}
	// Without a backbone there is nothing to pool with.
	page = "/admin/providers/" + strconv.FormatInt(other.ID, 10) + "/vod-categories"
	must(q.SetProviderVODPooled(ctx, db.SetProviderVODPooledParams{ID: other.ID, VodPooled: false}))
	if _, body = b.get(page); !strings.Contains(body, "No other provider is on this backbone") {
		t.Error("lonely backbone not explained")
	}
}
