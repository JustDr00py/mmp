//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source"
	"github.com/JustDr00py/mmp/internal/testpg"
)

func fakeVODXtream(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out any
		switch r.URL.Query().Get("action") {
		case "get_vod_categories":
			out = []any{map[string]any{"category_id": "1", "category_name": "Action"}, map[string]any{"category_id": "2", "category_name": "Kids"}}
		case "get_vod_streams":
			out = []any{
				map[string]any{"stream_id": 11, "name": "Die Hard", "category_id": "1", "container_extension": "mkv"},
				map[string]any{"stream_id": 12, "name": "Speed", "category_id": "1"},
				map[string]any{"stream_id": 21, "name": "Cars", "category_id": "2"},
			}
		case "get_series_categories":
			out = []any{map[string]any{"category_id": "5", "category_name": "Drama"}}
		case "get_series":
			out = []any{map[string]any{"series_id": 7, "name": "The Show", "category_id": "5"}}
		default:
			out = []any{}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVODAdminAndAccess is the 9.2 AC: catalog admin through the API, and
// customers see exactly the categories their bouquets grant.
func TestVODAdminAndAccess(t *testing.T) {
	env := newAPITestEnv(t)
	q := env.Store.(*db.Queries)
	pool, err := db.Open(context.Background(), db.PoolConfig{DSN: testpg.DSN("mmp_api_test")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	env.Refresh = source.NewRefresher(env.Log, pool, 80)
	h := apiServer(env)
	panel := fakeVODXtream(t)
	ctx := context.Background()

	// vod_enabled needs an Xtream provider.
	if code, obj, _ := call(t, h, http.MethodPost, "/api/v1/providers", map[string]any{"name": "M", "type": "m3u", "url": "http://m/", "vod_enabled": true}); code != http.StatusBadRequest {
		t.Errorf("m3u + vod = %d %v", code, obj)
	}
	code, p, _ := call(t, h, http.MethodPost, "/api/v1/providers", map[string]any{"name": "Panel", "type": "xtream", "url": panel.URL,
		"username": "u", "password": "pw", "vod_enabled": true})
	if code != http.StatusCreated || p["vod_enabled"] != true {
		t.Fatalf("create = %d %v", code, p)
	}
	pid := strconv.FormatInt(int64(p["id"].(float64)), 10)

	code, res, _ := call(t, h, http.MethodPost, "/api/v1/providers/"+pid+"/vod/refresh", nil)
	if code != 200 || res["movies"] != float64(3) || res["series"] != float64(1) {
		t.Fatalf("vod refresh = %d %v", code, res)
	}

	// Browsing.
	_, _, cats := call(t, h, http.MethodGet, "/api/v1/vod/categories?kind=movie", nil)
	catID := map[string]int64{}
	for _, c := range cats {
		m := c.(map[string]any)
		catID[m["name"].(string)] = int64(m["id"].(float64))
		if m["name"] == "Action" && m["items"] != float64(2) {
			t.Errorf("Action items = %v", m["items"])
		}
	}
	if len(catID) != 2 {
		t.Fatalf("movie categories = %v", cats)
	}
	_, _, scats := call(t, h, http.MethodGet, "/api/v1/vod/categories?kind=series", nil)
	dramaID := int64(scats[0].(map[string]any)["id"].(float64))
	if _, _, movies := call(t, h, http.MethodGet, "/api/v1/vod/movies?category_id="+strconv.FormatInt(catID["Action"], 10), nil); len(movies) != 2 {
		t.Errorf("Action movies = %v", movies)
	}
	if _, _, movies := call(t, h, http.MethodGet, "/api/v1/vod/movies?q=car", nil); len(movies) != 1 || movies[0].(map[string]any)["name"] != "Cars" {
		t.Errorf("search = %v", movies)
	}
	if _, _, shows := call(t, h, http.MethodGet, "/api/v1/vod/series", nil); len(shows) != 1 {
		t.Errorf("series = %v", shows)
	}
	if code, _, _ := call(t, h, http.MethodGet, "/api/v1/vod/categories?kind=music", nil); code != http.StatusBadRequest {
		t.Errorf("bad kind = %d", code)
	}

	// A bouquet with Action + Drama; alice has it, bob does not.
	code, b, _ := call(t, h, http.MethodPost, "/api/v1/bouquets", map[string]any{"name": "Movies"})
	if code != http.StatusCreated {
		t.Fatalf("bouquet = %d", code)
	}
	bid := strconv.FormatInt(int64(b["id"].(float64)), 10)
	code, added, _ := call(t, h, http.MethodPost, "/api/v1/bouquets/"+bid+"/vod-categories", map[string]any{"category_ids": []int64{catID["Action"], dramaID}})
	if code != 200 || added["added"] != float64(2) {
		t.Fatalf("add categories = %d %v", code, added)
	}
	if code, _, _ := call(t, h, http.MethodPost, "/api/v1/bouquets/"+bid+"/vod-categories", map[string]any{"category_ids": []int64{999999}}); code != http.StatusBadRequest {
		t.Errorf("unknown category = %d", code)
	}
	if _, _, list := call(t, h, http.MethodGet, "/api/v1/bouquets/"+bid+"/vod-categories", nil); len(list) != 2 {
		t.Errorf("bouquet categories = %v", list)
	}
	user := func(name string, bouquets []int64) int64 {
		code, u, _ := call(t, h, http.MethodPost, "/api/v1/users", map[string]any{"username": name, "password": name + "-pass-1",
			"role": "customer", "bouquet_ids": bouquets})
		if code != http.StatusCreated {
			t.Fatalf("user %s = %d %v", name, code, u)
		}
		return int64(u["id"].(float64))
	}
	bouquetID, _ := strconv.ParseInt(bid, 10, 64)
	alice, bob := user("alice", []int64{bouquetID}), user("bob", nil)

	names := func(userID int64, admin bool) []string {
		rows, err := q.ListUserVODItems(ctx, db.ListUserVODItemsParams{UserID: userID, IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}
	if got := names(alice, false); len(got) != 2 || got[0] != "Die Hard" || got[1] != "Speed" {
		t.Errorf("alice movies = %v, want the Action ones", got)
	}
	if got := names(bob, false); len(got) != 0 {
		t.Errorf("bob (no bouquet) movies = %v, want none", got)
	}
	if got := names(0, true); len(got) != 3 {
		t.Errorf("admin movies = %v, want all 3", got)
	}
	if cats, _ := q.ListUserVODCategories(ctx, db.ListUserVODCategoriesParams{Kind: "movie", UserID: alice}); len(cats) != 1 || cats[0].Name != "Action" {
		t.Errorf("alice categories = %v", cats)
	}
	if shows, _ := q.ListUserSeries(ctx, db.ListUserSeriesParams{UserID: bob}); len(shows) != 0 {
		t.Errorf("bob series = %v", shows)
	}
	shows, _ := q.ListUserSeries(ctx, db.ListUserSeriesParams{UserID: alice})
	if len(shows) != 1 {
		t.Fatalf("alice series = %v", shows)
	}
	// Direct lookups by id respect the same rule (playback, 9.4).
	movies, _ := q.ListUserVODItems(ctx, db.ListUserVODItemsParams{IsAdmin: true})
	var carsID int64
	for _, m := range movies {
		if m.Name == "Cars" {
			carsID = m.ID
		}
	}
	if _, err := q.GetUserVODItem(ctx, db.GetUserVODItemParams{ID: carsID, UserID: alice}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("alice may not get Cars (Kids not granted): %v", err)
	}
	if _, err := q.GetUserSeries(ctx, db.GetUserSeriesParams{ID: shows[0].ID, UserID: bob}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("bob may not get the series: %v", err)
	}

	// Removing the category, or switching VOD off, hides it again.
	call(t, h, http.MethodDelete, "/api/v1/bouquets/"+bid+"/vod-categories/"+strconv.FormatInt(dramaID, 10), nil)
	if shows, _ := q.ListUserSeries(ctx, db.ListUserSeriesParams{UserID: alice}); len(shows) != 0 {
		t.Errorf("series still visible after removing Drama")
	}
	code, upd, _ := call(t, h, http.MethodPut, "/api/v1/providers/"+pid, map[string]any{"name": "Panel", "type": "xtream", "url": panel.URL, "vod_enabled": false})
	if code != 200 || upd["vod_enabled"] != false {
		t.Fatalf("disable vod = %d %v", code, upd)
	}
	if got := names(alice, false); len(got) != 0 {
		t.Errorf("movies visible with VOD off: %v", got)
	}
	// Omitting vod_enabled keeps the flag; re-enabling restores access.
	call(t, h, http.MethodPut, "/api/v1/providers/"+pid, map[string]any{"name": "Panel", "type": "xtream", "url": panel.URL, "vod_enabled": true})
	call(t, h, http.MethodPut, "/api/v1/providers/"+pid, map[string]any{"name": "Panel", "type": "xtream", "url": panel.URL})
	if got := names(alice, false); len(got) != 2 {
		t.Errorf("after re-enable (and an update without the flag): %v", got)
	}
}
