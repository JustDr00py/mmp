# MMP — Go IPTV Multiplexer / Failover Proxy (Dispatcharr-style)

## Context
We want a Go reimplementation of Dispatcharr's core idea: ingest many IPTV sources (M3U and Xtream providers), group equivalent streams from different providers under one logical **channel**, and serve customers one reliable stream per channel. The server watches each live upstream and **transparently swaps to the next provider** when one stalls or dies, so the customer's connection is not dropped. It must support multiple customer accounts and expose both **M3U/XMLTV** and **Xtream Codes–compatible** endpoints, so any IPTV player (TiviMate, IPTV Smarters, VLC, Jellyfin/Plex via M3U) works.

Decisions: project lives in `~/Documents/mmp` (empty, Go 1.26 installed). Native Go passthrough proxy with optional per-profile ffmpeg. **Postgres only.** Embedded web admin UI plus a REST API.

## Step 0 — Write planning docs (first action after approval)
Create `~/Documents/mmp/Plan/`:
- `Plan/PLAN.md`: this plan in full (architecture, data model, proxy design, outputs, verification).
- `Plan/AGENTS.md`: guidance for coding agents working in the repo:
  - project purpose
  - layout map
  - stack and conventions: idiomatic Go, `slog`, wrapped errors, no global state, context everywhere
  - commands: `make dev`, `make test`, `go test -race ./...`, `sqlc generate`, `goose` migrate
  - rules: never log provider or customer credentials; proxy code must have race-tested unit tests; migrations are append-only; the `TASKS.md` workflow (pick a task, mark it in progress, check it off with a short note)
- `Plan/TASKS.md`: a checklist broken down from the milestones below (M1–M8). Each task is small enough for one PR, has an acceptance criterion, and is ordered by dependency.

## Tech stack
- `net/http` + `go-chi/chi/v5` router; `jackc/pgx/v5` (pgxpool); `pressly/goose/v3` migrations (embedded); `sqlc` for typed queries
- Admin UI: `html/template` + htmx (+ a small CSS file), embedded with `embed.FS`, so no JS build step
- `golang.org/x/crypto/bcrypt`; `prometheus/client_golang`; `log/slog`
- Optional external `ffmpeg`/`ffprobe` (stream profiles and probing only)
- `docker-compose.yml` with Postgres + mmp

## Layout
```
mmp/
  Plan/ PLAN.md AGENTS.md TASKS.md
  cmd/mmp/main.go                 # config, migrate, start workers + HTTP
  internal/config/
  internal/db/                    # migrations/*.sql, queries/*.sql, sqlc output
  internal/source/
    m3u/parser.go                 # streaming #EXTINF parser
    xtream/client.go              # provider-side Xtream API ingest
    epg/xmltv.go                  # XMLTV fetch/parse/merge (streaming)
    refresher.go                  # scheduled refresh of sources + EPG
  internal/channel/matcher.go     # group streams across providers (tvg-id, normalized name, regex rules)
  internal/proxy/
    manager.go                    # one Session per active channel; attach/detach; idle teardown
    session.go                    # upstream loop + failover state machine
    ringbuf.go                    # shared ring buffer, per-client cursors
    upstream_http.go | upstream_hls.go | upstream_ffmpeg.go
    tswatch.go                    # TS sync/continuity/bitrate/stall detection
    capacity.go                   # per-provider connection slot accounting
  internal/health/prober.go       # background probing, cooldown/backoff, scores
  internal/auth/                  # admin sessions, customer creds, API tokens
  internal/output/ m3u.go xmltv.go xtream.go
  internal/api/                   # REST JSON for admin UI
  internal/web/                   # embedded templates + static
  docker-compose.yml, Dockerfile, Makefile
```

## Data model (Postgres)
- `providers`: type (m3u|xtream), url, creds, `max_connections`, user_agent, refresh_interval, enabled, priority
- `provider_streams`: provider_id, external_id, name, tvg_id, logo, group, url, health_score, last_error, cooldown_until, last_seen
- `channels`: number, name, tvg_id, logo, group_id, epg mapping, stream_profile_id, enabled
- `channel_streams`: channel_id, provider_stream_id, `priority` (ordered failover list)
- `channel_groups`, `bouquets` + `bouquet_channels`
- `stream_profiles`: mode (passthrough|ffmpeg), ffmpeg_args, user_agent
- `epg_sources`, `epg_programs`
- `users`: username, password_hash, role (admin|customer), `max_connections`, expires_at, enabled, bouquets, `stream_token`
- `events`: failover and audit log

