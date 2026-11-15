-- VOD library: TMDB matching of titles panels gave no id. Titles are
-- matched as groups (one normalised title and year, on any provider).

-- name: ListUnmatchedMovieTitles :many
-- Titles without a TMDB id, not locked, not tried within retry_after;
-- the most copied first.
SELECT title_norm, year, min(title)::text AS title, count(*)::bigint AS copies
FROM vod_items
WHERE tmdb_id = '' AND NOT match_locked AND NOT disabled AND title_norm <> ''
  AND (match_tried_at IS NULL OR match_tried_at < now() - sqlc.arg('retry_after_secs')::bigint * interval '1 second')
GROUP BY title_norm, year
ORDER BY count(*) DESC, title_norm
LIMIT sqlc.arg('limit');

-- name: ListUnmatchedSeriesTitles :many
SELECT title_norm, year, min(title)::text AS title, count(*)::bigint AS copies
FROM series
WHERE tmdb_id = '' AND NOT match_locked AND NOT disabled AND title_norm <> ''
  AND (match_tried_at IS NULL OR match_tried_at < now() - sqlc.arg('retry_after_secs')::bigint * interval '1 second')
GROUP BY title_norm, year
ORDER BY count(*) DESC, title_norm
LIMIT sqlc.arg('limit');

-- name: SetMovieTitleMatch :execrows
UPDATE vod_items SET tmdb_id = sqlc.arg('tmdb_id'), imdb_id = sqlc.arg('imdb_id'), match_source = 'tmdb', match_tried_at = now()
WHERE title_norm = sqlc.arg('title_norm') AND year = sqlc.arg('year') AND tmdb_id = '' AND NOT match_locked;

-- name: SetSeriesTitleMatch :execrows
UPDATE series SET tmdb_id = sqlc.arg('tmdb_id'), imdb_id = sqlc.arg('imdb_id'), match_source = 'tmdb', match_tried_at = now()
WHERE title_norm = sqlc.arg('title_norm') AND year = sqlc.arg('year') AND tmdb_id = '' AND NOT match_locked;

-- name: MarkMovieTitleTried :exec
UPDATE vod_items SET match_tried_at = now()
WHERE title_norm = sqlc.arg('title_norm') AND year = sqlc.arg('year') AND tmdb_id = '' AND NOT match_locked;

-- name: MarkSeriesTitleTried :exec
UPDATE series SET match_tried_at = now()
WHERE title_norm = sqlc.arg('title_norm') AND year = sqlc.arg('year') AND tmdb_id = '' AND NOT match_locked;

-- name: GetTMDBSearch :one
-- A cached search no older than max_age.
SELECT tmdb_id FROM tmdb_search
WHERE kind = sqlc.arg('kind') AND query_key = sqlc.arg('query_key') AND year = sqlc.arg('year')
  AND searched_at > now() - sqlc.arg('max_age_secs')::bigint * interval '1 second';

-- name: PutTMDBSearch :exec
INSERT INTO tmdb_search (kind, query_key, year, tmdb_id) VALUES ($1, $2, $3, $4)
ON CONFLICT (kind, query_key, year) DO UPDATE SET tmdb_id = EXCLUDED.tmdb_id, searched_at = now();

-- name: GetTMDBCache :one
SELECT * FROM tmdb_cache WHERE kind = $1 AND tmdb_id = $2;

-- name: PutTMDBCache :exec
INSERT INTO tmdb_cache (kind, tmdb_id, imdb_id, data) VALUES ($1, $2, $3, $4)
ON CONFLICT (kind, tmdb_id) DO UPDATE SET imdb_id = EXCLUDED.imdb_id, data = EXCLUDED.data, fetched_at = now();

-- name: VODMatchCounts :one
-- Active titles by how their TMDB id is known, for the Settings page.
SELECT (SELECT count(*) FROM vod_items WHERE NOT disabled AND tmdb_id <> '')::bigint AS movies_matched,
       (SELECT count(*) FROM vod_items WHERE NOT disabled AND tmdb_id = '')::bigint AS movies_unmatched,
       (SELECT count(*) FROM series WHERE NOT disabled AND tmdb_id <> '')::bigint AS series_matched,
       (SELECT count(*) FROM series WHERE NOT disabled AND tmdb_id = '')::bigint AS series_unmatched;
