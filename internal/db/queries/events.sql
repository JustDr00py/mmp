-- Proxy and audit events (failovers, open failures, exhaustion).

-- name: InsertEvent :exec
INSERT INTO events (ts, type, channel_id, provider_id, from_stream_id, to_stream_id, message)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListRecentEvents :many
SELECT * FROM events
WHERE (sqlc.narg('channel_id')::bigint IS NULL OR channel_id = sqlc.narg('channel_id'))
ORDER BY ts DESC, id DESC
LIMIT sqlc.arg('limit');
