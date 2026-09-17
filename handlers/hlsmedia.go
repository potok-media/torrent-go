package handlers

import (
	"context"
	"fmt"
	"time"

	"Potok.Backend.TorrentGo/media"
	"Potok.Backend.TorrentGo/storage"
)

// In-process HLS production (TorrentGo v2, multivariant "HLS4"). Each rendition is produced INDEPENDENTLY
// and single-stream: the VIDEO rendition (audio-agnostic, produced once and shared) and one AUDIO rendition
// per source track (produced lazily — only the track the client actually loads). Because video and audio
// are separate renditions, switching audio never re-touches video, and an audio segment is seeked on the
// audio stream itself (every audio frame is a sync point) so there is no pre-roll seam. A segment is a
// function call over the RAM torrent cache — no subprocess, no producer to spawn/reposition/reap, no hang.

// streamLayout is a file's source stream map, probed once per file via in-process libav and cached: the
// primary video stream (+ its codec and source start-PTS, which drive the copy-vs-transcode grid choice)
// and the audio streams in rendition order, each with its codec.
type streamLayout struct {
	video         int      // source video stream index (-1 if none)
	videoCodec    string   // the video track's libav codec name ("h264" → copy grid, else transcode)
	videoStartSec float64  // the video track's source start-PTS in seconds (base for the uniform grid)
	audios        []int    // source stream indices of the audio tracks, in rendition order (index = relIndex)
	audioCodecs   []string // parallel to audios: each track's libav codec name ("aac", "ac3", …)
}

const (
	// Cold-start patience for the codec probe. On a just-added torrent the header + first-frame pieces
	// (which FindStreamInfo needs to resolve the pixel format) aren't downloaded yet, so a single probe reads
	// un-downloaded bytes and fails ("unspecified pixel format" / read error at the front). The head pieces
	// are priority-boosted (ClassColdProbe headFootBoost), so we retry with a fresh reader until the front
	// warms — the player already shows its buffering overlay until the first segment plays — instead of
	// hard-failing playback. Bounded so a genuinely un-probable file still surfaces an error.
	layoutProbeDeadline   = 40 * time.Second
	layoutProbeRetryEvery = 1500 * time.Millisecond
)

// getStreamLayout probes (once, cached) which source streams a file has, so a rendition request can map a
// relIndex to a real source stream index. Patient on a cold torrent (retries until the front warms). Every
// produce path (video/audio init + segment) and both playlist builds probe the SAME layout and arrive
// together on a cold open, so the up-to-40s probe is coalesced through singleflight — one probe, shared —
// with a detached, self-bounded ctx so one caller leaving can't cancel it for the rest.
func (h *HandlerContext) getStreamLayout(ctx context.Context, hashHex, fileIndexStr string) (*streamLayout, error) {
	key := hashHex + "_" + fileIndexStr
	if v, ok := h.hlsStreamLayout.Load(key); ok {
		return v.(*streamLayout), nil
	}
	v, err, _ := h.hlsLayoutSFG.Do(key, func() (interface{}, error) {
		if v, ok := h.hlsStreamLayout.Load(key); ok {
			return v.(*streamLayout), nil
		}
		// Detached from the caller (shared run), but hard-bounded so a probe that blocks on a cold read can
		// never hang the singleflight leader — the retry loop already stops at layoutProbeDeadline; the +5s
		// slack lets an in-flight ProbeTracks unwind via the IOInterrupter.
		pctx, cancel := context.WithTimeout(context.Background(), layoutProbeDeadline+5*time.Second)
		defer cancel()
		return h.probeStreamLayout(pctx, key, hashHex, fileIndexStr)
	})
	if err != nil {
		return nil, err
	}
	return v.(*streamLayout), nil
}

