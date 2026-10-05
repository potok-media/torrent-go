# Potok TorrentGo

Standalone BitTorrent streaming engine for Potok: torrent metadata, direct streaming,
HLS, subtitles, thumbnails, and an optional management web UI. This project lives in
`backend/potok-torrentgo/`, alongside `potok-searchengine` and `potok-gateway`.
It does not require the Gateway, SearchEngine, or PostgreSQL to build or run.

## Deploy

From this directory:

```sh
cp .env.example .env
docker compose up -d --build
curl http://localhost:5282/health
```

The first build compiles FFmpeg from source. The API listens on port `5282` by default;
the BitTorrent engine listens on `55123` (TCP+UDP, optionally published for inbound peer
connections). Downloads, the management catalog, and the torrent cache use persistent
Docker volumes (`torrentgo-downloads` → `/app/downloads`, `torrentgo-data` → `/app/data`,
`torrentgo-cache` → `/app/torrent-cache`). Set `TORRENTGO_ENABLE_WEBUI=true` and
`POTOK_AUTH_USER` / `POTOK_AUTH_PASS` in `.env` to enable and protect the management
panel. Set `GPU_DEVICE=/dev/dri:/dev/dri` on a Linux host with an Intel/AMD GPU to
enable hardware detection. TorrentGo probes the encoder with a real frame before
selecting it, prefers hardware decode + hardware encode, then software decode +
hardware encode, and uses a two-thread CPU encoder only as the final fallback.
`POTOK_VAAPI_DEVICE` selects a specific render node when several are mapped; the default
`POTOK_VIDEO_TRANSCODE_CONCURRENCY=1` prevents parallel HLS requests from saturating a
small GPU or all CPU cores.

The published image is `ghcr.io/potok-media/potok-torrentgo:latest` — for production,
pin a version tag and swap the Compose `build:` block for `image:`.
The full workspace stack uses this project through `../../docker-compose.local.yml`.
Standalone and full-stack Compose use the same container name; run one at a time.

## Environment

All configuration is environment variables (Compose maps them from `.env`):

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `5282` | HTTP API port |
| `POTOK_LISTEN_PORT` | `55123` | BitTorrent peer listen port (TCP+UDP) |
| `POTOK_LOG_LEVEL` | `info` | Log level: `debug` / `info` / `warn` / `error` |
| `TORRENTGO_ENABLE_WEBUI` | `false` | Mount the management web UI + `/api/manage/*` API |
| `POTOK_AUTH_USER` / `POTOK_AUTH_PASS` | empty | BasicAuth for the management contour (no-op if unset) |
| `POTOK_CONNS_PER_TORRENT` | `250` | Peer connections per torrent |
| `POTOK_HALF_OPEN_CONNS` | `120` | Half-open connection limit |
| `POTOK_CACHE_SIZE_MB` | `256` | Initial global piece-cache budget (RAM); runtime-adjustable via the settings API |
| `POTOK_HLS_CACHE_MB` | `256` | HLS segment cache size |
| `POTOK_MEM_LIMIT_MB` | `0` | Soft Go heap limit; `0` = none (respect external `GOMEMLIMIT`) |
| `POTOK_PRELOAD_MB` | `20` | Bytes preloaded per file on playback start |
| `POTOK_THUMB_CACHE_SIZE` | `200` | Thumbnail cache entries |
| `POTOK_THUMB_CACHE_TTL` | `5m` | Thumbnail cache entry TTL |
| `POTOK_TORRENT_IDLE_TIMEOUT` | `60s` | Grace before a torrent with no playback session is dropped |
| `POTOK_SESSION_TTL` | `25s` | Playback keepalive expiry |
| `POTOK_DISABLE_ANALYZER` | `false` | Disable the intro/outro timecode analyzer |
| `POTOK_DOWNLOAD_DIR` | `downloads` | Disk-mode piece storage (`<hash>.dat` + `.bitmap`); empty disables disk mode |
| `POTOK_DATA_DIR` | empty | Management catalog + persisted settings; empty = in-memory only |
| `TMDB_API_KEY` | built-in | TMDB v3 api_key for the Add-torrent lookup; empty returns 501 |
| `POTOK_DISABLE_HWACCEL` | `false` | `1`/`true` disables GPU probing entirely |
| `POTOK_VAAPI_DEVICE` | auto | Specific VAAPI render node (e.g. `/dev/dri/renderD128`) when several are mapped |
| `POTOK_VIDEO_TRANSCODE_CONCURRENCY` | `1` | Max parallel video transcodes (clamped to 8) |

`GPU_DEVICE` is a Compose-only variable (device mapping), not read by the app.

## API

Two contours:

