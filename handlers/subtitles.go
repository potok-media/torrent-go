package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Potok.Backend.TorrentGo/media"
	"Potok.Backend.TorrentGo/storage"
	"github.com/go-chi/chi/v5"
)

// errSubtitleWindowNotReady is returned when a windowed subtitle request lands on a region that isn't
// downloaded yet. The handler turns it into a fast HTTP 202 (retry) instead of demuxing into a cold
// region — the client re-requests the window once playback nears it.
var errSubtitleWindowNotReady = errors.New("subtitle window not yet downloaded")

// Windowed subtitle extraction: instead of demuxing the whole file (which pulls the entire torrent —
// 30GB for one track), the client asks for the ~2-min slice around the playhead and media.SubtitleWindow
// seeks straight to it (like thumbnails), reading only that window's pieces — the ones playback has
// already fetched. subtitleWindowSec is the bucket the client and this server agree on (WINDOW in the
// frontend); subtitleWindowLead adds a small overlap each side so a cue straddling a bucket boundary
// still appears in both windows (the client dedups). subtitleWindowTimeout is short because a window
// is cheap — a cold-ahead region simply fails and the client retries once playback nears it.
// subtitleWindowSec is deliberately SMALL: to collect the sparse subtitle packets for a window, the
// demuxer reads that window's whole interleaved container (video+audio too, hundreds of MB for 2 min).
// The window must therefore stay within the player's read-ahead (~30 pieces ≈ 100-120s) so those bytes
// are already in the RAM cache (instant) rather than pulling an un-downloaded tail over the network at
// background priority (which timed out). 30s fits inside the read-ahead across all realistic bitrates.
const (
	// 15s window: to collect the sparse subtitle packets the demuxer reads the whole interleaved container
	// for the slice (video+audio too), so a smaller window = fewer bytes per extraction and a read that
	// fits alongside the transcoding producer in the bounded RAM cache. A deadline-timed-out window returns
	// a 202 and the client retries (each retry warms the cache toward completion).
	subtitleWindowSec     = 15
	subtitleWindowLead    = 2
	subtitleWindowTimeout = 20 * time.Second
)

// Wall-clock cap for a subtitle extraction (a full-file in-process demux over the torrent cache).
// Generous because a still-downloading torrent must read the whole file, but bounded so a stuck demux
// can't hold the extraction semaphore forever.
const subtitleExtractTimeout = 240 * time.Second

// wholeFileEndSec is an "end" past any real duration — media.SubtitleWindow then reads the track to EOF
// (the full-file fallback path).
const wholeFileEndSec = 1e12

// resolveSubtitleFormat validates the ?format= query and returns the output format token plus the
// HTTP Content-Type to serve. Defaults to webvtt.
func resolveSubtitleFormat(format string) (subFormat, contentType string) {
	subFormat = "webvtt"
	contentType = "text/vtt; charset=utf-8"
	if format == "" || len(format) >= 10 {
		return
	}
	for _, r := range format {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return
		}
	}
	subFormat = format
	switch strings.ToLower(format) {
	case "ass", "ssa":
		contentType = "text/x-ssa; charset=utf-8"
	case "srt", "subrip":
		contentType = "text/srt; charset=utf-8"
	default:
		contentType = "text/plain; charset=utf-8"
	}
	return
}

// isTextSubtitleCodec reports whether a subtitle codec is text-based (extractable to ass/webvtt).
// Image-based subs (PGS/VobSub/DVB) can't be converted to text — they'd fail the batch extraction
// and the player can't render them anyway — so they're skipped.
func isTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "ass", "ssa", "subrip", "srt", "webvtt", "vtt", "mov_text", "text",
		"microdvd", "stl", "subviewer", "subviewer1", "jacosub", "sami", "realtext", "mpl2", "pjs", "vplayer":
		return true
	default:
		return false
	}
}

