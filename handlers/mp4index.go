package handlers

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// mp4Keyframes extracts the presentation timestamps (seconds) of every video keyframe from an
// MP4/MOV moov index, reading ONLY the box headers + the moov box (mdat payload is seeked over,
// never downloaded). Returns ok=false for fragmented MP4 (no single moov sample table) so the
// caller can fall back.
func mp4Keyframes(rs io.ReadSeeker) (keyframes []float64, ok bool, err error) {
	// Per-track scratch, reset when a new "trak" box begins. moov boxes arrive depth-first and
	// traks are not nested, so a single current-track pointer is sufficient.
	type track struct {
		handler   string
		timescale uint32
		stss      *mp4.Stss // sync (key) sample numbers, 1-based
		stts      *mp4.Stts // sample decode-time deltas (run-length)
		ctts      *mp4.Ctts // composition offsets (PTS = DTS + offset), optional
	}
	var cur *track
	var video *track
	sawMoof := false

	_, err = mp4.ReadBoxStructure(rs, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "moof":
			sawMoof = true
			return nil, nil
		case "moov", "mdia", "minf", "stbl":
			return h.Expand()
		case "trak":
			cur = &track{}
			res, e := h.Expand()
			if cur != nil && cur.handler == "vide" {
				video = cur
			}
			cur = nil
			return res, e
		case "hdlr":
			if cur == nil {
				return nil, nil
			}
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			ht := box.(*mp4.Hdlr).HandlerType
			cur.handler = string(ht[:])
			return nil, nil
		case "mdhd":
			if cur == nil {
				return nil, nil
			}
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			cur.timescale = box.(*mp4.Mdhd).Timescale
			return nil, nil
		case "stss":
			if cur == nil {
				return nil, nil
			}
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			cur.stss = box.(*mp4.Stss)
			return nil, nil
		case "stts":
			if cur == nil {
				return nil, nil
			}
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			cur.stts = box.(*mp4.Stts)
			return nil, nil
		case "ctts":
			if cur == nil {
				return nil, nil
			}
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			cur.ctts = box.(*mp4.Ctts)
			return nil, nil
		}
		return nil, nil
	})
	if err != nil {
		return nil, false, err
	}

	if video == nil || video.timescale == 0 {
		return nil, false, fmt.Errorf("no video track / timescale in moov")
	}
	// Fragmented MP4: samples live in moof boxes, the moov sample table is empty. We can't derive
	// keyframes from moov alone → let the caller fall back (transcode/uniform).
	if video.stss == nil || len(video.stss.SampleNumber) == 0 {
		if sawMoof {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("no stss (sync sample) box in video track")
	}

	// Per-sample decode times from stts (run-length of deltas), and composition offsets from ctts.
	// We only need up to the largest keyframe sample number.
	maxSample := uint32(0)
	for _, sn := range video.stss.SampleNumber {
		if sn > maxSample {
			maxSample = sn
		}
	}
	dts := make([]uint64, maxSample) // dts[i] = decode time of sample (i+1)
	var t uint64
	sample := uint32(0)
	for _, e := range video.stts.Entries {
		for c := uint32(0); c < e.SampleCount; c++ {
			if sample >= maxSample {
				break
			}
			dts[sample] = t
			t += uint64(e.SampleDelta)
			sample++
		}
		if sample >= maxSample {
			break
		}
	}

	// Composition offset per sample (PTS = DTS + offset). Absent ctts → offset 0.
	cts := make([]int64, maxSample)
	if video.ctts != nil {
		sample = 0
		for _, e := range video.ctts.Entries {
			off := int64(e.SampleOffsetV0)
			if e.SampleOffsetV1 != 0 {
				off = int64(e.SampleOffsetV1)
			}
			for c := uint32(0); c < e.SampleCount; c++ {
				if sample >= maxSample {
					break
				}
				cts[sample] = off
				sample++
			}
			if sample >= maxSample {
				break
			}
		}
	}

	ts := float64(video.timescale)
	keyframes = make([]float64, 0, len(video.stss.SampleNumber))
	for _, sn := range video.stss.SampleNumber {
		if sn == 0 || sn > maxSample {
			continue
		}
		pts := int64(dts[sn-1]) + cts[sn-1]
		if pts < 0 {
			pts = 0
		}
		keyframes = append(keyframes, float64(pts)/ts)
	}
	return keyframes, true, nil
}
