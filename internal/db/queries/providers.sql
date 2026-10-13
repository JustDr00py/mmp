-- Providers: basic CRUD used by the admin API and the refresher.

-- name: CreateProvider :one
INSERT INTO providers (
    name, type, url, username, password, epg_url,
    max_connections, user_agent, refresh_interval_secs, priority, enabled
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
) RETURNING *;

-- name: GetProvider :one
SELECT * FROM providers WHERE id = $1;

-- name: ListProviders :many
SELECT * FROM providers ORDER BY priority, name;

-- name: SetProviderEnabled :exec
UPDATE providers SET enabled = $2, updated_at = now() WHERE id = $1;

-- name: DeleteProvider :exec
DELETE FROM providers WHERE id = $1;
