-- VOD (movies) and series catalogs. Imports are bulk upserts (one statement
-- per table via unnest) because catalogs reach tens of thousands of rows.

-- name: UpsertVODCategories :exec
INSERT INTO vod_categories (provider_id, kind, external_id, name)
SELECT sqlc.arg('provider_id'), sqlc.arg('kind'), u.ext, u.name
FROM (SELECT unnest(sqlc.arg('external_ids')::text[]) AS ext, unnest(sqlc.arg('names')::text[]) AS name) AS u
ON CONFLICT (provider_id, kind, external_id) DO UPDATE
SET name = EXCLUDED.name, disabled = FALSE, last_seen = now();

-- name: SoftDisableMissingVODCategories :execrows
UPDATE vod_categories SET disabled = TRUE
WHERE provider_id = sqlc.arg('provider_id') AND kind = sqlc.arg('kind') AND NOT disabled
  AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: UpsertVODItems :exec
-- rating -1 and added 0 mean "unknown" (stored as NULL).
INSERT INTO vod_items (provider_id, external_id, name, category_external_id, icon, rating,
                       container_extension, tmdb_id, added_at)
SELECT sqlc.arg('provider_id'), u.ext, u.name, u.cat, u.icon, NULLIF(u.rating, -1),
       u.cext, u.tmdb, to_timestamp(NULLIF(u.added, 0))
FROM (SELECT unnest(sqlc.arg('external_ids')::text[]) AS ext, unnest(sqlc.arg('names')::text[]) AS name, unnest(sqlc.arg('category_ids')::text[]) AS cat, unnest(sqlc.arg('icons')::text[]) AS icon, unnest(sqlc.arg('ratings')::real[]) AS rating, unnest(sqlc.arg('container_extensions')::text[]) AS cext, unnest(sqlc.arg('tmdb_ids')::text[]) AS tmdb, unnest(sqlc.arg('added')::bigint[]) AS added) AS u
ON CONFLICT (provider_id, external_id) DO UPDATE
SET name = EXCLUDED.name, category_external_id = EXCLUDED.category_external_id, icon = EXCLUDED.icon,
    rating = EXCLUDED.rating, container_extension = EXCLUDED.container_extension,
    tmdb_id = EXCLUDED.tmdb_id, added_at = EXCLUDED.added_at, disabled = FALSE, last_seen = now();

-- name: CountActiveVODItems :one
SELECT count(*) FROM vod_items WHERE provider_id = $1 AND NOT disabled;

-- name: CountActiveVODItemsNotIn :one
SELECT count(*) FROM vod_items
WHERE provider_id = sqlc.arg('provider_id') AND NOT disabled
  AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: SoftDisableMissingVODItems :execrows
UPDATE vod_items SET disabled = TRUE
WHERE provider_id = sqlc.arg('provider_id') AND NOT disabled
  AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: UpsertSeries :exec
INSERT INTO series (provider_id, external_id, name, category_external_id, cover, plot, "cast",
                    director, genre, release_date, rating, last_modified)
SELECT sqlc.arg('provider_id'), u.ext, u.name, u.cat, u.cover, u.plot, u.cst, u.director, u.genre,
       u.released, NULLIF(u.rating, -1), to_timestamp(NULLIF(u.modified, 0))
FROM (SELECT unnest(sqlc.arg('external_ids')::text[]) AS ext, unnest(sqlc.arg('names')::text[]) AS name, unnest(sqlc.arg('category_ids')::text[]) AS cat, unnest(sqlc.arg('covers')::text[]) AS cover, unnest(sqlc.arg('plots')::text[]) AS plot, unnest(sqlc.arg('casts')::text[]) AS cst, unnest(sqlc.arg('directors')::text[]) AS director, unnest(sqlc.arg('genres')::text[]) AS genre, unnest(sqlc.arg('release_dates')::text[]) AS released, unnest(sqlc.arg('ratings')::real[]) AS rating, unnest(sqlc.arg('last_modified')::bigint[]) AS modified) AS u
ON CONFLICT (provider_id, external_id) DO UPDATE
SET name = EXCLUDED.name, category_external_id = EXCLUDED.category_external_id, cover = EXCLUDED.cover,
    plot = EXCLUDED.plot, "cast" = EXCLUDED."cast", director = EXCLUDED.director, genre = EXCLUDED.genre,
    release_date = EXCLUDED.release_date, rating = EXCLUDED.rating,
    -- a newer last_modified means the episode list may have changed: refetch lazily
    episodes_fetched_at = CASE WHEN series.last_modified IS DISTINCT FROM EXCLUDED.last_modified
                               THEN NULL ELSE series.episodes_fetched_at END,
    last_modified = EXCLUDED.last_modified, disabled = FALSE, last_seen = now();

-- name: CountActiveSeries :one
SELECT count(*) FROM series WHERE provider_id = $1 AND NOT disabled;

-- name: CountActiveSeriesNotIn :one
SELECT count(*) FROM series
WHERE provider_id = sqlc.arg('provider_id') AND NOT disabled
  AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: SoftDisableMissingSeries :execrows
UPDATE series SET disabled = TRUE
WHERE provider_id = sqlc.arg('provider_id') AND NOT disabled
  AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: MarkProviderVODRefreshed :exec
UPDATE providers SET vod_refreshed_at = now() WHERE id = $1;

-- name: GetSeries :one
SELECT * FROM series WHERE id = $1;

-- name: UpsertEpisodes :exec
INSERT INTO series_episodes (series_id, external_id, season, episode_num, title, container_extension,
                             plot, duration_secs, image, added_at)
SELECT sqlc.arg('series_id'), u.ext, u.season, u.num, u.title, u.cext, u.plot, u.dur, u.image,
       to_timestamp(NULLIF(u.added, 0))
FROM (SELECT unnest(sqlc.arg('external_ids')::text[]) AS ext, unnest(sqlc.arg('seasons')::int[]) AS season, unnest(sqlc.arg('episode_nums')::int[]) AS num, unnest(sqlc.arg('titles')::text[]) AS title, unnest(sqlc.arg('container_extensions')::text[]) AS cext, unnest(sqlc.arg('plots')::text[]) AS plot, unnest(sqlc.arg('durations')::int[]) AS dur, unnest(sqlc.arg('images')::text[]) AS image, unnest(sqlc.arg('added')::bigint[]) AS added) AS u
ON CONFLICT (series_id, external_id) DO UPDATE
SET season = EXCLUDED.season, episode_num = EXCLUDED.episode_num, title = EXCLUDED.title,
    container_extension = EXCLUDED.container_extension, plot = EXCLUDED.plot,
    duration_secs = EXCLUDED.duration_secs, image = EXCLUDED.image, added_at = EXCLUDED.added_at;

-- name: DeleteEpisodesNotIn :execrows
-- Episodes are fetched as a whole list per series, so missing ones go.
DELETE FROM series_episodes
WHERE series_id = sqlc.arg('series_id') AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: MarkSeriesEpisodesFetched :exec
UPDATE series SET episodes_fetched_at = now(), info = sqlc.narg('info') WHERE id = sqlc.arg('id');

-- name: ListSeriesEpisodes :many
SELECT * FROM series_episodes WHERE series_id = $1 ORDER BY season, episode_num, id;

-- name: SetProviderVODEnabled :exec
UPDATE providers SET vod_enabled = $2, updated_at = now() WHERE id = $1;
