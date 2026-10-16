-- Ordered failover lists: channel_streams.

-- name: AddStreamToChannel :exec
INSERT INTO channel_streams (channel_id, provider_stream_id, priority)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id, provider_stream_id) DO UPDATE SET priority = EXCLUDED.priority;

-- name: RemoveStreamFromChannel :exec
DELETE FROM channel_streams
WHERE channel_id = $1 AND provider_stream_id = $2;

-- name: ReorderChannelStreams :exec
UPDATE channel_streams AS cs SET priority = s.ord
FROM unnest(sqlc.arg('stream_ids')::bigint[]) WITH ORDINALITY AS s(stream_id, ord)
WHERE cs.channel_id = sqlc.arg('channel_id') AND cs.provider_stream_id = s.stream_id;

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
       pr.id AS provider_id, pr.max_connections, pr.user_agent,
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

-- name: SearchStreamsAllProviders :many
-- Admin stream picker: live streams of every provider matching a name, in
-- the groups selected for their provider.
SELECT ps.id, ps.name, ps.tvg_id, ps.group_title, ps.logo_url, ps.health_score,
       pr.id AS provider_id, pr.name AS provider_name
FROM provider_streams ps
JOIN providers pr ON pr.id = ps.provider_id
WHERE NOT ps.disabled AND ps.name ILIKE sqlc.arg('pattern')
  AND (NOT pr.group_filter OR EXISTS (SELECT 1 FROM provider_groups pg
       WHERE pg.provider_id = ps.provider_id AND pg.group_title = ps.group_title))
ORDER BY ps.name, pr.priority, ps.id
LIMIT sqlc.arg('limit');

-- name: GetStreamsByIDs :many
SELECT ps.id, ps.name, ps.tvg_id, ps.group_title, pr.id AS provider_id, pr.name AS provider_name
FROM provider_streams ps
JOIN providers pr ON pr.id = ps.provider_id
WHERE ps.id = ANY(sqlc.arg('ids')::bigint[]);
