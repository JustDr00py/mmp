# AGENTS.md — Guidance for coding agents working on MMP

## What this project is
MMP is a Go IPTV **multiplexer and failover proxy** (similar to Dispatcharr). It:
- ingests many upstream sources (M3U playlists, Xtream Codes providers, XMLTV EPG)
- groups equivalent streams from different providers into one logical **channel** with an ordered failover list
- serves customers one reliable stream per channel, sharing a single upstream connection across all viewers
- detects stalls, errors, and corrupt TS, and **swaps providers mid-stream without dropping the client**
- supports multiple users (admins and customers) with connection limits, expiry, and bouquets
- outputs M3U, XMLTV, and Xtream-compatible endpoints (`player_api.php`, `get.php`, `/live/u/p/id.ts`)

Read `Plan/PLAN.md` for the architecture. Pick work from `Plan/TASKS.md`.

## Layout
```
cmd/mmp/            entrypoint (config → migrate → workers → HTTP)
internal/config/    env + YAML config
internal/db/        migrations/ (goose, embedded), queries/ (sqlc), generated code
internal/source/    m3u/, xtream/, epg/, refresher.go — upstream ingest
internal/channel/   matcher: cross-provider stream grouping
internal/proxy/     manager, session, ringbuf, upstream_*, tswatch, capacity  ← the critical path
internal/health/    background prober, cooldowns, health scores
internal/auth/      admin sessions, customer creds, API tokens
internal/output/    m3u.go, xmltv.go, xtream.go — customer-facing endpoints
internal/api/       admin REST API (/api/v1)
internal/web/       embedded html/template + htmx UI, static assets
Plan/               PLAN.md, AGENTS.md, TASKS.md
```

## Stack
Go 1.26 · chi v5 · pgx v5 (pgxpool) · goose v3 · sqlc · html/template + htmx · bcrypt · prometheus client · log/slog. Postgres is the only supported database. ffmpeg/ffprobe are optional runtime dependencies used only by stream profiles and the prober.

## Commands
```
make dev              # docker compose up postgres + run mmp with live config
make test             # go test -race ./...
make test-integration # go test -race -tags=integration ./...  (needs compose Postgres)
make lint             # go vet ./... && staticcheck ./...
make generate         # sqlc generate
make migrate          # goose up (also runs automatically on startup)
```

## Conventions
- Write idiomatic Go and keep packages small. Accept interfaces and return structs.
- Pass `context.Context` as the first argument to anything that does I/O or blocks. Respect cancellation everywhere, especially in proxy goroutines.
- Avoid global mutable state. Wire dependencies explicitly in `cmd/mmp/main.go`.
- Wrap errors with context (`fmt.Errorf("refresh provider %d: %w", id, err)`), and use sentinel errors where callers branch on them.
- Use `log/slog` structured logging with keys like `channel_id`, `provider_id`, `stream_id`, `session_id`.
- Write all SQL in `internal/db/queries/*.sql` and generate it with sqlc. Don't hand-build SQL strings.
- Use table-driven tests. Use `httptest` fake providers for anything network-facing.

## Hard rules
1. **Never log credentials.** Provider URLs often embed usernames and passwords, and customer passwords appear in Xtream paths. Use the redaction helper before logging any URL.
2. **Proxy code must pass `go test -race`.** Any change in `internal/proxy/` needs unit tests covering concurrency (multiple readers, slow readers, failover during reads).
3. **Client connections must survive upstream failover.** Don't add code paths that close a viewer's response because one upstream failed. Close it only when every candidate is exhausted.
4. **One upstream per channel.** Never open a provider connection per viewer. Provider connection slots are a scarce, paid resource.
5. **Migrations are append-only.** Never edit a migration that has been committed. Add a new one instead.
6. **Customer output URLs point to MMP**, never to the upstream provider.
7. Keep Xtream responses wire-compatible: match field names and types (strings vs. numbers) used by real panels, because players are strict.

## Task workflow (`Plan/TASKS.md`)
1. Pick the first unchecked task whose dependencies are done.
2. Mark it in progress by changing `[ ]` to `[~]`.
3. Implement it and add tests. Check that `make test` and `make lint` pass.
4. Mark it `[x]` and append a one-line note (`— done: <what/where>`).
5. If you discover new work, add it as a new task under the right milestone. Don't silently expand scope.
