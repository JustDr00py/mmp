-- Providers: basic CRUD used by the admin API and the refresher.

-- name: CreateProvider :one
INSERT INTO providers (
    name, type, url, username, password, epg_url,
    max_connections, user_agent, refresh_interval_secs, priority, enabled,
    refresh_max_disable_pct, backbone
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
) RETURNING *;

-- name: GetProvider :one
SELECT * FROM providers WHERE id = $1;

-- name: UpdateProvider :one
UPDATE providers SET
    name = $2, type = $3, url = $4, username = $5, password = $6, epg_url = $7,
    max_connections = $8, user_agent = $9, refresh_interval_secs = $10,
    priority = $11, enabled = $12, refresh_max_disable_pct = $13, backbone = $14, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListProviders :many
SELECT * FROM providers ORDER BY priority, name;

-- name: SetProviderEnabled :exec
UPDATE providers SET enabled = $2, updated_at = now() WHERE id = $1;

-- name: DeleteProvider :exec
DELETE FROM providers WHERE id = $1;

-- name: ListBackbones :many
-- Backbone labels in use, with how many providers carry each.
SELECT backbone, count(*) AS providers FROM providers
WHERE backbone <> '' GROUP BY backbone ORDER BY lower(backbone), backbone;

-- name: SetProvidersBackbone :execrows
UPDATE providers SET backbone = sqlc.arg('backbone'), updated_at = now()
WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: SetProvidersEnabled :execrows
UPDATE providers SET enabled = sqlc.arg('enabled'), updated_at = now()
WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: SetProvidersPriority :execrows
UPDATE providers SET priority = sqlc.arg('priority'), updated_at = now()
WHERE id = ANY(sqlc.arg('ids')::bigint[]);

-- name: GetProvidersByIDs :many
SELECT * FROM providers WHERE id = ANY(sqlc.arg('ids')::bigint[]) ORDER BY priority, name;
