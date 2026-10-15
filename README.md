# MMP

MMP is a self-hosted IPTV multiplexer with automatic failover. It pulls live channels from several IPTV
providers (M3U playlists or Xtream Codes panels), groups the same channel from different providers into
one logical channel, and gives your viewers a single stream per channel. When the provider that is playing
stalls, errors or degrades, MMP switches to the next one **without dropping the viewer's connection**.

- One upstream connection per channel, shared by every viewer of it, so provider connection limits go further.
- Works with any IPTV player: M3U + XMLTV (VLC, Kodi, Plex, Jellyfin) and an Xtream Codes compatible API
  (IPTV Smarters, TiviMate, XCIPTV…). MPEG-TS and HLS output.
- Customer accounts with connection limits, expiry dates and per-user channel sets ("bouquets").
- Movies and series from Xtream providers, passed through to Xtream apps with seeking (opt-in per provider).
- Web admin UI with a live dashboard, Prometheus metrics, and a REST API.
- Single static binary; Postgres is the only dependency (ffmpeg optional).

**Contents:** [Quick start](#quick-start-docker) · [From zero to playback](#from-zero-to-playback) ·
[Running from source](#running-from-source) · [Configuration](#configuration) ·
[Customer URLs](#customer-urls) · [Admin API](#admin-api) · [Monitoring](#monitoring) ·
[Deployment notes](#deployment-notes) · [Troubleshooting](#troubleshooting) · [Development](#development)

---

## Quick start (Docker)

You need Docker (or Podman) with Compose.

1. **Get the code**

   ```sh
   git clone <this repository> mmp && cd mmp
   ```

2. **Edit `docker-compose.yml`**, in the `mmp` service:

   - `MMP_BASE_URL`: the address **players** will use to reach MMP, e.g. `http://192.168.1.10:8080`
     or `https://tv.example.com`. Every playlist, guide and stream URL MMP hands out is built from it, so
     `localhost` only works for players on the same machine.
   - Uncomment `MMP_ADMIN_USERNAME` / `MMP_ADMIN_PASSWORD` and choose your admin login. They are only used
     to create the first admin when none exists; you can remove them afterwards.
   - Optional: uncomment `target: mmp-ffmpeg` for the image with ffmpeg (needed only for
     [stream profiles](#stream-profiles-ffmpeg) in ffmpeg mode and the prober's ffprobe check).

3. **Start it**

   ```sh
   docker compose up -d --build      # Podman: podman compose up -d --build
   ```

   MMP applies its database migrations on start; `docker compose logs mmp` shows `server listening` when it is ready.

4. **Open the admin UI** at `http://<your-host>:8080/admin` and sign in with the admin login from step 2.

Before exposing MMP beyond your own network, read [Deployment notes](#deployment-notes) (Postgres
password and port, TLS).

## From zero to playback

The admin UI walks you through the same five steps. Each page links to the next.

### 1. Add a provider

**Providers → Add provider.**

| Field | Xtream Codes panel | M3U playlist |
|---|---|---|
| Type | Xtream Codes | M3U playlist |
| URL | the panel's base URL, e.g. `http://panel.example:8080` | the full playlist URL |
| Username / Password | your panel login | leave empty (or use them if your URL needs basic auth) |
| EPG URL | leave empty: the panel provides one | optional XMLTV URL for the guide |
| Max connections | **the number of simultaneous streams your subscription allows** | same |
| Priority | lower = preferred when several providers have a channel | same |

Save, then press **Refresh** on the provider's row. MMP imports its live streams; the row shows how many.
Click the provider's name to browse and search its streams. Add more providers the same way. Failover
needs at least two providers carrying the same channel.

### 2. Create channels

A **channel** is what your viewers see. It has an ordered failover list of provider streams. Two ways
to build channels:

- **Match** (fastest): MMP suggests channels that several providers carry, matching them by tvg-id, by
  normalised name (`UK: BBC One HD` ≈ `BBC One`), or by your own regex rules. Tick the suggestions you want
  and press **Apply selected**. With a single provider, set the filter to *Seen at ≥ 1 provider*.
- **By hand**: **Channels → New channel**, then search streams across all providers and **Add** them.

On a channel's page, drag streams (or use ↑/↓) to set the failover order. The first available stream
plays. Set the channel's **tvg-id** to the guide's channel id to get programme data (see the **Guide**
page for coverage).

### 3. Put channels in a bouquet

Customers only see channels from the **bouquets** granted to them. **Bouquets → New bouquet**, then tick
channels (or **Add all channels** of a group) and **Add selected**.

### 4. Create a customer

**Users → New user**: choose a username, leave the password blank to generate one, set **Max
connections** (simultaneous streams; 0 = unlimited) and an optional expiry date, tick the bouquet, and
**Create user**.

The next page shows the customer's **player setup**. **Copy the password now**: MMP only stores a hash,
so it is shown once. You can **Reset password** later to issue a new one.

### 5. Play

Pick whichever your player supports:

- **Xtream Codes login** (IPTV Smarters, TiviMate, XCIPTV, …): choose "Xtream Codes" / "Login with Xtream
  Codes API" and enter the **Server URL**, **Username** and **Password** from the player setup panel.
  Channels, groups and the guide load automatically.
- **M3U playlist** (VLC, Kodi, Plex, Jellyfin, …): use the **Playlist URL** and **Guide (XMLTV) URL**. In
  VLC: *Media → Open Network Stream →* paste the playlist URL. These links contain a private token instead
  of the password. Anyone with them can watch as that user, so **Rotate token** if they leak.
- **HLS** (Safari, browsers with hls.js, some smart TVs): Xtream apps can pick it themselves; for M3U, add
  `&output=m3u8` to a `get.php` URL.

Quick check without a player (needs ffmpeg installed; replace the placeholders):

```sh
ffprobe "http://<host>:8080/live/<username>/<password>/<channel-id>.ts"
```

Watch it on the **Dashboard**: the channel appears with the provider it plays from. If you stop that
provider, the dashboard shows the failover while the player keeps playing.

### Movies and series (optional, Xtream providers)

Edit an Xtream provider, tick **Import movies & series**, save, and press **Import catalog** on its row.
Browse the result under **Movies & Series**; opening a series loads its episodes from the provider. Then, on
a bouquet's page, add movie and series categories under **Movie & series categories**. Customers with that
bouquet see them in Xtream apps (and in M3U playlists with `&vod=1` / `&series=1`) and can seek while
watching. Each title playing uses one of the customer's connections and one of the provider's.

## Running from source

Requirements: Go 1.26+ and PostgreSQL (tested with 17, which the compose file uses).

```sh
make up                      # starts only Postgres via compose (COMPOSE="podman compose" for Podman)
make build                   # → ./mmp

MMP_ADMIN_USERNAME=admin MMP_ADMIN_PASSWORD='change-me-please' \
MMP_BASE_URL=http://192.168.1.10:8080 ./mmp
```

Flags:

| Flag | Meaning |
|---|---|
| `-config path.yaml` | load a YAML config file (optional; environment variables override it) |
| `-migrate` | apply database migrations and exit (they also run on every start) |
| `-version` | print the version and exit |

## Configuration

Settings come from built-in defaults, then the YAML file given with `-config`, then `MMP_*` environment
variables. Later sources win. Durations use Go syntax: `500ms`, `5s`, `10m`, `6h`. Invalid values stop
MMP at startup with a message naming the key.

<details>
<summary><strong>Example <code>mmp.yaml</code></strong></summary>

```yaml
listen: ":8080"
base_url: "https://tv.example.com"
db:
  dsn: "postgres://mmp:secret@db:5432/mmp?sslmode=disable"
proxy:
  stall_timeout: 5s
  hls_max_height: 1080        # pick ≤1080p variants from HLS providers
refresh:
  interval: 6h
probe:
  enabled: true
metrics:
  token: "a-long-random-string"
admin:                        # only used to create the first admin
  username: admin
  password: change-me-please
```

</details>

### General

| Key | Env | Default | Meaning |
|---|---|---|---|
| `listen` | `MMP_LISTEN` | `:8080` | address to listen on |
| `base_url` | `MMP_BASE_URL` | `http://localhost:8080` | public URL players use; every URL MMP hands out is built from it (no trailing slash) |
| `log.level` | `MMP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `MMP_LOG_FORMAT` | `text` | `text` or `json` |
| `db.dsn` | `MMP_DB_DSN` | `postgres://mmp:mmp@localhost:5432/mmp?sslmode=disable` | Postgres connection string |
| `db.max_conns` | `MMP_DB_MAX_CONNS` | `8` | connection pool size |
| `http.read_header_timeout` | `MMP_HTTP_READ_HEADER_TIMEOUT` | `10s` | |
| `http.shutdown_timeout` | `MMP_HTTP_SHUTDOWN_TIMEOUT` | `15s` | grace period for open requests on stop |
| `admin.username` / `admin.password` | `MMP_ADMIN_USERNAME` / `MMP_ADMIN_PASSWORD` | — | create the first admin if no enabled admin exists; never logged |

### Proxy and failover

| Key | Env | Default | Meaning |
|---|---|---|---|
| `proxy.stall_timeout` | `MMP_PROXY_STALL_TIMEOUT` | `12s` | no stream data for this long → fail over. Many providers deliver in bursts with several-second gaps; too short a value makes working streams fail over again and again |
| `proxy.health_window` | `MMP_PROXY_HEALTH_WINDOW` | `15s` | span the bitrate and corruption checks look at (at least `stall_timeout`) |
| `proxy.connect_timeout` | `MMP_PROXY_CONNECT_TIMEOUT` | `7s` | time allowed to connect to a provider and get headers |
| `proxy.low_bitrate_kbps` | `MMP_PROXY_LOW_BITRATE_KBPS` | `20` | fail over below this bitrate; `0` disables the check |
| `proxy.max_cc_error_rate` | `MMP_PROXY_MAX_CC_ERROR_RATE` | `0.05` | fail over above this share of corrupted (continuity-error) packets |
| `proxy.max_retry_window` | `MMP_PROXY_MAX_RETRY_WINDOW` | `30s` | if nothing works for this long, the channel gives up and viewers are disconnected |
| `proxy.cooldown_base` / `proxy.cooldown_max` | `MMP_PROXY_COOLDOWN_BASE` / `_MAX` | `5s` / `10m` | a failed stream is skipped for an exponentially growing time between these |
| `proxy.idle_grace` | `MMP_PROXY_IDLE_GRACE` | `10s` | keep a channel's provider connection this long after its last viewer leaves (fast zapping back) |
| `proxy.buffer_size_mb` | `MMP_PROXY_BUFFER_SIZE_MB` | `32` | stream buffer per active channel |
| `proxy.hls_max_bandwidth_kbps` / `proxy.hls_max_height` | `MMP_PROXY_HLS_MAX_BANDWIDTH_KBPS` / `_HEIGHT` | `0` / `0` | for HLS providers: pick the best variant within these caps (0 = no cap) |
| `proxy.sort_by_health` | `MMP_PROXY_SORT_BY_HEALTH` | `false` | try healthier streams first (needs the prober) instead of strict failover order |
| `proxy.ffmpeg_path` | `MMP_PROXY_FFMPEG_PATH` | `ffmpeg` on `PATH` | ffmpeg used by stream profiles |

### Providers, matching and guide

| Key | Env | Default | Meaning |
|---|---|---|---|
| `refresh.interval` | `MMP_REFRESH_INTERVAL` | `6h` | refresh interval new providers start with; each provider then keeps its own (provider form) |
| `refresh.epg_interval` | `MMP_REFRESH_EPG_INTERVAL` | `12h` | guide download interval |
| `refresh.max_disable_pct` | `MMP_REFRESH_MAX_DISABLE_PCT` | `80` | a refresh that would remove more than this % of a provider's streams is refused (probably a truncated answer); `100` turns the guard off; each provider can override it |
| `match.auto` | `MMP_MATCH_AUTO` | `false` | apply match suggestions automatically after every refresh |
| `match.min_providers` | `MMP_MATCH_MIN_PROVIDERS` | `2` | auto-match only creates channels seen at this many providers |

### Customer output

| Key | Env | Default | Meaning |
|---|---|---|---|
| `hls.enabled` | `MMP_HLS_ENABLED` | `true` | offer `.m3u8` (HLS) streams to players |
| `hls.segment` | `MMP_HLS_SEGMENT` | `2s` | target segment length (shorter = faster start, more requests) |
| `hls.window` | `MMP_HLS_WINDOW` | `6` | segments listed in a live playlist |
| `hls.idle_timeout` | `MMP_HLS_IDLE_TIMEOUT` | `30s` | an HLS client silent this long has left (frees its connection slot) |

### Health prober

The prober periodically samples channel streams to find dead ones before a viewer does. It's off by default
because each probe uses one of the provider's connections.

| Key | Env | Default | Meaning |
|---|---|---|---|
| `probe.enabled` | `MMP_PROBE_ENABLED` | `false` | turn the prober on |
| `probe.interval` | `MMP_PROBE_INTERVAL` | `30m` | probe each channel stream this often |
| `probe.recheck` | `MMP_PROBE_RECHECK` | `1m` | re-probe streams in cooldown this often (a success clears the cooldown) |
| `probe.sample` | `MMP_PROBE_SAMPLE` | `3s` | how long one probe reads |
| `probe.concurrency` | `MMP_PROBE_CONCURRENCY` | `2` | probes at once |
| `probe.ffprobe` | `MMP_PROBE_FFPROBE` | `false` | also require ffprobe to find audio or video (needs ffmpeg) |
| `probe.ffprobe_path` | `MMP_PROBE_FFPROBE_PATH` | `ffprobe` on `PATH` | |

### Movies and series

| Key | Env | Default | Meaning |
|---|---|---|---|
| `vod.info_ttl` | `MMP_VOD_INFO_TTL` | `24h` | re-fetch a movie's details from the panel after this |
| `vod.episode_ttl` | `MMP_VOD_EPISODE_TTL` | `6h` | re-fetch a series' episode list after this |

### Security and monitoring

| Key | Env | Default | Meaning |
|---|---|---|---|
| `auth.session_ttl` | `MMP_AUTH_SESSION_TTL` | `168h` | admin sign-in lifetime |
| `auth.max_attempts` | `MMP_AUTH_MAX_ATTEMPTS` | `10` | failed logins (per client IP and per username) before a lockout; `0` disables it |
| `auth.lockout` | `MMP_AUTH_LOCKOUT` | `5m` | lockout duration |
| `metrics.enabled` | `MMP_METRICS_ENABLED` | `true` | serve `/metrics` |
| `metrics.token` | `MMP_METRICS_TOKEN` | — | if set, scrapes need `Authorization: Bearer <token>` |

### Stream profiles (ffmpeg)

Most channels need nothing special. For a provider whose streams have broken timestamps, or to
transcode, create a profile under **Profiles** (mode *ffmpeg*, e.g. arguments `-c copy`) and pick it in
the channel's settings. MMP still connects to the provider itself and pipes the stream through ffmpeg, so
failover works the same. This needs ffmpeg installed (`mmp-ffmpeg` image).

## Customer URLs

`<base>` is `base_url`. All URLs point to MMP, never to a provider.

| URL | What |
|---|---|
| `<base>/player_api.php?username=U&password=P` | Xtream Codes API (login, categories, streams, short EPG) |
| `<base>/get.php?username=U&password=P&type=m3u_plus` | M3U playlist (`&output=m3u8` for HLS stream URLs) |
| `<base>/xmltv.php?username=U&password=P` | XMLTV guide |
| `<base>/playlist/<token>.m3u` · `<base>/epg/<token>.xml` | the same without the password (token from the user page) |
| `<base>/live/U/P/<id>.ts` · `<base>/t/<token>/<id>.ts` | a channel as MPEG-TS |
| `<base>/live/U/P/<id>.m3u8` · `<base>/t/<token>/<id>.m3u8` | a channel as HLS |
| `<base>/movie/U/P/<id>.<ext>` · `<base>/t/<token>/movie/<id>.<ext>` | a movie (seekable) |
| `<base>/series/U/P/<id>.<ext>` · `<base>/t/<token>/series/<id>.<ext>` | a series episode (seekable) |

Playlists include movies with `&vod=1` and series episodes with `&series=1` (on `get.php` and token playlists). Series load their episodes lazily, so a playlist lists the episodes of shows that were already opened in an Xtream app or the admin UI.

## Admin API

A JSON REST API under `/api/v1` covers everything the UI does: providers, channels, bouquets, matching,
users and stream profiles. It accepts the admin UI session, or an API token:

```sh
# create a token (shown once) while signed in, or with a session cookie from POST /api/v1/auth/login
curl -X POST -b cookies.txt -H 'Content-Type: application/json' -d '{"name":"scripts"}' \
  http://localhost:8080/api/v1/tokens
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/channels
```

Notable endpoints: `POST /providers/{id}/refresh` (`?force=1` to apply a refresh the
`max_disable_pct` guard refused), `GET /match/proposals`, `POST /match/apply`,
`PUT /channels/{id}/streams/order`, `POST /users/{id}/rotate-token`.

Movies and series: set `"vod_enabled": true` on an Xtream provider (`PUT /providers/{id}`), import with
`POST /providers/{id}/vod/refresh` (also runs on the provider's schedule), browse with
`GET /vod/categories?kind=movie|series`, `GET /vod/movies`, `GET /vod/series`, and grant categories with
`POST /bouquets/{id}/vod-categories` (`{"category_ids": [...]}`). Each movie or episode a customer plays
uses one of their connections and one of the provider's.

## Monitoring

- **Dashboard** (`/admin/`): live channels, the provider each one is playing from, viewers, provider
  connection slots, and recent failovers. Updates live.
- **`/healthz`**: liveness for load balancers and the container healthcheck.
- **`/metrics`**: Prometheus metrics: `mmp_sessions{state}`, `mmp_session_viewers`,
  `mmp_customer_connections`, `mmp_provider_slots_in_use{provider_id}`, `mmp_failovers_total`,
  `mmp_upstream_failures_total{reason}`, `mmp_upstream_open_failures_total`,
  `mmp_upstream_bytes_total`, `mmp_viewer_bytes_total`, and more. Example scrape config:

  ```yaml
  scrape_configs:
    - job_name: mmp
      static_configs: [{ targets: ["mmp:8080"] }]
      authorization: { credentials: "<metrics.token>" }   # only if metrics.token is set
  ```

## Deployment notes

- **`base_url` must be right.** Players follow the URLs MMP generates. If playlists open but channels
  don't play, check it first.
- **Postgres:** the compose file publishes port 5432 and uses the password `mmp`, which is convenient for
  development. For production, remove the postgres `ports:` entry and set a real password (in both
  `POSTGRES_PASSWORD` and `MMP_DB_DSN`). Back up with `pg_dump`. All state lives in Postgres.
- **TLS / reverse proxy:** put MMP behind nginx, Caddy or Traefik for HTTPS, and set `base_url` to the
  `https://` address (the admin cookie then becomes `Secure` automatically). Turn off response buffering
  for streams and the dashboard (`proxy_buffering off;` in nginx; MMP also sends `X-Accel-Buffering: no`)
  and allow long-lived responses.
- **Login rate limiting uses the client address MMP sees.** Behind a reverse proxy that address is the
  proxy's, so all clients share one lockout bucket. Raise `auth.max_attempts`, or rate-limit at the proxy.
- **Provider connection limits are enforced.** Set each provider's *Max connections* to what your
  subscription allows: MMP never opens more, and fails over to another provider when one is full.
- **Secrets:** customer passwords are bcrypt-hashed and shown once. Provider credentials and stream URLs
  are masked in logs and in the UI.

## Troubleshooting

| Symptom | Check |
|---|---|
| Playlist loads, channels don't play | `base_url`; the channel has streams (Channels page shows *none*); the Dashboard's recent events (open failures, *down*) |
| "connection limit reached" (HTTP 403) | the customer's *Max connections*. Closed players free their slot within seconds (HLS: `hls.idle_timeout`) |
| A channel keeps switching providers / the picture keeps going blank | Dashboard events show why. `stalled` or `low_bitrate` on streams that play fine elsewhere usually means the provider sends in bursts: raise `proxy.stall_timeout` (and `proxy.health_window`) above its gaps. `eof` right after a switch can be the provider kicking a connection over its limit |
| Channel shows no guide | Guide page → *Only channels without a guide*; set the channel's tvg-id to the guide's channel id; check the source downloaded |
| "refresh refused: would disable …" | the provider returned far fewer streams than before (often temporary). Retry later, or use *Disable anyway* if the change is real |
| Refresh fails with "connection refused" / timeout for a provider on the same machine | inside the container, `localhost` is the container itself: use the host's LAN IP in the provider URL |
| Admin login keeps failing | lockout after `auth.max_attempts` failures lasts `auth.lockout` (default 5 minutes) |
| Lost the admin password | have another admin reset it (Users → Reset password). `MMP_ADMIN_USERNAME`/`MMP_ADMIN_PASSWORD` only create an admin when no enabled admin exists |

## Development

```sh
make test              # unit tests (with -race)
make up                # Postgres for integration tests
make test-integration  # integration tests against Postgres
make lint              # go vet + staticcheck
make generate          # regenerate sqlc code after editing internal/db/queries/*.sql
```

Database migrations live in `internal/db/migrations` (append-only, embedded and applied at start). The
design and build plan are in `Plan/`.
