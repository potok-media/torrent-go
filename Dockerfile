# syntax=docker/dockerfile:1
#
# TorrentGo v2 build. The in-process media core (media/ → go-astiav → libav*) is cgo, so we build ffmpeg
# from source as SHARED libs to link against. Versions are pinned HERE (one place) — never branched in Go.
#   FFMPEG_VERSION — the ffmpeg tag we build the shared libav* from. Pinned to n7.0: go-astiav v0.39.0 is
#   blessed against ffmpeg n7.0, and 7.x buys OUR workload (H.264/HEVC demux+copy-remux, HEVC→H.264 hwaccel,
#   thumbnails, text-subtitle demux) nothing over 8.x while being the mature GA line. n7.0/n7.1 share sonames
#   (libavcodec 61 / libavformat 61 / libavutil 59); we build n7.0 to match go-astiav's blessed target.
# NOTE on jellyfin-ffmpeg: its .deb ships a runtime binary, not the C dev headers cgo needs to LINK — so the
# in-process engine is OUR ffmpeg build. jellyfin-ffmpeg7 (stable, ffmpeg 7.1.x) still ships in runtime as the
# CLI binary (FFMPEG_PATH) for the handlers not yet migrated off exec() + the subtitle bitmap/mov_text
# fallback; its role shrinks as handlers move to media/. (It's exec'd, not linked, so its 7.1.x vs our n7.0
# in-process libs never share linkage.)

ARG FFMPEG_VERSION=n7.0
ARG FFMPEG_PREFIX=/opt/ffmpeg

########## Stage 1 — ffmpeg shared libs (cgo build headers + the media/ core's runtime libs) ##########
FROM debian:bookworm-slim AS ffmpeg
ARG FFMPEG_VERSION
ARG FFMPEG_PREFIX
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends git ca-certificates build-essential yasm nasm pkg-config libx264-dev libva-dev; \
    git clone --depth 1 --branch n12.1.14.0 https://github.com/FFmpeg/nv-codec-headers.git /nv-codec-headers; \
    cd /nv-codec-headers; \
    make install PREFIX=/usr; \
    git clone --depth 1 --branch "${FFMPEG_VERSION}" https://github.com/FFmpeg/FFmpeg /src; \
    cd /src; \
    ./configure --prefix="${FFMPEG_PREFIX}" --enable-shared --disable-static --disable-programs --disable-doc \
        --enable-gpl --enable-libx264 --enable-vaapi --enable-nvenc; \
    make -j"$(nproc)"; \
    make install; \
    rm -rf /src /nv-codec-headers /var/lib/apt/lists/*
# --enable-libx264 provides the software H.264 encoder the transcode path uses (FindEncoder(CodecIDH264)).
# HARDWARE encoders get added next: --enable-vaapi (libva-dev) + --enable-nvenc (nv-codec-headers) so the
# transcode can offload to the GPU; VideoToolbox is macOS-only (local dev builds get it via brew ffmpeg@7).

########## Stage 2 — Go build (cgo, linked against the ffmpeg shared libs above) ##########
FROM golang:1.23-bookworm AS builder
ARG FFMPEG_PREFIX
# libx264-dev: our ffmpeg was built with --enable-libx264, so libavcodec.so has a DT_NEEDED on libx264.so.164.
# libx264 is a Debian system lib (NOT under /opt/ffmpeg), so `COPY --from=ffmpeg ${FFMPEG_PREFIX}` doesn't carry
# it — without this the cgo link fails with `undefined reference to x264_*`. Same bookworm base ⇒ same soname 164.
RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libx264-dev libva-dev && rm -rf /var/lib/apt/lists/*
COPY --from=ffmpeg ${FFMPEG_PREFIX} ${FFMPEG_PREFIX}
ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I${FFMPEG_PREFIX}/include" \
    CGO_LDFLAGS="-L${FFMPEG_PREFIX}/lib" \
    PKG_CONFIG_PATH="${FFMPEG_PREFIX}/lib/pkgconfig" \
    LD_LIBRARY_PATH="${FFMPEG_PREFIX}/lib"
WORKDIR /app
COPY . .
# go.sum is (re)resolved here: the ffmpeg dev libs are present so the cgo media/ package compiles, letting
# `go mod tidy` pin go-astiav/go-astisub/go-astikit. `go build ./...` then link-tests the whole cgo core.
RUN go mod tidy
RUN go build ./...
RUN go build -ldflags="-w -s" -o potok-torrent-go .

########## Stage 3 — runtime ##########
FROM debian:bookworm-slim
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates tzdata libchromaprint-tools libx264-164 libva2 libva-drm2 intel-media-va-driver mesa-va-drivers; \
    rm -rf /var/lib/apt/lists/*
# libx264-164: our /opt/ffmpeg libavcodec.so links libx264 at runtime (--enable-libx264). It's a Debian system
# lib, not part of the copied /opt/ffmpeg, so the runtime needs it or the in-process media/ engine can't load.
# Our ffmpeg n7.0 shared libs (built above) — what the in-process media/ (go-astiav) binary links at
# runtime. There is NO ffmpeg/ffprobe CLI anymore: HLS segmentation, probing, thumbnails, subtitles and
# the intro fingerprint's audio decode are all in-process libav over the torrent cache. The only external
# media tool left is `fpcalc` (from libchromaprint-tools) for intro/outro acoustic fingerprinting — it has
# no in-process Go equivalent and links its own system libav, kept separate from ours by LD_LIBRARY_PATH.
COPY --from=ffmpeg /opt/ffmpeg/lib /opt/ffmpeg/lib
ENV LD_LIBRARY_PATH=/opt/ffmpeg/lib

WORKDIR /app
COPY --from=builder /app/potok-torrent-go .

EXPOSE 5282
EXPOSE 55123/udp

VOLUME ["/app/torrent-cache"]

CMD ["./potok-torrent-go"]
