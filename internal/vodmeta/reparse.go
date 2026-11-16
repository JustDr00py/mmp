package vodmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/JustDr00py/mmp/internal/db"
)

// MatchVersion changes whenever TMDB matching accepts hits differently;
// titles TMDB had no match for are then searched again, and titles a
// search matched are checked again.
const MatchVersion = 3

const versionsKey = "vod_library_versions"

// versions are the parser and matcher versions stored titles were made
// with.
type versions struct {
	Parse int `json:"parse"`
	Match int `json:"match"`
}

const reparseBatch = 5000

// Upgrade brings stored titles up to this build's parser and matcher:
// titles are parsed again when the parser changed (merging and TMDB
// searches then use the new names, with no import needed), and cached
// TMDB searches are forgotten when matching changed. It reports whether it
// changed anything (a TMDB pass is then worth starting).
func Upgrade(ctx context.Context, dbtx db.DBTX, log *slog.Logger) (bool, error) {
	q := db.New(dbtx)
	var v versions
	raw, err := q.GetSetting(ctx, versionsKey)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		v = versions{Parse: 1, Match: 1} // stored by the first library build
	case err != nil:
		return false, fmt.Errorf("vod library versions: %w", err)
	default:
		_ = json.Unmarshal(raw, &v)
	}
	if v.Parse == ParseVersion && v.Match == MatchVersion {
		return false, nil
	}
	if v.Parse != ParseVersion {
		movies, series, err := reparse(ctx, q)
		if err != nil {
			return false, err
		}
		log.Info("vod library: titles parsed again", "movies", movies, "series", series)
		if _, err := q.InheritVODItemTmdbIDs(ctx); err != nil {
			return false, err
		}
		if _, err := q.InheritSeriesTmdbIDs(ctx); err != nil {
			return false, err
		}
	}
	if v.Match != MatchVersion {
		if err := q.ForgetTMDBSearches(ctx); err != nil {
			return false, fmt.Errorf("vod library: forget tmdb searches: %w", err)
		}
		log.Info("vod library: titles will be searched on TMDB again under the new matching rules")
	}
	raw, _ = json.Marshal(versions{Parse: ParseVersion, Match: MatchVersion})
	if err := q.SetSetting(ctx, db.SetSettingParams{Key: versionsKey, Value: raw}); err != nil {
		return false, fmt.Errorf("vod library versions: %w", err)
	}
	return true, nil
}

func reparse(ctx context.Context, q *db.Queries) (movies, series int, err error) {
	for after := int64(0); ; {
		rows, err := q.ListVODItemsToParse(ctx, db.ListVODItemsToParseParams{AfterID: after, Limit: reparseBatch})
		if err != nil || len(rows) == 0 {
			return movies, series, err
		}
		var u db.UpdateVODItemsParsedParams
		for _, r := range rows {
			p := Parse(r.Name, r.Category)
			u.Ids, u.Titles, u.TitleNorms = append(u.Ids, r.ID), append(u.Titles, p.Title), append(u.TitleNorms, p.Key())
			u.Years, u.Qualities, u.Langs = append(u.Years, int32(p.Year)), append(u.Qualities, p.Quality), append(u.Langs, p.Lang)
		}
		if err := q.UpdateVODItemsParsed(ctx, u); err != nil {
			return movies, series, err
		}
		movies += len(rows)
		after = rows[len(rows)-1].ID
		if len(rows) < reparseBatch {
			break
		}
	}
	for after := int64(0); ; {
		rows, err := q.ListSeriesToParse(ctx, db.ListSeriesToParseParams{AfterID: after, Limit: reparseBatch})
		if err != nil || len(rows) == 0 {
			return movies, series, err
		}
		var u db.UpdateSeriesParsedParams
		for _, r := range rows {
			p := ParseSeries(r.Name, r.Category, r.ReleaseDate)
			u.Ids, u.Titles, u.TitleNorms = append(u.Ids, r.ID), append(u.Titles, p.Title), append(u.TitleNorms, p.Key())
			u.Years, u.Qualities, u.Langs = append(u.Years, int32(p.Year)), append(u.Qualities, p.Quality), append(u.Langs, p.Lang)
		}
		if err := q.UpdateSeriesParsed(ctx, u); err != nil {
			return movies, series, err
		}
		series += len(rows)
		after = rows[len(rows)-1].ID
		if len(rows) < reparseBatch {
			return movies, series, nil
		}
	}
}
