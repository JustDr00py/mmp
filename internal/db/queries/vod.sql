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

-- Category selection (vod_category_filter).

-- name: SetProviderVODCategoryFilter :exec
UPDATE providers SET vod_category_filter = $2, updated_at = now() WHERE id = $1;

-- name: SetVODCategorySelection :exec
-- Selects exactly the listed categories of the provider.
UPDATE vod_categories SET selected = (id = ANY(sqlc.arg('ids')::bigint[]))
WHERE provider_id = sqlc.arg('provider_id')
  AND selected IS DISTINCT FROM (id = ANY(sqlc.arg('ids')::bigint[]));

-- name: ListSelectedVODCategories :many
-- The panel ids of the provider's selected categories, for the import.
SELECT kind, external_id FROM vod_categories WHERE provider_id = $1 AND selected;

-- name: DisableUnselectedVODItems :execrows
-- Applies a selection at once: movies outside the selected categories go.
UPDATE vod_items i SET disabled = TRUE
WHERE i.provider_id = $1 AND NOT i.disabled
  AND NOT EXISTS (SELECT 1 FROM vod_categories c WHERE c.provider_id = i.provider_id AND c.kind = 'movie'
                  AND c.external_id = i.category_external_id AND c.selected);

-- name: DisableUnselectedSeries :execrows
UPDATE series s SET disabled = TRUE
WHERE s.provider_id = $1 AND NOT s.disabled
  AND NOT EXISTS (SELECT 1 FROM vod_categories c WHERE c.provider_id = s.provider_id AND c.kind = 'series'
                  AND c.external_id = s.category_external_id AND c.selected);

-- Pooling (vod_pooled).

-- name: SetProviderVODPooled :exec
UPDATE providers SET vod_pooled = $2, updated_at = now() WHERE id = $1;

-- name: VODPoolOverlap :many
-- The other providers on a backbone, with how many of this provider's
-- active movies they hold under the same id and name: near all of them
-- means one panel, so pooling is safe.
SELECT pr.id, pr.name, pr.vod_pooled, pr.vod_enabled,
       (SELECT count(*) FROM vod_items a
        WHERE a.provider_id = sqlc.arg('provider_id') AND NOT a.disabled)::bigint AS movies,
       (SELECT count(*) FROM vod_items a
        JOIN vod_items b ON b.provider_id = pr.id AND b.external_id = a.external_id AND b.name = a.name
        WHERE a.provider_id = sqlc.arg('provider_id') AND NOT a.disabled)::bigint AS matching,
       (SELECT count(*) FROM vod_items b WHERE b.provider_id = pr.id)::bigint AS their_movies
FROM providers pr
WHERE pr.backbone = sqlc.arg('backbone')::text AND pr.backbone <> '' AND pr.id <> sqlc.arg('provider_id')
ORDER BY pr.priority, pr.name;

-- name: SetProviderVODEnabled :exec
UPDATE providers SET vod_enabled = $2, updated_at = now() WHERE id = $1;

-- Admin browsing.

-- name: ListVODCategoriesAdmin :many
-- Categories of one kind with their active item counts; provider_id NULL = all.
-- offered: not hidden by the provider's category selection.
SELECT c.id, c.provider_id, pr.name AS provider_name, c.kind, c.external_id, c.name, c.disabled, c.selected,
       (NOT pr.vod_category_filter OR c.selected)::boolean AS offered,
       (CASE WHEN c.kind = 'movie'
             THEN (SELECT count(*) FROM vod_items i WHERE i.provider_id = c.provider_id AND i.category_external_id = c.external_id AND NOT i.disabled)
             ELSE (SELECT count(*) FROM series s WHERE s.provider_id = c.provider_id AND s.category_external_id = c.external_id AND NOT s.disabled)
        END)::bigint AS items
FROM vod_categories c
JOIN providers pr ON pr.id = c.provider_id
WHERE c.kind = sqlc.arg('kind')
  AND (sqlc.narg('provider_id')::bigint IS NULL OR c.provider_id = sqlc.narg('provider_id'))
ORDER BY pr.priority, pr.name, c.name;

-- name: ListVODItemsAdmin :many
SELECT i.id, i.provider_id, i.external_id, i.name, i.category_external_id, i.icon, i.rating,
       i.container_extension, i.added_at, i.disabled
