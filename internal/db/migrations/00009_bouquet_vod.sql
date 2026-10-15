-- +goose Up
-- Bouquets grant movie and series categories, as they grant channels.
CREATE TABLE bouquet_vod_categories (
    bouquet_id      BIGINT NOT NULL REFERENCES bouquets(id)       ON DELETE CASCADE,
    vod_category_id BIGINT NOT NULL REFERENCES vod_categories(id) ON DELETE CASCADE,
    PRIMARY KEY (bouquet_id, vod_category_id)
);
CREATE INDEX idx_bouquet_vod_categories_cat ON bouquet_vod_categories (vod_category_id);

-- +goose Down
DROP TABLE IF EXISTS bouquet_vod_categories;
