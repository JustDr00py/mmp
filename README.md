# MMP

MMP is a self-hosted IPTV multiplexer with automatic failover. It pulls live channels from several IPTV
providers (M3U playlists or Xtream Codes panels), groups the same channel from different providers into
one logical channel, and gives your viewers a single stream per channel. When the provider that is playing
stalls, errors or degrades, MMP switches to the next one **without dropping the viewer's connection**.

- One upstream connection per channel, shared by every viewer of it, so provider connection limits go further.
- Works with any IPTV player: M3U + XMLTV (VLC, Kodi, Plex, Jellyfin) and an Xtream Codes compatible API
  (IPTV Smarters, TiviMate, XCIPTV…). MPEG-TS and HLS output.
- Customer accounts with connection limits, expiry dates and per-user channel sets ("bouquets"); channel
  groups become the categories players show.
- Movies and series from Xtream providers, passed through to Xtream apps with seeking (opt-in per provider).
- Web admin UI with a live dashboard (bitrate, stream details and viewers per channel; switch a channel's
  stream, stop it or disconnect a viewer), Prometheus metrics, and a REST API.
- Single static binary; Postgres is the only dependency (ffmpeg optional).

![Dashboard: a card per live channel with its stream, media details, bitrate and viewers](docs/screenshots/dashboard.png)

