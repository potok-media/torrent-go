package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/asticode/go-astiav"
)

// ErrSubtitleUnsupported means the track is a codec media/ can't render to text in-process (bitmap subs
// like PGS/VobSub, or an unknown text codec). The caller skips it / keeps the legacy fallback.
var ErrSubtitleUnsupported = errors.New("media: unsupported subtitle codec")

// SubtitleWindow demuxes the text subtitle track `subRel` (the N-th subtitle stream, 0-based) over
// [startSec, endSec] and renders it to `format` ("webvtt" or "ass"), IN-PROCESS. Text subs carry their
// payload IN the packet (no decoder needed): MKV ASS packets hold the Dialogue fields, subrip/mov_text
// hold the text. Cues keep ABSOLUTE timestamps so they drop onto the whole-file timeline. Bitmap subs
// (PGS/VobSub) return ErrSubtitleUnsupported.
func SubtitleWindow(ctx context.Context, src io.ReadSeeker, subRel int, startSec, endSec float64, format string) ([]byte, error) {
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	streams := fc.Streams()
	// Find the subRel-th subtitle stream.
	subIdx, seen := -1, 0
	for _, s := range streams {
		if s.CodecParameters().MediaType() == astiav.MediaTypeSubtitle {
			if seen == subRel {
				subIdx = s.Index()
				break
			}
			seen++
		}
	}
	if subIdx < 0 {
		return nil, fmt.Errorf("media: subtitle track %d not found", subRel)
	}
	sub := streams[subIdx]
	codec := sub.CodecParameters().CodecID().String()
	if !subtitleTextCodec(codec) {
		return nil, ErrSubtitleUnsupported
	}
	stb := sub.TimeBase()

	toSec := func(pts int64, tb astiav.Rational) float64 {
		if tb.Den() == 0 {
			return 0
		}
		return float64(pts) * float64(tb.Num()) / float64(tb.Den())
	}

	// Seek to the window start and read until any stream passes the window end. Seek on the VIDEO stream
	// when the container has one: MKV Cues index only the video track — seeking on a subtitle stream (no
	// index entries) makes libav fall back to a LINEAR parse from the segment start to the target
	// (seconds per call, per window). The cluster at/before startSec carries every track's packets anyway.
	if startSec > 0 {
		seekIdx, seekTB := subIdx, stb
		for _, s := range streams {
			if s.CodecParameters().MediaType() == astiav.MediaTypeVideo && !s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic) {
				seekIdx, seekTB = s.Index(), s.TimeBase()
				break
			}
		}
		if seekTB.Num() > 0 {
			startTS := int64(startSec * float64(seekTB.Den()) / float64(seekTB.Num()))
			_ = fc.SeekFrame(seekIdx, startTS, astiav.SeekFlags(astiav.SeekFlagBackward))
		}
	}

	type cue struct {
		start, end float64
		data       string
	}
	var cues []cue

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	for {
		if rerr := fc.ReadFrame(pkt); rerr != nil {
			if errors.Is(rerr, astiav.ErrEof) {
				break
			}
			pkt.Unref()
			return nil, fmt.Errorf("media: subtitle read: %w", rerr)
		}
		si := pkt.StreamIndex()
		psec := toSec(pkt.Pts(), streams[si].TimeBase())
		if psec > endSec {
			pkt.Unref()
			break // any stream past the window end → done (interleaved streams advance together)
		}
		if si == subIdx && psec >= startSec-1 {
			d := toSec(pkt.Duration(), stb)
			cues = append(cues, cue{start: psec, end: psec + d, data: string(pkt.Data())})
		}
		pkt.Unref()
	}

	var b strings.Builder
	if format == "ass" && (codec == "ass" || codec == "ssa") {
		// Header comes from the codec extradata (the [Script Info]/[V4+ Styles]/[Events] block); each
		// packet is the Dialogue fields minus Start/End, which we splice back from pts/duration.
		b.Write(sub.CodecParameters().ExtraData())
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		for _, c := range cues {
			b.WriteString(assDialogue(c.start, c.end, c.data))
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	}

	// WebVTT (universal): render every cue as plain text with markup stripped.
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		text := subtitleText(codec, c.data)
		if strings.TrimSpace(text) == "" {
			continue
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", vttTime(c.start), vttTime(c.end), text)
	}
	return []byte(b.String()), nil
}

func subtitleTextCodec(codec string) bool {
	switch codec {
	case "ass", "ssa", "subrip", "srt", "webvtt", "text", "mov_text":
		return true
	default:
		return false
	}
}

// subtitleText extracts the plain display text from a subtitle packet for WebVTT output.
func subtitleText(codec, data string) string {
	switch codec {
	case "ass", "ssa":
		// The Dialogue fields are "ReadOrder,Layer,Style,Name,ML,MR,MV,Effect,Text"; keep the Text (9th)
		// and strip ASS override tags {\...} and convert \N to newlines.
		parts := strings.SplitN(data, ",", 9)
		if len(parts) == 9 {
			data = parts[8]
		}
		data = stripASSTags(data)
		data = strings.ReplaceAll(data, "\\N", "\n")
		data = strings.ReplaceAll(data, "\\n", "\n")
		return strings.TrimSpace(data)
	case "mov_text":
		// 2-byte big-endian length prefix + UTF-8 text.
		if len(data) >= 2 {
			return strings.TrimSpace(data[2:])
		}
		return ""
	default: // subrip/srt/webvtt/text — the payload is (mostly) the text
		return strings.TrimSpace(data)
	}
}

func stripASSTags(s string) string {
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

// assDialogue rebuilds one ASS "Dialogue:" line from a packet: the packet holds
// "ReadOrder,Layer,Style,Name,ML,MR,MV,Effect,Text"; the output wants
// "Dialogue: Layer,Start,End,Style,Name,ML,MR,MV,Effect,Text".
func assDialogue(start, end float64, data string) string {
	p := strings.SplitN(data, ",", 9)
	if len(p) < 9 {
		return "Dialogue: 0," + assTime(start) + "," + assTime(end) + ",Default,,0,0,0,," + data
	}
	return "Dialogue: " + p[1] + "," + assTime(start) + "," + assTime(end) + "," +
		p[2] + "," + p[3] + "," + p[4] + "," + p[5] + "," + p[6] + "," + p[7] + "," + p[8]
}

// assTime formats seconds as ASS H:MM:SS.cc (centiseconds).
func assTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	cs := int64(sec*100 + 0.5)
	h := cs / 360000
	cs %= 360000
	m := cs / 6000
	cs %= 6000
	s := cs / 100
	cs %= 100
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}

// vttTime formats seconds as WebVTT HH:MM:SS.mmm.
func vttTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(sec*1000 + 0.5)
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
