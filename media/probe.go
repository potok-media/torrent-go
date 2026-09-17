package media

import (
	"context"
	"io"

	"github.com/asticode/go-astiav"
)

// avTimeBase is AV_TIME_BASE: FormatContext.Duration() is expressed in these units (microseconds).
const avTimeBase = 1_000_000

// Track is a demuxed stream summary — the shape the plugin/player descriptor needs.
type Track struct {
	Index       int     `json:"index"`
	Kind        string  `json:"kind"`        // "video" | "audio" | "subtitle" | "attachment" | "data" | "unknown"
	Codec       string  `json:"codec"`       // libav codec name, e.g. "h264", "hevc", "aac", "ac3", "ass", "subrip"
	Language    string  `json:"language"`    // stream metadata "language" tag (e.g. "eng", "rus"), "" if absent
	Title       string  `json:"title"`       // stream metadata "title" tag, "" if absent
	StartSec    float64 `json:"startSec"`    // stream start time (source PTS offset) in seconds; 0 if unset
	AttachedPic bool    `json:"attachedPic"` // a cover-art/thumbnail still (MediaType video, disposition ATTACHED_PIC) — NOT the real video stream, must never be selected for playback
}

// ProbeResult replaces the ffprobe JSON that metadata.go used to shell out for.
type ProbeResult struct {
	DurationSec float64 `json:"durationSec"`
	Tracks      []Track `json:"tracks"`
}

// ProbeTracks demuxes `src` in-process (custom AVIO, no ffprobe subprocess) and reports its duration and
// stream list. `ctx` bounds the probe: if the container index isn't resident and the read stalls, the
// IOInterrupter fires and this returns an error rather than hanging.
func ProbeTracks(ctx context.Context, src io.ReadSeeker) (*ProbeResult, error) {
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	res := &ProbeResult{DurationSec: float64(fc.Duration()) / avTimeBase}
	for _, s := range fc.Streams() {
		cp := s.CodecParameters()
		startSec := 0.0
		if st := s.StartTime(); st != noPTS {
			if tb := s.TimeBase(); tb.Den() != 0 {
				startSec = float64(st) * float64(tb.Num()) / float64(tb.Den())
			}
		}
		lang, title := streamTag(s, "language"), streamTag(s, "title")
		res.Tracks = append(res.Tracks, Track{
			Index:       s.Index(),
			Kind:        mediaKind(cp.MediaType()),
			Codec:       cp.CodecID().String(),
			Language:    lang,
			Title:       title,
			StartSec:    startSec,
			AttachedPic: s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic),
		})
	}
	return res, nil
}

// streamTag reads one metadata tag off a stream (e.g. "language", "title"), "" if absent.
func streamTag(s *astiav.Stream, key string) string {
	md := s.Metadata()
	if md == nil {
		return ""
	}
	if e := md.Get(key, nil, 0); e != nil {
		return e.Value()
	}
	return ""
}

func mediaKind(t astiav.MediaType) string {
	switch t {
	case astiav.MediaTypeVideo:
		return "video"
	case astiav.MediaTypeAudio:
		return "audio"
	case astiav.MediaTypeSubtitle:
		return "subtitle"
	case astiav.MediaTypeAttachment:
		return "attachment"
	case astiav.MediaTypeData:
		return "data"
	default:
		return "unknown"
	}
}
