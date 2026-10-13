-- Provider stream ingestion: upsert on (provider_id, external_id),
-- soft-disable rows absent from a refresh, mark refresh time, and browse.

-- name: UpsertProviderStream :one
INSERT INTO provider_streams (
    provider_id, external_id, name, tvg_id, logo_url, group_title, stream_url
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (provider_id, external_id) DO UPDATE SET
    name        = EXCLUDED.name,
    tvg_id      = EXCLUDED.tvg_id,
    logo_url    = EXCLUDED.logo_url,
    group_title = EXCLUDED.group_title,
    stream_url  = EXCLUDED.stream_url,
    last_seen   = now(),
    disabled    = FALSE
RETURNING id;

-- name: SoftDisableMissing :execrows
UPDATE provider_streams
SET disabled = TRUE
WHERE provider_id = $1
  AND disabled = FALSE
  AND NOT (external_id = ANY(sqlc.arg('external_ids')::text[]));

-- name: MarkProviderRefreshed :exec
UPDATE providers SET last_refreshed_at = now(), updated_at = now() WHERE id = $1;

-- name: ListDueProviders :many
SELECT * FROM providers
WHERE enabled
  AND (last_refreshed_at IS NULL
       OR last_refreshed_at <= now() - make_interval(secs => refresh_interval_secs))
ORDER BY priority, name;

-- name: ListProvidersDueForEpg :many
SELECT p.* FROM providers p
LEFT JOIN epg_sources s ON s.provider_id = p.id
WHERE p.enabled
  AND (p.type = 'xtream' OR p.epg_url <> '')
  AND (s.last_refreshed_at IS NULL
       OR s.last_refreshed_at <= now() - make_interval(secs => $1::double precision))
ORDER BY p.priority, p.name;

-- name: CountProviderStreams :one
SELECT count(*) FROM provider_streams WHERE provider_id = $1;

-- name: ListProviderStreams :many
SELECT * FROM provider_streams WHERE provider_id = $1 ORDER BY name LIMIT $2 OFFSET $3;

-- name: SearchProviderStreams :many
SELECT * FROM provider_streams
WHERE provider_id = $1 AND name ILIKE $2
ORDER BY name LIMIT $3 OFFSET $4;
