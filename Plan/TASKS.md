# TASKS.md — MMP build checklist

Legend: `[ ]` todo · `[~]` in progress · `[x]` done. Work top to bottom; each task has an acceptance criterion (AC).

## M1 — Scaffold
- [~] 1.1 `go mod init`, directory layout, Makefile, `.gitignore`, `git init`. AC: `go build ./...` succeeds.
- [ ] 1.2 `internal/config`: env + optional YAML (listen addr, DB DSN, base URL, timeouts). AC: unit test loads defaults and overrides.
- [ ] 1.3 `docker-compose.yml` with Postgres 17 + mmp service. AC: `docker compose up postgres` is healthy.
- [ ] 1.4 `internal/db`: pgxpool, embedded goose migrations run at startup, sqlc config. AC: the app starts and migrates an empty DB.
- [ ] 1.5 Initial schema migration (all tables from PLAN.md data model). AC: migration up/down round-trips.
- [ ] 1.6 HTTP server (chi), slog middleware, `/healthz`, graceful shutdown. AC: `curl /healthz` → 200 and SIGTERM drains cleanly.
- [ ] 1.7 URL/credential redaction helper. AC: test shows Xtream and M3U URLs with creds are masked.

## M2 — Ingest
- [ ] 2.1 `source/m3u` streaming parser (EXTINF attrs, group-title, CRLF, BOM, missing attrs). AC: fixture tests pass, and a 100k-entry file parses with bounded memory.
- [ ] 2.2 `source/xtream` client: auth, `get_live_categories`, `get_live_streams`, stream URL building. AC: tests against an httptest fake panel.
- [ ] 2.3 `source/epg` XMLTV streaming parser + storage of programmes. AC: fixture test, with timezone offsets handled.
- [ ] 2.4 `refresher`: scheduled + on-demand refresh, upserted by `(provider_id, external_id)`, with stale rows soft-disabled. AC: integration test refreshes twice and the counts stay correct.
- [ ] 2.5 Admin API: providers CRUD, "refresh now", list/search provider streams. AC: API tests.

## M3 — Channels & matching
- [ ] 3.1 Channels, groups, and bouquets CRUD API. AC: API tests.
- [ ] 3.2 `channel_streams` priority list API (add/remove/reorder). AC: reorder persists.
- [ ] 3.3 Name normalizer (strip HD/FHD/4K, country prefixes, separators). AC: table-driven tests.
- [ ] 3.4 Matcher: tvg-id → normalized name → regex rules; propose or auto-create channels. AC: test with 3 fake providers groups the same channel correctly.

## M4 — Proxy core (critical path)
- [ ] 4.1 `ringbuf`: single writer, many readers with independent cursors, live-edge join, and overrun handling (a slow reader skips ahead). AC: race tests with slow/fast readers.
- [ ] 4.2 `tswatch`: 188-byte alignment/resync, CC error counting, bitrate window, stall timer. AC: synthetic broken TS tests.
- [ ] 4.3 `capacity`: per-provider slot acquire/release with the max_connections limit. AC: concurrent acquire test never exceeds the limit.
- [ ] 4.4 `upstream_http`: open a TS over HTTP with UA/headers and context cancel. AC: httptest test.
- [ ] 4.5 `session`: candidate selection, read loop, failover state machine, cooldown/backoff, and discontinuity/PAT-PMT splice. AC: failover tests (stall, 503, slot-full) keep the client stream continuous.
- [ ] 4.6 `manager`: session per channel, attach/detach, idle_grace teardown. AC: N clients → exactly 1 upstream request, and the slot is released after grace.
- [ ] 4.7 Internal admin-only endpoint `/api/v1/channels/{id}/stream` for testing playback. AC: plays in VLC/ffplay.
- [ ] 4.8 Failover events written to `events` + slog. AC: event row created on switch.

## M5 — Users & customer outputs
- [ ] 5.1 Users model: bcrypt, roles, expiry, max_connections, bouquets, stream_token. AC: unit tests.
- [ ] 5.2 Admin login (session cookie) + API tokens; middleware. AC: unauthorized → 401.
- [ ] 5.3 Customer auth (Xtream user/pass + token) with per-user connection accounting. AC: the (max+1)th connection is rejected.
- [ ] 5.4 M3U output: `/get.php` (m3u / m3u_plus) and `/playlist/{token}.m3u`. AC: VLC loads the playlist, and URLs point to MMP.
- [ ] 5.5 XMLTV output: `/xmltv.php`, `/epg/{token}.xml`, filtered by bouquet. AC: validates as XMLTV.
- [ ] 5.6 Xtream API: `player_api.php` (user_info/server_info, live categories/streams, short EPG, simple data table, empty VOD/series). AC: golden JSON tests, and IPTV Smarters/TiviMate log in.
- [ ] 5.7 `/live/{user}/{pass}/{id}.ts` wired to the proxy manager. AC: E2E playback through Xtream creds.

## M6 — More upstream types & health
- [ ] 6.1 `upstream_hls`: playlist polling, segment fetch, continuous TS out, and handling of discontinuity. AC: test against a fake HLS server.
- [ ] 6.2 Stream profiles + `upstream_ffmpeg` (remux/transcode via pipe, process cleanup). AC: a profile with `-c copy` plays, and no zombies remain after teardown.
- [ ] 6.3 Background prober: TS sniff + optional ffprobe, health_score, cooldown clear. AC: a dead stream gets cooldown, and recovery clears it.
- [ ] 6.4 Optional auto-sort of candidates by health. AC: setting toggles ordering.
- [ ] 6.5 `.m3u8` customer output (HLS wrapper over the session). AC: plays in Safari/hls.js.

## M7 — Admin web UI & observability
- [ ] 7.1 Layout, login page, embedded static/htmx. AC: login works.
- [ ] 7.2 Providers & streams pages (CRUD, refresh, search). AC: manual check.
- [ ] 7.3 Channels page: stream priority drag-reorder, match suggestions. AC: manual check.
- [ ] 7.4 Users page: limits, expiry, bouquets, copy M3U/Xtream credentials. AC: manual check.
- [ ] 7.5 EPG & profiles pages. AC: manual check.
- [ ] 7.6 Live dashboard via SSE: sessions, viewers, active provider, failovers, slot usage. AC: updates live during the failover demo.
- [ ] 7.7 Prometheus `/metrics` (sessions, viewers, failovers, bytes, upstream errors). AC: scrape works.

## M8 — Hardening & release
- [ ] 8.1 Rate limiting / lockout on customer and admin auth endpoints. AC: test.
- [ ] 8.2 Audit pass that credentials are never logged (grep + tests). AC: clean.
- [ ] 8.3 Multi-stage Dockerfile (with an optional ffmpeg variant). AC: image runs via compose.
- [ ] 8.4 README: setup, config reference, player setup guide. AC: a fresh user can get to playback.
- [ ] 8.5 Full E2E run per PLAN.md Verification section. AC: all checks pass.
