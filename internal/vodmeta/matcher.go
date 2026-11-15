package vodmeta

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

const (
	// matchRetry is how long a title TMDB had no match for waits before
	// it is searched again (TMDB grows; panels rename).
	matchRetry = 30 * 24 * time.Hour
	// matchBatch is how many titles of a kind are read at a time; a pass
	// goes on until every title without an id was tried once.
	matchBatch = 3000
	// maxFailures in a row end a run (TMDB or the network is down).
	maxFailures = 20
)

// MatchStatus is what the last (or running) TMDB pass did.
type MatchStatus struct {
	Running           bool
	Started, Finished time.Time
	Searched          int // title groups looked up
	Matched, Missed   int
	Err               string
}

// Matcher finds TMDB ids for titles panels gave none, in the background
// after VOD refreshes or when asked. A title is searched once for all its
// copies (one normalised title and year), and searches are cached, misses
// included, so refreshes cost no TMDB calls for titles already tried.
type Matcher struct {
	DB   db.DBTX
	Log  *slog.Logger
	HTTP *http.Client // optional
	Base string       // TMDB API root; TMDBBase if empty (tests)
	Gap  time.Duration
	// Batch is how many titles are read at a time (matchBatch if zero).
	Batch int32

	running atomic.Bool
	mu      sync.Mutex
	status  MatchStatus
}

// Status is the last pass's outcome (or the running one's so far).
func (m *Matcher) Status() MatchStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.Running = m.running.Load()
	return s
}

// Trigger starts a pass in the background unless one is running or
// matching is off; it reports whether one started.
func (m *Matcher) Trigger() bool {
	s, err := LoadSettings(context.Background(), db.New(m.DB))
	if err != nil || !s.Matching() || !m.running.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer m.running.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
		defer cancel()
		if _, err := m.run(ctx, s); err != nil {
			m.Log.Warn("vod library: tmdb matching", "err", err)
		}
	}()
	return true
}

// Run makes one pass now (tests and callers that wait for it).
func (m *Matcher) Run(ctx context.Context) (MatchStatus, error) {
	s, err := LoadSettings(ctx, db.New(m.DB))
	if err != nil || !s.Matching() {
		return MatchStatus{}, err
	}
	if !m.running.CompareAndSwap(false, true) {
		return m.Status(), errors.New("a TMDB pass is already running")
	}
	defer m.running.Store(false)
	return m.run(ctx, s)
}

type titleGroup struct {
	norm, title string
	year        int32
}

func (m *Matcher) run(ctx context.Context, s Settings) (MatchStatus, error) {
	st := MatchStatus{Started: time.Now()}
	m.setStatus(st)
	q := db.New(m.DB)
	client := &TMDB{Key: s.TMDBKey, Base: m.Base, HTTP: m.HTTP, Gap: m.Gap}
	finish := func(err error) (MatchStatus, error) {
		st.Finished = time.Now()
		if err != nil {
			st.Err = err.Error()
		}
		m.setStatus(st)
		m.Log.Info("vod library: tmdb pass", "searched", st.Searched, "matched", st.Matched, "missed", st.Missed,
			"took", st.Finished.Sub(st.Started).Round(time.Second), "err", st.Err)
		return st, err
	}
	failures := 0
	for _, kind := range []string{"movie", "series"} {
		seen := map[titleGroup]bool{} // a title that failed is not retried this pass
	batches:
		for {
			groups, err := m.unmatched(ctx, q, kind)
			if err != nil {
				return finish(err)
			}
			fresh := groups[:0]
			for _, g := range groups {
				if !seen[g] {
					seen[g] = true
					fresh = append(fresh, g)
				}
			}
			if len(fresh) == 0 {
				break batches
			}
			for _, g := range fresh {
				id, err := m.lookup(ctx, q, client, kind, g)
				if errors.Is(err, ErrTMDBAuth) || ctx.Err() != nil {
					return finish(cmpErr(ctx.Err(), err))
				}
				if err != nil {
					if failures++; failures >= maxFailures {
						return finish(err)
					}
					m.Log.Debug("vod library: tmdb lookup", "kind", kind, "title", g.title, "year", g.year, "err", err)
					continue
				}
				failures = 0
				st.Searched++
				if id == "" {
					st.Missed++
					err = markTried(ctx, q, kind, g)
				} else {
					st.Matched++
					err = m.apply(ctx, q, client, kind, g, id)
				}
				if err != nil {
					return finish(err)
				}
				if st.Searched%50 == 0 {
					m.setStatus(st)
				}
			}
		}
	}
	// Ids found here reach copies whose names differ only in year noise.
	if _, err := q.InheritVODItemTmdbIDs(ctx); err != nil {
		return finish(err)
	}
	if _, err := q.InheritSeriesTmdbIDs(ctx); err != nil {
		return finish(err)
	}
	return finish(nil)
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (m *Matcher) setStatus(s MatchStatus) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *Matcher) unmatched(ctx context.Context, q *db.Queries, kind string) ([]titleGroup, error) {
	retry := int64(matchRetry / time.Second)
	batch := m.Batch
	if batch <= 0 {
		batch = matchBatch
	}
	var out []titleGroup
	if kind == "movie" {
		rows, err := q.ListUnmatchedMovieTitles(ctx, db.ListUnmatchedMovieTitlesParams{RetryAfterSecs: retry, Limit: batch})
		for _, r := range rows {
			out = append(out, titleGroup{r.TitleNorm, r.Title, r.Year})
		}
		return out, err
	}
	rows, err := q.ListUnmatchedSeriesTitles(ctx, db.ListUnmatchedSeriesTitlesParams{RetryAfterSecs: retry, Limit: batch})
	for _, r := range rows {
		out = append(out, titleGroup{r.TitleNorm, r.Title, r.Year})
	}
	return out, err
}