FROM vod_items i
WHERE (sqlc.narg('provider_id')::bigint IS NULL OR i.provider_id = sqlc.narg('provider_id'))
  AND (sqlc.narg('category_ids')::bigint[] IS NULL OR EXISTS (
        SELECT 1 FROM vod_categories c WHERE c.id = ANY(sqlc.narg('category_ids')::bigint[]) AND c.kind = 'movie'
          AND c.provider_id = i.provider_id AND c.external_id = i.category_external_id))
  AND (sqlc.narg('search')::text IS NULL OR i.name ILIKE sqlc.narg('search'))
ORDER BY i.name, i.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: ListSeriesAdmin :many
SELECT s.id, s.provider_id, s.external_id, s.name, s.category_external_id, s.cover, s.rating,
       s.release_date, s.episodes_fetched_at, s.disabled
FROM series s
WHERE (sqlc.narg('provider_id')::bigint IS NULL OR s.provider_id = sqlc.narg('provider_id'))
  AND (sqlc.narg('category_ids')::bigint[] IS NULL OR EXISTS (
        SELECT 1 FROM vod_categories c WHERE c.id = ANY(sqlc.narg('category_ids')::bigint[]) AND c.kind = 'series'
          AND c.provider_id = s.provider_id AND c.external_id = s.category_external_id))
  AND (sqlc.narg('search')::text IS NULL OR s.name ILIKE sqlc.narg('search'))
ORDER BY s.name, s.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- Bouquet membership.

-- name: AddVODCategoriesToBouquet :execrows
INSERT INTO bouquet_vod_categories (bouquet_id, vod_category_id)
SELECT sqlc.arg('bouquet_id'), unnest(sqlc.arg('category_ids')::bigint[])
ON CONFLICT DO NOTHING;

-- name: RemoveVODCategoryFromBouquet :execrows
DELETE FROM bouquet_vod_categories WHERE bouquet_id = $1 AND vod_category_id = $2;

-- name: ListBouquetVODCategories :many
SELECT c.id, c.provider_id, c.kind, c.name, c.disabled,
       (NOT pr.vod_category_filter OR c.selected)::boolean AS offered
FROM bouquet_vod_categories b
JOIN vod_categories c ON c.id = b.vod_category_id
JOIN providers pr ON pr.id = c.provider_id
WHERE b.bouquet_id = $1
ORDER BY c.kind, c.name;

-- Customer access: a user sees the offered categories granted by their
-- bouquets (admins: every offered category), and the active titles inside
-- them. Pooled providers (vod_offered_categories.pool_key) count as one:
-- a category or title they share is listed once, under its oldest row's
-- id, and a grant of any copy of a category grants them all.

-- name: ListUserVODCategories :many
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
)
SELECT min(v.id)::bigint AS id, v.kind, (array_agg(v.name ORDER BY v.id))[1]::text AS name
FROM vod_offered_categories v
LEFT JOIN granted g ON g.id = v.id
WHERE v.kind = sqlc.arg('kind')
GROUP BY v.pool_key, v.kind, v.external_id
HAVING sqlc.arg('is_admin')::bool OR bool_or(g.id IS NOT NULL)
ORDER BY min(v.provider_priority), 3, 1;

-- name: ListUserVODItems :many
-- category_id NULL = every visible category; any copy's id selects it.
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
), cats AS (
    SELECT v.pool_key, v.external_id, min(v.id)::bigint AS id, array_agg(v.id)::bigint[] AS ids
    FROM vod_offered_categories v LEFT JOIN granted g ON g.id = v.id
    WHERE v.kind = 'movie'
    GROUP BY v.pool_key, v.external_id
    HAVING sqlc.arg('is_admin')::bool OR bool_or(g.id IS NOT NULL)
), titles AS (
    SELECT DISTINCT ON (v.pool_key, i.external_id)
           i.id, i.name, i.icon, i.rating, i.container_extension, i.tmdb_id, i.added_at, k.id AS category_id
    FROM vod_items i
    JOIN vod_offered_categories v ON v.provider_id = i.provider_id AND v.kind = 'movie' AND v.external_id = i.category_external_id
    JOIN cats k ON k.pool_key = v.pool_key AND k.external_id = v.external_id
    WHERE NOT i.disabled
      AND (sqlc.narg('category_id')::bigint IS NULL OR sqlc.narg('category_id')::bigint = ANY(k.ids))
    ORDER BY v.pool_key, i.external_id, i.id
)
SELECT t.id, t.name, t.icon, t.rating, t.container_extension, t.tmdb_id, t.added_at, t.category_id
FROM titles t ORDER BY t.name, t.id;

