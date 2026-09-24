# Potok TorrentGo

Standalone BitTorrent streaming engine for Potok: torrent metadata, direct streaming,
HLS, subtitles, thumbnails, and an optional management web UI. This project lives in
`backend/potok-torrentgo/`, alongside `potok-searchengine` and `potok-gateway`.
It does not require the Gateway, SearchEngine, or PostgreSQL to build or run.

## Run with Docker

From this directory:

```sh
cp .env.example .env
docker compose up -d --build
curl http://localhost:5282/health
```

The first build compiles FFmpeg from source. The API listens on port `5282` by default.
Downloads, the management catalog, and the torrent cache use persistent Docker volumes.
Set `TORRENTGO_ENABLE_WEBUI=true` and `POTOK_AUTH_USER` / `POTOK_AUTH_PASS` in `.env`
to enable and protect the management panel. Set `GPU_DEVICE=/dev/dri:/dev/dri` on a Linux
host with an Intel/AMD GPU to enable hardware detection. TorrentGo probes the encoder with
a real frame before selecting it, prefers hardware decode + hardware encode, then software
decode + hardware encode, and uses a two-thread CPU encoder only as the final fallback.
`POTOK_VAAPI_DEVICE` selects a specific render node when several are mapped; the default
`POTOK_VIDEO_TRANSCODE_CONCURRENCY=1` prevents parallel HLS requests from saturating a
small GPU or all CPU cores.

The image name remains `ghcr.io/potok-media/potok-torrentgo:latest`.
The full workspace stack uses this project through `../../docker-compose.local.yml`.
Standalone and full-stack Compose use the same container name; run one at a time.

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

## CI and releases

CI builds the Docker builder stage and runs the Go tests and vet with the pinned
FFmpeg libraries. The **Docker Publish** workflow owns TorrentGo version tags,
publishes its GHCR image with a registry build cache, and creates TorrentGo releases.
The Backend repository publishes only its own Gateway image.

The repository starts with the extracted TorrentGo history from `potok-media/backend`.
This local extraction has no remote configured.
