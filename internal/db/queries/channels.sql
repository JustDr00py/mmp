-- Channel groups (customer-facing categories).

-- name: CreateChannelGroup :one
INSERT INTO channel_groups (name, sort_order) VALUES ($1, $2) RETURNING *;

-- name: EnsureChannelGroup :one
-- The group with this name, created (sorted last) if it does not exist.
INSERT INTO channel_groups (name, sort_order)
VALUES ($1, (SELECT COALESCE(max(sort_order), 0) + 1 FROM channel_groups))
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING *;

-- name: ListChannelGroups :many
SELECT * FROM channel_groups ORDER BY sort_order, name;

-- name: SetChannelGroupGuideMode :exec
UPDATE channel_groups SET guide_mode = $2 WHERE id = $1;

-- name: UpdateChannelGroup :one
UPDATE channel_groups SET name = $2, sort_order = $3 WHERE id = $1 RETURNING *;

-- name: DeleteChannelGroup :exec
DELETE FROM channel_groups WHERE id = $1;

-- name: GetChannelGroup :one
SELECT * FROM channel_groups WHERE id = $1;

-- name: ChannelGroupCounts :many
-- Channels per group; group_id 0 counts the channels without a group.
SELECT COALESCE(group_id, 0)::bigint AS group_id, count(*) AS channels
FROM channels GROUP BY 1;

-- name: SetChannelsGroup :execrows
-- Moves channels into a group (NULL = no group).
UPDATE channels SET group_id = sqlc.narg('group_id'), updated_at = now()
WHERE id = ANY(sqlc.arg('channel_ids')::bigint[])
  AND group_id IS DISTINCT FROM sqlc.narg('group_id');

-- name: ChannelSourceGroups :many
-- The provider group of each channel's first failover stream that has one:
-- for the listed channels, or every channel without a group when
-- channel_ids is NULL.
SELECT DISTINCT ON (cs.channel_id) cs.channel_id, btrim(ps.group_title) AS group_title
FROM channel_streams cs
JOIN channels c ON c.id = cs.channel_id
JOIN provider_streams ps ON ps.id = cs.provider_stream_id
WHERE btrim(ps.group_title) <> ''
  AND CASE WHEN sqlc.narg('channel_ids')::bigint[] IS NULL THEN c.group_id IS NULL
           ELSE c.id = ANY(sqlc.narg('channel_ids')::bigint[]) END
ORDER BY cs.channel_id, cs.priority, ps.id;

-- Channels.

-- name: CreateChannel :one
INSERT INTO channels (number, name, tvg_id, logo_url, group_id, stream_profile_id)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1;

-- name: ListChannels :many
-- group_id 0 lists the channels without a group, bouquet_id 0 those in no
-- bouquet.
SELECT * FROM channels
WHERE (sqlc.narg('group_id')::bigint IS NULL OR group_id IS NOT DISTINCT FROM NULLIF(sqlc.narg('group_id')::bigint, 0))
  AND (sqlc.narg('search')::text IS NULL OR name ILIKE sqlc.narg('search'))
  AND (sqlc.narg('bouquet_id')::bigint IS NULL
       OR (sqlc.narg('bouquet_id')::bigint = 0) <> EXISTS (
           SELECT 1 FROM bouquet_channels bc WHERE bc.channel_id = channels.id
             AND (sqlc.narg('bouquet_id')::bigint = 0 OR bc.bouquet_id = sqlc.narg('bouquet_id')::bigint)))
ORDER BY number NULLS LAST, name LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateChannel :one
UPDATE channels SET
    number = $2, name = $3, tvg_id = $4, logo_url = $5, group_id = $6,
    stream_profile_id = $7, enabled = $8, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: RenameChannel :exec
UPDATE channels SET name = $2, updated_at = now() WHERE id = $1;

-- name: SetChannelGuideMode :exec
-- '' follows the channel's group.
UPDATE channels SET guide_mode = $2, updated_at = now() WHERE id = $1;

-- name: DeleteChannel :exec
DELETE FROM channels WHERE id = $1;

-- name: DeleteChannels :execrows
DELETE FROM channels WHERE id = ANY(sqlc.arg('channel_ids')::bigint[]);

-- name: DeleteGroupChannels :execrows
-- Deletes the channels in a group, before the group itself goes.
DELETE FROM channels WHERE group_id = $1;

-- name: NextChannelNumber :one
SELECT COALESCE(max(number), 100) + 1 FROM channels;

-- name: GetChannelByTvgID :one
SELECT * FROM channels WHERE tvg_id = $1 AND tvg_id <> '' LIMIT 1;

-- name: ListUserChannels :many
-- Enabled channels in any of the user's bouquets (customer playlists),
-- with the name of their first failover stream (for stream-name guides)
-- and where their guide comes from (the channel's mode, else its group's).
SELECT DISTINCT c.id, c.number, c.name, c.tvg_id, c.logo_url,
       COALESCE(g.id, 0)::bigint AS group_id, COALESCE(g.name, '') AS group_name,
       COALESCE(g.sort_order, 0) AS group_sort, c.created_at,
       COALESCE(NULLIF(c.guide_mode, ''), g.guide_mode, 'auto')::text AS guide_mode,
       COALESCE((SELECT ps.name FROM channel_streams cs JOIN provider_streams ps ON ps.id = cs.provider_stream_id
                  WHERE cs.channel_id = c.id ORDER BY cs.priority, ps.id LIMIT 1), '')::text AS stream_name