-- name: ListUserSeries :many
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
), cats AS (
    SELECT v.pool_key, v.external_id, min(v.id)::bigint AS id, array_agg(v.id)::bigint[] AS ids
    FROM vod_offered_categories v LEFT JOIN granted g ON g.id = v.id
    WHERE v.kind = 'series'
    GROUP BY v.pool_key, v.external_id
    HAVING sqlc.arg('is_admin')::bool OR bool_or(g.id IS NOT NULL)
), titles AS (
    SELECT DISTINCT ON (v.pool_key, s.external_id)
           s.id, s.name, s.cover, s.plot, s."cast", s.director, s.genre, s.release_date, s.rating,
           s.last_modified, k.id AS category_id
    FROM series s
    JOIN vod_offered_categories v ON v.provider_id = s.provider_id AND v.kind = 'series' AND v.external_id = s.category_external_id
    JOIN cats k ON k.pool_key = v.pool_key AND k.external_id = v.external_id
    WHERE NOT s.disabled
      AND (sqlc.narg('category_id')::bigint IS NULL OR sqlc.narg('category_id')::bigint = ANY(k.ids))
    ORDER BY v.pool_key, s.external_id, s.id
)
SELECT t.id, t.name, t.cover, t.plot, t."cast", t.director, t.genre, t.release_date, t.rating, t.last_modified, t.category_id
FROM titles t ORDER BY t.name, t.id;

-- name: ListUserVODItemCopies :many
-- The movie with this id and its copies on pooled providers, if the user
-- may see it (else no rows), in playback order (provider priority).
-- category_id is the pooled category's listed id.
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
), x AS (
    SELECT (CASE WHEN pr.vod_pooled AND pr.backbone <> '' THEN 'b:' || pr.backbone ELSE 'p:' || pr.id::text END)::text AS pool_key,
           i.external_id
    FROM vod_items i JOIN providers pr ON pr.id = i.provider_id
    WHERE i.id = sqlc.arg('id')
)
SELECT i.id, i.provider_id, i.external_id, i.name, i.container_extension, i.info, i.info_fetched_at,
       i.icon, i.rating, i.tmdb_id, i.added_at,
       (SELECT min(c.id) FROM vod_offered_categories c
        WHERE c.pool_key = v.pool_key AND c.kind = 'movie' AND c.external_id = v.external_id)::bigint AS category_id
FROM vod_items i
JOIN vod_offered_categories v ON v.provider_id = i.provider_id AND v.kind = 'movie' AND v.external_id = i.category_external_id
JOIN x ON x.pool_key = v.pool_key AND x.external_id = i.external_id
WHERE NOT i.disabled
  AND (sqlc.arg('is_admin')::bool OR EXISTS (
        SELECT 1 FROM vod_offered_categories c JOIN granted g ON g.id = c.id
        WHERE c.pool_key = v.pool_key AND c.kind = 'movie' AND c.external_id = v.external_id))
ORDER BY v.provider_priority, i.provider_id, i.id;

-- name: ListUserSeriesCopies :many
-- ListUserVODItemCopies for a series.
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
), x AS (
    SELECT (CASE WHEN pr.vod_pooled AND pr.backbone <> '' THEN 'b:' || pr.backbone ELSE 'p:' || pr.id::text END)::text AS pool_key,
           s.external_id
    FROM series s JOIN providers pr ON pr.id = s.provider_id
    WHERE s.id = sqlc.arg('id')
)
SELECT s.*,
       (SELECT min(c.id) FROM vod_offered_categories c
        WHERE c.pool_key = v.pool_key AND c.kind = 'series' AND c.external_id = v.external_id)::bigint AS category_id
FROM series s
JOIN vod_offered_categories v ON v.provider_id = s.provider_id AND v.kind = 'series' AND v.external_id = s.category_external_id
JOIN x ON x.pool_key = v.pool_key AND x.external_id = s.external_id
WHERE NOT s.disabled
  AND (sqlc.arg('is_admin')::bool OR EXISTS (
        SELECT 1 FROM vod_offered_categories c JOIN granted g ON g.id = c.id
        WHERE c.pool_key = v.pool_key AND c.kind = 'series' AND c.external_id = v.external_id))
ORDER BY v.provider_priority, s.provider_id, s.id;

