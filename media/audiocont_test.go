package media

// Verifies the continuous-AAC fix: two consecutive audio segments produced by copy-slicing ONE continuous encode
// must ABUT exactly (seg[N].end == seg[N+1].start) with an interior 1024-sample grid — the property the current
// per-segment re-encode path lacks (it overlaps ~22ms → hls.js bufferAppendError). Gated on a local AC3 file.
//
//	go test ./media/ -run TestContinuousAAC -v   (cgo env from `brew --prefix ffmpeg@7`)

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/asticode/go-astiav"
)

const audioContTestFile = "/private/tmp/claude-501/-Users-egorrrmiller-potok/4adba8c2-604a-40d7-9b26-873fb774c6d9/scratchpad/seekdiag.mkv"

func TestContinuousAACAbuts(t *testing.T) {
	if _, err := os.Stat(audioContTestFile); err != nil {
		t.Skip("POTOK test file absent: " + audioContTestFile)
	}
	ctx := context.Background()

	// Probe: first audio stream index + the video stream's start-PTS (the shared grid base).
	f0, err := os.Open(audioContTestFile)
	if err != nil {
		t.Fatal(err)
	}
	fc, cleanup, err := openDemux(ctx, f0)
	if err != nil {
		t.Fatal(err)
	}
	aIdx, videoBase := -1, 0.0
	for _, s := range fc.Streams() {
		tb := s.TimeBase()
		startSec := 0.0
		if st := s.StartTime(); st != noPTS && tb.Den() != 0 {
			startSec = float64(st) * float64(tb.Num()) / float64(tb.Den())
		}
		switch s.CodecParameters().MediaType() {
		case astiav.MediaTypeVideo:
			if !s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic) {
				videoBase = startSec
			}
		case astiav.MediaTypeAudio:
			if aIdx < 0 {
				aIdx = s.Index()
			}
		}
	}
	cleanup()
	f0.Close()
	if aIdx < 0 {
		t.Fatal("no audio stream")
	}

	// Transcode the whole track once (synchronous over a plain file — a valid io.ReadSeeker).
	cont := NewContinuousAAC(0, 0)
	fr, err := os.Open(audioContTestFile)
	if err != nil {
		t.Fatal(err)
	}
	cont.Run(ctx, fr, aIdx)
	fr.Close()
	if cont.err != nil {
		t.Fatalf("transcode failed: %v", cont.err)
	}
	if !cont.complete {
		t.Fatal("transcode did not complete")
	}
	sr := cont.sampleRate
	t.Logf("sampleRate=%d frames=%d priming=%d videoBase=%.3f", sr, len(cont.frames), cont.priming, videoBase)
	if sr <= 0 || len(cont.frames) == 0 {
		t.Fatalf("empty transcode: sr=%d frames=%d", sr, len(cont.frames))
	}
	if cont.priming <= 0 {
		t.Errorf("expected a single non-zero priming (ffmpeg-aac ~1024), got %d", cont.priming)
	}

	const segSec = 6.0
	bnd := func(n int) int64 { return int64(float64(n)*segSec*float64(sr) + videoBase*float64(sr) + 0.5) }

	init, err := InitFromAAC(cont)
	if err != nil {
		t.Fatal(err)
	}

	// Produce segment n, demux init+seg, return absolute first pts, max(pts+dur), packet count, and whether the
	// interior pts step is exactly 1024 samples (all values in the mp4 audio timebase == 1/sampleRate == samples).
	measure := func(n int) (first, end int64, npkt int, step1024 bool) {
		segBytes, serr := SegmentFromAAC(cont, bnd(n), bnd(n+1))
		if serr != nil {
			t.Fatalf("segment %d: %v", n, serr)
		}
		full := append(append([]byte{}, init...), segBytes...)
		mfc, mcl, derr := openDemux(ctx, bytes.NewReader(full))
		if derr != nil {
			t.Fatalf("demux segment %d: %v", n, derr)
		}
		defer mcl()
		pkt := astiav.AllocPacket()
		defer pkt.Free()
		first, end = -1, -1
		prev := int64(-1)
		step1024 = true
		for {
			if mfc.ReadFrame(pkt) != nil {
				pkt.Unref()
				break
			}
			p := pkt.Pts()
			if first < 0 {
				first = p
			}
			if prev >= 0 && p-prev != 1024 {
				step1024 = false
			}
			prev = p
			if e := p + pkt.Duration(); e > end {
				end = e
			}
			npkt++
			pkt.Unref()
		}
		return
	}

	f100, e100, n100, s100 := measure(100)
	f101, e101, n101, s101 := measure(101)
	t.Logf("seg[100] first=%d end=%d pkts=%d step1024=%v", f100, e100, n100, s100)
	t.Logf("seg[101] first=%d end=%d pkts=%d step1024=%v", f101, e101, n101, s101)

	// (1) ABUT: seg[100].end must equal seg[101].start — both slice ONE continuous 1024-grid, so the boundary
	//     partitions the grid with no shared/dropped frame. Exact (allow ±1 sample for rounding).
	if d := e100 - f101; d < -1 || d > 1 {
		t.Errorf("NOT abutting: seg100.end=%d vs seg101.start=%d (diff=%d samples; current per-segment path overlaps ~1024)", e100, f101, d)
	}
	// (2) interior grid is exactly one AAC frame.
	if !s100 || !s101 {
		t.Errorf("interior pts step must be 1024 samples (s100=%v s101=%v)", s100, s101)
	}
	// (3) offset gone: seg[100] starts within one frame ABOVE the declared grid boundary (sub-frame slack only).
	off := f100 - bnd(100)
	if off < 0 || off >= 1024 {
		t.Errorf("seg100 start=%d not in [boundary=%d, +1024): off=%d samples", f100, bnd(100), off)
	}
	t.Logf(">>> ABUT diff=%d samples | grid offset=%d samples (%.4fs) — want diff≈0, offset<0.0214s",
		e100-f101, off, float64(off)/float64(sr))
	_ = e101
	_ = n101
}
