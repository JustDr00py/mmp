-- +goose Up
-- Settings changed from the admin UI (one JSON document per key), e.g. the
-- automatic backup schedule.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS settings;
