//go:build integration

package web

import (
	"context"
	"encoding/json"
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

// TestVODLibraryUpgrade: titles stored by an older parser ("EN ★ x" kept
// whole) are parsed again at startup and older TMDB misses forgotten; a
// pass then goes on past its batch size until every title was tried, and
// a series is matched by its exact title when the panel's year is off.
func TestVODLibraryUpgrade(t *testing.T) {
	var searches atomic.Int32
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		q := r.URL.Query().Get("query")
		switch r.URL.Path {
		case "/search/movie":
			if strings.Contains(q, "★") {
				t.Errorf("searched %q", q)
			}
			w.Write([]byte(`{"results":[{"id":` + map[string]string{"Moana": "277834", "Allied": "369885", "Red Eye": "9315"}[q] + `0,"title":"` + q + `","release_date":"2016-01-01"}]}`))
		case "/search/tv":
			if r.URL.Query().Get("first_air_date_year") != "" {
				w.Write([]byte(`{"results":[]}`)) // the panel's year (2013) is not TMDB's (2008)
				return
			}
			w.Write([]byte(`{"results":[{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20"}]}`))
		default:
			w.Write([]byte(`{"id":1}`))
		}
	}))
	defer tmdb.Close()
	var m *vodmeta.Matcher
	var pool *pgxpool.Pool
	_, q := pgUI(t, func(d *Deps, p *pgxpool.Pool) {
		pool = p
		m = &vodmeta.Matcher{DB: p, Log: d.Log, Base: tmdb.URL, Gap: time.Millisecond, Batch: 1}
	})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Alpha", Type: "xtream", Url: "http://alpha.example/",
		Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	must(err)
	// As the first library build stored them: the prefix kept in the title.
	names := []string{"EN ★  Moana - 2016 [FHD]", "EN ★  Allied - 2016 [FHD]", "EN ★  Red Eye - 2016 [4K]"}
	arg := db.UpsertVODItemsParams{ProviderID: p.ID}
	for i, n := range names {
		arg.ExternalIds, arg.Names, arg.CategoryIds = append(arg.ExternalIds, strconv.Itoa(i)), append(arg.Names, n), append(arg.CategoryIds, "")
		arg.Icons, arg.Ratings, arg.ContainerExtensions = append(arg.Icons, ""), append(arg.Ratings, -1), append(arg.ContainerExtensions, "mp4")
		arg.TmdbIds, arg.Added = append(arg.TmdbIds, ""), append(arg.Added, 0)
		old := strings.SplitN(n, " - ", 2)[0]
		arg.Titles, arg.TitleNorms, arg.Years = append(arg.Titles, old), append(arg.TitleNorms, vodmeta.Key(old)), append(arg.Years, 2016)
		arg.Qualities, arg.Langs = append(arg.Qualities, ""), append(arg.Langs, "")
	}
	must(q.UpsertVODItems(ctx, arg))
	must(q.UpsertSeries(ctx, db.UpsertSeriesParams{ProviderID: p.ID, ExternalIds: []string{"9"}, Names: []string{"EN ★ Breaking Bad"},
		CategoryIds: []string{""}, Covers: []string{""}, Plots: []string{""}, Casts: []string{""}, Directors: []string{""}, Genres: []string{""},
		ReleaseDates: []string{"2013-08-11"}, Ratings: []float32{-1}, LastModified: []int64{0}, TmdbIds: []string{""},
		Titles: []string{"EN ★ Breaking Bad"}, TitleNorms: []string{"en breaking bad"}, Years: []int32{2013}, Qualities: []string{""}, Langs: []string{""}}))
	// An old miss, cached and marked tried.
	must(q.PutTMDBSearch(ctx, db.PutTMDBSearchParams{Kind: "movie", QueryKey: "en moana", Year: 2016, TmdbID: ""}))
	must(q.MarkMovieTitleTried(ctx, db.MarkMovieTitleTriedParams{TitleNorm: "en moana", Year: 2016}))

	changed, err := vodmeta.Upgrade(ctx, pool, m.Log)
	if err != nil || !changed {
		t.Fatalf("upgrade = %v, %v", changed, err)
	}
	items, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 10})
	for _, it := range items {
		if strings.Contains(it.Title, "★") || it.Lang != "English" || it.Year != 2016 {
			t.Errorf("after upgrade: %q lang %q year %d", it.Title, it.Lang, it.Year)
		}
	}
	if changed, _ := vodmeta.Upgrade(ctx, pool, m.Log); changed {
		t.Error("second upgrade changed something")
	}
	vodmeta.SaveSettings(ctx, q, vodmeta.Settings{TMDBKey: "k", Match: true})
	st, err := m.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Batch 1: three movies and a series in one pass, every one found.
	if st.Searched != 4 || st.Matched != 4 {
		t.Errorf("pass = %+v, want 4 looked up and found", st)
	}
	shows, _ := q.ListSeriesAdmin(ctx, db.ListSeriesAdminParams{Limit: 5})
	if len(shows) != 1 || shows[0].TmdbID != "1396" || shows[0].Title != "Breaking Bad" {
		t.Errorf("series = %+v", shows)
	}
}

