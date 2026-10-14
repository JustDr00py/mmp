-- +goose Up
-- Per-provider override of the refresh disable threshold: the maximum share
-- of a provider's active streams one refresh may soft-disable without the
-- caller forcing it. NULL = use the global default (refresh.max_disable_pct).

ALTER TABLE providers
    ADD COLUMN refresh_max_disable_pct INT CHECK (refresh_max_disable_pct BETWEEN 0 AND 100);

-- +goose Down

ALTER TABLE providers DROP COLUMN refresh_max_disable_pct;
