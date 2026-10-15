package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sysadmin/mmp/internal/db"
	"github.com/sysadmin/mmp/internal/source/xtream"
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
}

// RefreshVOD imports the provider's movie and series catalogs (not
// episodes, which load lazily per series) in one transaction. The
// disable-threshold guard of live refreshes applies to each catalog.
func (r *Refresher) RefreshVOD(ctx context.Context, p db.Provider, force bool) (VODResult, error) {
	var res VODResult
	if p.Type != "xtream" {
		return res, ErrVODUnsupported
	}
	r.log.Info("refreshing vod", "provider_id", p.ID, "force", force)
	client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
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

	mp := movieParams(p.ID, movies)
	sp := seriesParams(p.ID, shows)
	res.Movies, res.Series = len(mp.ExternalIds), len(sp.ExternalIds)

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

	for kind, cats := range map[string][]xtream.VODCategory{"movie": movieCats, "series": seriesCats} {
		ids, names := categoryParams(cats)
		res.Categories += len(ids)
		if err := q.UpsertVODCategories(ctx, db.UpsertVODCategoriesParams{ProviderID: p.ID, Kind: kind, ExternalIds: ids, Names: names}); err != nil {
			return res, fmt.Errorf("refresh vod %d: %s categories: %w", p.ID, kind, err)
		}
		if len(ids) > 0 { // an empty category list is a panel hiccup, not "all gone"
			if _, err := q.SoftDisableMissingVODCategories(ctx, db.SoftDisableMissingVODCategoriesParams{ProviderID: p.ID, Kind: kind, ExternalIds: ids}); err != nil {
				return res, fmt.Errorf("refresh vod %d: %s categories: %w", p.ID, kind, err)
			}
		}
	}
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
	r.log.Info("vod refreshed", "provider_id", p.ID, "movies", res.Movies, "series", res.Series,
		"movies_disabled", res.MoviesDisabled, "series_disabled", res.SeriesDisabled)
	return res, nil
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

// movieParams builds the bulk upsert, dropping rows without an id and
// duplicate ids (the last one wins), which one upsert statement cannot hold.
func movieParams(providerID int64, movies []xtream.VODStream) db.UpsertVODItemsParams {
	p := db.UpsertVODItemsParams{ProviderID: providerID}
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
		}
		p.Names[i], p.CategoryIds[i], p.Icons[i] = m.Name, string(m.CategoryID), m.StreamIcon
		p.Ratings[i], p.ContainerExtensions[i] = rating(m.Rating), ext(m.ContainerExtension)
		p.TmdbIds[i], p.Added[i] = string(m.TMDB), m.Added.Int()
	}
	return p
}

func seriesParams(providerID int64, shows []xtream.Series) db.UpsertSeriesParams {
	p := db.UpsertSeriesParams{ProviderID: providerID}
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
		}
		p.Names[i], p.CategoryIds[i], p.Covers[i], p.Plots[i] = s.Name, string(s.CategoryID), s.Cover, s.Plot
		p.Casts[i], p.Directors[i], p.Genres[i] = s.Cast, s.Director, s.Genre
		p.ReleaseDates[i], p.Ratings[i], p.LastModified[i] = s.Released(), rating(s.Rating), s.LastModified.Int()
	}
	return p
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
	client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
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
	client, err := xtream.New(p.Url, p.Username, p.Password, p.UserAgent)
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
