package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Potok.Backend.TorrentGo/media"
	"Potok.Backend.TorrentGo/storage"
)

// Continuous-AAC handler glue (see plan vivid-moseying-sloth). For a non-AAC audio track we transcode the whole
// track once to ONE continuous AAC (a background goroutine that follows the download), cache the encoded frames,
// and serve audio init/segments by copy-slicing that cache on the shared segList — so segments abut exactly
// instead of overlapping (the per-segment re-encode bug). AAC-source tracks keep the plain copy path.

const audioContWaitDeadline = 40 * time.Second

// getAudioCont returns the continuous-AAC transcoder for one non-AAC audio track, starting it (once) if needed.
// Coalesced via singleflight so concurrent init/segment requests share one transcoder; the goroutine is detached
// (context.Background) so a caller leaving doesn't cancel it for the rest, and cancellable on dropTorrent.
func (h *HandlerContext) getAudioCont(ctx context.Context, hashHex, fileIndexStr string, rel int, sl *segList, n int) (*media.ContinuousAAC, error) {
	key := fmt.Sprintf("%s_%s_%d", hashHex, fileIndexStr, rel)
	if v, ok := h.audioCont.Load(key); ok {
		cont := v.(*media.ContinuousAAC)
		if sl == nil {
			return cont, nil
		}
		startSec := cont.StartSec()
		producedTo, _, _ := cont.Status()
		sr := cont.SampleRate()

		lo := sl.srcStart(n)
		duration := sl.extinf(n)

		isSeek := false
		if lo < startSec {
			isSeek = true
		} else if sr > 0 {
			producedToSec := startSec + float64(producedTo)/float64(sr)
			if lo > producedToSec+1.5*duration {
				isSeek = true
			}
		}

		if !isSeek {
			return cont, nil
		}

		slog.Info("Audio track seek/track-switch detected, restarting continuous transcoder",
			"track", rel,
			"fromSec", startSec,
			"toSec", lo,
			"producedToSec", startSec+float64(producedTo)/float64(sr),
			"segmentDuration", duration,
		)
		cont.Cancel()
		cont.Free()
		h.audioCont.Delete(key)
		h.audioContCount.Add(-1)
	}
	v, err, _ := h.audioContSFG.Do(key, func() (interface{}, error) {
		if v, ok := h.audioCont.Load(key); ok {
			return v, nil
		}
		// Cap concurrent transcoders (~100MB each) so audio-heavy multi-stream load can't blow past the
		// memory budget. Bounds the count, not the piece cache — that stays under the global accountant.
		if max := h.maxAudioTranscoders(); max > 0 && int(h.audioContCount.Load()) >= max {
			return nil, fmt.Errorf("audiocont: too many concurrent transcoders (limit %d)", max)
		}
		layout, lerr := h.getStreamLayout(ctx, hashHex, fileIndexStr)
		if lerr != nil {
			return nil, lerr
		}
		if rel < 0 || rel >= len(layout.audios) {
			return nil, fmt.Errorf("audio track %d out of range", rel)
		}
		srcIdx := layout.audios[rel]

		var startSec float64 = 0
		if sl != nil && n > 0 {
			startSec = sl.srcStart(n)
		}

		cont := media.NewContinuousAAC(n, startSec)
		bgCtx, cancel := context.WithCancel(context.Background())
		cont.SetCancel(cancel)
		h.audioCont.Store(key, cont)
		h.audioContCount.Add(1)
		go func() {
			defer cancel()
			rs, _, oerr := h.openTorrentFileReader(bgCtx, hashHex, fileIndexStr, storage.ClassPlayback)
			if oerr != nil {
				cont.Fail(fmt.Errorf("audiocont: open reader: %w", oerr))
				return
			}
			defer rs.Close()
			cont.Run(bgCtx, rs, srcIdx)
		}()
		return cont, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*media.ContinuousAAC), nil
}

// produceAudioInitCont waits for the continuous encoder to open (frame 0 → codec config known) then builds the
// shared init from the frozen AAC config.
func (h *HandlerContext) produceAudioInitCont(ctx context.Context, cont *media.ContinuousAAC) ([]byte, error) {
	deadline := time.Now().Add(audioContWaitDeadline)
	for {
		if _, _, cerr := cont.Status(); cerr != nil {
			return nil, cerr
		}
		if cont.Ready() {
			return media.InitFromAAC(cont)
		}
		if time.Now().After(deadline) {
			return nil, errors.New("audiocont: codec config not ready (cold start)")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// produceAudioSegmentCont waits until the continuous transcode has reached this segment's window, then copy-slices
// it. Checks cont.err on EVERY tick so a failed transcode fails fast instead of spinning the whole timeout.
func (h *HandlerContext) produceAudioSegmentCont(ctx context.Context, cont *media.ContinuousAAC, sl *segList, n int) ([]byte, error) {
	deadline := time.Now().Add(audioContWaitDeadline)
	for {
		producedTo, complete, cerr := cont.Status()
		if cerr != nil {
			return nil, cerr
		}
		if sr := cont.SampleRate(); sr > 0 {
			// Variant A clock mapping: frame pts already carry the audio start offset (seeded from the first
			// decoded frame), so the window is just the shared-grid source time × sampleRate. hi(n)==lo(n+1)
			// by construction, so consecutive segments select disjoint, adjacent frames (no overlap, no gap).
			lo := int64(sl.srcStart(n)*float64(sr) + 0.5)
			hi := int64((sl.srcStart(n)+sl.extinf(n))*float64(sr) + 0.5)
			if complete || producedTo >= hi {
				data, serr := media.SegmentFromAAC(cont, lo, hi)
				if serr == nil {
					slog.Debug("[HLS4-DIAG] audiocont", "n", n, "lo", lo, "hi", hi, "producedTo", producedTo, "complete", complete, "bytes", len(data))
				}
				return data, serr
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("audiocont: segment %d not ready (transcode behind playhead)", n)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// dropAudioCont stops + frees every continuous-AAC transcoder for a dropped torrent (called from dropTorrent).
func (h *HandlerContext) dropAudioCont(prefix string) {
	h.audioCont.Range(func(k, v interface{}) bool {
		if key, _ := k.(string); strings.HasPrefix(key, prefix) {
			if cont, ok := v.(*media.ContinuousAAC); ok {
				cont.Cancel()
				cont.Free()
			}
			h.audioCont.Delete(k)
			h.audioContCount.Add(-1)
		}
		return true
	})
}

// maxAudioTranscoders is the concurrent continuous-AAC transcoder cap, DERIVED from the global RAM budget
// (~100MB each). Scales with the single UI budget knob. 0 = unlimited (no storage).
func (h *HandlerContext) maxAudioTranscoders() int {
	if h.Engine != nil && h.Engine.Storage != nil {
		return h.Engine.Storage.DerivedMaxAudioTranscoders()
	}
	return 0
}
