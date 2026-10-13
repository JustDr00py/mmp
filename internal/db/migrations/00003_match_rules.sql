-- +goose Up
-- Admin regex rules for the channel matcher: provider streams whose raw name
-- matches pattern (Go RE2, case-insensitive) are grouped under channel_name.

CREATE TABLE match_rules (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pattern      TEXT        NOT NULL,
    channel_name TEXT        NOT NULL,
    priority     INT         NOT NULL DEFAULT 0, -- lower is tried first
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS match_rules;