## Core: the failover proxy
1. **One upstream per channel, shared.** The first viewer creates a `Session`. Later viewers attach to its ring buffer at the live edge, so N customers use 1 provider connection.
2. **Selection:** walk `channel_streams` by priority, skipping streams in cooldown and streams whose provider has no free slot.
3. **Health watch (in-line):** checks for a byte stall (default 5s), low bitrate, lost TS sync, bursts of continuity errors, and HTTP errors or EOF.
4. **Failover:** the failed stream gets exponential cooldown and the next candidate opens. **Client connections stay open.** The splice happens at a packet boundary after a fresh PAT/PMT, with `discontinuity_indicator` set. If all candidates fail, the session retries with backoff up to `max_retry_window`, then closes.
5. **Teardown:** when the last viewer leaves, the session waits `idle_grace` (10s) before releasing the provider slot.
6. **Background prober:** short GET + TS sniff (optionally `ffprobe`) that updates health_score, clears cooldowns, and optionally re-sorts candidates by health.
7. **Customer limits:** per-user `max_connections` and expiry are enforced on attach.

## Ingest & matching
- Scheduled refresh: Xtream (`get_live_categories`, `get_live_streams`, `xmltv.php`) and streaming M3U parse. Records are upserted by `(provider_id, external_id)`, and stale rows are soft-disabled.
- The matcher proposes groupings by tvg-id, then by normalized name, then by admin regex rules. Admins confirm, or the matcher auto-creates channels.

## Customer outputs
- **M3U:** `/get.php?username=&password=&type=m3u_plus` and `/playlist/{token}.m3u`. URLs always point to MMP, never to the provider.
- **XMLTV:** `/xmltv.php` and `/epg/{token}.xml`, filtered per user.
- **Xtream API:** `/player_api.php` (user_info/server_info, `get_live_categories`, `get_live_streams`, `get_short_epg`, `get_simple_data_table`); `/live/{user}/{pass}/{id}.ts` (`.m3u8` later). VOD and series actions return empty arrays in v1.
- **Admin REST** under `/api/v1`.

## Admin web UI (htmx + SSE)
Pages: dashboard (live sessions, viewers, active provider, failover events, slot usage), providers, streams browser, channels (drag-order priorities, match suggestions), EPG, profiles, users (limits, expiry, bouquets, copy credentials), settings.

## Milestones (expanded into TASKS.md)
1. Scaffold: module, config, pgx pool, goose, sqlc, slog, docker-compose, `/healthz`.
2. Ingest: M3U parser, Xtream client, refresher, provider/stream API.
3. Channels, matcher, priority lists.
4. **Proxy core:** ringbuf, manager/session, HTTP-TS upstream, tswatch, failover, capacity.
5. Users/auth + M3U/XMLTV/Xtream outputs + connection limits.
6. HLS upstream, ffmpeg profiles, background prober.
7. Web UI + SSE dashboard + `/metrics`.
8. Hardening: auth rate limiting, credential redaction, graceful shutdown, Dockerfile.

## Verification
- **Unit tests:** M3U parser fixtures, XMLTV merge, name normalizer, ring buffer (slow reader, overrun), tswatch with synthetic broken TS.
- **Failover integration:** `httptest` fake providers serve a looping TS fixture made with `ffmpeg -f lavfi testsrc`. Run three cases: provider A stalls, A returns 503, and A hits its slot limit. In each, assert the client keeps receiving bytes, the switch to B happens within `stall_timeout + ε`, and the client connection stays open.
- **Shared session:** N clients produce exactly 1 upstream request.
- **DB integration tests** against compose Postgres (`-tags=integration`).
- **Manual E2E:** `docker compose up`; play `/live/u/p/1.ts` in VLC; kill fake provider A and confirm playback continues and the dashboard logs the failover. Log in with IPTV Smarters/TiviMate via Xtream and confirm categories, channels, and EPG load.
- `go vet`, `staticcheck`, `go test -race ./...`.
