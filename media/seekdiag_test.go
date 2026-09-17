package media

// Temporary diagnostic for the HLS4 audio-rendition latency (produceMs ≈ 8s per 6s audio segment).
// Hypothesis under test: SeekFrame on an MKV AUDIO stream has no Cues/index entries → libav falls back to
// a LINEAR parse from the segment start to the target timestamp on every produce. This times each phase
// (open / seek / first-packet read / full Segment) for the video vs the audio stream of a local MKV.
//
// Run:
//   POTOK_SEEK_DIAG=/path/to/file.mkv go test ./media/ -run TestSeekDiag -v
// Remove after the fix is confirmed.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/asticode/go-astiav"
)

func TestSeekDiag(t *testing.T) {
	path := os.Getenv("POTOK_SEEK_DIAG")
	if path == "" {
		t.Skip("POTOK_SEEK_DIAG not set")
	}
	const targetSec = 850.0
	const segDur = 6.0

	for _, kind := range []astiav.MediaType{astiav.MediaTypeVideo, astiav.MediaTypeAudio} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}

		t0 := time.Now()
		fc, cleanup, err := openDemux(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		openMs := time.Since(t0).Milliseconds()

		idx := -1
		for _, s := range fc.Streams() {
			if s.CodecParameters().MediaType() == kind {
				idx = s.Index()
				break
			}
		}
		if idx < 0 {
			t.Fatalf("no %v stream", kind)
		}
		tb := fc.Streams()[idx].TimeBase()
		target := int64(targetSec * float64(tb.Den()) / float64(tb.Num()))

		t1 := time.Now()
		seekErr := fc.SeekFrame(idx, target, astiav.SeekFlags(astiav.SeekFlagBackward))
		seekMs := time.Since(t1).Milliseconds()

		// Read up to the first packet OF THIS STREAM after the seek.
		pkt := astiav.AllocPacket()
		t2 := time.Now()
		firstPts := int64(-1)
		for {
			if e := fc.ReadFrame(pkt); e != nil {
				break
			}
			if pkt.StreamIndex() == idx {
				firstPts = pkt.Pts()
				pkt.Unref()
				break
			}
			pkt.Unref()
		}
		readMs := time.Since(t2).Milliseconds()
		pkt.Free()

		t.Logf("%-6v idx=%d: open=%dms seek=%dms (err=%v) firstPktRead=%dms firstPts=%.2fs",
			kind, idx, openMs, seekMs, seekErr, readMs,
			float64(firstPts)*float64(tb.Num())/float64(tb.Den()))
		cleanup()
		f.Close()

		// Full produce path, single-stream rendition segment at the same offset.
		f2, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t3 := time.Now()
		data, segErr := Segment(context.Background(), f2, targetSec, segDur, []int{idx}, false)
		t.Logf("%-6v Segment(%gs,+%gs)=%dms bytes=%d err=%v", kind, targetSec, segDur,
			time.Since(t3).Milliseconds(), len(data), segErr)
		f2.Close()
	}
}
