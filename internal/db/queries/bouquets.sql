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

-- name: CloneBouquet :one
-- A new bouquet named name with the channels and movie and series
-- categories of src_id; the users it is granted to are not copied.
WITH b AS (
    INSERT INTO bouquets (name, sort_order)
    SELECT sqlc.arg('name'), src.sort_order FROM bouquets src WHERE src.id = sqlc.arg('src_id')
    RETURNING id
), chans AS (
    INSERT INTO bouquet_channels (bouquet_id, channel_id)
    SELECT b.id, bc.channel_id FROM b, bouquet_channels bc WHERE bc.bouquet_id = sqlc.arg('src_id')
), cats AS (
    INSERT INTO bouquet_vod_categories (bouquet_id, vod_category_id)
    SELECT b.id, bv.vod_category_id FROM b, bouquet_vod_categories bv WHERE bv.bouquet_id = sqlc.arg('src_id')
)
SELECT id FROM b;
