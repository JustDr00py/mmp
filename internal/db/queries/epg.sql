-- EPG source bookkeeping and program storage. Programs are replaced
-- wholesale per source on every refresh (guides are large but stateless).

-- name: UpsertEpgSource :one
INSERT INTO epg_sources (name, url, provider_id)
VALUES ($1, $2, $3)
ON CONFLICT (provider_id) WHERE provider_id IS NOT NULL DO UPDATE SET
    name = EXCLUDED.name,
    url  = EXCLUDED.url
RETURNING *;

-- name: MarkEpgSourceRefreshed :exec
-- A guide just loaded, with how many programs and channels it holds.
UPDATE epg_sources
SET last_refreshed_at = now(), program_count = sqlc.arg('programs'), channel_count = sqlc.arg('channels')
WHERE id = sqlc.arg('id');

-- name: ClearEpgSourcePrograms :execrows
DELETE FROM epg_programs WHERE epg_source_id = $1;

-- name: InsertEpgPrograms :copyfrom
INSERT INTO epg_programs (
    epg_source_id, channel_tvg_id, title, sub_title, description, start_time, stop_time
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountEpgPrograms :one
SELECT count(*) FROM epg_programs WHERE epg_source_id = $1;

-- name: GetEpgSourceByProvider :one
SELECT * FROM epg_sources WHERE provider_id = $1;

-- name: ListGuidePrograms :many
-- Programs overlapping [since, until) for the given lower-cased tvg-ids.
-- Each tvg-id takes its whole schedule from one source: the one with the
-- best priority (the source's own, else its provider's) that has
-- programs in the window, so overlapping guides never interleave.
WITH best AS (
    SELECT DISTINCT ON (lower(p.channel_tvg_id))
           lower(p.channel_tvg_id) AS tvg_key, p.epg_source_id
    FROM epg_programs p
    JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
    LEFT JOIN providers pr ON pr.id = s.provider_id
    WHERE lower(p.channel_tvg_id) = ANY(sqlc.arg('tvg_keys')::text[])
      AND p.stop_time > sqlc.arg('since') AND p.start_time < sqlc.arg('until')
    ORDER BY lower(p.channel_tvg_id), COALESCE(s.priority, pr.priority, 2147483647), s.id
)
SELECT b.tvg_key::text AS tvg_key, p.id, p.title, p.sub_title, p.description, p.start_time, p.stop_time
FROM best b
JOIN epg_programs p
  ON p.epg_source_id = b.epg_source_id AND lower(p.channel_tvg_id) = b.tvg_key
WHERE p.stop_time > sqlc.arg('since') AND p.start_time < sqlc.arg('until')
ORDER BY b.tvg_key, p.start_time;

-- name: ListEpgSourceStats :many
-- Admin overview of guide sources.
SELECT s.id, s.name, s.url, s.enabled, s.last_refreshed_at, s.priority,
       pr.id AS provider_id, COALESCE(pr.name, '')::text AS provider_name,
       s.program_count AS programs, s.channel_count AS channels
FROM epg_sources s
LEFT JOIN providers pr ON pr.id = s.provider_id
ORDER BY COALESCE(s.priority, pr.priority) NULLS LAST, s.name;

-- name: ChannelGuideStatus :many
-- Per-channel guide coverage, as customers see it: the highest-priority
-- enabled source with programs for the channel's tvg-id (case-insensitive)
-- in the next 24h, its program count there and what is on now.
-- missing_only keeps channels without upcoming programs.
SELECT * FROM (
    SELECT c.id, c.number, c.name, c.tvg_id, c.enabled,
           COALESCE(b.name, '')::text AS source_name,
           COALESCE((SELECT ps.name FROM channel_streams cs JOIN provider_streams ps ON ps.id = cs.provider_stream_id
                      WHERE cs.channel_id = c.id ORDER BY cs.priority, ps.id LIMIT 1), '')::text AS stream_name,
           (SELECT count(*) FROM epg_programs p
             WHERE p.epg_source_id = b.id AND lower(p.channel_tvg_id) = lower(c.tvg_id)
               AND p.stop_time > now() AND p.start_time < now() + interval '24 hours') AS upcoming,
           COALESCE((SELECT p.title FROM epg_programs p
             WHERE p.epg_source_id = b.id AND lower(p.channel_tvg_id) = lower(c.tvg_id)
               AND p.start_time <= now() AND p.stop_time > now()
             ORDER BY p.start_time DESC LIMIT 1), '')::text AS now_title
    FROM channels c
    LEFT JOIN LATERAL (
        SELECT s.id, s.name FROM epg_programs p
        JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
        LEFT JOIN providers pr ON pr.id = s.provider_id
        WHERE c.tvg_id <> '' AND lower(p.channel_tvg_id) = lower(c.tvg_id)
          AND p.stop_time > now() AND p.start_time < now() + interval '24 hours'
        ORDER BY COALESCE(s.priority, pr.priority, 2147483647), s.id
        LIMIT 1
    ) b ON true
    WHERE (sqlc.narg('search')::text IS NULL OR c.name ILIKE sqlc.narg('search'))
) g
WHERE NOT sqlc.arg('missing_only')::bool OR g.upcoming = 0
ORDER BY g.number NULLS LAST, g.name
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: SetProviderGuidePriority :execrows
-- A provider guide's own priority; NULL uses the provider's.
UPDATE epg_sources SET priority = sqlc.narg('priority') WHERE provider_id = sqlc.arg('provider_id');

-- Extra guide sources: XMLTV URLs not tied to a provider (e.g. a
-- community guide), managed on the Guide page.

-- name: GetEpgSource :one
SELECT * FROM epg_sources WHERE id = $1;

-- name: CreateExtraEpgSource :one
INSERT INTO epg_sources (name, url, priority, enabled) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: UpdateExtraEpgSource :one
UPDATE epg_sources SET name = $2, url = $3, priority = $4, enabled = $5
WHERE id = $1 AND provider_id IS NULL
RETURNING *;

-- name: DeleteExtraEpgSource :execrows
DELETE FROM epg_sources WHERE id = $1 AND provider_id IS NULL;

-- name: ListExtraEpgSourcesDue :many
SELECT * FROM epg_sources
WHERE provider_id IS NULL AND enabled
  AND (last_refreshed_at IS NULL OR last_refreshed_at <= now() - make_interval(secs => $1::double precision))
ORDER BY priority NULLS LAST, id;
