//go:build integration

package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sysadmin/mmp/internal/db"
)

// fakeVODPanel serves a mutable movie/series catalog the way real panels
// do (mixed string/number types, duplicate ids).
type fakeVODPanel struct {
	mu          sync.Mutex
	movies      []map[string]any
	shows       []map[string]any
	episodes    map[string]any // series_id -> episodes payload
	infoHits    atomic.Int32
	vodInfoHits atomic.Int32
	infoFails   atomic.Bool
	srv         *httptest.Server
}

func newFakeVODPanel(t *testing.T) *fakeVODPanel {
	f := &fakeVODPanel{episodes: map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out any
		switch r.URL.Query().Get("action") {
		case "get_live_categories", "get_live_streams":
			out = []any{} // no live channels on this panel
		case "get_vod_categories":
			out = []map[string]any{{"category_id": "1", "category_name": "Action"}, {"category_id": 2, "category_name": "Comedy"}}
		case "get_series_categories":
			out = []map[string]any{{"category_id": "10", "category_name": "Drama"}}
		case "get_vod_streams":
			out = f.movies
		case "get_series":
			out = f.shows
		case "get_vod_info":
			f.vodInfoHits.Add(1)
			if f.infoFails.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out = map[string]any{"info": map[string]any{"plot": "Movie plot " + r.URL.Query().Get("vod_id"), "duration_secs": 5400},
				"movie_data": map[string]any{"stream_id": r.URL.Query().Get("vod_id")}}
		case "get_series_info":
			f.infoHits.Add(1)
			if f.infoFails.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out = map[string]any{"info": map[string]any{"name": "show", "plot": "a plot"}, "episodes": f.episodes[r.URL.Query().Get("series_id")]}
		default:
			out = map[string]any{"user_info": map[string]any{"auth": 1}}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func movie(id any, name string, cat any) map[string]any {
	return map[string]any{"stream_id": id, "name": name, "category_id": cat, "rating": "7.5", "added": "1700000000",
		"container_extension": "mkv", "stream_icon": "http://img/" + name + ".jpg"}
}

func TestRefreshVOD(t *testing.T) {
	w, q := newTestDB(t)
	ctx := context.Background()
	panel := newFakeVODPanel(t)
	for i := range 20 {
		panel.movies = append(panel.movies, movie(fmt.Sprint(100+i), fmt.Sprintf("Movie %d", i), "1"))
	}
	panel.movies = append(panel.movies, movie(105, "Movie 5 (dup)", 2)) // duplicate id, numeric types
	panel.shows = []map[string]any{
		{"series_id": 7, "name": "Show A", "category_id": "10", "rating": 8, "last_modified": "1700000000", "plot": "p"},
		{"series_id": "8", "name": "Show B", "category_id": "10", "releaseDate": "2020-01-01"},
	}
	panel.episodes["7"] = map[string]any{
		"1": []any{map[string]any{"id": "701", "episode_num": 1, "title": "Pilot", "container_extension": "mp4", "info": map[string]any{"duration_secs": 2700, "plot": "first"}}},
		"2": []any{map[string]any{"id": "702", "episode_num": "1", "title": "Return", "info": []any{}}},
	}
	p, err := q.CreateProvider(ctx, db.CreateProviderParams{Name: "Panel", Type: "xtream", Url: panel.srv.URL,
		Username: "u", Password: "pw", MaxConnections: 1, RefreshIntervalSecs: 3600, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRefresher(slog.New(slog.DiscardHandler), w.pool, 80)

	res, err := ref.RefreshVOD(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Movies != 20 || res.Series != 2 || res.Categories != 3 {
		t.Fatalf("result = %+v", res)
	}
	var name, cat, ext string
	var rating float32
	var added time.Time
	if err := w.pool.QueryRow(ctx, `SELECT name, category_external_id, container_extension, rating, added_at FROM vod_items WHERE provider_id=$1 AND external_id='105'`, p.ID).
		Scan(&name, &cat, &ext, &rating, &added); err != nil {
		t.Fatal(err)
	}
	if name != "Movie 5 (dup)" || cat != "2" || ext != "mkv" || rating != 7.5 || added.Unix() != 1700000000 {
		t.Errorf("movie 105 = %q %q %q %v %v (duplicate: last wins)", name, cat, ext, rating, added)
	}
	if pr, _ := q.GetProvider(ctx, p.ID); pr.VodRefreshedAt == nil {
		t.Error("vod_refreshed_at not set")
	}

	// Second import: one movie gone, one renamed, one show gone.
	panel.mu.Lock()
	panel.movies = panel.movies[1:20]
	panel.movies[0]["name"] = "Movie 1 Remastered"
	panel.shows = panel.shows[:1]
	panel.mu.Unlock()
	res, err = ref.RefreshVOD(ctx, p, false)
	if err != nil || res.MoviesDisabled != 1 || res.SeriesDisabled != 1 {
		t.Fatalf("second import = %+v, %v", res, err)
	}
	var active, disabled int
	w.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT disabled), count(*) FILTER (WHERE disabled) FROM vod_items WHERE provider_id=$1`, p.ID).Scan(&active, &disabled)
	if active != 19 || disabled != 1 {
		t.Errorf("movies active/disabled = %d/%d", active, disabled)
	}
	w.pool.QueryRow(ctx, `SELECT name FROM vod_items WHERE provider_id=$1 AND external_id='101'`, p.ID).Scan(&name)
	if name != "Movie 1 Remastered" {
		t.Errorf("rename not applied: %q", name)
	}

	// Truncated catalog (1 of 19 movies): refused; force applies it.
	panel.mu.Lock()
	all := panel.movies
	panel.movies = all[:1]
	panel.mu.Unlock()
	var refused *DisableRefusedError
	if _, err := ref.RefreshVOD(ctx, p, false); !errors.As(err, &refused) || refused.WouldDisable != 18 {
		t.Fatalf("truncated import: err = %v", err)
	}
	if res, err := ref.RefreshVOD(ctx, p, true); err != nil || res.MoviesDisabled != 18 {
		t.Fatalf("forced = %+v, %v", res, err)
	}
	panel.mu.Lock()
	panel.movies = all
	panel.mu.Unlock()
	if _, err := ref.RefreshVOD(ctx, p, false); err != nil { // coming back re-enables them
		t.Fatal(err)
	}
	w.pool.QueryRow(ctx, `SELECT count(*) FROM vod_items WHERE provider_id=$1 AND NOT disabled`, p.ID).Scan(&active)
	if active != 19 {
		t.Errorf("re-enabled movies = %d, want 19", active)
	}

	// Episodes: fetched lazily, cached within the TTL.
	var showID int64
	w.pool.QueryRow(ctx, `SELECT id FROM series WHERE provider_id=$1 AND external_id='7'`, p.ID).Scan(&showID)
	show, _ := q.GetSeries(ctx, showID)
	eps, err := ref.SeriesEpisodes(ctx, p, show, time.Hour)
	if err != nil || len(eps) != 2 || eps[0].Season != 1 || eps[0].Title != "Pilot" || eps[0].DurationSecs != 2700 || eps[1].Season != 2 {
		t.Fatalf("episodes = %+v, %v", eps, err)
	}
	show, _ = q.GetSeries(ctx, showID)
	if show.EpisodesFetchedAt == nil || string(show.Info) == "" {
		t.Fatalf("series not marked fetched: %+v", show)
	}
	if _, err := ref.SeriesEpisodes(ctx, p, show, time.Hour); err != nil || panel.infoHits.Load() != 1 {
		t.Errorf("within TTL: hits = %d, %v", panel.infoHits.Load(), err)
	}
	// Expired + an episode removed upstream: refetched, the episode goes.
	panel.mu.Lock()
	panel.episodes["7"] = map[string]any{"1": []any{map[string]any{"id": "701", "episode_num": 1, "title": "Pilot"}}}
	panel.mu.Unlock()
	if eps, err := ref.SeriesEpisodes(ctx, p, show, 0); err != nil || len(eps) != 1 || panel.infoHits.Load() != 2 {
		t.Errorf("refetch = %d episodes, hits %d, %v", len(eps), panel.infoHits.Load(), err)
	}
	// Panel down: cached episodes are served.
	panel.infoFails.Store(true)
	show, _ = q.GetSeries(ctx, showID)
	if eps, err := ref.SeriesEpisodes(ctx, p, show, 0); err != nil || len(eps) != 1 {
		t.Errorf("panel down: %d episodes, %v", len(eps), err)
	}
	// A newer last_modified invalidates the cached episodes.
	panel.mu.Lock()
	panel.shows[0]["last_modified"] = "1800000000"
	panel.mu.Unlock()
	panel.infoFails.Store(false)
	if _, err := ref.RefreshVOD(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if show, _ = q.GetSeries(ctx, showID); show.EpisodesFetchedAt != nil {
		t.Error("last_modified change did not invalidate the episode cache")
	}

	// Movie info: fetched once, cached within the TTL, served from cache
	// when the panel fails.
	var movieID int64
	w.pool.QueryRow(ctx, `SELECT id FROM vod_items WHERE provider_id=$1 AND external_id='110'`, p.ID).Scan(&movieID)
	info, err := ref.MovieInfo(ctx, movieID, time.Hour)
	if err != nil || !strings.Contains(string(info), "Movie plot 110") || panel.vodInfoHits.Load() != 1 {
		t.Fatalf("movie info = %s, %v (hits %d)", info, err, panel.vodInfoHits.Load())
	}
	if _, err := ref.MovieInfo(ctx, movieID, time.Hour); err != nil || panel.vodInfoHits.Load() != 1 {
		t.Errorf("within TTL refetched: hits %d", panel.vodInfoHits.Load())
	}
	panel.infoFails.Store(true)
	if info, err := ref.MovieInfo(ctx, movieID, 0); err != nil || !strings.Contains(string(info), "Movie plot 110") || panel.vodInfoHits.Load() != 2 {
		t.Errorf("panel down: %s, %v (hits %d)", info, err, panel.vodInfoHits.Load())
	}
	panel.infoFails.Store(false)

	// Scheduled refreshes import VOD for vod_enabled panels, even one
	// without live channels (its live refresh fails).
	vodOnly, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "VOD only", Type: "xtream", Url: panel.srv.URL,
		Username: "u2", Password: "pw", RefreshIntervalSecs: 3600, Enabled: true})
	if err := q.SetProviderVODEnabled(ctx, db.SetProviderVODEnabledParams{ID: vodOnly.ID, VodEnabled: true}); err != nil {
		t.Fatal(err)
	}
	ref.refreshDue(ctx, 24*time.Hour)
	w.pool.QueryRow(ctx, `SELECT count(*) FROM vod_items WHERE provider_id=$1`, vodOnly.ID).Scan(&active)
	if active != 19 {
		t.Errorf("scheduled VOD import for a VOD-only panel: %d movies", active)
	}

	// M3U providers have no catalog.
	m3u, _ := q.CreateProvider(ctx, db.CreateProviderParams{Name: "M", Type: "m3u", Url: "http://x/", RefreshIntervalSecs: 3600, Enabled: true})
	if _, err := ref.RefreshVOD(ctx, m3u, false); !errors.Is(err, ErrVODUnsupported) {
		t.Errorf("m3u: err = %v", err)
	}
}
