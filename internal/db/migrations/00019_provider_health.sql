-- +goose Up
-- Whether a provider answers: the last error of a refresh or health check
-- ('' = it answered), since when it has been failing, and when it was
-- last checked.
ALTER TABLE providers
    ADD COLUMN last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN failing_since TIMESTAMPTZ,
    ADD COLUMN checked_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE providers
    DROP COLUMN IF EXISTS checked_at,
    DROP COLUMN IF EXISTS failing_since,
    DROP COLUMN IF EXISTS last_error;
