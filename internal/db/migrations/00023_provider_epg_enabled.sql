-- +goose Up
-- Whether a provider's guide is downloaded. Connections to the same
-- service serve the same guide, so all but one can turn theirs off
-- instead of storing (and searching) copies.
ALTER TABLE providers ADD COLUMN epg_enabled BOOLEAN NOT NULL DEFAULT TRUE;

-- +goose Down
ALTER TABLE providers DROP COLUMN IF EXISTS epg_enabled;
