-- +goose Up
-- VOD pooling: providers that share a backbone and tick vod_pooled are
-- accounts on one panel, whose movie, series and category ids mean the
-- same thing on each. Customers see each pooled title once, and playback
-- uses whichever account has a free connection.
ALTER TABLE providers ADD COLUMN vod_pooled BOOLEAN NOT NULL DEFAULT FALSE;

-- The categories customers may be offered: active, of an enabled provider
-- that imports VOD, and chosen if the provider filters categories. pool_key
-- names the pool: 'b:<backbone>' for pooled providers, else 'p:<id>'.
CREATE VIEW vod_offered_categories AS
SELECT c.id, c.provider_id, c.kind, c.external_id, c.name,
       (CASE WHEN pr.vod_pooled AND pr.backbone <> '' THEN 'b:' || pr.backbone ELSE 'p:' || pr.id::text END)::text AS pool_key,
       pr.priority AS provider_priority
FROM vod_categories c
JOIN providers pr ON pr.id = c.provider_id AND pr.enabled AND pr.vod_enabled
WHERE NOT c.disabled AND (NOT pr.vod_category_filter OR c.selected);

-- +goose Down
DROP VIEW IF EXISTS vod_offered_categories;
ALTER TABLE providers DROP COLUMN IF EXISTS vod_pooled;
