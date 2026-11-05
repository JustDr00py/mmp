-- +goose Up
-- A provider address for use through the VPN: some providers give VPN
-- users their own host, and their regular one refuses VPN addresses. MMP
-- uses it instead of url while it runs behind gluetun; blank uses url.
ALTER TABLE providers ADD COLUMN vpn_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE providers DROP COLUMN IF EXISTS vpn_url;
