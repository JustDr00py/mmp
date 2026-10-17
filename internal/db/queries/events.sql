-- Proxy and audit events (failovers, open failures, exhaustion).

-- name: InsertEvent :exec
INSERT INTO events (ts, type, channel_id, provider_id, from_stream_id, to_stream_id, message)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListRecentEvents :many
SELECT * FROM events
WHERE (sqlc.narg('channel_id')::bigint IS NULL OR channel_id = sqlc.narg('channel_id'))
ORDER BY ts DESC, id DESC
LIMIT sqlc.arg('limit');

-- name: ListEvents :many
-- The events page: newest first, optionally for one channel, one provider
-- (the event's own, or that of a stream it names) or one type.
SELECT * FROM events e
WHERE (sqlc.narg('channel_id')::bigint IS NULL OR e.channel_id = sqlc.narg('channel_id'))
  AND (sqlc.narg('type')::text IS NULL OR e.type = sqlc.narg('type'))
  AND (sqlc.narg('provider_id')::bigint IS NULL OR e.provider_id = sqlc.narg('provider_id')
       OR EXISTS (SELECT 1 FROM provider_streams ps WHERE ps.provider_id = sqlc.narg('provider_id')
                  AND ps.id IN (e.from_stream_id, e.to_stream_id)))
ORDER BY e.ts DESC, e.id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: PruneEvents :execrows
-- Deletes up to 10000 events older than before (call until it returns 0).
DELETE FROM events WHERE id IN (
    SELECT old.id FROM events old WHERE old.ts < sqlc.arg('before')::timestamptz LIMIT 10000);
