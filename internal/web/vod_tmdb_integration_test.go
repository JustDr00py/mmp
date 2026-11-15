//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/vodmeta"
)

// TestVODTMDBMatching: Settings → VOD library checks and stores a TMDB key
// (never showing it back), then TMDB matching finds the titles panels sent
// no id for, once per title for all its copies, and caches its searches.
func TestVODTMDBMatching(t *testing.T) {
	var searches atomic.Int32
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "good-key-1234" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/configuration":
			w.Write([]byte(`{}`))
		case "/search/movie":
			searches.Add(1)
			if r.URL.Query().Get("query") == "Heat" {
				w.Write([]byte(`{"results":[{"id":949,"title":"Heat","release_date":"1995-12-15"}]}`))
				return
			}
			w.Write([]byte(`{"results":[]}`))
		case "/movie/949":
			w.Write([]byte(`{"id":949,"imdb_id":"tt0113277","title":"Heat"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer tmdb.Close()

	var m *vodmeta.Matcher
	b, q := pgUI(t, func(d *Deps, pool *pgxpool.Pool) {
		m = &vodmeta.Matcher{DB: pool, Log: d.Log, Base: tmdb.URL, Gap: time.Millisecond}
		d.VODLibrary = m
	})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"Alpha", "Bravo"} {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + strings.ToLower(name) + ".example/",
			Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Priority: int32(i), Backbone: name})
		must(err)
		must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: p.ID, ExternalIds: []string{"1", "2"},
			Names: []string{"Heat (1995)", "Nowhere Film (2001)"}, CategoryIds: []string{"", ""}, Icons: []string{"", ""},
			Ratings: []float32{-1, -1}, ContainerExtensions: []string{"mp4", "mp4"}, TmdbIds: []string{"", ""}, Added: []int64{0, 0},
			Titles: []string{"Heat", "Nowhere Film"}, TitleNorms: []string{"heat", "nowhere film"}, Years: []int32{1995, 2001},
			Qualities: []string{"", ""}, Langs: []string{"", ""}}))
	}

	// A refused key is not stored.
	resp, body := b.post("/admin/settings/vodlibrary", url.Values{"tmdb_key": {"wrong-key"}, "match": {"on"}}, b.base)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "TMDB refused that key") {
		t.Fatalf("bad key = %d", resp.StatusCode)
	}
	if s, _ := vodmeta.LoadSettings(ctx, q); s.TMDBKey != "" {
		t.Fatal("refused key stored")
	}
	// A good one is, and saving starts a pass.
	resp, _ = b.post("/admin/settings/vodlibrary", url.Values{"tmdb_key": {"good-key-1234"}, "match": {"on"}}, b.base)
	if !strings.Contains(resp.Header.Get("Location"), "msg=vodlibrary") {
		t.Fatalf("save = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for st := m.Status(); st.Running || st.Finished.IsZero(); st = m.Status() {
		if time.Now().After(deadline) {
			t.Fatalf("pass did not finish: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := m.Status(); st.Searched != 2 || st.Matched != 1 || st.Missed != 1 || st.Err != "" {
		t.Errorf("status = %+v, want 2 titles looked up (one per title, not per copy), 1 found", st)
	}
	items, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 10})
	heats := 0
	for _, it := range items {
		row, _ := q.GetVODItem(ctx, it.ID)
		switch row.TitleNorm {
		case "heat":
			heats++
			if row.TmdbID != "949" || row.ImdbID != "tt0113277" || row.MatchSource != "tmdb" {
				t.Errorf("heat = tmdb %q imdb %q source %q", row.TmdbID, row.ImdbID, row.MatchSource)
			}
		default:
			if row.TmdbID != "" || row.MatchTriedAt == nil {
				t.Errorf("nowhere = tmdb %q tried %v", row.TmdbID, row.MatchTriedAt)
			}
		}
	}
	if heats != 2 {
		t.Errorf("%d copies of Heat, want both providers'", heats)
	}
	// Nothing is searched again.
	n := searches.Load()
	if _, err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if searches.Load() != n {
		t.Errorf("second pass searched %d more times, want 0", searches.Load()-n)
	}

	// The page never shows the key, only its end and the counts.
	_, body = b.get("/admin/settings")
	if strings.Contains(body, "good-key-1234") {
		t.Error("settings page shows the TMDB key")
	}
	for _, want := range []string{"Saved …1234", "Movies: 2 with a TMDB id, 2 without", "Match now", "0 titles looked up"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	// Saving with the field empty keeps the key.
	b.post("/admin/settings/vodlibrary", url.Values{"tmdb_key": {""}, "clean_titles": {"on"}}, b.base)
	if s, _ := vodmeta.LoadSettings(ctx, q); s.TMDBKey != "good-key-1234" || s.Match || !s.CleanTitles {
		t.Errorf("after saving with an empty key: %+v", s)
	}
}

// TestVODFixMatch: the catalog page shows a title's copies and match, and
// Set (checked on TMDB), Split and Reset fix it, locked against imports.
func TestVODFixMatch(t *testing.T) {
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movie/949":
			w.Write([]byte(`{"id":949,"imdb_id":"tt0113277","title":"Heat","poster_path":"/heat.jpg"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer tmdb.Close()
	b, q := pgUI(t, func(d *Deps, pool *pgxpool.Pool) {
		d.VODLibrary = &vodmeta.Matcher{DB: pool, Log: d.Log, Base: tmdb.URL, Gap: time.Millisecond}
	})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(vodmeta.SaveSettings(ctx, q, vodmeta.Settings{TMDBKey: "k"}))
	var ids []int64
	for i, name := range []string{"Alpha", "Bravo"} {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + strings.ToLower(name) + ".example/",
			Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Priority: int32(i), Backbone: name})
		must(err)
		must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: p.ID, ExternalIds: []string{"1"}, Names: []string{"EN | Heat (1995)"},
			CategoryIds: []string{""}, Icons: []string{""}, Ratings: []float32{-1}, ContainerExtensions: []string{"mp4"},
			TmdbIds: []string{"77"}, Added: []int64{0}, Titles: []string{"Heat"}, TitleNorms: []string{"heat"},
			Years: []int32{1995}, Qualities: []string{""}, Langs: []string{"English"}}))
		items, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 10})
		for _, it := range items {
			if it.ProviderID == p.ID {
				ids = append(ids, it.ID)
			}
		}
	}
	_, body := b.get("/admin/vod?kind=movie")
	for _, want := range []string{"Heat (1995)", "2 copies", "TMDB 77", "from the panel", "English"} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog lacks %q", want)
		}
	}
	match := func(id int64, form url.Values) string {
		t.Helper()
		form.Set("back", "/admin/vod?kind=movie")
		resp, _ := b.post("/admin/vod/movie/"+strconv.FormatInt(id, 10)+"/match", form, b.base)
		return resp.Header.Get("Location")
	}
	// An id TMDB does not know is refused; a real one is saved with its details.
	if loc := match(ids[0], url.Values{"action": {"set"}, "tmdb_id": {"404"}}); !strings.Contains(loc, "err=TMDB+has+no+movie") {
		t.Errorf("unknown id: %s", loc)
	}
	if loc := match(ids[0], url.Values{"action": {"set"}, "tmdb_id": {"abc"}}); !strings.Contains(loc, "err=") {
		t.Errorf("non-numeric id: %s", loc)
	}
	if loc := match(ids[0], url.Values{"action": {"set"}, "tmdb_id": {"949"}}); !strings.Contains(loc, "msg=matched") {
		t.Fatalf("set: %s", loc)
	}
	it, _ := q.GetVODItem(ctx, ids[0])
	if it.TmdbID != "949" || it.ImdbID != "tt0113277" || !it.MatchLocked || it.MatchSource != "manual" {
		t.Errorf("after set: %q %q %v %q", it.TmdbID, it.ImdbID, it.MatchLocked, it.MatchSource)
	}
	// An import keeps it.
	must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: it.ProviderID, ExternalIds: []string{"1"}, Names: []string{"EN | Heat (1995)"},
		CategoryIds: []string{""}, Icons: []string{""}, Ratings: []float32{-1}, ContainerExtensions: []string{"mp4"},
		TmdbIds: []string{"77"}, Added: []int64{0}}))
	if it, _ := q.GetVODItem(ctx, ids[0]); it.TmdbID != "949" {
		t.Errorf("import overwrote a manual match: %q", it.TmdbID)
	}
	_, body = b.get("/admin/vod?kind=movie&msg=matched")
	for _, want := range []string{"set by hand", "locked", "Match saved and locked"} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog lacks %q", want)
		}
	}
	// Split, then reset: the panel's id comes back with the next import.
	if loc := match(ids[1], url.Values{"action": {"split"}}); !strings.Contains(loc, "msg=split") {
		t.Errorf("split: %s", loc)
	}
	if it, _ := q.GetVODItem(ctx, ids[1]); it.TmdbID != "" || !it.MatchLocked {
		t.Errorf("after split: %q %v", it.TmdbID, it.MatchLocked)
	}
	if loc := match(ids[1], url.Values{"action": {"reset"}}); !strings.Contains(loc, "msg=reset") {
		t.Errorf("reset: %s", loc)
	}
	it, _ = q.GetVODItem(ctx, ids[1])
	must(q.UpsertVODItems(ctx, db.UpsertVODItemsParams{ProviderID: it.ProviderID, ExternalIds: []string{"1"}, Names: []string{"EN | Heat (1995)"},
		CategoryIds: []string{""}, Icons: []string{""}, Ratings: []float32{-1}, ContainerExtensions: []string{"mp4"},
		TmdbIds: []string{"77"}, Added: []int64{0}}))
	if it, _ := q.GetVODItem(ctx, ids[1]); it.TmdbID != "77" || it.MatchLocked || it.MatchSource != "panel" {
		t.Errorf("after reset and import: %q %v %q", it.TmdbID, it.MatchLocked, it.MatchSource)
	}
	// Only titles without an id, when asked.
	must(q.SetVODItemMatch(ctx, db.SetVODItemMatchParams{ID: ids[1], TmdbID: ""}))
	if rows, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{UnmatchedOnly: true, Limit: 10}); len(rows) != 1 || rows[0].ID != ids[1] {
		t.Errorf("unmatched only = %+v", rows)
	}
}
