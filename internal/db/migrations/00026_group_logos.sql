-- +goose Up
-- A group's icon (an image URL), sent to Xtream clients as category_icon
-- when the "group icons" setting is on. '' = none.
ALTER TABLE channel_groups ADD COLUMN logo_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE channel_groups DROP COLUMN IF EXISTS logo_url;
