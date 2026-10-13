-- +goose Up
-- The background prober records when it last checked each stream, so its
-- schedule survives restarts instead of re-probing everything at boot.
ALTER TABLE provider_streams ADD COLUMN last_probed_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE provider_streams DROP COLUMN IF EXISTS last_probed_at;
