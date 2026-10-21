-- +goose Up
-- What a guide holds, counted when it is loaded: counting a large guide's
-- programs on every Guide page view took seconds.
ALTER TABLE epg_sources
    ADD COLUMN program_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN channel_count BIGINT NOT NULL DEFAULT 0;

UPDATE epg_sources s SET program_count = c.programs, channel_count = c.channels
FROM (SELECT epg_source_id, count(*) AS programs, count(DISTINCT lower(channel_tvg_id)) AS channels
      FROM epg_programs GROUP BY epg_source_id) c
WHERE c.epg_source_id = s.id;

-- +goose Down
ALTER TABLE epg_sources DROP COLUMN IF EXISTS program_count, DROP COLUMN IF EXISTS channel_count;
