-- Admin sessions and API tokens. Secrets are looked up by their SHA-256.

-- name: CreateSession :exec
INSERT INTO admin_sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: GetSessionUser :one
SELECT u.* FROM admin_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM admin_sessions WHERE token_hash = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM admin_sessions WHERE expires_at <= now();

-- name: CreateAPIToken :one
INSERT INTO api_tokens (user_id, name, token_hash) VALUES ($1, $2, $3)
RETURNING id, user_id, name, created_at, last_used_at;

-- name: ListAPITokens :many
SELECT t.id, t.user_id, u.username, t.name, t.created_at, t.last_used_at
FROM api_tokens t JOIN users u ON u.id = t.user_id
ORDER BY t.id;

-- name: DeleteAPIToken :execrows
DELETE FROM api_tokens WHERE id = $1;

-- name: GetAPITokenUser :one
SELECT u.*, t.id AS token_id FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1;

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;