// TestVODMatchRecheck: after the matching rules change, titles a search
// matched under the old ones are looked up again: a wrong id is corrected
// ("Matrix", 1999 had been matched to a 1998 short of that name rather
// than The Matrix), a right one kept, and one no longer found keeps its id.
func TestVODMatchRecheck(t *testing.T) {
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/movie":
			switch r.URL.Query().Get("query") {
			case "Matrix":
				w.Write([]byte(`{"results":[{"id":555879,"title":"Matrix","release_date":"1998-12-31"},
					{"id":603,"title":"The Matrix","release_date":"1999-03-31"}]}`))
			case "Heat":
				w.Write([]byte(`{"results":[{"id":949,"title":"Heat","release_date":"1995-12-15"}]}`))
			default:
				w.Write([]byte(`{"results":[]}`))
			}
		case "/movie/603":
			w.Write([]byte(`{"id":603,"imdb_id":"tt0133093","title":"The Matrix"}`))
		default:
			w.Write([]byte(`{"id":1}`))
		}
	}))
	defer tmdb.Close()
	var m *vodmeta.Matcher
	var pool *pgxpool.Pool
	_, q := pgUI(t, func(d *Deps, p *pgxpool.Pool) {
		pool = p
		m = &vodmeta.Matcher{DB: p, Log: d.Log, Base: tmdb.URL, Gap: time.Millisecond}
	})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	titles := map[string]struct {
		year int32
		old  string
	}{"Matrix": {1999, "555879"}, "Heat": {1995, "949"}, "Lost Title": {2010, "123"}}
	for i, name := range []string{"Alpha", "Bravo"} {
		p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: name, Type: "xtream", Url: "http://" + strings.ToLower(name) + ".example/",
			Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true, Priority: int32(i), Backbone: name})
		must(err)
		arg := db.UpsertVODItemsParams{ProviderID: p.ID}
		for title, tt := range titles {
			n := "EN ★ " + title + " - " + strconv.Itoa(int(tt.year)) + " [4K]"
			arg.ExternalIds, arg.Names, arg.CategoryIds = append(arg.ExternalIds, title), append(arg.Names, n), append(arg.CategoryIds, "")
			arg.Icons, arg.Ratings, arg.ContainerExtensions = append(arg.Icons, ""), append(arg.Ratings, -1), append(arg.ContainerExtensions, "mp4")
			arg.TmdbIds, arg.Added = append(arg.TmdbIds, ""), append(arg.Added, 0)
			arg.Titles, arg.TitleNorms, arg.Years = append(arg.Titles, title), append(arg.TitleNorms, vodmeta.Key(title)), append(arg.Years, tt.year)
			arg.Qualities, arg.Langs = append(arg.Qualities, "4k"), append(arg.Langs, "English")
		}
		must(q.UpsertVODItems(ctx, arg))
	}
	// As the previous rules matched them, searches cached.
	for title, tt := range titles {
		_, err := q.SetMovieTitleMatch(ctx, db.SetMovieTitleMatchParams{TmdbID: tt.old, TitleNorm: vodmeta.Key(title), Year: tt.year})
		must(err)
		must(q.PutTMDBSearch(ctx, db.PutTMDBSearchParams{Kind: "movie", QueryKey: vodmeta.Key(title), Year: tt.year, TmdbID: tt.old}))
	}
	raw, _ := json.Marshal(map[string]int{"parse": vodmeta.ParseVersion, "match": vodmeta.MatchVersion - 1})
	must(q.SetSetting(ctx, db.SetSettingParams{Key: "vod_library_versions", Value: raw}))

	if changed, err := vodmeta.Upgrade(ctx, pool, m.Log); err != nil || !changed {
		t.Fatalf("upgrade = %v, %v", changed, err)
	}
	must(vodmeta.SaveSettings(ctx, q, vodmeta.Settings{TMDBKey: "k", Match: true}))
	st, err := m.Run(ctx)
	must(err)
	if st.Rechecked != 3 || st.Corrected != 1 || st.Searched != 0 {
		t.Errorf("pass = %+v, want 3 checked again, 1 corrected, nothing new", st)
	}
	want := map[string]string{"matrix": "603", "heat": "949", "lost title": "123"}
	items, _ := q.ListVODItemsAdmin(ctx, db.ListVODItemsAdminParams{Limit: 20})
	if len(items) != 6 {
		t.Fatalf("%d items, want 6", len(items))
	}
	for _, it := range items {
		row, _ := q.GetVODItem(ctx, it.ID)
		if row.TmdbID != want[row.TitleNorm] || row.MatchSource != "tmdb" || row.MatchTriedAt == nil {
			t.Errorf("%q = tmdb %q source %q tried %v, want %q", row.TitleNorm, row.TmdbID, row.MatchSource, row.MatchTriedAt, want[row.TitleNorm])
		}
		if row.TitleNorm == "matrix" && row.ImdbID != "tt0133093" {
			t.Errorf("matrix imdb = %q", row.ImdbID)
		}
	}
	// Done once: the next pass checks nothing again.
	if st, _ := m.Run(ctx); st.Rechecked != 0 {
		t.Errorf("second pass rechecked %d", st.Rechecked)
	}
}
