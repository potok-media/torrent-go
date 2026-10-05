package handlers

import (
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// statusPollPath matches GET /api/torrents/{hash} exactly (the player's status poll) — NOT sub-paths
// like /diagnostics or /files/..., which are rare and stay logged.
var statusPollPath = regexp.MustCompile(`^/api/torrents/[0-9a-fA-F]{40}$`)

// AccessLog is the HTTP access log, replacing chi's middleware.Logger (whose stdlib format and
// all-or-nothing paths flooded the container log with keepalive/thumbnail/HLS-segment polls).
// High-frequency polling endpoints are muted on success but ALWAYS logged on failure (status >= 400),
// so a broken keepalive or thumbnail never goes silent.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // implicit 200 on a bare Write
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		}
		switch {
		case status >= 500:
			slog.Error("http", attrs...)
		case status >= 400:
			slog.Warn("http", attrs...)
		case quietAccessPath(r):
			// high-frequency poll, success — silent
		default:
			slog.Info("http", attrs...)
		}
	})
}

// quietAccessPath reports whether a request is routine polling that carries no diagnostic value on
// success: health checks, playback keepalive/stop, scrubbing thumbnails, HLS playlists+segments, and the
// player's per-second torrent status poll.
func quietAccessPath(r *http.Request) bool {
	p := r.URL.Path
	if p == "/health" || p == "/health/" {
		return true
	}
	if p == "/api/playback/keepalive" || p == "/api/playback/stop" {
		return true
	}
	if strings.HasSuffix(p, "/thumbnail") {
		return true
	}
	if strings.Contains(p, "/hls/") {
		return true
	}
	if r.Method == http.MethodGet && statusPollPath.MatchString(p) {
		return true
	}
	return false
}