**Contents:** [Quick start](#quick-start-docker) · [From zero to playback](#from-zero-to-playback) ·
[Running from source](#running-from-source) · [Configuration](#configuration) ·
[Customer URLs](#customer-urls) · [Admin API](#admin-api) · [Monitoring](#monitoring) ·
[Deployment notes](#deployment-notes) · [Troubleshooting](#troubleshooting) · [Development](#development)

---

## Quick start (Docker)

You need Docker (or Podman) with Compose. Releases are published as images for amd64 and arm64
(e.g. a Raspberry Pi 4/5): `ghcr.io/justdr00py/mmp:<version>`, plus `<version>-ffmpeg` with ffmpeg.

1. **Get the compose files** at the release you want (see
   [Releases](https://github.com/JustDr00py/mmp/releases)):

   ```sh
   git clone --branch v0.1.0 https://github.com/JustDr00py/mmp.git && cd mmp
   ```

2. **Create your `.env`** (it holds the secrets and is ignored by git):

   ```sh
   cp .env.example .env
   ```

   Then edit it:

   - `POSTGRES_PASSWORD`: a long random password, e.g. the output of `openssl rand -hex 24`.
   - `MMP_BASE_URL`: the address **players** will use to reach MMP, e.g. `http://192.168.1.10:8080`
     or `https://tv.example.com`. Every playlist, guide and stream URL MMP hands out is built from it, so
     `localhost` only works for players on the same machine.
   - `MMP_ADMIN_USERNAME` / `MMP_ADMIN_PASSWORD`: your admin login. They are only used to create the first
     admin when none exists; change passwords later on the **Users** page. Keep values containing `$` in
     single quotes.

   - `MMP_IMAGE`: pin the release, e.g. `ghcr.io/justdr00py/mmp:v0.1.0`. Use `v0.1.0-ffmpeg` for the image
     with ffmpeg (needed only for [stream profiles](#stream-profiles-ffmpeg) in ffmpeg mode, the
     prober's ffprobe check and sound for AC-3 channels in the admin preview player).

   Postgres listens on `127.0.0.1` only.

3. **Start it**

   ```sh
   docker compose pull               # Podman: podman compose pull
   docker compose up -d --no-build   # Podman: podman compose up -d --no-build
   ```

   (To build from source instead: `docker compose up -d --build`.)

   MMP applies its database migrations on start; `docker compose logs mmp` shows `server listening` when it is ready.

4. **Open the admin UI** at `http://<your-host>:8080/admin` and sign in with the admin login from your `.env`.

Before exposing MMP beyond your own network, read [Deployment notes](#deployment-notes) (Postgres
password and port, TLS).

### Through a VPN (optional)

`docker-compose.vpn.yml` runs the same stack with all of MMP's outgoing traffic (playlists, streams,
movies, guides) sent through a VPN by [gluetun](https://github.com/qdm12/gluetun). Players on your
network still connect to MMP directly.

1. Fill in the **VPN** section of `.env` (see `.env.example`): your provider, WireGuard key and address or
   OpenVPN login, and optionally a server country or city near your IPTV providers.
2. Use this file **instead of** `docker-compose.yml` (stop the other stack first; the database is kept):

   ```sh
   docker compose down
   docker compose -f docker-compose.vpn.yml up -d --build
   ```

If the tunnel drops, gluetun's firewall blocks MMP's internet traffic until it is back, so nothing
reaches your providers unprotected; streams fail over or wait meanwhile. Check the tunnel with
`docker compose -f docker-compose.vpn.yml logs gluetun`. After recreating gluetun, also recreate mmp
(`... up -d --force-recreate mmp`), since it lives in gluetun's network. Rootless Podman may need
`WIREGUARD_IMPLEMENTATION=userspace`.

### Upgrading

Set `MMP_IMAGE` in `.env` to the new version, then:

```sh
git pull --tags && git checkout vX.Y.Z   # newer compose files, if the release notes say so
docker compose pull && docker compose up -d --no-build
```

Coming from v0.2.2 or older, run `docker compose down` before `up -d` once: the compose file now pins
its Docker network (see [Deployment notes](#deployment-notes)), and a plain `up -d` can leave postgres
unreachable by name.

Migrations run on start, so back up first (**Backups → Back up now**). To roll back, set the previous
version and run the same commands; if the newer release changed the database, restore the backup you
made before upgrading.

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

A **channel** is what your viewers see. It has an ordered failover list of provider streams.

First, trim what each provider offers: big panels carry hundreds of regional groups. On **Providers**, press
**Groups** on a provider, type in the filter (e.g. `US`), tick the groups you want and save with
*Only the groups ticked below*. Match and the stream search then only offer streams from those groups;
groups the provider adds later stay hidden until you tick them. Channels already using a stream keep it.
A provider added on the Providers page starts with **no** groups chosen (its row says *choose groups*), so
its first refresh imports streams without matching any; saving its groups runs automatic matching (when
on) with them right away. Pick *All groups* to use everything.
(API: `GET`/`PUT /api/v1/providers/{id}/groups`.)

Two ways to build channels:

- **Match** (fastest): MMP finds the same channel at your different providers (same tvg-id, same name once
  prefixes like `US:` and tags like `HD` are ignored, or one of your rules) and builds it with failover in one step.
  1. Narrow the list: type in **Search**, pick a **Provider group** (e.g. `US| SPORTS`: only that group's
     streams are used), and choose **Carried by** (*2+ providers* = channels that get failover; *any provider*
     also lists channels only one provider has).
  2. Under **When applying**, choose the channel group new channels go in (by default one named after the
     provider group, so players keep the provider's categories) and tick the bouquets to add them to (any
     number, or **all**), so customers see them right away.
  3. Check the suggestions. You can rename a new channel or untick individual streams on its row (editing a row
     selects it). Then press **Apply selected**, or **Apply all N matching** for everything the filter shows,
     across pages.
  A `+` ending a name counts (ESPN+, AMC+ and Paramount+ are not ESPN, AMC and Paramount). Event slots, which
  providers rename for every event (`US - ESPN PLUS 01 : WSL Pro Portugal OCT 22 – 3:30 AM ET`,
  `College: SMU vs Arizona State @ Oct 22 5:00 PM :ESPN+  14`), become channels named after the slot
  (`ESPN PLUS 01`, `ESPN+ 14`), not the event on when you matched; renamed slots stay on their channel.
  Applied suggestions leave the list. **Advanced: matching rules** handles channels whose names differ too much
  between providers: a regular expression (e.g. `lakers`) puts every matching stream under one channel name.
  When the same channel shows up as two suggestions (one provider uses another tvg-id), tick both and press
  **Merge selected**. If the channel already exists, tick the suggestion, pick the channel in **Add to channel**
  and press the button: the streams join it, and new streams like them are offered to it later. Merges are kept
  across provider refreshes; undo them under **Advanced**.

  ![Match: the same channel found at several providers, ready to become one channel with failover](docs/screenshots/match.png)

- **By hand**: **Channels → New channel**, then search streams across all providers and **Add** them.

On a channel's page, drag streams (or use ↑/↓) to set the failover order. The first available stream
plays. Set the channel's **tvg-id** to the guide's channel id to get program data (see the **Guide**
page for coverage).

**Guide sources:** Xtream providers bring their own guide, M3U providers can have an EPG URL, and on the
**Guide** page you can add **extra guide sources**: any XMLTV URL (`.xml` or `.xml.gz`), such as a community
guide with better coverage. They download on the guide schedule (`refresh.epg_interval`). When several
sources list a channel, the lowest priority number wins among those whose schedule still runs past now: a
guide that stopped updating never hides a current one, whatever its priority. A provider's guide uses the
provider's priority unless you give it its own on the Guide page, so you can rank guides without changing
the failover order. The channel coverage table there shows which guide each channel gets.

**Guide from stream names:** event feeds (PPV, game passes) rarely have a guide, but their stream names
often say what is on: `Next | Jets @ Patriots | NFL Game Pass | 2026-10-18 | 17:00`. For channels without a
guide, MMP reads the name of the channel's first stream and builds one: "Up next: Jets @ Patriots" for a
day before, the event for three hours from its start, then "Ended: …" for a day. It understands a status
(`Next`, `Live`, `End`…), ISO or written dates (`Sun 18 Oct`), 12- and 24-hour times and zones such as
`GMT`, `CEST`, `ET` or `PT`; times without a zone use the zone set under **Settings → Guide from stream
names**. Event slots named like `US - ESPN PLUS 01 : WSL Pro Portugal OCT 22 – 3:30 AM ET / 8:30 AM UK`
or `College: SMU vs Arizona State @ Oct 22 5:00 PM :ESPN+  14` work too: the event beside the slot's label,
starting at the first time given. Names without a time give an
all-day entry, and other channels without a guide get one with their name (both can be turned off there).
A stream's event-slot title wins over the channel's name, so a channel named after an old event shows the
stream's current one. Channels without a tvg-id get one (`mmp.<id>`) in playlists so
players link them to it. Real guides win, unless you say otherwise: a channel's **Guide** setting (or its
group's, on the **Groups** page) is **Auto** (the XMLTV guide, else the stream name), **Stream name** (the
stream-name guide first, for event channels whose XMLTV only has placeholders like "No Event Scheduled";
it works even with the stream-name guide turned off) or **XMLTV only**.

**East/West and +1 feeds:** providers often give a channel's East and West (or `+1`) feeds the same tvg-id.
Match keeps them apart (East and unlabelled feeds go together; West/Pacific, Mountain and `+1`…`+9` are
separate channels), so a channel never fails over into a feed that airs hours later.

**One tvg-id, several channels:** some providers also reuse a tvg-id for different channels, such as
Spectrum SportsNet's Lakers and Dodgers feeds. When a provider lists differently named streams under one
tvg-id, Match only joins streams with that tvg-id if their names match too.

**Channel groups** are the categories players show (Xtream categories, M3U `group-title`); a channel without
one appears under *Uncategorized*. Bouquets decide *which* channels a customer gets, groups only arrange
them. On **Groups**, create, rename, reorder (sort order = order in the player) and delete groups (deleting
one asks whether its channels stay, without a group, or go with it), or press **Use provider groups** to put
every channel without a group into one named after its provider group. On **Channels**, tick channels and
**Move selected to…** a group, no group, or their provider group, or **Delete selected**; the **No group**
filter finds the uncategorized ones.

![Groups: the categories players show, with their sort order and channel counts](docs/screenshots/groups.png)

**Keep it up to date automatically:** on **Settings**, turn on *Match channels after each provider refresh*.
After every refresh, new streams join the channels they match as extra failover streams. With *Create
channels for new streams* on, a channel is also created once enough providers offer it, in the group and
bouquets you choose there. Existing channels never change group or bouquets.

**Labelled names:** if a provider puts a label before every name (`AT&T: MTV`), list `AT&T` under
**Settings → Name prefixes to ignore**, and those streams match the plain `MTV`. Superscript tags such as
`ᴿᴬᵂ` or `ᴴᴰ` are always left out when matching.

**See what is playing:** the ▶ next to a stream on Match, on a channel's page and in its stream search, or
next to a channel on **Channels**, opens a small player in the corner (drag it by its title bar; Esc or ✕
closes it). A stream preview uses one of the provider's connections (never more than its limit) unless a
channel is already playing that stream; a channel preview joins the channel like any viewer. Previews stop
after 15 minutes and show on the dashboard as *PREVIEW*. With ffmpeg installed, the audio is converted to
AAC so channels with AC-3 sound are not silent in the browser; the video is passed through as it is, so
HEVC streams only play in browsers that support HEVC.

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

**By language:** providers label categories with a language (`EN | Action`, `|FR| Films`, `ENGLISH MOVIES`).
MMP reads that label: **Movies & Series** has a language filter, and a bouquet can add every movie and/or
series category in one language at once. Categories without a recognised label are listed as *No language
label*; add those by hand.

**Stopping imports:** unticking **Import movies & series** hides that provider's catalog from customers but
keeps it stored, so ticking it again restores everything. To free the space, use **Delete imported catalog**
on the provider's edit page (with importing off). It removes the movies, series, episodes and categories, and
those categories leave bouquets; the provider's live streams stay.

## Running from source

Requirements: Go 1.26+ and PostgreSQL (tested with 17, which the compose file uses).

```sh
cp .env.example .env         # set POSTGRES_PASSWORD; make and compose both read it
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

Some settings live on the admin UI's **Settings** page instead, and take effect without a restart. They are
stored in the database, so backups include them. For those, the configuration file and environment only
give the starting values: they are copied in on the first start, and later changes to them are ignored.
These are marked *Settings page* below. Today that is automatic matching.

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
| | `TZ` | `UTC` | time zone for log timestamps and the times the admin UI shows, e.g. `America/Los_Angeles` (the compose files pass it from `.env`) |
| `db.dsn` | `MMP_DB_DSN` | `postgres://mmp:mmp@localhost:5432/mmp?sslmode=disable` | Postgres connection string |
| `db.max_conns` | `MMP_DB_MAX_CONNS` | `8` | connection pool size |
| `http.read_header_timeout` | `MMP_HTTP_READ_HEADER_TIMEOUT` | `10s` | |
| `http.shutdown_timeout` | `MMP_HTTP_SHUTDOWN_TIMEOUT` | `15s` | grace period for open requests on stop |
| `http.trusted_proxies` | `MMP_HTTP_TRUSTED_PROXIES` | none | reverse proxies (IPs or CIDRs, comma-separated in the env var) whose `X-Forwarded-For` names the real client; see below |
| `admin.username` / `admin.password` | `MMP_ADMIN_USERNAME` / `MMP_ADMIN_PASSWORD` | — | create the first admin if no enabled admin exists; never logged |

### Proxy and failover

A stream that played well for 30 seconds and is then dropped by the provider is reopened once before
MMP fails over, since providers close connections now and then. A provider that answers *429 Too Many
Requests* (no free connection on the account) is skipped for its `Retry-After` (10s if it sends none,
30s at most) without counting against the stream. When a stream fails while playing, the other streams
on its provider's **backbone** go to the back of the failover list for 5 minutes, since resellers of the
same network usually carry the same broken feed; the failover event says when it skipped them. Streams
without a backbone are never moved.

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
| `match.auto` | `MMP_MATCH_AUTO` | `false` | *Settings page.* Apply match suggestions automatically after every refresh |
| `match.min_providers` | `MMP_MATCH_MIN_PROVIDERS` | `2` | *Settings page.* Auto-match only creates channels seen at this many providers |
| `match.auto_new_channels` | `MMP_MATCH_AUTO_NEW_CHANNELS` | `true` | *Settings page.* `false`: auto-match only adds new streams to channels you already have (extra failover), never creates channels |
| `match.auto_group` | `MMP_MATCH_AUTO_GROUP` | `source` | *Settings page.* Channel group for channels auto-match creates: `source` (named after the provider group), `none`, or a group name (created if missing). Existing channels keep their group |
| `match.auto_bouquets` | `MMP_MATCH_AUTO_BOUQUETS` | `auto` | *Settings page.* Bouquets for channels auto-match creates. `auto`: the bouquets holding most (over half) of the other channels in the new channel's group, none if the group has no other channels; `none`; or bouquet names (comma-separated in the env var). Existing channels that only gain a stream keep their bouquets |

### Customer output

| Key | Env | Default | Meaning |
|---|---|---|---|
| `hls.enabled` | `MMP_HLS_ENABLED` | `true` | offer `.m3u8` (HLS) streams to players |
| `hls.segment` | `MMP_HLS_SEGMENT` | `2s` | target segment length (shorter = faster start, more requests) |
| `hls.window` | `MMP_HLS_WINDOW` | `6` | segments listed in a live playlist |
| `hls.idle_timeout` | `MMP_HLS_IDLE_TIMEOUT` | `30s` | an HLS client silent this long has left (frees its connection slot). An HLS stream link only plays on the network that opened it (plus its IPv4/IPv6 counterpart), so sharing it does not get around *Max connections* |

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
| `probe.ffprobe_path` | `MMP_PROBE_FFPROBE_PATH` | `ffprobe` on `PATH` | also used for the dashboard's stream details, prober on or off |

### Movies and series

| Key | Env | Default | Meaning |
|---|---|---|---|
| `vod.info_ttl` | `MMP_VOD_INFO_TTL` | `24h` | re-fetch a movie's details from the panel after this |
| `vod.episode_ttl` | `MMP_VOD_EPISODE_TTL` | `6h` | re-fetch a series' episode list after this |

### Security and monitoring

| Key | Env | Default | Meaning |
|---|---|---|---|
| `auth.session_ttl` | `MMP_AUTH_SESSION_TTL` | `168h` | admin sign-in lifetime |
| `auth.max_attempts` | `MMP_AUTH_MAX_ATTEMPTS` | `10` | failed logins before a lockout, counted per client address (IPv6 per /64) and per username; `0` disables it. A username lockout does not apply where that user logged in successfully in the last 30 days, so a stranger guessing a customer's password cannot lock the customer's own players out |
| `auth.lockout` | `MMP_AUTH_LOCKOUT` | `5m` | lockout duration |
| `metrics.enabled` | `MMP_METRICS_ENABLED` | `true` | serve `/metrics` |
| `metrics.token` | `MMP_METRICS_TOKEN` | — | if set, scrapes need `Authorization: Bearer <token>` |

### Stream profiles (ffmpeg)

Most channels need nothing special. For a provider whose streams have broken timestamps, or to
transcode, create a profile under **Profiles** (mode *ffmpeg*, e.g. arguments `-c copy`) and pick it in
the channel's settings. MMP still connects to the provider itself and pipes the stream through ffmpeg, so
failover works the same. This needs ffmpeg installed (`mmp-ffmpeg` image).

## Backups

**Backups** (in the sidebar under Settings) holds everything you set up: providers, channels, bouquets, users,
movies & series and settings. Guide data and admin login sessions are left out; guides download again
after a restore. Backups contain provider logins and customer tokens, so keep downloaded copies safe.

- **Back up now** writes a backup into the backup folder (`MMP_BACKUP_DIR`, in Docker the `backups`
  volume at `/var/lib/mmp/backups`). Download any backup from the list.
- **Automatic backups** are off until you turn them on: every 6 or 12 hours, daily or weekly (Sundays) at a
  time you choose, keeping the last *N*. Manual, uploaded and safety backups are only deleted by you.
- **Restore** replaces all data with a backup after you type `RESTORE`. MMP first saves the current data as a
  `pre-restore` backup, then restores, keeps your automatic-backup setting, and restarts itself (streams
  pause for a few seconds; everyone signs in again). Backups from older MMP versions restore and are
  upgraded; backups from newer versions are refused.
- **Upload** a backup file to restore it on another machine (e.g. moving to production).

From the command line, for when the admin UI is not usable:

```sh
docker compose exec mmp mmp -backup                                # writes a manual backup
docker compose stop mmp
docker compose run --rm mmp -restore /var/lib/mmp/backups/<file>   # replaces all data
docker compose start mmp
```

To copy backups off the server: `docker compose cp mmp:/var/lib/mmp/backups ./backups`.

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

- **Dashboard** (`/admin/`): a card per live channel with its uptime, stream profile, what the guide
  says is on, the stream it plays (pick another and press play to switch by hand; a manual switch
  is not a failure, so nothing cools down), current and average bitrate, data received, and who is
  watching (IP, user, connected since). Stop a channel or disconnect a viewer from the card; players
  may reconnect. Codecs come from the stream itself; resolution, frame rate and audio layout need
  `ffprobe` (the `-ffmpeg` image, or `probe.ffprobe_path`). Also provider connection slots and recent
  events. Updates live.
- **Events** (`/admin/events`): every failover with the streams and providers involved and why it
  happened (no data, low bitrate, a corrupted stream, the provider ending it), switches by an admin,
  streams that would not open (with the error) and channels that went down. Filter by type, provider
  or channel (a channel's page links to its own). Kept 30 days by default; change it on **Settings**.
- **Customer activity** (each user's page): when and from where they were last seen, the networks
  they used (a warning when three or more were active within a day, a sign of a shared login), the
  live channels they watched (start, length, address, TS or HLS) and wrong passwords with the address
  they came from. The Users list shows everyone's last seen. Successful logins are recorded at most
  once a minute per network, so busy players add little. This stores customers' IP addresses: it is
  kept as long as events and deleted with the user.
- **Audit log** (`/admin/audit`): who did what and from which address: admin sign-ins and failed
  attempts, every change made in the admin UI or through the API (with the item's name, read before a
  delete removes it), and backup downloads. Changes that were refused are not listed, and what was
  typed into forms (passwords included) is never stored. Filter by admin or by what was changed.
  Kept as long as events.
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
- **Postgres:** the password comes from `POSTGRES_PASSWORD` in `.env`, and the port is published on
  `127.0.0.1` only (for local tools and tests); remove the postgres `ports:` entry if nothing on the host
  needs it. Postgres stores the password in its volume on first start, so changing it later also needs
  `ALTER ROLE mmp PASSWORD '...'` inside the database. Back up with `pg_dump`. All state lives in Postgres.
- **TLS / reverse proxy:** put MMP behind nginx, Caddy or Traefik for HTTPS, and set `base_url` to the
  `https://` address (the admin cookie then becomes `Secure` automatically). Turn off response buffering
  for streams and the dashboard (`proxy_buffering off;` in nginx; MMP also sends `X-Accel-Buffering: no`)
  and allow long-lived responses.
- **Client addresses behind a reverse proxy:** MMP sees the proxy's address on every connection, so the
  dashboard lists every viewer under it and all clients share one login-lockout bucket. List the proxy
  in `http.trusted_proxies`: the address MMP sees it connect from, which is what the dashboard shows
  for every viewer until you set this. MMP then takes the client from `X-Forwarded-For`, which Caddy,
  nginx (`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`) and Traefik set. Only
  connections from those addresses are believed; anyone else's headers are ignored, so trust nothing
  that clients can reach directly.

  With the compose files and the proxy installed on the host, proxying to `localhost:8080`, MMP sees the
  Docker network's gateway. Set in `.env`:

  ```
  MMP_HTTP_BIND=127.0.0.1                # publish MMP to this host only: players go through the proxy
  MMP_HTTP_TRUSTED_PROXIES=172.28.0.1    # docker-compose.yml's gateway (172.29.0.1 with the VPN file)
  ```

  `docker-compose.yml` pins its network to `172.28.0.0/24` (`MMP_SUBNET`), so the gateway stays the
  same across `down`/`up`. A stack created before that pin must move to it with `docker compose down`
  then `up -d`, once: a plain `up -d` recreates the network but can leave postgres without its name
  on it (MMP then logs `lookup postgres ... no such host`; `docker compose up -d --force-recreate
  postgres` fixes that). If the proxy runs in a container instead, trust that container's address. Caddy on the host:

  ```
  tv.example.com {
      reverse_proxy localhost:8080 {
          flush_interval -1
      }
  }
  ```
- **Provider connection limits are enforced.** Set each provider's *Max connections* to what your
  subscription allows: MMP never opens more, and fails over to another provider when one is full.
- **Secrets:** customer passwords are bcrypt-hashed and shown once. Provider credentials and stream URLs
  are masked in logs and in the UI.

## Troubleshooting

| Symptom | Check |
|---|---|
| Playlist loads, channels don't play | `base_url`; the channel has streams (Channels page shows *none*); the **Events** page (open failures with their error, *down*) |
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
make test-integration  # integration tests against Postgres (password from .env; MMP_TEST_PG overrides)
make lint              # go vet + staticcheck
make generate          # regenerate sqlc code after editing internal/db/queries/*.sql
```

Database migrations live in `internal/db/migrations` (append-only, embedded and applied at start). The
design and build plan are in `Plan/`.

## Releasing

Push a tag such as `v0.2.0`. GitHub Actions runs the full test suite (with Postgres), builds the amd64
and arm64 images, pushes `ghcr.io/justdr00py/mmp:v0.2.0`, `:0.2`, `:latest` (and the `-ffmpeg`
variants), and creates the GitHub release. `v0.*` and tags with a suffix (`v1.0.0-rc1`) are marked as
pre-releases; suffixed tags do not move `latest`.

## License

MMP is free software under the [GNU Affero General Public License v3.0](LICENSE): you may use, change
and share it, and if you offer a modified version to others over a network, you must publish your
changes under the same license.