-- name: ListUserEpisodeCopies :many
-- One episode, if the user may see its series, once per copy of the
-- series on pooled providers (episode ids are the panel's, so any copy's
-- account plays it), in playback order.
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
), x AS (
    SELECT (CASE WHEN pr.vod_pooled AND pr.backbone <> '' THEN 'b:' || pr.backbone ELSE 'p:' || pr.id::text END)::text AS pool_key,
           s0.external_id AS series_external_id, s0.name AS series_name,
           e.id, e.external_id, e.container_extension, e.season, e.episode_num, e.title
    FROM series_episodes e
    JOIN series s0 ON s0.id = e.series_id
    JOIN providers pr ON pr.id = s0.provider_id
    WHERE e.id = sqlc.arg('id')
)
SELECT x.id, x.external_id, x.container_extension, s.id AS series_id, s.provider_id,
       x.series_name, x.season, x.episode_num, x.title
FROM x
JOIN series s ON s.external_id = x.series_external_id AND NOT s.disabled
JOIN vod_offered_categories v ON v.provider_id = s.provider_id AND v.kind = 'series'
                             AND v.external_id = s.category_external_id AND v.pool_key = x.pool_key
WHERE sqlc.arg('is_admin')::bool OR EXISTS (
        SELECT 1 FROM vod_offered_categories c JOIN granted g ON g.id = c.id
        WHERE c.pool_key = v.pool_key AND c.kind = 'series' AND c.external_id = v.external_id)
ORDER BY v.provider_priority, s.provider_id, s.id;

-- name: GetVODItem :one
SELECT * FROM vod_items WHERE id = $1;

-- name: SetVODItemInfo :exec
UPDATE vod_items SET info = $2, info_fetched_at = now() WHERE id = $1;

-- name: ListUserCachedEpisodes :many
-- Episodes of the user's visible series whose episode lists are already
-- loaded (series load lazily; playlists do not trigger panel calls). An
-- episode of pooled series copies is listed once.
WITH granted AS (
    SELECT DISTINCT bv.vod_category_id AS id FROM bouquet_vod_categories bv
    JOIN user_bouquets ub ON ub.bouquet_id = bv.bouquet_id WHERE ub.user_id = sqlc.arg('user_id')
), cats AS (
    SELECT v.pool_key, v.external_id
    FROM vod_offered_categories v LEFT JOIN granted g ON g.id = v.id
    WHERE v.kind = 'series'
    GROUP BY v.pool_key, v.external_id
    HAVING sqlc.arg('is_admin')::bool OR bool_or(g.id IS NOT NULL)
), eps AS (
    SELECT DISTINCT ON (v.pool_key, s.external_id, e.external_id)
           e.id, e.season, e.episode_num, e.title, e.container_extension, s.id AS series_id,
           s.name AS series_name, s.cover
    FROM series_episodes e
    JOIN series s ON s.id = e.series_id AND NOT s.disabled AND s.episodes_fetched_at IS NOT NULL
    JOIN vod_offered_categories v ON v.provider_id = s.provider_id AND v.kind = 'series' AND v.external_id = s.category_external_id
    JOIN cats k ON k.pool_key = v.pool_key AND k.external_id = v.external_id
    ORDER BY v.pool_key, s.external_id, e.external_id, s.id
)
SELECT t.id, t.season, t.episode_num, t.title, t.container_extension, t.series_id, t.series_name, t.cover
FROM eps t ORDER BY t.series_name, t.series_id, t.season, t.episode_num, t.id;

-- name: ProviderVODSize :one
SELECT (SELECT count(*) FROM vod_items i WHERE i.provider_id = $1)::bigint AS movies,
       (SELECT count(*) FROM series s WHERE s.provider_id = $1)::bigint AS series,
       (SELECT count(*) FROM vod_categories c WHERE c.provider_id = $1)::bigint AS categories;

-- name: DeleteProviderVOD :one
-- Removes a provider's imported catalog in one statement. Episodes and
-- bouquet grants go with their series and categories (ON DELETE CASCADE).
WITH items AS (DELETE FROM vod_items WHERE vod_items.provider_id = $1 RETURNING 1),
     shows AS (DELETE FROM series WHERE series.provider_id = $1 RETURNING 1),
     cats AS (DELETE FROM vod_categories WHERE vod_categories.provider_id = $1 RETURNING 1),
     stamp AS (UPDATE providers SET vod_refreshed_at = NULL WHERE providers.id = $1 RETURNING 1)
SELECT (SELECT count(*) FROM items)::bigint AS movies,
       (SELECT count(*) FROM shows)::bigint AS series,
       (SELECT count(*) FROM cats)::bigint AS categories;
