package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JustDr00py/mmp/internal/db"
	"github.com/JustDr00py/mmp/internal/source/xtream"
	"github.com/JustDr00py/mmp/internal/vodmeta"
)

// ErrVODUnsupported: only Xtream panels expose movie and series catalogs.
var ErrVODUnsupported = errors.New("VOD import needs an Xtream provider")

// VODResult summarises one catalog import.
type VODResult struct {
	Movies         int `json:"movies"`
	Series         int `json:"series"`
	Categories     int `json:"categories"`
	MoviesDisabled int `json:"movies_disabled"`
	SeriesDisabled int `json:"series_disabled"`
	// Titles in categories the provider's selection leaves out: not
	// imported (and disabled if stored before).
	MoviesSkipped int `json:"movies_skipped"`
	SeriesSkipped int `json:"series_skipped"`
}

// RefreshVOD imports the provider's movie and series catalogs (not
// episodes, which load lazily per series) in one transaction. The
// disable-threshold guard of live refreshes applies to each catalog. With
// the provider's category filter on, only titles in selected categories
// are kept; the guard still weighs the panel's whole answer, so leaving
// categories out on purpose never reads as a truncated catalog.
func (r *Refresher) RefreshVOD(ctx context.Context, p db.Provider, force bool) (VODResult, error) {
	var res VODResult
	if p.Type != "xtream" {
		return res, ErrVODUnsupported
	}
	r.log.Info("refreshing vod", "provider_id", p.ID, "force", force)
	client, err := xtream.New(ProviderURL(p), p.Username, p.Password, p.UserAgent)
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: %w", p.ID, err)
	}
	movieCats, err := client.VODCategories(ctx)
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: get_vod_categories: %w", p.ID, err)
	}
	movies, err := client.VODStreams(ctx)
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: get_vod_streams: %w", p.ID, err)
	}
	seriesCats, err := client.SeriesCategories(ctx)
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: get_series_categories: %w", p.ID, err)
	}
	shows, err := client.Series(ctx)
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: get_series: %w", p.ID, err)
	}

	mp := movieParams(p.ID, movies, categoryNames(movieCats))
	sp := seriesParams(p.ID, shows, categoryNames(seriesCats))

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: begin: %w", p.ID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	q := db.New(tx)

	if !force {
		if err := r.vodGuard(ctx, q, p, "movies", mp.ExternalIds, q.CountActiveVODItems, func(ids []string) (int64, error) {
			return q.CountActiveVODItemsNotIn(ctx, db.CountActiveVODItemsNotInParams{ProviderID: p.ID, ExternalIds: ids})
		}); err != nil {
			return res, err
		}
		if err := r.vodGuard(ctx, q, p, "series", sp.ExternalIds, q.CountActiveSeries, func(ids []string) (int64, error) {
			return q.CountActiveSeriesNotIn(ctx, db.CountActiveSeriesNotInParams{ProviderID: p.ID, ExternalIds: ids})
		}); err != nil {
			return res, err
		}
	}

	if res.Categories, err = storeCategories(ctx, q, p.ID, movieCats, seriesCats); err != nil {
		return res, fmt.Errorf("refresh vod %d: %w", p.ID, err)
	}
	if p.VodCategoryFilter {
		sel, err := q.ListSelectedVODCategories(ctx, p.ID)
		if err != nil {
			return res, fmt.Errorf("refresh vod %d: selection: %w", p.ID, err)
		}
		keep := map[string]map[string]bool{"movie": {}, "series": {}}
		for _, c := range sel {
			keep[c.Kind][c.ExternalID] = true
		}
		var skipped int
		mp, skipped = keepMovies(mp, keep["movie"])
		res.MoviesSkipped = skipped
		sp, skipped = keepSeries(sp, keep["series"])
		res.SeriesSkipped = skipped
	}
	res.Movies, res.Series = len(mp.ExternalIds), len(sp.ExternalIds)
	if err := q.UpsertVODItems(ctx, mp); err != nil {
		return res, fmt.Errorf("refresh vod %d: movies: %w", p.ID, err)
	}
	n, err := q.SoftDisableMissingVODItems(ctx, db.SoftDisableMissingVODItemsParams{ProviderID: p.ID, ExternalIds: mp.ExternalIds})
	if err != nil {
		return res, fmt.Errorf("refresh vod %d: movies: %w", p.ID, err)
	}
	res.MoviesDisabled = int(n)
	if err := q.UpsertSeries(ctx, sp); err != nil {
		return res, fmt.Errorf("refresh vod %d: series: %w", p.ID, err)
	}
	if n, err = q.SoftDisableMissingSeries(ctx, db.SoftDisableMissingSeriesParams{ProviderID: p.ID, ExternalIds: sp.ExternalIds}); err != nil {
		return res, fmt.Errorf("refresh vod %d: series: %w", p.ID, err)
	}
	res.SeriesDisabled = int(n)
	if err := q.MarkProviderVODRefreshed(ctx, p.ID); err != nil {
		return res, fmt.Errorf("refresh vod %d: mark: %w", p.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("refresh vod %d: commit: %w", p.ID, err)
	}
	r.inheritTmdbIDs(ctx)
	if r.AfterVODRefresh != nil {
		r.AfterVODRefresh(p)
	}
	r.log.Info("vod refreshed", "provider_id", p.ID, "movies", res.Movies, "series", res.Series,
		"movies_disabled", res.MoviesDisabled, "series_disabled", res.SeriesDisabled,
		"movies_skipped", res.MoviesSkipped, "series_skipped", res.SeriesSkipped)
	return res, nil
}

