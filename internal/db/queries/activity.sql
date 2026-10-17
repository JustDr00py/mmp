-- name: InsertViewingSession :exec
INSERT INTO viewing_sessions (user_id, channel_id, ip, output, started_at, ended_at)
SELECT $1, $2, $3, $4, $5, $6 WHERE EXISTS (SELECT 1 FROM users WHERE id = $1);

-- name: TouchUserNetwork :exec
INSERT INTO user_networks AS n (user_id, network, last_ip, first_seen, last_seen)
SELECT sqlc.arg('user_id'), sqlc.arg('network'), sqlc.arg('ip'), sqlc.arg('at'), sqlc.arg('at')
WHERE EXISTS (SELECT 1 FROM users WHERE id = sqlc.arg('user_id'))
ON CONFLICT (user_id, network) DO UPDATE
SET last_ip = EXCLUDED.last_ip, last_seen = GREATEST(n.last_seen, EXCLUDED.last_seen);

-- name: InsertAuthFailure :exec
INSERT INTO auth_failures (ts, user_id, username, ip)
VALUES ($1, (SELECT id FROM users WHERE username = $2), $2, $3);

-- name: UserViewingSessions :many
-- A user's watch history, newest first; channel_name is '' for a deleted
-- channel.
SELECT v.id, v.channel_id, COALESCE(c.name, '')::text AS channel_name, v.ip, v.output, v.started_at, v.ended_at
FROM viewing_sessions v LEFT JOIN channels c ON c.id = v.channel_id
WHERE v.user_id = $1
ORDER BY v.started_at DESC, v.id DESC
LIMIT $2;

-- name: UserNetworks :many
SELECT network, last_ip, first_seen, last_seen FROM user_networks
WHERE user_id = $1 ORDER BY last_seen DESC LIMIT $2;

-- name: UserAuthFailures :many
SELECT ts, ip FROM auth_failures WHERE user_id = $1 ORDER BY ts DESC, id DESC LIMIT $2;

-- name: UsersLastSeen :many
-- Each user's newest network: when and from where they were last seen.
SELECT DISTINCT ON (user_id) user_id, last_seen, last_ip FROM user_networks
ORDER BY user_id, last_seen DESC;

-- name: PruneViewingSessions :execrows
DELETE FROM viewing_sessions WHERE id IN (
    SELECT old.id FROM viewing_sessions old WHERE old.ended_at < sqlc.arg('before')::timestamptz LIMIT 10000);

-- name: PruneUserNetworks :execrows
DELETE FROM user_networks WHERE last_seen < sqlc.arg('before')::timestamptz;

-- name: PruneAuthFailures :execrows
DELETE FROM auth_failures WHERE id IN (
    SELECT old.id FROM auth_failures old WHERE old.ts < sqlc.arg('before')::timestamptz LIMIT 10000);
