-- +goose Up
-- Upstream outcomes counted per provider, minute and backbone (the backbone
-- as it was when the outcome happened): the history the health scores are
-- computed from. The proxy upserts a row per key once a minute.
CREATE TABLE provider_outcome_buckets (
    bucket_start TIMESTAMPTZ NOT NULL,
    provider_id  BIGINT      NOT NULL,
    backbone     TEXT        NOT NULL DEFAULT '',
    outcome      TEXT        NOT NULL,
    count        INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket_start, provider_id, backbone, outcome)
);
CREATE INDEX idx_outcome_buckets_provider ON provider_outcome_buckets (provider_id, bucket_start);

-- +goose Down
DROP TABLE IF EXISTS provider_outcome_buckets;
