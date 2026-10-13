-- Users (admins and customers) and their bouquet grants.

-- name: CreateUser :one
INSERT INTO users (username, password_hash, role, max_connections, expires_at, enabled, stream_token)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: GetUserByStreamToken :one
SELECT * FROM users WHERE stream_token = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: UpdateUser :one
UPDATE users SET
    username = $2, password_hash = $3, role = $4, max_connections = $5,
    expires_at = $6, enabled = $7, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetUserStreamToken :one
UPDATE users SET stream_token = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: ClearUserBouquets :exec
DELETE FROM user_bouquets WHERE user_id = $1;

-- name: AddUserBouquets :exec
INSERT INTO user_bouquets (user_id, bouquet_id)
SELECT $1, unnest(sqlc.arg('bouquet_ids')::bigint[])
ON CONFLICT DO NOTHING;

-- name: ListUserBouquetIDs :many
SELECT bouquet_id FROM user_bouquets WHERE user_id = $1 ORDER BY bouquet_id;

-- name: CountAdmins :one
SELECT count(*) FROM users WHERE role = 'admin' AND enabled;
