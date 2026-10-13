-- +goose Up
-- Refresher bookkeeping: when a provider was last refreshed, and a unique
-- provider link on EPG sources so per-provider EPG rows can be upserted.

ALTER TABLE providers ADD COLUMN last_refreshed_at TIMESTAMPTZ;

CREATE UNIQUE INDEX idx_epg_sources_provider
    ON epg_sources(provider_id) WHERE provider_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_epg_sources_provider;
ALTER TABLE providers DROP COLUMN IF EXISTS last_refreshed_at;