FROM user_bouquets ub
JOIN bouquet_channels bc ON bc.bouquet_id = ub.bouquet_id
JOIN channels c ON c.id = bc.channel_id
LEFT JOIN channel_groups g ON g.id = c.group_id
WHERE ub.user_id = $1 AND c.enabled
ORDER BY c.number NULLS LAST, c.name, c.id;

-- name: ListEnabledChannels :many
-- Every enabled channel (admin playlists), as ListUserChannels.
SELECT c.id, c.number, c.name, c.tvg_id, c.logo_url,
       COALESCE(g.id, 0)::bigint AS group_id, COALESCE(g.name, '') AS group_name,
       COALESCE(g.sort_order, 0) AS group_sort, c.created_at,
       COALESCE(NULLIF(c.guide_mode, ''), g.guide_mode, 'auto')::text AS guide_mode,
       COALESCE((SELECT ps.name FROM channel_streams cs JOIN provider_streams ps ON ps.id = cs.provider_stream_id
                  WHERE cs.channel_id = c.id ORDER BY cs.priority, ps.id LIMIT 1), '')::text AS stream_name
FROM channels c
LEFT JOIN channel_groups g ON g.id = c.group_id
WHERE c.enabled
ORDER BY c.number NULLS LAST, c.name, c.id;

-- name: GetChannelAccess :one
-- Whether a user may play a channel (enabled, and in one of their bouquets
-- unless they are an admin) and how many streams back it.
SELECT EXISTS (
           SELECT 1 FROM channels c
           WHERE c.id = sqlc.arg('channel_id') AND c.enabled
             AND (sqlc.arg('is_admin')::bool OR EXISTS (
                 SELECT 1 FROM user_bouquets ub
                 JOIN bouquet_channels bc ON bc.bouquet_id = ub.bouquet_id
                 WHERE ub.user_id = sqlc.arg('user_id') AND bc.channel_id = c.id))
       ) AS allowed,
       (SELECT count(*) FROM channel_streams cs WHERE cs.channel_id = sqlc.arg('channel_id'))::int AS streams;

-- name: ChannelNamesByIDs :many
SELECT id, name FROM channels WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: LiveChannelInfo :many
-- What the dashboard's channel cards show besides the session: logo,
-- stream profile, and what is on now (by tvg-id, case-insensitively, from
-- the highest-priority guide that has it, as customers see it).
SELECT c.id, c.number, c.name, c.logo_url,
       COALESCE(sp.name, '')::text AS profile_name, COALESCE(sp.mode, '')::text AS profile_mode,
       COALESCE((SELECT p.title FROM epg_programs p
          JOIN epg_sources s ON s.id = p.epg_source_id AND s.enabled
          LEFT JOIN providers pr ON pr.id = s.provider_id
         WHERE c.tvg_id <> '' AND lower(p.channel_tvg_id) = lower(c.tvg_id)
           AND p.start_time <= now() AND p.stop_time > now()
         ORDER BY COALESCE(s.priority, pr.priority, 2147483647), s.id, p.start_time DESC LIMIT 1), '')::text AS now_title
FROM channels c
LEFT JOIN stream_profiles sp ON sp.id = c.stream_profile_id
WHERE c.id = ANY(sqlc.arg('ids')::bigint[]);

-- name: LiveChannelStreams :many
-- The failover lists of several channels, for the dashboard's stream picker.
SELECT cs.channel_id, cs.provider_stream_id, ps.name AS stream_name, pr.name AS provider_name
FROM channel_streams cs
JOIN provider_streams ps ON ps.id = cs.provider_stream_id
JOIN providers pr ON pr.id = ps.provider_id
WHERE cs.channel_id = ANY(sqlc.arg('ids')::bigint[])
ORDER BY cs.channel_id, cs.priority, pr.priority, ps.name;

-- name: SetupProgress :one
-- Counts behind the dashboard's setup checklist.
SELECT
    (SELECT count(*) FROM providers)::bigint AS providers,
    (SELECT count(*) FROM provider_streams WHERE NOT disabled)::bigint AS streams,
    (SELECT count(*) FROM providers WHERE group_filter)::bigint AS filtered_providers,
    (SELECT count(*) FROM channels)::bigint AS channels,
    (SELECT count(*) FROM bouquets b WHERE EXISTS (SELECT 1 FROM bouquet_channels bc WHERE bc.bouquet_id = b.id))::bigint AS bouquets,
    (SELECT count(*) FROM users u WHERE u.role = 'customer'
        AND EXISTS (SELECT 1 FROM user_bouquets ub WHERE ub.user_id = u.id))::bigint AS customers;

-- name: ChannelNumbersByIDs :many
SELECT id, number FROM channels WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: ClearChannelNumbers :exec
-- Frees the numbers of channels about to be renumbered (numbers are
-- unique, so a permutation cannot be written in place).
UPDATE channels SET number = NULL WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: SetChannelNumbers :exec
-- ids[i] gets numbers[i].
UPDATE channels SET number = (sqlc.arg('numbers')::int[])[array_position(sqlc.arg('ids')::bigint[], id)], updated_at = now()
WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: SetChannelGroupOrder :exec
-- Sorts the listed groups in the given order: 10, 20, 30…
UPDATE channel_groups SET sort_order = 10 * array_position(sqlc.arg('ids')::bigint[], id)
WHERE id = ANY(sqlc.arg('ids')::bigint[]);
