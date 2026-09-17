package handlers

import (
	"io"
	"sort"
	"time"

	mkv "github.com/remko/go-mkvparse"
)

// mkvKeyframes extracts video keyframe timestamps (seconds) from a Matroska/WebM Cues index,
// reading ONLY the SeekHead + Info + Tracks + Cues sections (via ParseSections — no cluster/media
// download). Returns ok=false when the file has no Cues (some MKVs omit them) so the caller can
// fall back to transcoding.
func mkvKeyframes(rs io.ReadSeeker) (keyframes []float64, ok bool, err error) {
	h := &mkvCueHandler{timecodeScale: 1_000_000, videoTrack: -1}
	if err := mkv.ParseSections(rs, h, mkv.InfoElement, mkv.TracksElement, mkv.CuesElement); err != nil {
		return nil, false, err
	}
	if len(h.cues) == 0 {
		return nil, false, nil // no Cues → fall back
	}

	scaleSec := float64(h.timecodeScale) / 1e9
	times := make([]float64, 0, len(h.cues))
	for _, c := range h.cues {
		// Keep cues for the video track; if a cue carries no track id, keep it (single-track / video-only Cues).
		if h.videoTrack >= 0 && len(c.tracks) > 0 {
			matched := false
			for _, tr := range c.tracks {
				if tr == h.videoTrack {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		times = append(times, float64(c.time)*scaleSec)
	}
	if len(times) == 0 {
		return nil, false, nil
	}
	sort.Float64s(times)
	return times, true, nil
}

type cuePoint struct {
	time   int64
	tracks []int64
}

// mkvCueHandler is a stateful mkvparse.Handler collecting TimecodeScale, the video track number,
// and every CuePoint's time + referenced tracks. Track filtering is deferred until after parsing,
// since Tracks and Cues may be visited in either order.
type mkvCueHandler struct {
	timecodeScale int64
	videoTrack    int64

	// current TrackEntry scratch
	inTrackEntry bool
	curTrackNum  int64
	curTrackType int64

	// current CuePoint scratch
	inCuePoint bool
	curCueTime int64
	curCueSet  bool
	curTracks  []int64

	cues []cuePoint
}

func (h *mkvCueHandler) HandleMasterBegin(id mkv.ElementID, _ mkv.ElementInfo) (bool, error) {
	switch id {
	case mkv.TrackEntryElement:
		h.inTrackEntry = true
		h.curTrackNum = -1
		h.curTrackType = -1
	case mkv.CuePointElement:
		h.inCuePoint = true
		h.curCueTime = 0
		h.curCueSet = false
		h.curTracks = nil
	}
	return true, nil // descend into all masters within the parsed sections
}

func (h *mkvCueHandler) HandleMasterEnd(id mkv.ElementID, _ mkv.ElementInfo) error {
	switch id {
	case mkv.TrackEntryElement:
		if h.curTrackType == 1 && h.curTrackNum >= 0 { // 1 = video
			h.videoTrack = h.curTrackNum
		}
		h.inTrackEntry = false
	case mkv.CuePointElement:
		if h.curCueSet {
			h.cues = append(h.cues, cuePoint{time: h.curCueTime, tracks: h.curTracks})
		}
		h.inCuePoint = false
	}
	return nil
}

func (h *mkvCueHandler) HandleInteger(id mkv.ElementID, v int64, _ mkv.ElementInfo) error {
	switch id {
	case mkv.TimecodeScaleElement:
		if v > 0 {
			h.timecodeScale = v
		}
	case mkv.TrackNumberElement:
		if h.inTrackEntry {
			h.curTrackNum = v
		}
	case mkv.TrackTypeElement:
		if h.inTrackEntry {
			h.curTrackType = v
		}
	case mkv.CueTimeElement:
		if h.inCuePoint {
			h.curCueTime = v
			h.curCueSet = true
		}
	case mkv.CueTrackElement:
		if h.inCuePoint {
			h.curTracks = append(h.curTracks, v)
		}
	}
	return nil
}

func (h *mkvCueHandler) HandleString(mkv.ElementID, string, mkv.ElementInfo) error  { return nil }
func (h *mkvCueHandler) HandleFloat(mkv.ElementID, float64, mkv.ElementInfo) error  { return nil }
func (h *mkvCueHandler) HandleDate(mkv.ElementID, time.Time, mkv.ElementInfo) error { return nil }
func (h *mkvCueHandler) HandleBinary(mkv.ElementID, []byte, mkv.ElementInfo) error  { return nil }