// inheritTmdbIDs lends TMDB ids between copies of a title on any provider
// (see InheritVODItemTmdbIDs). It runs outside an import's transaction,
// since it touches every provider's rows; a failure only delays merging.
func (r *Refresher) inheritTmdbIDs(ctx context.Context) {
	q := db.New(r.pool)
	if _, err := q.InheritVODItemTmdbIDs(ctx); err != nil {
		r.log.Warn("vod: lending movie tmdb ids", "err", err)
	}
	if _, err := q.InheritSeriesTmdbIDs(ctx); err != nil {
		r.log.Warn("vod: lending series tmdb ids", "err", err)
	}
}

// RefreshVODCategories imports only the provider's movie and series
// category lists (two small panel calls), so they can be chosen before a
// first, possibly huge, catalog import. It returns how many it stored.
func (r *Refresher) RefreshVODCategories(ctx context.Context, p db.Provider) (int, error) {
	if p.Type != "xtream" {
		return 0, ErrVODUnsupported
	}
	client, err := xtream.New(ProviderURL(p), p.Username, p.Password, p.UserAgent)
	if err != nil {
		return 0, fmt.Errorf("vod categories %d: %w", p.ID, err)
	}
	movieCats, err := client.VODCategories(ctx)
	if err != nil {
		return 0, fmt.Errorf("vod categories %d: get_vod_categories: %w", p.ID, err)
	}
	seriesCats, err := client.SeriesCategories(ctx)
	if err != nil {
		return 0, fmt.Errorf("vod categories %d: get_series_categories: %w", p.ID, err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("vod categories %d: begin: %w", p.ID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	n, err := storeCategories(ctx, db.New(tx), p.ID, movieCats, seriesCats)
	if err != nil {
		return 0, fmt.Errorf("vod categories %d: %w", p.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("vod categories %d: commit: %w", p.ID, err)
	}
	r.log.Info("vod categories refreshed", "provider_id", p.ID, "categories", n)
	return n, nil
}

// storeCategories upserts both category lists and disables the ones the
// panel no longer lists. It returns how many categories the panel listed.
func storeCategories(ctx context.Context, q *db.Queries, providerID int64, movieCats, seriesCats []xtream.VODCategory) (int, error) {
	n := 0
	for kind, cats := range map[string][]xtream.VODCategory{"movie": movieCats, "series": seriesCats} {
		ids, names := categoryParams(cats)
		n += len(ids)
		if err := q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: providerID, Kind: kind, ExternalIds: ids, Names: names}); err != nil {
			return n, fmt.Errorf("%s categories: %w", kind, err)
		}
		if len(ids) > 0 { // an empty category list is a panel hiccup, not "all gone"
			if _, err := q.SoftDisableMissingVODCategories(ctx, db.SoftDisableMissingVODCategoriesParams{ProviderID: providerID, Kind: kind, ExternalIds: ids}); err != nil {
				return n, fmt.Errorf("%s categories: %w", kind, err)
			}
		}
	}
	return n, nil
}

// keepMovies drops the movies outside the kept categories, returning how
// many it dropped.
func keepMovies(p db.UpsertVODItemsParams, keep map[string]bool) (db.UpsertVODItemsParams, int) {
	idx := keptIndexes(p.CategoryIds, keep)
	out := db.UpsertVODItemsParams{ProviderID: p.ProviderID,
		ExternalIds: pick(p.ExternalIds, idx), Names: pick(p.Names, idx), CategoryIds: pick(p.CategoryIds, idx),
		Icons: pick(p.Icons, idx), Ratings: pick(p.Ratings, idx), ContainerExtensions: pick(p.ContainerExtensions, idx),
		TmdbIds: pick(p.TmdbIds, idx), Added: pick(p.Added, idx), Titles: pick(p.Titles, idx),
		TitleNorms: pick(p.TitleNorms, idx), Years: pick(p.Years, idx), Qualities: pick(p.Qualities, idx), Langs: pick(p.Langs, idx)}
	return out, len(p.ExternalIds) - len(out.ExternalIds)
}

// keepSeries is keepMovies for series.
func keepSeries(p db.UpsertSeriesParams, keep map[string]bool) (db.UpsertSeriesParams, int) {
	idx := keptIndexes(p.CategoryIds, keep)
	out := db.UpsertSeriesParams{ProviderID: p.ProviderID,
		ExternalIds: pick(p.ExternalIds, idx), Names: pick(p.Names, idx), CategoryIds: pick(p.CategoryIds, idx),
		Covers: pick(p.Covers, idx), Plots: pick(p.Plots, idx), Casts: pick(p.Casts, idx),
		Directors: pick(p.Directors, idx), Genres: pick(p.Genres, idx), ReleaseDates: pick(p.ReleaseDates, idx),
		Ratings: pick(p.Ratings, idx), LastModified: pick(p.LastModified, idx), TmdbIds: pick(p.TmdbIds, idx),
		Titles: pick(p.Titles, idx), TitleNorms: pick(p.TitleNorms, idx), Years: pick(p.Years, idx),
		Qualities: pick(p.Qualities, idx), Langs: pick(p.Langs, idx)}
	return out, len(p.ExternalIds) - len(out.ExternalIds)
}

func keptIndexes(cats []string, keep map[string]bool) []int {
	var idx []int
	for i, cat := range cats {
		if keep[cat] {
			idx = append(idx, i)
		}
	}
	return idx
}

// pick returns s's elements at idx, never nil: a NULL list would disable
// nothing, and unnest pads a NULL array column with NULLs.
func pick[T any](s []T, idx []int) []T {
	out := make([]T, 0, len(idx))
	for _, i := range idx {
		out = append(out, s[i])
	}
	return out
}

// vodGuard refuses an import that would disable more than the provider's
// threshold of an active catalog (a truncated answer).
func (r *Refresher) vodGuard(ctx context.Context, q *db.Queries, p db.Provider, what string, seen []string,
	countActive func(context.Context, int64) (int64, error), countMissing func([]string) (int64, error)) error {
	maxPct := r.MaxDisablePct
	if p.RefreshMaxDisablePct != nil {
		maxPct = int(*p.RefreshMaxDisablePct)
	}
	if maxPct >= 100 {
		return nil
	}
	missing, err := countMissing(seen)
	if err != nil || missing == 0 {
		return err
	}
	active, err := countActive(ctx, p.ID)
	if err != nil {
		return err
	}
	refused := &DisableRefusedError{ProviderID: int(p.ID), WouldDisable: int(missing), Active: int(active), MaxPct: maxPct}
	if refused.Pct() > maxPct {
		return fmt.Errorf("refresh vod %d: %s: %w", p.ID, what, refused)
	}
	return nil
}

func categoryParams(cats []xtream.VODCategory) (ids, names []string) {
	seen := map[string]bool{}
	for _, c := range cats {
		id := strings.TrimSpace(string(c.CategoryID))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids, names = append(ids, id), append(names, c.CategoryName)
	}
	return ids, names
}

// rating is the panel's rating, or -1 (stored as NULL) when unknown.
func rating(f xtream.FlexString) float32 {
	if x, ok := f.Float(); ok && x >= 0 {
		return float32(x)
	}
	return -1
}

func ext(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), ".")
	if s == "" {
		return "mp4"
	}
	return s
}

