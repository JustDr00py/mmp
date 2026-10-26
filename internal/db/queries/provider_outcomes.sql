-- Upstream outcome counts for the provider, backbone and stream health scores.

-- name: UpsertOutcomeBucket :exec
INSERT INTO provider_outcome_buckets (bucket_start, provider_id, stream_id, backbone, outcome, count)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (bucket_start, provider_id, stream_id, backbone, outcome)
DO UPDATE SET count = provider_outcome_buckets.count + EXCLUDED.count;

-- name: ListOutcomeHoursSince :many
-- The window's outcomes summed per hour: scoring decays by the hour, so
-- this is all the detail it needs, and far fewer rows than the minutes.
SELECT date_trunc('hour', bucket_start)::timestamptz AS hour_start, provider_id, stream_id, backbone, outcome,
       SUM(count)::bigint AS count
FROM provider_outcome_buckets
WHERE bucket_start >= sqlc.arg('since')::timestamptz
GROUP BY date_trunc('hour', bucket_start), provider_id, stream_id, backbone, outcome;

-- name: PruneOutcomeBuckets :execrows
DELETE FROM provider_outcome_buckets WHERE bucket_start < sqlc.arg('before')::timestamptz;
