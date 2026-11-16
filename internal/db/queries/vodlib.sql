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

-- name: ListVODItemsToParse :many
-- A page of movies with their category names, to parse again.
SELECT i.id, i.name, COALESCE(c.name, '')::text AS category
FROM vod_items i
LEFT JOIN vod_categories c ON c.provider_id = i.provider_id AND c.kind = 'movie' AND c.external_id = i.category_external_id
WHERE i.id > sqlc.arg('after_id') ORDER BY i.id LIMIT sqlc.arg('limit');

-- name: ListSeriesToParse :many
SELECT s.id, s.name, s.release_date, COALESCE(c.name, '')::text AS category
FROM series s
LEFT JOIN vod_categories c ON c.provider_id = s.provider_id AND c.kind = 'series' AND c.external_id = s.category_external_id
WHERE s.id > sqlc.arg('after_id') ORDER BY s.id LIMIT sqlc.arg('limit');

-- name: UpdateVODItemsParsed :exec
-- Stores titles parsed again; a changed title or year is worth a new
-- TMDB search.
UPDATE vod_items i SET title = u.title, title_norm = u.norm, year = u.year, quality = u.quality, lang = u.lang,
    match_tried_at = CASE WHEN i.title_norm IS DISTINCT FROM u.norm OR i.year IS DISTINCT FROM u.year THEN NULL ELSE i.match_tried_at END
FROM (SELECT unnest(sqlc.arg('ids')::bigint[]) AS id, unnest(sqlc.arg('titles')::text[]) AS title, unnest(sqlc.arg('title_norms')::text[]) AS norm,
             unnest(sqlc.arg('years')::int[]) AS year, unnest(sqlc.arg('qualities')::text[]) AS quality, unnest(sqlc.arg('langs')::text[]) AS lang) AS u
WHERE i.id = u.id;

-- name: UpdateSeriesParsed :exec
UPDATE series s SET title = u.title, title_norm = u.norm, year = u.year, quality = u.quality, lang = u.lang,
    match_tried_at = CASE WHEN s.title_norm IS DISTINCT FROM u.norm OR s.year IS DISTINCT FROM u.year THEN NULL ELSE s.match_tried_at END
FROM (SELECT unnest(sqlc.arg('ids')::bigint[]) AS id, unnest(sqlc.arg('titles')::text[]) AS title, unnest(sqlc.arg('title_norms')::text[]) AS norm,
             unnest(sqlc.arg('years')::int[]) AS year, unnest(sqlc.arg('qualities')::text[]) AS quality, unnest(sqlc.arg('langs')::text[]) AS lang) AS u
WHERE s.id = u.id;

-- name: ForgetTMDBSearches :exec
-- After the matching rules change: every cached search goes; titles TMDB
-- had no match for are searched again, and titles a search matched are
-- checked again (see ListRecheckMovieTitles), keeping their id meanwhile.
WITH searches AS (DELETE FROM tmdb_search RETURNING 1),
     items AS (UPDATE vod_items SET match_tried_at = NULL
               WHERE (tmdb_id = '' OR match_source = 'tmdb') AND match_tried_at IS NOT NULL RETURNING 1)
UPDATE series SET match_tried_at = NULL
WHERE (tmdb_id = '' OR match_source = 'tmdb') AND match_tried_at IS NOT NULL;

-- name: ListRecheckMovieTitles :many
-- Titles a TMDB search matched under older rules, to search again.
SELECT title_norm, year, min(title)::text AS title, min(tmdb_id)::text AS tmdb_id, count(*)::bigint AS copies
FROM vod_items
WHERE match_source = 'tmdb' AND match_tried_at IS NULL AND NOT match_locked AND NOT disabled AND title_norm <> ''
GROUP BY title_norm, year
ORDER BY count(*) DESC, title_norm
LIMIT sqlc.arg('limit');

-- name: ListRecheckSeriesTitles :many
SELECT title_norm, year, min(title)::text AS title, min(tmdb_id)::text AS tmdb_id, count(*)::bigint AS copies
FROM series
WHERE match_source = 'tmdb' AND match_tried_at IS NULL AND NOT match_locked AND NOT disabled AND title_norm <> ''
GROUP BY title_norm, year
ORDER BY count(*) DESC, title_norm
LIMIT sqlc.arg('limit');

-- name: RecheckMovieTitle :execrows
-- Stores a checked-again title's id: a new one when tmdb_id is given,
-- else the one it had.
UPDATE vod_items SET tmdb_id = COALESCE(NULLIF(sqlc.arg('tmdb_id')::text, ''), tmdb_id),
    imdb_id = CASE WHEN sqlc.arg('tmdb_id')::text = '' THEN imdb_id ELSE sqlc.arg('imdb_id')::text END,
    match_tried_at = now()
WHERE title_norm = sqlc.arg('title_norm') AND year = sqlc.arg('year') AND match_source = 'tmdb' AND NOT match_locked;

-- name: RecheckSeriesTitle :execrows
UPDATE series SET tmdb_id = COALESCE(NULLIF(sqlc.arg('tmdb_id')::text, ''), tmdb_id),
    imdb_id = CASE WHEN sqlc.arg('tmdb_id')::text = '' THEN imdb_id ELSE sqlc.arg('imdb_id')::text END,
    match_tried_at = now()
WHERE title_norm = sqlc.arg('title_norm') AND year = sqlc.arg('year') AND match_source = 'tmdb' AND NOT match_locked;
