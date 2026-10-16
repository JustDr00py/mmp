-- name: ListProviderGroups :many
-- Every group of a provider's live streams, with counts and whether it is
-- selected (selection only matters while the provider's group_filter is on).
SELECT ps.group_title,
       count(*) AS streams,
       (pg.group_title IS NOT NULL)::boolean AS selected
FROM provider_streams ps
LEFT JOIN provider_groups pg ON pg.provider_id = ps.provider_id AND pg.group_title = ps.group_title
WHERE ps.provider_id = $1 AND NOT ps.disabled
GROUP BY ps.group_title, pg.group_title
ORDER BY ps.group_title;

-- name: ClearProviderGroups :exec
DELETE FROM provider_groups WHERE provider_id = $1;

-- name: AddProviderGroups :exec
INSERT INTO provider_groups (provider_id, group_title)
SELECT sqlc.arg('provider_id'), unnest(sqlc.arg('group_titles')::text[])
ON CONFLICT DO NOTHING;

-- name: SetProviderGroupFilter :exec
UPDATE providers SET group_filter = $2, updated_at = now() WHERE id = $1;

-- name: ProviderGroupSummary :many
-- Selected/total group counts per provider, for the providers list.
SELECT ps.provider_id,
       count(DISTINCT ps.group_title) AS total,
       count(DISTINCT pg.group_title) AS selected
FROM provider_streams ps
LEFT JOIN provider_groups pg ON pg.provider_id = ps.provider_id AND pg.group_title = ps.group_title
WHERE NOT ps.disabled
GROUP BY ps.provider_id;
