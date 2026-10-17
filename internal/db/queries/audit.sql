-- name: InsertAudit :exec
INSERT INTO admin_audit (ts, user_id, username, ip, action, target_type, target_id, target_name, method, path, via)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: ListAudit :many
-- The audit page: newest first, optionally for one admin (username) or one
-- kind of target.
SELECT * FROM admin_audit
WHERE (sqlc.narg('username')::text IS NULL OR username = sqlc.narg('username'))
  AND (sqlc.narg('target_type')::text IS NULL OR target_type = sqlc.narg('target_type'))
ORDER BY ts DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: AuditUsernames :many
SELECT DISTINCT username FROM admin_audit WHERE username <> '' ORDER BY username;

-- name: AuditTargetTypes :many
SELECT DISTINCT target_type FROM admin_audit WHERE target_type <> '' ORDER BY target_type;

-- name: PruneAudit :execrows
DELETE FROM admin_audit WHERE id IN (
    SELECT old.id FROM admin_audit old WHERE old.ts < sqlc.arg('before')::timestamptz LIMIT 10000);
