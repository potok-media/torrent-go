package media

// Temporary diagnostic for the HLS4 video-buffer hole (hls.js "bufferSeekOverHole" on v/seg<N>.m4s).
// Reproduces the EXACT grid the handler serves (keyframe-aligned copy grid for H.264, else uniform), produces
// seg[N] and seg[N+1], demuxes init+seg to read each fragment's REAL covered PTS range, and reports the GAP
// between seg[N]'s end and seg[N+1]'s start — the size/sign of the hole hls.js is seeking over.
//
// Run (point at the actual local file; N defaults to 214):
//   POTOK_SEEK_DIAG=/path/file.mkv POTOK_SEG_N=214 go test ./media/ -run TestSegmentHole -v
// Remove after the fix is confirmed.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/asticode/go-astiav"
)

// diagSegSeconds MUST match handlers.hlsSegmentSeconds so the reconstructed grid matches what is served.
const diagSegSeconds = 6.0

func TestSegmentHole(t *testing.T) {
	path := os.Getenv("POTOK_SEEK_DIAG")
	if path == "" {
		t.Skip("POTOK_SEEK_DIAG not set")
	}
	segN := 214
	if v := os.Getenv("POTOK_SEG_N"); v != "" {
		fmt.Sscanf(v, "%d", &segN)
	}
	ctx := context.Background()

	// --- video stream: index, codec, timebase ---
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	vIdx, vCodec := -1, ""
	var vtb astiav.Rational
	isH264 := false
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			vIdx = s.Index()
			vCodec = s.CodecParameters().CodecID().String()
			isH264 = s.CodecParameters().CodecID() == astiav.CodecIDH264
			vtb = s.TimeBase()
			break
		}
	}
	if vIdx < 0 {
		cleanup()
		src.Close()
		t.Fatal("no video stream")
	}
	toSecV := func(v int64) float64 { return float64(v) * float64(vtb.Num()) / float64(vtb.Den()) }

	// --- build the segment grid the handler would serve, up to seg[N+1]'s end (need starts[segN+2]) ---
	var starts []float64 // 0-based content starts, base-relative (starts[0] == 0)
	var base float64
	if isH264 {
		// COPY grid: keyframe-aligned greedy cut, identical to handlers.computeSegList.
		firstKF := true
		var segStart float64
		pkt := astiav.AllocPacket()
		for {
			if e := fc.ReadFrame(pkt); e != nil {
				pkt.Unref()
				break
			}
			if pkt.StreamIndex() != vIdx || !pkt.Flags().Has(astiav.PacketFlagKey) {
				pkt.Unref()
				continue
			}
			kf := toSecV(pkt.Pts())
			pkt.Unref()
			if firstKF {
				base, segStart, starts, firstKF = kf, kf, []float64{0}, false
				continue
			}
			if kf-segStart >= diagSegSeconds-0.001 {
				starts = append(starts, kf-base)
				segStart = kf
			}
			if len(starts) >= segN+3 {
				break
			}
		}
		pkt.Free()
	} else {
		// TRANSCODE grid: uniform from the video start-PTS, identical to handlers.uniformSegList.
		pkt := astiav.AllocPacket()
		for {
			if e := fc.ReadFrame(pkt); e != nil {
				pkt.Unref()
				break
			}
			if pkt.StreamIndex() == vIdx {
				base = toSecV(pkt.Pts())
				pkt.Unref()
				break
			}
			pkt.Unref()
		}
		pkt.Free()
		for i := 0; i <= segN+2; i++ {
			starts = append(starts, float64(i)*diagSegSeconds)
		}
	}
	cleanup()
	src.Close()

	if len(starts) < segN+3 {
		t.Fatalf("file too short: reconstructed only %d segment starts, need seg %d and %d (+1 boundary)", len(starts), segN, segN+1)
	}

	srcStart := func(i int) float64 { return starts[i] + base }
	extinf := func(i int) float64 { return starts[i+1] - starts[i] }

	// init segment (what the client loads via EXT-X-MAP)
	fInit, _ := os.Open(path)
	initSeg, err := InitSegment(ctx, fInit, []int{vIdx}, !isH264)
	fInit.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Produce segment n as the handler does, demux init+seg, return its REAL covered [minPts, max(pts+dur)).
	// min/max (not first/last in read order) is B-frame-safe.
	measure := func(n int) (lo, hi float64, npkt int) {
		f, _ := os.Open(path)
		seg, e := Segment(ctx, f, srcStart(n), extinf(n), []int{vIdx}, !isH264)
		f.Close()
		if e != nil {
			t.Fatalf("segment %d: %v", n, e)
		}
		full := append(append([]byte{}, initSeg...), seg...)
		mfc, mcl, e := openDemux(ctx, bytes.NewReader(full))
		if e != nil {
			t.Fatalf("demux seg %d: %v", n, e)
		}
		defer mcl()
		tb := mfc.Streams()[0].TimeBase()
		toSec := func(v int64) float64 { return float64(v) * float64(tb.Num()) / float64(tb.Den()) }
		pkt := astiav.AllocPacket()
		defer pkt.Free()
		lo, hi = 1e18, -1e18
		for {
			if e := mfc.ReadFrame(pkt); e != nil {
				pkt.Unref()
				break
			}
			p := toSec(pkt.Pts())
			end := toSec(pkt.Pts() + pkt.Duration())
			if p < lo {
				lo = p
			}
			if end > hi {
				hi = end
			}
			npkt++
			pkt.Unref()
		}
		return lo, hi, npkt
	}

	loA, hiA, nA := measure(segN)
	loB, hiB, nB := measure(segN + 1)
	gap := loB - hiA

	grid := "transcode(uniform)"
	if isH264 {
		grid = "copy(keyframe)"
	}
	t.Logf("codec=%s grid=%s base=%.3f", vCodec, grid, base)
	t.Logf("DECLARED: seg[%d]=[%.3f,%.3f) extinf=%.3f | seg[%d]=[%.3f,%.3f) extinf=%.3f",
		segN, srcStart(segN), srcStart(segN+1), extinf(segN),
		segN+1, srcStart(segN+1), srcStart(segN+2), extinf(segN+1))
	t.Logf("REAL:     seg[%d] covered=[%.3f,%.3f) pkts=%d", segN, loA, hiA, nA)
	t.Logf("REAL:     seg[%d] covered=[%.3f,%.3f) pkts=%d", segN+1, loB, hiB, nB)
	verdict := "ok"
	if gap > 0.001 {
		verdict = "HOLE"
	} else if gap < -0.001 {
		verdict = "OVERLAP"
	}
	t.Logf(">>> GAP seg[%d].end→seg[%d].start = %+.4f s  (%s)", segN, segN+1, gap, verdict)
}
