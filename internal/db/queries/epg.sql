-- EPG source bookkeeping and programme storage. Programmes are replaced
-- wholesale per source on every refresh (guides are large but stateless).

-- name: UpsertEpgSource :one
INSERT INTO epg_sources (name, url, provider_id)
VALUES ($1, $2, $3)
ON CONFLICT (provider_id) WHERE provider_id IS NOT NULL DO UPDATE SET
    name = EXCLUDED.name,
    url  = EXCLUDED.url
RETURNING *;

-- name: MarkEpgSourceRefreshed :exec
UPDATE epg_sources SET last_refreshed_at = now() WHERE id = $1;

-- name: ClearEpgSourceProgrammes :execrows
DELETE FROM epg_programmes WHERE epg_source_id = $1;

-- name: InsertEpgProgrammes :copyfrom
INSERT INTO epg_programmes (
    epg_source_id, channel_tvg_id, title, sub_title, description, start_time, stop_time
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountEpgProgrammes :one
SELECT count(*) FROM epg_programmes WHERE epg_source_id = $1;

-- name: GetEpgSourceByProvider :one
SELECT * FROM epg_sources WHERE provider_id = $1;

-- name: ListGuideProgrammes :many
-- Programmes overlapping [since, until) for the given lower-cased tvg-ids.
-- Each tvg-id takes its whole schedule from one source: the one with the
-- best provider priority that has programmes in the window, so overlapping
-- guides from different providers never interleave.
WITH best AS (
    SELECT DISTINCT ON (lower(p.channel_tvg_id))
           lower(p.channel_tvg_id) AS tvg_key, p.epg_source_id
    FROM epg_programmes p
    JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
    LEFT JOIN providers pr ON pr.id = s.provider_id
    WHERE lower(p.channel_tvg_id) = ANY(sqlc.arg('tvg_keys')::text[])
      AND p.stop_time > sqlc.arg('since') AND p.start_time < sqlc.arg('until')
    ORDER BY lower(p.channel_tvg_id), COALESCE(pr.priority, 2147483647), s.id
)
SELECT b.tvg_key::text AS tvg_key, p.id, p.title, p.sub_title, p.description, p.start_time, p.stop_time
FROM best b
JOIN epg_programmes p
  ON p.epg_source_id = b.epg_source_id AND lower(p.channel_tvg_id) = b.tvg_key
WHERE p.stop_time > sqlc.arg('since') AND p.start_time < sqlc.arg('until')
ORDER BY b.tvg_key, p.start_time;

-- name: ListEpgSourceStats :many
-- Admin overview of guide sources.
SELECT s.id, s.name, s.url, s.enabled, s.last_refreshed_at,
       pr.id AS provider_id, COALESCE(pr.name, '')::text AS provider_name,
       (SELECT count(*) FROM epg_programmes p WHERE p.epg_source_id = s.id) AS programmes,
       (SELECT count(DISTINCT lower(p.channel_tvg_id)) FROM epg_programmes p WHERE p.epg_source_id = s.id) AS channels
FROM epg_sources s
LEFT JOIN providers pr ON pr.id = s.provider_id
ORDER BY pr.priority NULLS LAST, s.name;

-- name: ChannelGuideStatus :many
-- Per-channel guide coverage: programmes in the next 24h from enabled
-- sources and what is on now, matched by tvg-id case-insensitively.
-- missing_only keeps channels without upcoming programmes.
SELECT * FROM (
    SELECT c.id, c.number, c.name, c.tvg_id, c.enabled,
           (SELECT count(*) FROM epg_programmes p
              JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
             WHERE c.tvg_id <> '' AND lower(p.channel_tvg_id) = lower(c.tvg_id)
               AND p.stop_time > now() AND p.start_time < now() + interval '24 hours') AS upcoming,
           COALESCE((SELECT p.title FROM epg_programmes p
              JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
             WHERE c.tvg_id <> '' AND lower(p.channel_tvg_id) = lower(c.tvg_id)
               AND p.start_time <= now() AND p.stop_time > now()
             ORDER BY p.start_time DESC LIMIT 1), '')::text AS now_title
    FROM channels c
    WHERE (sqlc.narg('search')::text IS NULL OR c.name ILIKE sqlc.narg('search'))
) g
WHERE NOT sqlc.arg('missing_only')::bool OR g.upcoming = 0
ORDER BY g.number NULLS LAST, g.name
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');
