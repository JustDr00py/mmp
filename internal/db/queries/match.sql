-- Channel matcher: admin regex rules and the inputs to a matching pass.

-- name: CreateMatchRule :one
INSERT INTO match_rules (pattern, channel_name, priority, enabled)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: ListMatchRules :many
SELECT * FROM match_rules ORDER BY priority, id;

-- name: DeleteMatchRule :exec
DELETE FROM match_rules WHERE id = $1;

-- name: ListUnassignedStreams :many
-- Live streams of enabled providers not yet on any channel's failover list.
SELECT ps.id, ps.provider_id, ps.name, ps.tvg_id, ps.logo_url,
       pr.priority AS provider_priority
FROM provider_streams ps
JOIN providers pr ON pr.id = ps.provider_id
WHERE pr.enabled
  AND NOT ps.disabled
  AND NOT EXISTS (SELECT 1 FROM channel_streams cs WHERE cs.provider_stream_id = ps.id)
ORDER BY pr.priority, ps.id;

-- name: ListChannelMatchKeys :many
SELECT id, name, tvg_id FROM channels ORDER BY id;

-- name: MaxChannelStreamPriority :one
SELECT COALESCE(max(priority), 0)::int FROM channel_streams WHERE channel_id = $1;
