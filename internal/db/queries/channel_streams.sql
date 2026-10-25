-- Ordered failover lists: channel_streams.

-- name: AddStreamToChannel :exec
INSERT INTO channel_streams (channel_id, provider_stream_id, priority)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id, provider_stream_id) DO UPDATE SET priority = EXCLUDED.priority;

-- name: RemoveStreamFromChannel :exec
DELETE FROM channel_streams
WHERE channel_id = $1 AND provider_stream_id = $2;

-- name: ListChannelLinks :many
-- Every channel's streams, with both names, for matching checks.
SELECT cs.channel_id, c.name AS channel_name, c.tvg_id AS channel_tvg_id,
       ps.id AS provider_stream_id, ps.name AS stream_name, ps.tvg_id AS stream_tvg_id
FROM channel_streams cs
JOIN channels c ON c.id = cs.channel_id
JOIN provider_streams ps ON ps.id = cs.provider_stream_id
ORDER BY cs.channel_id, cs.priority;

-- name: ReorderChannelStreams :exec
UPDATE channel_streams AS cs SET priority = s.ord
FROM unnest(sqlc.arg('stream_ids')::bigint[]) WITH ORDINALITY AS s(stream_id, ord)
WHERE cs.channel_id = sqlc.arg('channel_id') AND cs.provider_stream_id = s.stream_id;

-- name: SortChannelStreamsByProvider :execrows
-- Stable re-rank of failover lists by provider priority: ties keep their
-- current order. Scope: channel_ids, or channels holding a stream from
-- provider_ids, or every channel when both are null.
WITH ranked AS (
    SELECT cs.channel_id, cs.provider_stream_id,
           (ROW_NUMBER() OVER (PARTITION BY cs.channel_id
                               ORDER BY pr.priority, cs.priority, ps.id))::int AS rn
    FROM channel_streams cs
    JOIN provider_streams ps ON ps.id = cs.provider_stream_id
    JOIN providers pr ON pr.id = ps.provider_id
    WHERE (sqlc.narg('channel_ids')::bigint[] IS NULL OR cs.channel_id = ANY(sqlc.narg('channel_ids')::bigint[]))
      AND (sqlc.narg('provider_ids')::bigint[] IS NULL OR cs.channel_id IN (
            SELECT cs2.channel_id FROM channel_streams cs2
            JOIN provider_streams ps2 ON ps2.id = cs2.provider_stream_id
            WHERE ps2.provider_id = ANY(sqlc.narg('provider_ids')::bigint[])))
)
UPDATE channel_streams AS cs SET priority = ranked.rn
FROM ranked
WHERE cs.channel_id = ranked.channel_id AND cs.provider_stream_id = ranked.provider_stream_id
  AND cs.priority <> ranked.rn;

-- name: ListChannelStreams :many
SELECT
    cs.channel_id, cs.priority, cs.added_at,
    ps.id AS provider_stream_id, ps.name AS stream_name, ps.tvg_id AS stream_tvg_id,
    ps.logo_url AS stream_logo_url, ps.group_title, ps.stream_url,
    ps.health_score, ps.cooldown_until, ps.disabled,
    pr.id AS provider_id, pr.name AS provider_name, pr.priority AS provider_priority
FROM channel_streams cs
JOIN provider_streams ps ON ps.id = cs.provider_stream_id
JOIN providers pr ON pr.id = ps.provider_id
WHERE cs.channel_id = $1
ORDER BY cs.priority, pr.priority, ps.name;

-- Bouquets.

-- name: CreateBouquet :one
INSERT INTO bouquets (name, sort_order) VALUES ($1, $2) RETURNING *;

-- name: ListBouquets :many
SELECT * FROM bouquets ORDER BY sort_order, name;

-- name: GetBouquet :one
SELECT * FROM bouquets WHERE id = $1;

-- name: UpdateBouquet :one
UPDATE bouquets SET name = $2, sort_order = $3 WHERE id = $1 RETURNING *;

-- name: DeleteBouquet :exec
DELETE FROM bouquets WHERE id = $1;

-- name: AddChannelToBouquet :exec
INSERT INTO bouquet_channels (bouquet_id, channel_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: RemoveChannelFromBouquet :exec
DELETE FROM bouquet_channels WHERE bouquet_id = $1 AND channel_id = $2;

-- name: ListBouquetChannels :many
SELECT c.* FROM bouquet_channels bc
JOIN channels c ON c.id = bc.channel_id
WHERE bc.bouquet_id = $1
ORDER BY c.number NULLS LAST, c.name;

-- name: ListChannelCandidates :many
-- The proxy's failover list: live streams of enabled providers, preferred first.
-- The channel's stream profile (if any) rides along on every row.
SELECT ps.id AS stream_id, ps.stream_url, ps.health_score,
       pr.id AS provider_id, pr.max_connections, pr.user_agent, pr.backbone,
       COALESCE(sp.mode, 'passthrough')::text AS profile_mode,
       COALESCE(sp.ffmpeg_args, '')::text AS profile_ffmpeg_args,
       COALESCE(sp.user_agent, '')::text AS profile_user_agent
FROM channel_streams cs
JOIN provider_streams ps ON ps.id = cs.provider_stream_id
JOIN providers pr ON pr.id = ps.provider_id
JOIN channels c ON c.id = cs.channel_id
LEFT JOIN stream_profiles sp ON sp.id = c.stream_profile_id
WHERE cs.channel_id = $1
  AND pr.enabled
  AND NOT ps.disabled
ORDER BY cs.priority, pr.priority, ps.id;

-- name: ChannelStreamCounts :many
-- Failover-list length per channel, for the admin list.
SELECT channel_id, count(*) AS streams FROM channel_streams GROUP BY channel_id;

-- name: ChannelBouquetCounts :many
-- Bouquets per channel, for the admin list.
SELECT channel_id, count(*) AS bouquets FROM bouquet_channels GROUP BY channel_id;

-- name: SearchStreamsAllProviders :many
-- Admin stream picker: live streams of every provider matching a name, in
-- the groups selected for their provider, optionally on one backbone.
SELECT ps.id, ps.name, ps.tvg_id, ps.group_title, ps.logo_url, ps.health_score,
       pr.id AS provider_id, pr.name AS provider_name, pr.backbone
FROM provider_streams ps
JOIN providers pr ON pr.id = ps.provider_id
WHERE NOT ps.disabled AND ps.name ILIKE sqlc.arg('pattern')
  AND (sqlc.narg('backbone')::text IS NULL OR pr.backbone = sqlc.narg('backbone'))
  AND (NOT pr.group_filter OR EXISTS (SELECT 1 FROM provider_groups pg
       WHERE pg.provider_id = ps.provider_id AND pg.group_title = ps.group_title))
ORDER BY ps.name, pr.priority, ps.id
LIMIT sqlc.arg('limit');

-- name: GetStreamsByIDs :many
SELECT ps.id, ps.name, ps.tvg_id, ps.group_title, pr.id AS provider_id, pr.name AS provider_name
FROM provider_streams ps
JOIN providers pr ON pr.id = ps.provider_id
WHERE ps.id = ANY(sqlc.arg('ids')::bigint[]);
