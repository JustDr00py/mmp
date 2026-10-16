-- +goose Up
-- Extra guide sources (not tied to a provider) carry their own priority;
-- provider guides keep using their provider's. Lower wins, as for providers.
ALTER TABLE epg_sources ADD COLUMN priority INT;

-- +goose Down
ALTER TABLE epg_sources DROP COLUMN IF EXISTS priority;
