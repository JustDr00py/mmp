-- Bouquet management for the admin UI.

-- name: BouquetCounts :many
SELECT b.id,
       (SELECT count(*) FROM bouquet_channels bc WHERE bc.bouquet_id = b.id) AS channels,
       (SELECT count(*) FROM user_bouquets ub WHERE ub.bouquet_id = b.id) AS users
FROM bouquets b;

-- name: AddChannelsToBouquet :execrows
INSERT INTO bouquet_channels (bouquet_id, channel_id)
SELECT sqlc.arg('bouquet_id'), unnest(sqlc.arg('channel_ids')::bigint[])
ON CONFLICT DO NOTHING;

-- name: AddGroupToBouquet :execrows
-- Every channel of a group (group_id NULL = channels without a group).
INSERT INTO bouquet_channels (bouquet_id, channel_id)
SELECT sqlc.arg('bouquet_id'), c.id FROM channels c
WHERE c.group_id IS NOT DISTINCT FROM sqlc.narg('group_id')::bigint
ON CONFLICT DO NOTHING;

-- name: RemoveChannelsFromBouquet :execrows
DELETE FROM bouquet_channels
WHERE bouquet_id = sqlc.arg('bouquet_id') AND channel_id = ANY(sqlc.arg('channel_ids')::bigint[]);
