package vodmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TMDBBase is TMDB's API root.
const TMDBBase = "https://api.themoviedb.org/3"

// ErrTMDBAuth: TMDB refused the key.
var ErrTMDBAuth = errors.New("TMDB refused the API key")

// TMDB is a small client for the searches and details matching needs. It
// paces itself (TMDB allows about 40 requests a second) and waits out a
// 429 as TMDB asks.
type TMDB struct {
	Key  string // v3 API key, or a v4 read access token
	Base string // TMDBBase if empty
	HTTP *http.Client
	// Gap is the least time between requests (50ms if zero).
	Gap time.Duration

	mu   sync.Mutex
	last time.Time
}

// TMDBResult is one search hit.
type TMDBResult struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`          // movies
	Name          string `json:"name"`           // tv
	OriginalTitle string `json:"original_title"` // movies
	OriginalName  string `json:"original_name"`  // tv
	ReleaseDate   string `json:"release_date"`   // movies, "2006-01-02"
	FirstAirDate  string `json:"first_air_date"` // tv
}

// Titles are the hit's title and original title.
func (r TMDBResult) Titles() (string, string) {
	if r.Title != "" || r.OriginalTitle != "" {
		return r.Title, r.OriginalTitle
	}
	return r.Name, r.OriginalName
}

// Year is the hit's release (or first air) year, or 0.
func (r TMDBResult) Year() int { return releaseYearOf(r.ReleaseDate + r.FirstAirDate) }

func releaseYearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

// tmdbKind is TMDB's word for a kind ("movie" or "series").
func tmdbKind(kind string) string {
	if kind == "series" {
		return "tv"
	}
	return "movie"
}

// Search searches movies or series ("movie", "series") by title; year 0
// searches every year.
func (c *TMDB) Search(ctx context.Context, kind, title string, year int) ([]TMDBResult, error) {
	q := url.Values{"query": {title}, "include_adult": {"false"}}
	if year > 0 {
		if kind == "series" {
			q.Set("first_air_date_year", strconv.Itoa(year))
		} else {
			q.Set("year", strconv.Itoa(year))
		}
	}
	var out struct {
		Results []TMDBResult `json:"results"`
	}
	if err := c.get(ctx, "/search/"+tmdbKind(kind), q, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Details returns a title's TMDB answer (with external ids) and its IMDb
// id, if TMDB knows one.
func (c *TMDB) Details(ctx context.Context, kind, id string) (json.RawMessage, string, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/"+tmdbKind(kind)+"/"+url.PathEscape(id), url.Values{"append_to_response": {"external_ids"}}, &raw); err != nil {
		return nil, "", err
	}
	var ids struct {
		ImdbID      string `json:"imdb_id"`
		ExternalIDs struct {
			ImdbID string `json:"imdb_id"`
		} `json:"external_ids"`
	}
	_ = json.Unmarshal(raw, &ids)
	imdb := ids.ImdbID
	if imdb == "" {
		imdb = ids.ExternalIDs.ImdbID
	}
	return raw, imdb, nil
}

// Check makes one cheap call, to tell whether the key works.
func (c *TMDB) Check(ctx context.Context) error {
	var out json.RawMessage
	return c.get(ctx, "/configuration", nil, &out)
}

func (c *TMDB) pace(ctx context.Context) error {
	gap := c.Gap
	if gap == 0 {
		gap = 50 * time.Millisecond
	}
	c.mu.Lock()
	wait := time.Until(c.last.Add(gap))
	if wait < 0 {
		wait = 0
	}
	c.last = time.Now().Add(wait)
	c.mu.Unlock()
	return sleep(ctx, wait)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *TMDB) get(ctx context.Context, path string, q url.Values, out any) error {
	base := c.Base
	if base == "" {
		base = TMDBBase
	}
	if q == nil {
		q = url.Values{}
	}
	bearer := strings.HasPrefix(c.Key, "eyJ") // a v4 token is a JWT
	if !bearer {
		q.Set("api_key", c.Key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	for attempt := 0; ; attempt++ {
		if err := c.pace(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+q.Encode(), nil)
		if err != nil {
			return fmt.Errorf("tmdb %s: %w", path, err)
		}
		req.Header.Set("Accept", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+c.Key)
		}
		resp, err := hc.Do(req)
		if err != nil {
			// The URL may carry the key: report the cause only.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("tmdb %s: %w", path, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		switch {
		case err != nil:
			return fmt.Errorf("tmdb %s: %w", path, err)
		case resp.StatusCode == http.StatusTooManyRequests && attempt < 3:
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 30 {
				wait = time.Duration(s) * time.Second
			}
			if err := sleep(ctx, wait); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return ErrTMDBAuth
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("tmdb %s: not found", path)
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("tmdb %s: status %d", path, resp.StatusCode)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("tmdb %s: %w", path, err)
		}
		return nil
	}
}

// Pick chooses the search hit that is the title: its title or original
// title the same (by Key), and its year within one of the panel's (a
// cinema and a home release often differ). Without a year only a single
// such hit counts. Hits come in TMDB's order (most relevant first).
func Pick(results []TMDBResult, title string, year int) (TMDBResult, bool) {
	key := Key(title)
	if key == "" {
		return TMDBResult{}, false
	}
	var same []TMDBResult
	for _, r := range results {
		t, o := r.Titles()
		if Key(t) == key || Key(o) == key {
			same = append(same, r)
		}
	}
	if year == 0 {
		if len(same) == 1 {
			return same[0], true
		}
		return TMDBResult{}, false
	}
	for _, d := range []int{0, 1} { // an exact year beats a near one
		for _, r := range same {
			if y := r.Year(); y != 0 && (y-year == d || year-y == d) {
				return r, true
			}
		}
	}
	return TMDBResult{}, false
}
