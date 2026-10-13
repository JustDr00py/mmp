-- Channel groups (customer-facing categories).

-- name: CreateChannelGroup :one
INSERT INTO channel_groups (name, sort_order) VALUES ($1, $2) RETURNING *;

-- name: ListChannelGroups :many
SELECT * FROM channel_groups ORDER BY sort_order, name;

-- name: UpdateChannelGroup :one
UPDATE channel_groups SET name = $2, sort_order = $3 WHERE id = $1 RETURNING *;

-- name: DeleteChannelGroup :exec
DELETE FROM channel_groups WHERE id = $1;

-- Channels.

-- name: CreateChannel :one
INSERT INTO channels (number, name, tvg_id, logo_url, group_id, stream_profile_id)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1;

-- name: ListChannels :many
SELECT * FROM channels
WHERE (sqlc.narg('group_id')::bigint IS NULL OR group_id = sqlc.narg('group_id'))
  AND (sqlc.narg('search')::text IS NULL OR name ILIKE sqlc.narg('search'))
ORDER BY number NULLS LAST, name LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateChannel :one
UPDATE channels SET
    number = $2, name = $3, tvg_id = $4, logo_url = $5, group_id = $6,
    stream_profile_id = $7, enabled = $8, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteChannel :exec
DELETE FROM channels WHERE id = $1;

-- name: NextChannelNumber :one
SELECT COALESCE(max(number), 100) + 1 FROM channels;

-- name: GetChannelByTvgID :one
SELECT * FROM channels WHERE tvg_id = $1 AND tvg_id <> '' LIMIT 1;

-- name: ListUserChannels :many
-- Enabled channels in any of the user's bouquets (customer playlists).
SELECT DISTINCT c.id, c.number, c.name, c.tvg_id, c.logo_url,
       COALESCE(g.id, 0)::bigint AS group_id, COALESCE(g.name, '') AS group_name,
       COALESCE(g.sort_order, 0) AS group_sort, c.created_at
FROM user_bouquets ub
JOIN bouquet_channels bc ON bc.bouquet_id = ub.bouquet_id
JOIN channels c ON c.id = bc.channel_id
LEFT JOIN channel_groups g ON g.id = c.group_id
WHERE ub.user_id = $1 AND c.enabled
ORDER BY c.number NULLS LAST, c.name, c.id;

-- name: ListEnabledChannels :many
-- Every enabled channel (admin playlists).
SELECT c.id, c.number, c.name, c.tvg_id, c.logo_url,
       COALESCE(g.id, 0)::bigint AS group_id, COALESCE(g.name, '') AS group_name,
       COALESCE(g.sort_order, 0) AS group_sort, c.created_at
FROM channels c
LEFT JOIN channel_groups g ON g.id = c.group_id
WHERE c.enabled
ORDER BY c.number NULLS LAST, c.name, c.id;
