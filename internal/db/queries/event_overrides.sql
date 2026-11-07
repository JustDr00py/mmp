-- Game routing (internal/eventroute).

-- name: ListGroupChannelStreams :many
-- The channels of the given groups with their streams' names (a channel
-- with no streams has one row with a NULL stream), for game routing.
SELECT c.id AS channel_id, c.name AS channel_name, c.group_id::bigint AS group_id,
       ps.id AS provider_stream_id, ps.name AS stream_name
FROM channels c
LEFT JOIN channel_streams cs ON cs.channel_id = c.id
LEFT JOIN provider_streams ps ON ps.id = cs.provider_stream_id AND NOT ps.disabled
WHERE c.enabled AND c.group_id = ANY(sqlc.arg('group_ids')::bigint[])
ORDER BY c.id, cs.priority;

-- name: UpsertEventOverride :exec
INSERT INTO channel_event_overrides (channel_id, source_channel_id, title, starts_at, ends_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (channel_id) DO UPDATE SET source_channel_id = EXCLUDED.source_channel_id,
    title = EXCLUDED.title, starts_at = EXCLUDED.starts_at, ends_at = EXCLUDED.ends_at, updated_at = now();

-- name: DeleteEventOverridesExcept :execrows
-- Drops the overrides a routing pass did not find again (or all of them
-- with an empty keep).
DELETE FROM channel_event_overrides WHERE NOT (channel_id = ANY(sqlc.arg('keep')::bigint[]));

-- name: ListEventOverrides :many
-- Every override with the slot channel's name, for the admin pages.
SELECT o.channel_id, o.source_channel_id, s.name AS source_name, o.title, o.starts_at, o.ends_at
FROM channel_event_overrides o
JOIN channels s ON s.id = o.source_channel_id
ORDER BY o.starts_at, o.channel_id;

-- name: RemoveSlotLinksFromGroups :execrows
-- Takes event slot streams off team channels: links on channels of the
-- target groups to streams that a channel of the source groups also has.
-- Routing gives a team its game's slot while the game is on instead.
DELETE FROM channel_streams cs
USING channels c
WHERE c.id = cs.channel_id
  AND c.group_id = ANY(sqlc.arg('target_group_ids')::bigint[])
  AND EXISTS (
      SELECT 1 FROM channel_streams s
      JOIN channels sc ON sc.id = s.channel_id
      WHERE s.provider_stream_id = cs.provider_stream_id
        AND sc.group_id = ANY(sqlc.arg('source_group_ids')::bigint[]));
