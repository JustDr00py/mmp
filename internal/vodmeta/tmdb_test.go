package vodmeta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPick(t *testing.T) {
	res := []TMDBResult{
		{ID: 1, Title: "The Matrix Reloaded", ReleaseDate: "2003-05-15"},
		{ID: 603, Title: "The Matrix", ReleaseDate: "1999-03-30"},
		{ID: 9, Title: "The Matrix", ReleaseDate: "2021-01-01"}, // a namesake
	}
	for _, tc := range []struct {
		title string
		year  int
		want  int64
	}{
		{"The Matrix", 1999, 603},
		{"the matrix", 2000, 603}, // a year off: a later release
		{"The Matrix", 2021, 9},
		{"The Matrix", 1990, 0},      // no year near
		{"The Matrix", 0, 0},         // two namesakes and no year to choose
		{"Matrix Reloaded", 2003, 0}, // titles must be the same
	} {
		got, ok := Pick(res, tc.title, tc.year)
		if (tc.want == 0) == ok || ok && got.ID != tc.want {
			t.Errorf("Pick(%q, %d) = %d, %v; want %d", tc.title, tc.year, got.ID, ok, tc.want)
		}
	}
	// Original titles count; a lone hit without a year is enough.
	tv := []TMDBResult{{ID: 1396, Name: "Breaking Bad", OriginalName: "Breaking Bad", FirstAirDate: "2008-01-20"},
		{ID: 5, Name: "La Casa de Papel", OriginalName: "Money Heist"}}
	if got, ok := Pick(tv, "Breaking Bad", 0); !ok || got.ID != 1396 {
		t.Errorf("tv pick = %d %v", got.ID, ok)
	}
	if got, ok := Pick(tv, "Money Heist", 0); !ok || got.ID != 5 {
		t.Errorf("original title pick = %d %v", got.ID, ok)
	}
}

func TestTMDBClient(t *testing.T) {
	var calls, limited atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		key := r.URL.Query().Get("api_key")
		if auth := r.Header.Get("Authorization"); auth != "" {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
		if key != "good" && key != "eyJtoken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if limited.Add(1) == 1 { // the first call is told to wait
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		switch r.URL.Path {
		case "/search/movie":
			if r.URL.Query().Get("query") != "The Matrix" || r.URL.Query().Get("year") != "1999" {
				t.Errorf("search query = %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"results":[{"id":603,"title":"The Matrix","release_date":"1999-03-30"}]}`))
		case "/search/tv":
			if r.URL.Query().Get("first_air_date_year") != "2008" {
				t.Errorf("tv search query = %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"results":[]}`))
		case "/tv/1396":
			w.Write([]byte(`{"id":1396,"external_ids":{"imdb_id":"tt0903747"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	c := &TMDB{Key: "good", Base: srv.URL, Gap: time.Millisecond}
	start := time.Now()
	res, err := c.Search(ctx, "movie", "The Matrix", 1999)
	if err != nil || len(res) != 1 || res[0].ID != 603 || res[0].Year() != 1999 {
		t.Fatalf("search = %+v, %v", res, err)
	}
	if time.Since(start) < time.Second {
		t.Error("429 not waited out")
	}
	if res, err := c.Search(ctx, "series", "Breaking Bad", 2008); err != nil || len(res) != 0 {
		t.Errorf("tv search = %+v, %v", res, err)
	}
	bearer := &TMDB{Key: "eyJtoken", Base: srv.URL, Gap: time.Millisecond}
	if _, imdb, err := bearer.Details(ctx, "series", "1396"); err != nil || imdb != "tt0903747" {
		t.Errorf("details = %q, %v", imdb, err)
	}
	bad := &TMDB{Key: "secret-bad-key", Base: srv.URL, Gap: time.Millisecond}
	if err := bad.Check(ctx); !errors.Is(err, ErrTMDBAuth) {
		t.Errorf("bad key = %v", err)
	}
	// Transport errors never carry the key (it is in the URL).
	srv.Close()
	err = bad.Check(ctx)
	if err == nil || strings.Contains(err.Error(), "secret-bad-key") {
		t.Errorf("transport error = %v", err)
	}
}
