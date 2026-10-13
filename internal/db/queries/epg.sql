-- EPG source bookkeeping and programme storage. Programmes are replaced
-- wholesale per source on every refresh (guides are large but stateless).

-- name: UpsertEpgSource :one
INSERT INTO epg_sources (name, url, provider_id)
VALUES ($1, $2, $3)
ON CONFLICT (provider_id) WHERE provider_id IS NOT NULL DO UPDATE SET
    name = EXCLUDED.name,
    url  = EXCLUDED.url
RETURNING *;

-- name: MarkEpgSourceRefreshed :exec
UPDATE epg_sources SET last_refreshed_at = now() WHERE id = $1;

-- name: ClearEpgSourceProgrammes :execrows
DELETE FROM epg_programmes WHERE epg_source_id = $1;

-- name: InsertEpgProgrammes :copyfrom
INSERT INTO epg_programmes (
    epg_source_id, channel_tvg_id, title, sub_title, description, start_time, stop_time
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountEpgProgrammes :one
SELECT count(*) FROM epg_programmes WHERE epg_source_id = $1;

-- name: GetEpgSourceByProvider :one
SELECT * FROM epg_sources WHERE provider_id = $1;