- **Plugin/streaming routes are open** (no auth) — players and the gateway hit them
  without credentials: `POST /api/torrents`, per-torrent status/files/metadata,
  progressive `/stream`, the HLS tree (`.../hls/master.m3u8` + renditions), subtitles,
  thumbnails, and the playback keepalive/stop lifecycle. CORS is permissive (`*`).
- **`/api/manage/*` is the management contour** for the standalone web UI, mounted
  only with `TORRENTGO_ENABLE_WEBUI=true` and protected by HTTP Basic auth
  (`POTOK_AUTH_USER`/`POTOK_AUTH_PASS`): torrent list, stats, settings, diagnostics,
  TMDB lookup, library save/download, pin control. These operations stay in the spec
  but are marked `x-scalar-ignore`, so the interactive reference shows only the
  plugin-facing API.

Errors are `text/plain` via `http.Error`; the one exception is metadata-resolution
timeout, which returns `504` with a JSON `{error: "METADATA_TIMEOUT"}` body.

Interactive docs (Scalar UI) live at **`/docs`**, the machine-readable spec at
**`/openapi.yaml`** — both unauthenticated and always on.

The spec is generated from code annotations with
[swaggo/swag](https://github.com/swaggo/swag) and committed at `docs/swagger.yaml`.
Regenerate after touching handler annotations or DTOs:

```sh
go run github.com/swaggo/swag/cmd/swag@v1.16.4 init -g main.go --outputTypes yaml -o docs
```

CI runs the same command and fails on `git diff docs/`, so the committed spec can
never drift from the code. swag is invoked via `go run` — it adds no dependency
to `go.mod`.

## Build and test

Requirements: Go 1.23 or newer, a C compiler, `pkg-config`, and FFmpeg 7 development
headers and shared libraries compatible with `go-astiav v0.39.0`. The Dockerfile pins
FFmpeg `n7.0` and provides a reproducible build environment:

```sh
docker build --target builder -t potok-torrentgo:test .
docker run --rm potok-torrentgo:test go test ./...
docker run --rm potok-torrentgo:test go vet ./...
```

For a native build with those dependencies installed:

```sh
go test ./...
go vet ./...
go build -o potok-torrentgo .
./potok-torrentgo
```

On macOS with Homebrew FFmpeg 7, set
`PKG_CONFIG_PATH="$(brew --prefix ffmpeg@7)/lib/pkgconfig"` before these commands.
Some media integration tests require a local sample: use `POTOK_TEST_MEDIA` for the
segment round-trip test and `POTOK_SEEK_DIAG` for the seek/tiling diagnostics.
Note: `TestToneMapFiltersAreIncludedInFFmpegBuild` requires the VAAPI-enabled FFmpeg
build and only passes in the Docker builder image (Linux), not on macOS Homebrew.

## Logging

All logs are `log/slog` text on stdout; `POTOK_LOG_LEVEL` (`debug` / `info` / `warn` /
`error`, default `info`) sets the app level. The level and message are ANSI-colored by
severity (debug gray, info green, warn yellow, error standard red). Logging policy is
fixed in code:

- The in-process FFmpeg (libav) is clamped to errors and bridged into slog — its probe
  noise (unknown attachment codecs, analyzeduration hints, swscaler deprecations) never
  reaches the log.
- The HTTP access log mutes high-frequency polls on success (`/health`, playback
  keepalive/stop, thumbnails, HLS playlists+segments, torrent status polls); any status
  ≥ 400 is always logged (≥ 500 as error).

At `info` you see lifecycle events: torrent add/resolve/drop (with reason and
final stats), playback session start/stop/expiry and 429 rejections, stream
starts, HLS grid decisions (copy vs transcode), and all warnings/errors.
The transcode pipeline's codec/device selection (`video transcoder opened` /
`audio transcoder opened` with decoder, encoder, hardwareDecode, provider) is
logged once per unique pipeline — repeated per-segment opens stay at `debug`,
along with per-request segment/thumbnail/keepalive traffic and speed snapshots.

## CI and releases

CI builds the Docker builder stage and runs the Go tests and vet with the pinned
FFmpeg libraries. The **Docker Publish** workflow owns TorrentGo version tags,
publishes its GHCR image with a registry build cache, and creates TorrentGo releases.
The Backend repository publishes only its own Gateway image.

The repository starts with the extracted TorrentGo history from `potok-media/backend`.
This local extraction has no remote configured.

🔗 [Live](https://potok.rip) · [Wiki](https://potok.rip/wiki) · [GitHub](https://github.com/potok-media/torrent-go) · [DeepWiki](https://deepwiki.com/potok-media/torrent-go)
