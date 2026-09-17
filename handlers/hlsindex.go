package handlers

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"Potok.Backend.TorrentGo/storage"
	"github.com/anacrolix/torrent/metainfo"
)

// hlsSegmentSeconds is the target segment length. For the COPY path this is a MINIMUM — computeSegList
// still cuts on real keyframes, so sparse-keyframe (typical WEB-DL) copy segments stay GOP-length; it only
// bites the transcode (uniform, forced-keyframe) path + dense-keyframe copy. 6s is the HLS norm: ~3x fewer
// demux/seek/transcode calls than 2s (each segment is a fresh in-process produce) and fewer forced keyframes.
const hlsSegmentSeconds = 6.0

// segList is the deterministic segmentation of a file: a fixed list of content-time boundaries used
// to render the VOD playlist and to position the ffmpeg producer. For H.264 with a readable
// container index the boundaries fall on real keyframes (copy); otherwise they're uniform and the
// producer transcodes with forced keyframes so the real cuts match.
type segList struct {
	contentStarts []float64 // 0-based content start of each segment (contentStarts[0] == 0)
	contentEnd    float64   // 0-based content end of the whole file
	base          float64   // source PTS of the first frame; srcStart(i) = contentStarts[i] + base
	transcode     bool      // producer must transcode (non-H.264 or no usable keyframe index)
}

func (s *segList) count() int { return len(s.contentStarts) }

func (s *segList) extinf(i int) float64 {
	if i < 0 || i >= len(s.contentStarts) {
		return 0
	}
	end := s.contentEnd
	if i+1 < len(s.contentStarts) {
		end = s.contentStarts[i+1]
	}
	d := end - s.contentStarts[i]
	if d < 0 {
		return 0
	}
	return d
}

// srcStart is the source-PTS seek point for segment i (what ffmpeg -ss receives).
func (s *segList) srcStart(i int) float64 {
	if i <= 0 {
		return s.base
	}
	if i >= len(s.contentStarts) {
		i = len(s.contentStarts) - 1
	}
	return s.contentStarts[i] + s.base
}

