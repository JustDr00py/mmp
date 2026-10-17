-- name: GetSetting :one
SELECT value FROM settings WHERE key = $1;

-- name: SetSetting :exec
INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: SeedSetting :execrows
-- Stores a setting only if it was never saved (the starting value).
INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING;
