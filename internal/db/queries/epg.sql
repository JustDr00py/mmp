-- EPG source bookkeeping and program storage. Programs are replaced
-- wholesale per source on every refresh (guides are large but stateless).

-- name: UpsertEpgSource :one
-- No row when the provider's guide is off (or the provider is gone).
INSERT INTO epg_sources (name, url, provider_id)
SELECT sqlc.arg('name'), sqlc.arg('url'), p.id FROM providers p WHERE p.id = sqlc.arg('provider_id') AND p.epg_enabled
ON CONFLICT (provider_id) WHERE provider_id IS NOT NULL DO UPDATE SET
    name = EXCLUDED.name,
    url  = EXCLUDED.url
RETURNING *;

-- name: MarkEpgSourceRefreshed :exec
-- A guide just loaded, with how many programs and channels it holds.
UPDATE epg_sources
SET last_refreshed_at = now(), program_count = sqlc.arg('programs'), channel_count = sqlc.arg('channels')
WHERE id = sqlc.arg('id');

-- name: TryLockEpgSource :one
-- Holds a guide source until the transaction ends, so two downloads of
-- one guide never replace its programs at once; false while another holds it.
SELECT pg_try_advisory_xact_lock(hashtext('epg_source'), sqlc.arg(id)::int4)::bool AS locked;

-- name: ClearEpgSourcePrograms :execrows
DELETE FROM epg_programs WHERE epg_source_id = $1;

-- name: ClearEpgSourceChannels :exec
DELETE FROM epg_channels WHERE epg_source_id = $1;

-- name: InsertEpgChannels :copyfrom
INSERT INTO epg_channels (epg_source_id, tvg_id, display_names, program_count) VALUES ($1, $2, $3, $4);

-- name: SearchGuideChannels :many
-- The channels enabled guides list, for finding a channel's guide id:
-- every word of the search must appear in the id or a display name
-- (words are LIKE patterns), optionally within one guide, only ids with
-- programs, or only ids no channel uses yet. Exact id matches first, then
-- those with programs. used_by counts the lineup's channels already on the id.
SELECT e.tvg_id, e.display_names, e.program_count, s.id AS source_id, s.name AS source_name,
       (SELECT count(*) FROM channels c WHERE c.tvg_id <> '' AND lower(c.tvg_id) = lower(e.tvg_id)) AS used_by
FROM epg_channels e
JOIN epg_sources s ON s.id = e.epg_source_id AND s.enabled
LEFT JOIN providers pr ON pr.id = s.provider_id
WHERE NOT EXISTS (
    SELECT 1 FROM unnest(sqlc.arg('words')::text[]) w
    WHERE NOT (e.tvg_id ILIKE w OR array_to_string(e.display_names, ' ') ILIKE w)
)
  AND (sqlc.narg('source_id')::bigint IS NULL OR s.id = sqlc.narg('source_id'))
  AND (NOT sqlc.arg('with_programs')::bool OR e.program_count > 0)
  AND (NOT sqlc.arg('unused')::bool OR NOT EXISTS (
    SELECT 1 FROM channels c WHERE c.tvg_id <> '' AND lower(c.tvg_id) = lower(e.tvg_id)))
ORDER BY lower(e.tvg_id) = lower(sqlc.arg('exact')::text) DESC, e.program_count > 0 DESC,
         lower(e.tvg_id), COALESCE(s.priority, pr.priority, 2147483647), s.id
LIMIT sqlc.arg('limit');

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
-- Each tvg-id takes its whole schedule from one source, so overlapping
-- guides never interleave: of the sources with programs in the window,
-- one whose schedule runs past now (else, one that has gone stale), by
-- best priority (the source's own, else its provider's). A guide that
-- stopped updating so never hides a current one behind its priority.
WITH cand AS (
    SELECT lower(p.channel_tvg_id) AS tvg_key, p.epg_source_id,
           max(p.stop_time) > sqlc.arg('now')::timestamptz AS current,
           COALESCE(s.priority, pr.priority, 2147483647) AS prio
    FROM epg_programs p
    JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
    LEFT JOIN providers pr ON pr.id = s.provider_id
    WHERE lower(p.channel_tvg_id) = ANY(sqlc.arg('tvg_keys')::text[])
      AND p.stop_time > sqlc.arg('since') AND p.start_time < sqlc.arg('until')
    GROUP BY 1, 2, 4
), best AS (
    SELECT DISTINCT ON (tvg_key) tvg_key, epg_source_id
    FROM cand
    ORDER BY tvg_key, current DESC, prio, epg_source_id
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
-- missing_only keeps channels without upcoming programs. guide_mode is
-- the channel's (else its group's) guide setting.
SELECT * FROM (
    SELECT c.id, c.number, c.name, c.tvg_id, c.enabled,
           COALESCE(NULLIF(c.guide_mode, ''), cg.guide_mode, 'auto')::text AS guide_mode,
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
    LEFT JOIN channel_groups cg ON cg.id = c.group_id
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

-- name: SetProviderGuideEnabled :exec
-- Turns a provider's guide on or off. Off also deletes its downloaded
-- guide (programs and channel list go with it).
WITH p AS (
    UPDATE providers SET epg_enabled = sqlc.arg('enabled'), updated_at = now()
    WHERE providers.id = sqlc.arg('id')
    RETURNING providers.id, providers.epg_enabled
)
DELETE FROM epg_sources s USING p WHERE s.provider_id = p.id AND NOT p.epg_enabled;

-- name: ListGuideSourceNames :many
-- Enabled guides with a downloaded channel list, for the guide channel
-- search's guide filter.
SELECT s.id, s.name FROM epg_sources s
WHERE s.enabled AND EXISTS (SELECT 1 FROM epg_channels e WHERE e.epg_source_id = s.id)
ORDER BY lower(s.name), s.id;