// probeStreamLayout runs the actual cold-patient probe loop for getStreamLayout and caches the result.
func (h *HandlerContext) probeStreamLayout(ctx context.Context, key, hashHex, fileIndexStr string) (*streamLayout, error) {
	deadline := time.Now().Add(layoutProbeDeadline)
	for {
		rs, _, err := h.openTorrentFileReader(ctx, hashHex, fileIndexStr, storage.ClassColdProbe)
		if err == nil {
			probe, perr := media.ProbeTracks(ctx, rs)
			rs.Close()
			if perr == nil {
				layout := &streamLayout{video: -1}
				for _, t := range probe.Tracks {
					switch t.Kind {
					case "video":
						// Skip cover-art/thumbnail stills (disposition ATTACHED_PIC): they are MediaType video
						// but a single static image, so picking one as THE video stream transcodes garbage
						// (non-monotonic PTS → muxer 500). Pick the first REAL video stream instead.
						if t.AttachedPic {
							continue
						}
						if layout.video < 0 {
							layout.video = t.Index
							layout.videoCodec = t.Codec
							layout.videoStartSec = t.StartSec
						}
					case "audio":
						layout.audios = append(layout.audios, t.Index)
						layout.audioCodecs = append(layout.audioCodecs, t.Codec)
					}
				}
				h.hlsStreamLayout.Store(key, layout)
				return layout, nil
			}
			err = perr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("media: stream layout probe failed (cold start): %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(layoutProbeRetryEvery):
		}
	}
}

// --- VIDEO rendition (audio-agnostic; produced once, shared by every audio choice) ---

func (h *HandlerContext) produceVideoInit(ctx context.Context, hashHex, fileIndexStr string) ([]byte, error) {
	layout, err := h.getStreamLayout(ctx, hashHex, fileIndexStr)
	if err != nil {
		return nil, err
	}
	if layout.video < 0 {
		return nil, fmt.Errorf("no video stream")
	}
	sl, err := h.getSegList(ctx, hashHex, fileIndexStr)
	if err != nil {
		return nil, err
	}
	rs, _, err := h.openTorrentFileReader(ctx, hashHex, fileIndexStr, storage.ClassPlayback)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	// sl.transcode: H.264 with a readable keyframe index → copy; else transcode to H.264.
	return media.InitSegment(ctx, rs, []int{layout.video}, sl.transcode)
}

func (h *HandlerContext) produceVideoSegment(ctx context.Context, hashHex, fileIndexStr string, sl *segList, n int) ([]byte, error) {
	layout, err := h.getStreamLayout(ctx, hashHex, fileIndexStr)
	if err != nil {
		return nil, err
	}
	if layout.video < 0 {
		return nil, fmt.Errorf("no video stream")
	}
	return h.produceSegmentRetry(ctx, hashHex, fileIndexStr, []int{layout.video}, sl, n, sl.transcode)
}

// --- AUDIO renditions (one per source track; produced lazily) ---

func (h *HandlerContext) produceAudioInit(ctx context.Context, hashHex, fileIndexStr string, rel int) ([]byte, error) {
	layout, err := h.getStreamLayout(ctx, hashHex, fileIndexStr)
	if err != nil {
		return nil, err
	}
	if rel < 0 || rel >= len(layout.audios) {
		return nil, fmt.Errorf("audio track %d out of range", rel)
	}
	// Non-AAC: the init comes from the continuous-AAC transcoder's frozen codec config, not a per-segment
	// encoder — so init + every segment describe the identical AAC stream (see continuous-AAC plan).
	if layout.audioCodecs[rel] != "aac" {
		cont, cerr := h.getAudioCont(ctx, hashHex, fileIndexStr, rel, nil, 0)
		if cerr != nil {
			return nil, cerr
		}
		return h.produceAudioInitCont(ctx, cont)
	}
	rs, _, err := h.openTorrentFileReader(ctx, hashHex, fileIndexStr, storage.ClassPlayback)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	// AAC source: plain copy remux.
	return media.InitSegment(ctx, rs, []int{layout.audios[rel]}, false)
}

func (h *HandlerContext) produceAudioSegment(ctx context.Context, hashHex, fileIndexStr string, rel int, sl *segList, n int) ([]byte, error) {
	layout, err := h.getStreamLayout(ctx, hashHex, fileIndexStr)
	if err != nil {
		return nil, err
	}
	if rel < 0 || rel >= len(layout.audios) {
		return nil, fmt.Errorf("audio track %d out of range", rel)
	}
	// Non-AAC (AC3/EAC3/DTS/…): copy-slice the continuous AAC transcode so consecutive segments abut exactly.
	// Per-segment re-encoding (the else branch's transcode) gives each segment its own AAC frame phase → overlap
	// → hls.js bufferAppendError. AAC source tiles fine as a plain copy.
	if layout.audioCodecs[rel] != "aac" {
		cont, cerr := h.getAudioCont(ctx, hashHex, fileIndexStr, rel, sl, n)
		if cerr != nil {
			return nil, cerr
		}
		return h.produceAudioSegmentCont(ctx, cont, sl, n)
	}
	return h.produceSegmentRetry(ctx, hashHex, fileIndexStr, []int{layout.audios[rel]}, sl, n, false)
}

// produceSegmentRetry produces one fMP4 media segment for a single-stream rendition. A far seek can land
// the demux in a region the torrent hasn't fully downloaded yet: the reader returns bytes at the download
// front, the demuxer misparses (bad EBML / non-monotonic dts) and the mux rejects it. Those pieces fill in
// within moments, so retry a few times with a short backoff (a fresh reader each time).
func (h *HandlerContext) produceSegmentRetry(ctx context.Context, hashHex, fileIndexStr string, streams []int, sl *segList, n int, transcode bool) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		rs, _, oerr := h.openTorrentFileReader(ctx, hashHex, fileIndexStr, storage.ClassPlayback)
		if oerr != nil {
			return nil, oerr
		}
		data, serr := media.Segment(ctx, rs, sl.srcStart(n), sl.extinf(n), streams, transcode)
		_ = rs.Close()
		if serr == nil {
			return data, nil
		}
		lastErr = serr
	}
	return nil, lastErr
}

// produceSubSegment renders one subtitle-rendition segment as WebVTT: the cues in this segment's time
// window, with ABSOLUTE timestamps (media.SubtitleWindow), so hls.js drops them onto the shared timeline.
// Only the client's ACTIVE subtitle track is ever fetched, so this stays lazy/windowed.
func (h *HandlerContext) produceSubSegment(ctx context.Context, hashHex, fileIndexStr string, rel int, sl *segList, n int) ([]byte, error) {
	rs, _, err := h.openTorrentFileReader(ctx, hashHex, fileIndexStr, storage.ClassAheadDemux)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	start := sl.srcStart(n)
	return media.SubtitleWindow(ctx, rs, rel, start, start+sl.extinf(n), "webvtt")
}
