-- +goose Up
-- Per-provider movie/series category selection, like provider_groups for
-- live streams. With vod_category_filter on, only titles in selected
-- categories are imported (the rest are skipped, or disabled if already
-- stored), and unselected categories are hidden from customers, bouquets
-- and the catalog page. Categories themselves are always imported, so they
-- can be chosen; ones the panel adds later stay unselected.
ALTER TABLE providers ADD COLUMN vod_category_filter BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE vod_categories ADD COLUMN selected BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE vod_categories DROP COLUMN IF EXISTS selected;
ALTER TABLE providers DROP COLUMN IF EXISTS vod_category_filter;