// tmdbID is the panel's tmdb field when it is a real id: panels send "",
// "0", "null" or junk for unknown.
func tmdbID(f xtream.FlexString) string {
	s := strings.TrimSpace(string(f))
	if s == "" || strings.Trim(s, "0") == "" {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s
}

// movieParams builds the bulk upsert, dropping rows without an id and
// duplicate ids (the last one wins), which one upsert statement cannot
// hold. catNames (category id to name) lends titles a language label.
func movieParams(providerID int64, movies []xtream.VODStream, catNames map[string]string) db.UpsertVODItemsParams {
	p := db.UpsertVODItemsParams{ProviderID: providerID, ExternalIds: []string{}}
	at := map[string]int{}
	for _, m := range movies {
		id := strings.TrimSpace(string(m.StreamID))
		if id == "" {
			continue
		}
		i, dup := at[id]
		if !dup {
			i = len(p.ExternalIds)
			at[id] = i
			p.ExternalIds = append(p.ExternalIds, id)
			p.Names, p.CategoryIds, p.Icons = append(p.Names, ""), append(p.CategoryIds, ""), append(p.Icons, "")
			p.Ratings, p.ContainerExtensions = append(p.Ratings, 0), append(p.ContainerExtensions, "")
			p.TmdbIds, p.Added = append(p.TmdbIds, ""), append(p.Added, 0)
			p.Titles, p.TitleNorms, p.Years = append(p.Titles, ""), append(p.TitleNorms, ""), append(p.Years, 0)
			p.Qualities, p.Langs = append(p.Qualities, ""), append(p.Langs, "")
		}
		cat := string(m.CategoryID)
		t := vodmeta.Parse(m.Name, catNames[cat])
		p.Names[i], p.CategoryIds[i], p.Icons[i] = m.Name, cat, m.StreamIcon
		p.Ratings[i], p.ContainerExtensions[i] = rating(m.Rating), ext(m.ContainerExtension)
		p.TmdbIds[i], p.Added[i] = tmdbID(m.TMDB), m.Added.Int()
		p.Titles[i], p.TitleNorms[i], p.Years[i] = t.Title, t.Key(), int32(t.Year)
		p.Qualities[i], p.Langs[i] = t.Quality, t.Lang
	}
	return p
}

func seriesParams(providerID int64, shows []xtream.Series, catNames map[string]string) db.UpsertSeriesParams {
	p := db.UpsertSeriesParams{ProviderID: providerID, ExternalIds: []string{}}
	at := map[string]int{}
	for _, s := range shows {
		id := strings.TrimSpace(string(s.SeriesID))
		if id == "" {
			continue
		}
		i, dup := at[id]
		if !dup {
			i = len(p.ExternalIds)
			at[id] = i
			p.ExternalIds = append(p.ExternalIds, id)
			p.Names, p.CategoryIds, p.Covers, p.Plots = append(p.Names, ""), append(p.CategoryIds, ""), append(p.Covers, ""), append(p.Plots, "")
			p.Casts, p.Directors, p.Genres = append(p.Casts, ""), append(p.Directors, ""), append(p.Genres, "")
			p.ReleaseDates, p.Ratings, p.LastModified = append(p.ReleaseDates, ""), append(p.Ratings, 0), append(p.LastModified, 0)
			p.TmdbIds, p.Titles, p.TitleNorms = append(p.TmdbIds, ""), append(p.Titles, ""), append(p.TitleNorms, "")
			p.Years, p.Qualities, p.Langs = append(p.Years, 0), append(p.Qualities, ""), append(p.Langs, "")
		}
		cat := string(s.CategoryID)
		t := vodmeta.Parse(s.Name, catNames[cat])
		if t.Year == 0 {
			t.Year = vodmeta.ReleaseYear(s.Released())
		}
		p.Names[i], p.CategoryIds[i], p.Covers[i], p.Plots[i] = s.Name, cat, s.Cover, s.Plot
		p.Casts[i], p.Directors[i], p.Genres[i] = s.Cast, s.Director, s.Genre
		p.ReleaseDates[i], p.Ratings[i], p.LastModified[i] = s.Released(), rating(s.Rating), s.LastModified.Int()
		p.TmdbIds[i], p.Titles[i], p.TitleNorms[i] = tmdbID(s.TMDB), t.Title, t.Key()
		p.Years[i], p.Qualities[i], p.Langs[i] = int32(t.Year), t.Quality, t.Lang
	}
	return p
}

// categoryNames maps category ids to names.
func categoryNames(cats []xtream.VODCategory) map[string]string {
	m := make(map[string]string, len(cats))
	for _, c := range cats {
		m[strings.TrimSpace(string(c.CategoryID))] = c.CategoryName
	}
	return m
}

// SeriesEpisodes returns a series' episodes, fetching them from the panel
// when never fetched or older than ttl. If the panel fails, cached episodes
// (if any) are returned with the error logged.
func (r *Refresher) SeriesEpisodes(ctx context.Context, p db.Provider, s db.Series, ttl time.Duration) ([]db.SeriesEpisode, error) {
	q := db.New(r.pool)
	if s.EpisodesFetchedAt != nil && time.Since(*s.EpisodesFetchedAt) < ttl {
		return q.ListSeriesEpisodes(ctx, s.ID)
	}
	fresh, err := r.fetchEpisodes(ctx, p, s)
	if err != nil {
		if s.EpisodesFetchedAt != nil {
			r.log.Warn("series episodes: refetch failed, serving cached", "series_id", s.ID, "err", err)
			return q.ListSeriesEpisodes(ctx, s.ID)
		}
		return nil, err
	}
	return fresh, nil
}

func (r *Refresher) fetchEpisodes(ctx context.Context, p db.Provider, s db.Series) ([]db.SeriesEpisode, error) {
	if p.Type != "xtream" {
		return nil, ErrVODUnsupported
	}
	client, err := xtream.New(ProviderURL(p), p.Username, p.Password, p.UserAgent)
	if err != nil {
		return nil, err
	}
	info, err := client.SeriesInfo(ctx, s.ExternalID)
	if err != nil {
		return nil, fmt.Errorf("series %d: get_series_info: %w", s.ID, err)
	}
	ep := db.UpsertEpisodesParams{SeriesID: s.ID}
	at := map[string]int{}
	for _, e := range info.Episodes {
		id := strings.TrimSpace(string(e.ID))
		if id == "" {
			continue
		}
		i, dup := at[id]
		if !dup {
			i = len(ep.ExternalIds)
			at[id] = i
			ep.ExternalIds = append(ep.ExternalIds, id)
			ep.Seasons, ep.EpisodeNums, ep.Titles = append(ep.Seasons, 0), append(ep.EpisodeNums, 0), append(ep.Titles, "")
			ep.ContainerExtensions, ep.Plots, ep.Durations = append(ep.ContainerExtensions, ""), append(ep.Plots, ""), append(ep.Durations, 0)
			ep.Images, ep.Added = append(ep.Images, ""), append(ep.Added, 0)
		}
		d := e.Details()
		ep.Seasons[i], ep.EpisodeNums[i], ep.Titles[i] = int32(e.Season.Int()), int32(e.EpisodeNum.Int()), e.Title
		ep.ContainerExtensions[i], ep.Plots[i], ep.Durations[i] = ext(e.ContainerExtension), d.Plot, int32(d.DurationSecs.Int())
		ep.Images[i], ep.Added[i] = d.MovieImage, e.Added.Int()
	}
	var infoJSON []byte
	if len(info.Info) > 0 && info.Info[0] == '{' {
		infoJSON = info.Info
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	q := db.New(tx)
	if err := q.UpsertEpisodes(ctx, ep); err != nil {
		return nil, fmt.Errorf("series %d: episodes: %w", s.ID, err)
	}
	if _, err := q.DeleteEpisodesNotIn(ctx, db.DeleteEpisodesNotInParams{SeriesID: s.ID, ExternalIds: ep.ExternalIds}); err != nil {
		return nil, fmt.Errorf("series %d: episodes: %w", s.ID, err)
	}
	if err := q.MarkSeriesEpisodesFetched(ctx, db.MarkSeriesEpisodesFetchedParams{ID: s.ID, Info: infoJSON}); err != nil {
		return nil, fmt.Errorf("series %d: mark: %w", s.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return db.New(r.pool).ListSeriesEpisodes(ctx, s.ID)
}

// MovieInfo returns a movie's panel info block, fetching get_vod_info when
// it was never fetched or is older than ttl. On a panel error the cached
// block (possibly nil) is returned with the error logged.
func (r *Refresher) MovieInfo(ctx context.Context, itemID int64, ttl time.Duration) (json.RawMessage, error) {
	q := db.New(r.pool)
	item, err := q.GetVODItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if item.InfoFetchedAt != nil && time.Since(*item.InfoFetchedAt) < ttl {
		return item.Info, nil
	}
	p, err := q.GetProvider(ctx, item.ProviderID)
	if err != nil {
		return nil, err
	}
	if p.Type != "xtream" {
		return item.Info, nil
	}
	client, err := xtream.New(ProviderURL(p), p.Username, p.Password, p.UserAgent)
	if err != nil {
		return item.Info, err
	}
	info, err := client.VODInfo(ctx, item.ExternalID)
	if err != nil {
		r.log.Warn("movie info: panel fetch failed, serving cached", "vod_item_id", item.ID, "err", err)
		return item.Info, nil
	}
	if err := q.SetVODItemInfo(ctx, db.SetVODItemInfoParams{ID: item.ID, Info: info}); err != nil {
		r.log.Warn("movie info: cache write failed", "vod_item_id", item.ID, "err", err)
	}
	return info, nil
}

// SeriesEpisodesByID is SeriesEpisodes for a series id; it also returns
// the series row (with its cached info block) as stored after the fetch.
func (r *Refresher) SeriesEpisodesByID(ctx context.Context, seriesID int64, ttl time.Duration) (db.Series, []db.SeriesEpisode, error) {
	q := db.New(r.pool)
	s, err := q.GetSeries(ctx, seriesID)
	if err != nil {
		return s, nil, err
	}
	p, err := q.GetProvider(ctx, s.ProviderID)
	if err != nil {
		return s, nil, err
	}
	eps, err := r.SeriesEpisodes(ctx, p, s, ttl)
	if err != nil {
		return s, nil, err
	}
	if fresh, err := q.GetSeries(ctx, seriesID); err == nil {
		s = fresh
	}
	return s, eps, nil
}