// getSegList builds (and caches) the segmentation for a file. Always returns a usable list: an
// index-based copy list when possible, else a uniform transcode list.
func (h *HandlerContext) getSegList(ctx context.Context, hashHex, fileIndexStr string) (*segList, error) {
	key := hashHex + "_" + fileIndexStr
	if v, ok := h.hlsSegList.Load(key); ok {
		return v.(*segList), nil
	}
	// Coalesce concurrent cold builds: the video (v/index.m3u8) and audio (a/{rel}/index.m3u8) playlist
	// requests arrive together and build the SAME list, and buildSegList blocks for tens of seconds on a
	// cold torrent (duration probe + layout probe + Cues read). Without this, each request independently
	// runs that wait and they compete for the same cold pieces. Detached ctx (context.Background()) so one
	// caller disconnecting can't cancel the shared build for the other — every inner probe/read carries its
	// own deadline (getOrProbeDuration 45s, layoutProbeDeadline 40s, indexReadDeadline 25s).
	v, err, _ := h.hlsSegSFG.Do(key, func() (interface{}, error) {
		if v, ok := h.hlsSegList.Load(key); ok {
			return v.(*segList), nil
		}
		sl, berr := h.buildSegList(context.Background(), hashHex, fileIndexStr)
		if berr != nil {
			return nil, berr
		}
		h.hlsSegList.Store(key, sl)
		return sl, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*segList), nil
}

func (h *HandlerContext) buildSegList(ctx context.Context, hashHex, fileIndexStr string) (*segList, error) {
	dur, err := h.getOrProbeDuration(ctx, hashHex, fileIndexStr)
	if err != nil || dur <= 0 {
		return nil, fmt.Errorf("duration unavailable: %w", err)
	}

	// Probe the stream layout IN-PROCESS (libav, no ffprobe subprocess). The video codec + source
	// start-PTS drive the grid: only H.264 gets the keyframe-aligned COPY grid; anything else is a uniform
	// grid that media/ transcodes segment-by-segment.
	layout, lerr := h.getStreamLayout(ctx, hashHex, fileIndexStr)
	if lerr != nil {
		return uniformSegList(dur, 0), nil // probe failed → safest is a uniform (transcode) grid from 0
	}

	if layout.videoCodec == "h264" {
		if sl := h.tryIndexSegList(ctx, hashHex, fileIndexStr, dur); sl != nil {
			return sl, nil
		}
	}
	// Uniform boundaries from the video's start-PTS; media/ transcodes each segment with an IDR-led start.
	return uniformSegList(dur, layout.videoStartSec), nil
}

const (
	// Bounded wait for a container keyframe index to become READABLE. MKV Cues live at the file tail, so
	// on a just-added torrent the read fails (truncated → parse error) until those (priority-boosted)
	// pieces arrive. We retry for this long before giving up and transcoding, so a file with a valid index
	// lands on the fast copy path instead of transcoding the whole session.
	indexReadDeadline   = 25 * time.Second
	indexReadRetryEvery = 1 * time.Second
)

// tryIndexSegList reads the container keyframe index (MP4 moov / MKV Cues) and derives keyframe-aligned
// boundaries so an H.264 file is COPIED, not transcoded. Returns nil (→ caller transcodes) only when the
// container genuinely has no usable index. When the index is PRESENT but not yet readable (its bytes —
// the tail for MKV — haven't downloaded, which reads back as a parse error), it retries for a bounded
// time so the file moves to the copy path once the tail arrives.
func (h *HandlerContext) tryIndexSegList(ctx context.Context, hashHex, fileIndexStr string, dur float64) *segList {
	deadline := time.Now().Add(indexReadDeadline)
	for {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		// ClassMKVIndex boosts a wide file-tail window so the multi-piece MKV Cues download in time → this
		// H.264 file lands on the COPY path instead of transcoding. Safe here (runs after getStreamLayout has
		// warmed the front, so the tail boost can't starve the header/codec probe).
		rs, ext, err := h.openTorrentFileReader(rctx, hashHex, fileIndexStr, storage.ClassMKVIndex)
		if err != nil {
			cancel()
			return nil
		}
		var kfs []float64
		var ok bool
		switch ext {
		case ".mp4", ".mov", ".m4v":
			kfs, ok, err = mp4Keyframes(rs)
		case ".mkv", ".webm":
			kfs, ok, err = mkvKeyframes(rs)
		default:
			rs.Close()
			cancel()
			return nil // non-indexable container → transcode
		}
		rs.Close()
		cancel()

		if err == nil && ok && len(kfs) >= 1 {
			return computeSegList(kfs, dur) // index read → keyframe-aligned copy grid
		}
		if err == nil {
			return nil // parsed cleanly, no usable index → genuinely must transcode
		}
		// err != nil → the index is there but its bytes aren't readable yet (tail still downloading).
		// Retry until the boosted tail pieces arrive, bounded so a truly unparseable index still falls back.
		if time.Now().After(deadline) {
			slog.Warn("keyframe index unreadable in time → transcode grid", "hash", hashHex, "file", fileIndexStr, "ext", ext, "err", err)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(indexReadRetryEvery):
		}
	}
}

// computeSegList turns a sorted list of source-PTS keyframes + total duration into keyframe-aligned
// content boundaries. Greedy rule: open a new segment at the first keyframe that is >=
// hlsSegmentSeconds past the current segment start — this reproduces ffmpeg -f hls -hls_time cuts
// exactly at every interior boundary.
func computeSegList(kfs []float64, dur float64) *segList {
	base := kfs[0]
	starts := []float64{0}
	segStart := kfs[0]
	for _, kf := range kfs[1:] {
		if kf-segStart >= hlsSegmentSeconds-0.001 {
			starts = append(starts, kf-base)
			segStart = kf
		}
	}
	contentEnd := dur - base
	if contentEnd <= starts[len(starts)-1] {
		contentEnd = starts[len(starts)-1] + hlsSegmentSeconds
	}
	return &segList{contentStarts: starts, contentEnd: contentEnd, base: base, transcode: false}
}

func uniformSegList(dur, base float64) *segList {
	contentEnd := dur - base
	if contentEnd <= 0 {
		contentEnd = dur
	}
	starts := []float64{}
	for t := 0.0; t < contentEnd-0.001; t += hlsSegmentSeconds {
		starts = append(starts, t)
	}
	if len(starts) == 0 {
		starts = []float64{0}
	}
	return &segList{contentStarts: starts, contentEnd: contentEnd, base: base, transcode: true}
}

// openTorrentFileReader returns a seekable reader over a torrent file plus its lowercase extension, opened
// with the given ReadClass (ClassColdProbe for header/index probes, ClassPlayback for segment production).
func (h *HandlerContext) openTorrentFileReader(ctx context.Context, hashHex, fileIndexStr string, class storage.ReadClass) (*storage.Reader, string, error) {
	var infoHash metainfo.Hash
	hexBytes, err := hex.DecodeString(hashHex)
	if err != nil || len(hexBytes) != 20 {
		return nil, "", fmt.Errorf("bad hash")
	}
	copy(infoHash[:], hexBytes)

	t, ok := h.Engine.Client.Torrent(infoHash)
	if !ok {
		return nil, "", fmt.Errorf("torrent not active")
	}
	if t.Info() == nil {
		select {
		case <-t.GotInfo():
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	idx := 0
	if _, err := fmt.Sscanf(fileIndexStr, "%d", &idx); err != nil || idx < 1 {
		return nil, "", fmt.Errorf("bad file index")
	}
	files := t.Files()
	if idx-1 < 0 || idx-1 >= len(files) {
		return nil, "", fmt.Errorf("file index out of range")
	}
	file := files[idx-1]
	cache, ok := h.Engine.Storage.GetCache(infoHash)
	if !ok {
		return nil, "", fmt.Errorf("no storage cache")
	}
	rs := storage.NewReader(ctx, t, cache, file.Offset(), file.Length(), class, time.Time{})
	ext := strings.ToLower(filepath.Ext(file.Path()))
	return rs, ext, nil
}
