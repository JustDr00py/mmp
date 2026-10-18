-- +goose Up
-- The upstream network a provider resells (many resellers share one).
-- A free-text label for grouping and filtering providers; '' = unknown.
ALTER TABLE providers ADD COLUMN backbone TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE providers DROP COLUMN IF EXISTS backbone;