func (h *HandlerContext) HandleGetSubtitles(w http.ResponseWriter, r *http.Request) {
	hashHex := chi.URLParam(r, "hash")
	fileIndexStr := chi.URLParam(r, "fileIndex")
	trackIndexStr := chi.URLParam(r, "trackIndex")

	subFormat, contentType := resolveSubtitleFormat(r.URL.Query().Get("format"))

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Windowed request (?start=<sec>): serve just the time-slice around the playhead. Falls back to the
	// full-file path when start is absent (uploads / legacy) or when this file's container can't be
	// seeked (subtitleWinBad, set after a windowed pass produced nothing).
	fileKey := fmt.Sprintf("%s_%s", hashHex, fileIndexStr)
	_, winBad := h.subtitleWinBad.Load(fileKey)
	startStr := r.URL.Query().Get("start")

	var data []byte
	var err error
	if startStr != "" && !winBad {
		startSec := 0
		if f, perr := strconv.ParseFloat(startStr, 64); perr == nil && f > 0 {
			startSec = int(f)
		}
		bucket := (startSec / subtitleWindowSec) * subtitleWindowSec
		windowKey := fmt.Sprintf("%s_%s_%s_%s_w%d", hashHex, fileIndexStr, trackIndexStr, subFormat, bucket)
		data, err = h.getOrExtractSubtitleWindow(r.Context(), hashHex, fileIndexStr, trackIndexStr, subFormat, bucket, windowKey)
	} else {
		cacheKey := fmt.Sprintf("%s_%s_%s_%s", hashHex, fileIndexStr, trackIndexStr, subFormat)
		data, err = h.getOrExtractSubtitle(r.Context(), hashHex, fileIndexStr, trackIndexStr, subFormat, cacheKey)
	}
	if err != nil {
		if r.Context().Err() != nil {
			return // client disconnected, nothing to write
		}
		if errors.Is(err, errSubtitleWindowNotReady) {
			// Region not downloaded yet — cheap retry, no demux. The client re-requests as playback nears.
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		slog.Warn("subtitle extraction failed", "error", err)
		http.Error(w, "subtitle extraction failed", http.StatusInternalServerError)
		return
	}

	// The subtitle content for a given (hash, file, track, format) tuple is immutable, so attach a
	// content ETag and serve conditional requests — seeks/replays become instant.
	hsh := fnv.New64a()
	_, _ = hsh.Write(data)
	etag := fmt.Sprintf("\"%x\"", hsh.Sum64())

	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("ETag", etag)

	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	_, _ = w.Write(data)
}

// getOrExtractSubtitle returns the requested track+format bytes, serving from the RAM cache whenever
// possible. The FIRST request for a file triggers ONE in-process demux pass that extracts ALL text
// subtitle tracks at once (shared across concurrent requests via singleflight); every later request —
// any track, seek or replay — is a cache hit. A track/format the batch didn't produce falls back to a
// single-track extraction (also cached). This turns "N parallel full-file demuxes on every play"
// into "one demux per file, ever".
func (h *HandlerContext) getOrExtractSubtitle(ctx context.Context, hashHex, fileIndexStr, trackIndexStr, subFormat, cacheKey string) ([]byte, error) {
	if v, ok := h.subtitleCache.Load(cacheKey); ok {
		return v.([]byte), nil
	}

	fileKey := fmt.Sprintf("%s_%s", hashHex, fileIndexStr)
	if _, done := h.subtitleExtracted.Load(fileKey); !done {
		_, err, _ := h.subtitleSFG.Do(fileKey, func() (interface{}, error) {
			if _, done := h.subtitleExtracted.Load(fileKey); done {
				return nil, nil
			}
			// Detach from any single caller's request ctx: singleflight shares one run across all waiters,
			// so a leader disconnect must not kill the shared demux (extractAllSubtitles adds its own cap).
			if err := h.extractAllSubtitles(context.Background(), hashHex, fileIndexStr); err != nil {
				return nil, err
			}
			// Mark done even on best-effort partial success so the full-file demux never repeats.
			h.subtitleExtracted.Store(fileKey, true)
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
	}

	if v, ok := h.subtitleCache.Load(cacheKey); ok {
		return v.([]byte), nil
	}

	// The batch pass didn't yield this exact (track, format) — extract just this one and cache it.
	return h.extractSingleSubtitle(ctx, hashHex, fileIndexStr, trackIndexStr, subFormat, cacheKey)
}

// extractAllSubtitles demuxes the file once in-process (media.SubtitleWindow per track) and stores every
// text subtitle track straight into the RAM cache. Best-effort: a failed track still caches whatever the
// others produced; anything missing falls back to a single-track extraction later.
func (h *HandlerContext) extractAllSubtitles(ctx context.Context, hashHex, fileIndexStr string) error {
	metaBytes, err := h.probeAndCacheMetadata(ctx, hashHex, fileIndexStr)
	if err != nil {
		return fmt.Errorf("subtitle metadata probe failed: %w", err)
	}
	var meta ClientMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("parse metadata for subtitles: %w", err)
	}

	// Admit as a HEAVY extraction (whole-file demux) — serialized so several can't peg the CPU.
	release, acqErr := h.extExec.Acquire(ctx, extHeavy)
	if acqErr != nil {
		return acqErr
	}
	defer release()

	extractCtx, cancel := context.WithTimeout(ctx, subtitleExtractTimeout)
	defer cancel()

	stored, tracks := 0, 0
	for _, tr := range meta.Tracks {
		if tr.Type != "subtitle" || !isTextSubtitleCodec(tr.Codec) {
			continue
		}
		tracks++
		format := "webvtt"
		if c := strings.ToLower(tr.Codec); c == "ass" || c == "ssa" {
			format = "ass"
		}
		rs, _, oerr := h.openTorrentFileReader(extractCtx, hashHex, fileIndexStr, storage.ClassPatientDemux)
		if oerr != nil {
			continue
		}
		data, serr := media.SubtitleWindow(extractCtx, rs, tr.RelIndex, 0, wholeFileEndSec, format)
		rs.Close()
		if serr != nil {
			if extractCtx.Err() != nil {
				return extractCtx.Err()
			}
			continue // unsupported/failed track — skip (keep the others)
		}
		if len(data) == 0 {
			continue
		}
		key := fmt.Sprintf("%s_%s_%d_%s", hashHex, fileIndexStr, tr.RelIndex, format)
		h.subtitleCache.Store(key, data)
		stored++
	}
	slog.Info("batch subtitle extraction done", "hash", hashHex, "file", fileIndexStr, "tracks", tracks, "stored", stored)
	return nil
}

// extractSingleSubtitle extracts one subtitle track in the requested format (the fallback path for a
// track/format the batch pass didn't cover) and caches the result.
func (h *HandlerContext) extractSingleSubtitle(ctx context.Context, hashHex, fileIndexStr, trackIndexStr, subFormat, cacheKey string) ([]byte, error) {
	release, acqErr := h.extExec.Acquire(ctx, extHeavy)
	if acqErr != nil {
		return nil, acqErr
	}
	defer release()

	subRel, _ := strconv.Atoi(trackIndexStr)
	extractCtx, cancel := context.WithTimeout(ctx, subtitleExtractTimeout)
	defer cancel()

	rs, _, oerr := h.openTorrentFileReader(extractCtx, hashHex, fileIndexStr, storage.ClassPatientDemux)
	if oerr != nil {
		return nil, oerr
	}
	defer rs.Close()

	data, err := media.SubtitleWindow(extractCtx, rs, subRel, 0, wholeFileEndSec, subFormat)
	if err != nil {
		if extractCtx.Err() != nil {
			return nil, extractCtx.Err()
		}
		return nil, fmt.Errorf("single subtitle extraction failed: %w", err)
	}
	h.subtitleCache.Store(cacheKey, data)
	return data, nil
}

// getOrExtractSubtitleWindow returns one track's bytes for the time bucket [bucket, bucket+window).
// The FIRST request for a bucket triggers ONE in-process pass that seeks to the window and extracts ALL
// text tracks for it (shared via singleflight on the bucket key); any later request — another track,
// the same window, a replay — is a cache hit. If the pass yields nothing (container can't be seeked),
// it marks the file windowing-unsupported and falls back to the full-file path for this track.
func (h *HandlerContext) getOrExtractSubtitleWindow(ctx context.Context, hashHex, fileIndexStr, trackIndexStr, subFormat string, bucket int, windowKey string) ([]byte, error) {
	if v, ok := h.subtitleCache.Load(windowKey); ok {
		return v.([]byte), nil
	}

	batchKey := fmt.Sprintf("%s_%s_w%d", hashHex, fileIndexStr, bucket)
	_, err, _ := h.subtitleSFG.Do(batchKey, func() (interface{}, error) {
		if _, ok := h.subtitleCache.Load(windowKey); ok {
			return nil, nil
		}
		// Detached ctx (see getOrExtractSubtitle): the window run is shared via singleflight, so a leader
		// disconnect must not cancel it for the followers. extractWindowSubtitles adds its own deadline.
		stored, e := h.extractWindowSubtitles(context.Background(), hashHex, fileIndexStr, bucket)
		if e != nil {
			return nil, e
		}
		if stored == 0 {
			// The window seek produced no track output — the container has no usable index. Stop paying
			// failed seeks for this file; every subsequent request routes to the full-file path.
			h.subtitleWinBad.Store(fmt.Sprintf("%s_%s", hashHex, fileIndexStr), true)
		}
		return nil, nil
	})
	if err != nil {
		// The windowed read hit its deadline — the region isn't downloaded fast enough YET, not a real
		// failure. Return a fast 202 (retry): the AheadDemux read requested the window pieces at Normal,
		// and each retry warms the cache toward completion. Only a non-deadline error is a true 500.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errSubtitleWindowNotReady
		}
		return nil, err
	}

	if v, ok := h.subtitleCache.Load(windowKey); ok {
		return v.([]byte), nil
	}
	// This track wasn't in the window batch (e.g. a format the batch doesn't produce, or windowing just
	// got marked unsupported) — serve it via the full-file path so the client always gets something.
	cacheKey := fmt.Sprintf("%s_%s_%s_%s", hashHex, fileIndexStr, trackIndexStr, subFormat)
	return h.getOrExtractSubtitle(ctx, hashHex, fileIndexStr, trackIndexStr, subFormat, cacheKey)
}

// extractWindowSubtitles seeks in-process to [bucket-lead, bucket+window+lead] and stores every text
// subtitle track for that slice under its window key. media.SubtitleWindow keeps each cue's ABSOLUTE
// container timestamp, so the emitted cues drop straight onto the player's whole-file timeline. Returns
// how many tracks it stored (0 ⇒ the seek yielded nothing ⇒ caller falls back to full-file).
func (h *HandlerContext) extractWindowSubtitles(ctx context.Context, hashHex, fileIndexStr string, bucket int) (int, error) {
	metaBytes, err := h.probeAndCacheMetadata(ctx, hashHex, fileIndexStr)
	if err != nil {
		return 0, fmt.Errorf("subtitle metadata probe failed: %w", err)
	}
	var meta ClientMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return 0, fmt.Errorf("parse metadata for subtitles: %w", err)
	}

	ssStart := float64(bucket - subtitleWindowLead)
	if ssStart < 0 {
		ssStart = 0
	}
	endSec := float64(bucket + subtitleWindowSec + subtitleWindowLead)

	release, acqErr := h.extExec.Acquire(ctx, extWindow)
	if acqErr != nil {
		return 0, acqErr
	}
	defer release()

	// The window read is bounded by this timeout: media/'s IOInterrupter fires when it lapses, so a cold
	// (un-downloaded) region errors out fast → the caller returns a 202 and the client retries.
	extractCtx, cancel := context.WithTimeout(ctx, subtitleWindowTimeout)
	defer cancel()

	stored, tracks := 0, 0
	for _, tr := range meta.Tracks {
		if tr.Type != "subtitle" || !isTextSubtitleCodec(tr.Codec) {
			continue
		}
		tracks++
		format := "webvtt"
		if c := strings.ToLower(tr.Codec); c == "ass" || c == "ssa" {
			format = "ass"
		}
		rs, _, oerr := h.openTorrentFileReader(extractCtx, hashHex, fileIndexStr, storage.ClassAheadDemux)
		if oerr != nil {
			if extractCtx.Err() != nil {
				return stored, extractCtx.Err()
			}
			continue
		}
		data, serr := media.SubtitleWindow(extractCtx, rs, tr.RelIndex, ssStart, endSec, format)
		rs.Close()
		if serr != nil {
			if extractCtx.Err() != nil {
				return stored, extractCtx.Err() // cold region / lapsed deadline → caller turns into a 202
			}
			continue // unsupported/failed track — skip, keep the others
		}
		key := fmt.Sprintf("%s_%s_%d_%s_w%d", hashHex, fileIndexStr, tr.RelIndex, format, bucket)
		h.subtitleCache.Store(key, data)
		stored++
	}
	slog.Info("windowed subtitle extraction done", "hash", hashHex, "file", fileIndexStr, "bucket", bucket, "tracks", tracks, "stored", stored)
	return stored, nil
}