// lookup returns the group's TMDB id ("" for no match), from the search
// cache or a search: with the year first, then without (TMDB's year is
// the first release; a panel's may be a later one).
func (m *Matcher) lookup(ctx context.Context, q *db.Queries, c *TMDB, kind string, g titleGroup) (string, error) {
	id, err := q.GetTMDBSearch(ctx, db.GetTMDBSearchParams{Kind: kind, QueryKey: g.norm, Year: g.year, MaxAgeSecs: int64(matchRetry / time.Second)})
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	years := []int{int(g.year)}
	if g.year > 0 {
		years = append(years, 0)
	}
	var last []TMDBResult
	for _, y := range years {
		res, err := c.Search(ctx, kind, g.title, y)
		if err != nil {
			return "", err
		}
		last = res
		if hit, ok := Pick(res, g.title, int(g.year)); ok {
			id = strconv.FormatInt(hit.ID, 10)
			break
		}
	}
	// A panel's year for a series is often its latest season's: the only
	// show of exactly that title is it, whatever the year.
	if id == "" && kind == "series" {
		if hit, ok := Pick(last, g.title, 0); ok {
			id = strconv.FormatInt(hit.ID, 10)
		}
	}
	if err := q.PutTMDBSearch(ctx, db.PutTMDBSearchParams{Kind: kind, QueryKey: g.norm, Year: g.year, TmdbID: id}); err != nil {
		return "", err
	}
	return id, nil
}

// apply stores a match on every copy of the group, with the title's
// details (fetched once per TMDB id; a failure only leaves the IMDb id out).
func (m *Matcher) apply(ctx context.Context, q *db.Queries, c *TMDB, kind string, g titleGroup, id string) error {
	imdb := ""
	if cached, err := q.GetTMDBCache(ctx, db.GetTMDBCacheParams{Kind: kind, TmdbID: id}); err == nil {
		imdb = cached.ImdbID
	} else if raw, im, err := c.Details(ctx, kind, id); err == nil {
		imdb = im
		if err := q.PutTMDBCache(ctx, db.PutTMDBCacheParams{Kind: kind, TmdbID: id, ImdbID: im, Data: raw}); err != nil {
			return err
		}
	} else if errors.Is(err, ErrTMDBAuth) {
		return err
	}
	var err error
	if kind == "movie" {
		_, err = q.SetMovieTitleMatch(ctx, db.SetMovieTitleMatchParams{TmdbID: id, ImdbID: imdb, TitleNorm: g.norm, Year: g.year})
	} else {
		_, err = q.SetSeriesTitleMatch(ctx, db.SetSeriesTitleMatchParams{TmdbID: id, ImdbID: imdb, TitleNorm: g.norm, Year: g.year})
	}
	return err
}

func markTried(ctx context.Context, q *db.Queries, kind string, g titleGroup) error {
	if kind == "movie" {
		return q.MarkMovieTitleTried(ctx, db.MarkMovieTitleTriedParams{TitleNorm: g.norm, Year: g.year})
	}
	return q.MarkSeriesTitleTried(ctx, db.MarkSeriesTitleTriedParams{TitleNorm: g.norm, Year: g.year})
}

// Counts are the active titles with and without a TMDB id.
func (m *Matcher) Counts(ctx context.Context) (db.VODMatchCountsRow, error) {
	return db.New(m.DB).VODMatchCounts(ctx)
}

// CheckKey tells whether TMDB accepts key (one cheap call).
func (m *Matcher) CheckKey(ctx context.Context, key string) error {
	return (&TMDB{Key: key, Base: m.Base, HTTP: m.HTTP}).Check(ctx)
}

// Fetch loads a title's TMDB details into the cache (a manual match) and
// returns TMDB's title for it; TMDB answering "not found" means no such id.
func (m *Matcher) Fetch(ctx context.Context, kind, id string) (string, error) {
	s, err := LoadSettings(ctx, db.New(m.DB))
	if err != nil {
		return "", err
	}
	if s.TMDBKey == "" {
		return "", nil // nothing to check with
	}
	raw, imdb, err := (&TMDB{Key: s.TMDBKey, Base: m.Base, HTTP: m.HTTP}).Details(ctx, kind, id)
	if err != nil {
		return "", err
	}
	if err := db.New(m.DB).PutTMDBCache(ctx, db.PutTMDBCacheParams{Kind: kind, TmdbID: id, ImdbID: imdb, Data: raw}); err != nil {
		return "", err
	}
	var d struct{ Title, Name string }
	_ = json.Unmarshal(raw, &d)
	return cmpOr(d.Title, d.Name), nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
