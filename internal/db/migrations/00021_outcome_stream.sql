-- +goose Up
-- Playback outcomes also name the stream they were for, so streams on one
-- channel can be compared. Probe outcomes keep stream_id 0: probing every
-- stream on a timer would multiply the rows for no added signal.
ALTER TABLE provider_outcome_buckets DROP CONSTRAINT provider_outcome_buckets_pkey;
ALTER TABLE provider_outcome_buckets ADD COLUMN stream_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE provider_outcome_buckets ADD PRIMARY KEY (bucket_start, provider_id, stream_id, backbone, outcome);

-- +goose Down
-- Stream-level rows cannot be merged into provider rows without losing the
-- key; they are dropped. Provider-level rows (stream_id 0) are kept.
DELETE FROM provider_outcome_buckets WHERE stream_id <> 0;
ALTER TABLE provider_outcome_buckets DROP CONSTRAINT provider_outcome_buckets_pkey;
ALTER TABLE provider_outcome_buckets DROP COLUMN stream_id;
ALTER TABLE provider_outcome_buckets ADD PRIMARY KEY (bucket_start, provider_id, backbone, outcome);
