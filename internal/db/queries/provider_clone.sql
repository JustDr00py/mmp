-- Cloning a provider: a new account with the same settings, group choice
-- and movie & series category choice. Run together in one transaction.

-- name: CloneProvider :one
-- Copies every setting of the source; status (refresh times, errors) starts
-- fresh. The streams arrive with the clone's first refresh.
INSERT INTO providers (
    name, type, url, username, password, epg_url,
    max_connections, user_agent, refresh_interval_secs, priority, enabled,
    refresh_max_disable_pct, vod_enabled, group_filter, backbone, epg_enabled,
    vpn_url, vod_category_filter, vod_pooled
)
SELECT sqlc.arg('name'), s.type, sqlc.arg('url'), sqlc.arg('username'), sqlc.arg('password'), sqlc.arg('epg_url'),
       s.max_connections, s.user_agent, s.refresh_interval_secs, s.priority, s.enabled,
       s.refresh_max_disable_pct, s.vod_enabled, s.group_filter, s.backbone, s.epg_enabled,
       s.vpn_url, s.vod_category_filter, s.vod_pooled
FROM providers s WHERE s.id = sqlc.arg('source_id')
RETURNING *;

-- name: CopyProviderGroups :execrows
-- The chosen groups are kept by title, so they apply from the first refresh.
INSERT INTO provider_groups (provider_id, group_title)
SELECT sqlc.arg('target_id'), g.group_title FROM provider_groups g WHERE g.provider_id = sqlc.arg('source_id')
ON CONFLICT DO NOTHING;

-- name: CopyVODCategories :execrows
-- The source's categories with their selection: the clone's first catalog
-- import keeps the selection (its upsert leaves "selected" alone) and so
-- imports the same categories.
INSERT INTO vod_categories (provider_id, kind, external_id, name, disabled, selected)
SELECT sqlc.arg('target_id'), c.kind, c.external_id, c.name, c.disabled, c.selected
FROM vod_categories c WHERE c.provider_id = sqlc.arg('source_id')
ON CONFLICT (provider_id, kind, external_id) DO NOTHING;

-- name: CopyBouquetVODGrants :execrows
-- Bouquets granted a source category are granted the clone's copy of it.
INSERT INTO bouquet_vod_categories (bouquet_id, vod_category_id)
SELECT b.bouquet_id, d.id
FROM bouquet_vod_categories b
JOIN vod_categories s ON s.id = b.vod_category_id AND s.provider_id = sqlc.arg('source_id')
JOIN vod_categories d ON d.provider_id = sqlc.arg('target_id') AND d.kind = s.kind AND d.external_id = s.external_id
ON CONFLICT DO NOTHING;

-- name: ProviderCloneSummary :one
-- What a clone of the provider carries over, for the clone page.
SELECT (SELECT count(*) FROM provider_groups g WHERE g.provider_id = sqlc.arg('id'))::bigint AS groups_chosen,
       (SELECT count(*) FROM vod_categories c WHERE c.provider_id = sqlc.arg('id') AND c.selected)::bigint AS categories_chosen,
       (SELECT count(*) FROM vod_categories c WHERE c.provider_id = sqlc.arg('id'))::bigint AS categories,
       (SELECT count(DISTINCT b.bouquet_id) FROM bouquet_vod_categories b
          JOIN vod_categories c ON c.id = b.vod_category_id AND c.provider_id = sqlc.arg('id'))::bigint AS bouquets;
