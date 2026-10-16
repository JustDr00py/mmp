-- +goose Up
-- US spelling: epg_programmes -> epg_programs (with its constraints, index
-- and sequence). The XMLTV element itself stays <programme> (the standard).
ALTER TABLE epg_programmes RENAME TO epg_programs;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programmes_pkey TO epg_programs_pkey;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programmes_check TO epg_programs_check;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programmes_epg_source_id_fkey TO epg_programs_epg_source_id_fkey;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programmes_epg_source_id_channel_tvg_id_start_time_key
    TO epg_programs_epg_source_id_channel_tvg_id_start_time_key;
ALTER INDEX idx_epg_programmes_key_time RENAME TO idx_epg_programs_key_time;
ALTER SEQUENCE epg_programmes_id_seq RENAME TO epg_programs_id_seq;

-- +goose Down
ALTER SEQUENCE epg_programs_id_seq RENAME TO epg_programmes_id_seq;
ALTER INDEX idx_epg_programs_key_time RENAME TO idx_epg_programmes_key_time;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programs_epg_source_id_channel_tvg_id_start_time_key
    TO epg_programmes_epg_source_id_channel_tvg_id_start_time_key;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programs_epg_source_id_fkey TO epg_programmes_epg_source_id_fkey;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programs_check TO epg_programmes_check;
ALTER TABLE epg_programs RENAME CONSTRAINT epg_programs_pkey TO epg_programmes_pkey;
ALTER TABLE epg_programs RENAME TO epg_programmes;
