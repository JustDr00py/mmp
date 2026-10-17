-- Channel matcher: admin regex rules and the inputs to a matching pass.

-- name: CreateMatchRule :one
INSERT INTO match_rules (pattern, channel_name, priority, enabled)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: ListMatchRules :many
SELECT * FROM match_rules ORDER BY priority, id;

-- name: DeleteMatchRule :exec
DELETE FROM match_rules WHERE id = $1;

-- name: ListUnassignedStreams :many
-- Live streams of enabled providers not yet on any channel's failover list,
-- in the groups selected for their provider.
SELECT ps.id, ps.provider_id, ps.name, ps.tvg_id, ps.logo_url, ps.group_title,
       pr.priority AS provider_priority
FROM provider_streams ps
JOIN providers pr ON pr.id = ps.provider_id
WHERE pr.enabled
  AND NOT ps.disabled
  AND NOT EXISTS (SELECT 1 FROM channel_streams cs WHERE cs.provider_stream_id = ps.id)
  AND (NOT pr.group_filter OR EXISTS (SELECT 1 FROM provider_groups pg
       WHERE pg.provider_id = ps.provider_id AND pg.group_title = ps.group_title))
ORDER BY pr.priority, ps.id;

-- name: ListChannelMatchKeys :many
SELECT id, name, tvg_id FROM channels ORDER BY id;

-- name: MaxChannelStreamPriority :one
SELECT COALESCE(max(priority), 0)::int FROM channel_streams WHERE channel_id = $1;

-- name: GroupBouquetShares :many
-- For each of the channel groups, the bouquets holding some of its channels:
-- how many, and how many channels the group has. Channels in exclude (the
-- ones an apply just created) are not counted.
SELECT c.group_id::bigint AS group_id, bc.bouquet_id, count(*) AS in_bouquet,
       (SELECT count(*) FROM channels c2
         WHERE c2.group_id = c.group_id AND NOT (c2.id = ANY(sqlc.arg('exclude')::bigint[]))) AS group_size
FROM channels c
JOIN bouquet_channels bc ON bc.channel_id = c.id
WHERE c.group_id = ANY(sqlc.arg('group_ids')::bigint[])
  AND NOT (c.id = ANY(sqlc.arg('exclude')::bigint[]))
GROUP BY c.group_id, bc.bouquet_id;
