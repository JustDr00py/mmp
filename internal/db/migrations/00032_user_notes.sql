-- +goose Up
-- Free-text admin notes on a user (who they are, how they pay, ...). Shown
-- only in the admin UI and API, never to the user's player.
ALTER TABLE users ADD COLUMN notes TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS notes;
