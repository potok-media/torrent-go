package media

// Temporary diagnostic for the HLS4 audio-buffer pathology (hls.js racing/skipping audio segments).
// Question under test: does a produced audio-only segment cover EXACTLY its [start, start+6) grid slot,
// or does it overshoot/misplace (which would make hls.js buffered-end jump past neighbours and race)?
// Produces 3 consecutive audio segments + inits from a local MKV, then demuxes init+seg in memory and
// reports the REAL first/last packet times and total covered duration per segment.
//
// Run:
//   POTOK_SEEK_DIAG=/path/to/file.mkv go test ./media/ -run TestAudioTiling -v
// Remove after the fix is confirmed.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/asticode/go-astiav"
)

// rawTfdt scans a media segment's bytes for the tfdt box and returns its raw baseMediaDecodeTime —
// ground truth about what the muxer wrote, independent of any demuxer normalization.
func rawTfdt(seg []byte) (uint64, bool) {
	i := bytes.Index(seg, []byte("tfdt"))
	if i < 0 || i+16 > len(seg) {
		return 0, false
	}
	version := seg[i+4]
	if version == 1 {
		return binary.BigEndian.Uint64(seg[i+8 : i+16]), true
	}
	return uint64(binary.BigEndian.Uint32(seg[i+8 : i+12])), true
}

func TestAudioTiling(t *testing.T) {
	path := os.Getenv("POTOK_SEEK_DIAG")
	if path == "" {
		t.Skip("POTOK_SEEK_DIAG not set")
	}
	ctx := context.Background()

	// Find the audio stream index of the source.
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	aIdx := -1
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			aIdx = s.Index()
			break
		}
	}
	cleanup()
	src.Close()
	if aIdx < 0 {
		t.Fatal("no audio stream")
	}

	// Shared init (what the client gets via EXT-X-MAP).
	fInit, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	initSeg, err := InitSegment(ctx, fInit, []int{aIdx}, false)
	fInit.Close()
	if err != nil {
		t.Fatal(err)
	}

	const segDur = 6.0
	for i := 0; i < 3; i++ {
		start := 600.0 + float64(i)*segDur

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		seg, err := Segment(ctx, f, start, segDur, []int{aIdx}, false)
		f.Close()
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}

		// Demux init+segment from memory and measure the REAL covered range.
		full := append(append([]byte{}, initSeg...), seg...)
		mfc, mcleanup, err := openDemux(ctx, bytes.NewReader(full))
		if err != nil {
			t.Fatalf("demux produced segment %d: %v", i, err)
		}
		tb := mfc.Streams()[0].TimeBase()
		toSec := func(v int64) float64 { return float64(v) * float64(tb.Num()) / float64(tb.Den()) }

		pkt := astiav.AllocPacket()
		first, last := int64(-1), int64(-1)
		var lastDur int64
		n := 0
		for {
			if e := mfc.ReadFrame(pkt); e != nil {
				if !errors.Is(e, astiav.ErrEof) {
					t.Logf("segment %d: read err after %d pkts: %v", i, n, e)
				}
				break
			}
			if first < 0 {
				first = pkt.Pts()
			}
			last = pkt.Pts()
			lastDur = pkt.Duration()
			n++
			pkt.Unref()
		}
		pkt.Free()
		mcleanup()

		end := toSec(last + lastDur)
		tfdt, tfdtOK := rawTfdt(seg)
		t.Logf("seg[%d] want=[%.2f,%.2f) got=[%.3f,%.3f) dur=%.3fs pkts=%d bytes=%d rawTfdt=%d (ok=%v)",
			i, start, start+segDur, toSec(first), end, end-toSec(first), n, len(seg), tfdt, tfdtOK)
	}
}

// TestVideoTiling — same measurement for a VIDEO-only rendition segment: is the zero-based timestamp
// specific to the audio-transcode path, or universal to single-stream fMP4 output?
func TestVideoTiling(t *testing.T) {
	path := os.Getenv("POTOK_SEEK_DIAG")
	if path == "" {
		t.Skip("POTOK_SEEK_DIAG not set")
	}
	ctx := context.Background()

	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	vIdx := -1
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			vIdx = s.Index()
			break
		}
	}
	cleanup()
	src.Close()

	fInit, _ := os.Open(path)
	initSeg, err := InitSegment(ctx, fInit, []int{vIdx}, false)
	fInit.Close()
	if err != nil {
		t.Fatal(err)
	}

	f, _ := os.Open(path)
	seg, err := Segment(ctx, f, 600.0, 6.0, []int{vIdx}, false) // copy path (h264)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	full := append(append([]byte{}, initSeg...), seg...)
	mfc, mcleanup, err := openDemux(ctx, bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	defer mcleanup()
	tb := mfc.Streams()[0].TimeBase()
	toSec := func(v int64) float64 { return float64(v) * float64(tb.Num()) / float64(tb.Den()) }
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	first, last := int64(-1), int64(-1)
	for {
		if e := mfc.ReadFrame(pkt); e != nil {
			break
		}
		if first < 0 {
			first = pkt.Pts()
		}
		last = pkt.Pts()
		pkt.Unref()
	}
	t.Logf("video seg want=[600,606) got=[%.3f,%.3f]", toSec(first), toSec(last))
}

// TestDecodedAudioPts — do DECODED AC3 frames carry absolute pts, or NOPTS? (If NOPTS, the encoder's
// nextPts seed computes garbage → negative → the muxer shifts the whole segment to zero.)
func TestDecodedAudioPts(t *testing.T) {
	path := os.Getenv("POTOK_SEEK_DIAG")
	if path == "" {
		t.Skip("POTOK_SEEK_DIAG not set")
	}
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	fc, cleanup, err := openDemux(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var ast *astiav.Stream
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			ast = s
			break
		}
	}
	dec := astiav.AllocCodecContext(astiav.FindDecoder(ast.CodecParameters().CodecID()))
	defer dec.Free()
	_ = ast.CodecParameters().ToCodecContext(dec)
	if err := dec.Open(astiav.FindDecoder(ast.CodecParameters().CodecID()), nil); err != nil {
		t.Fatal(err)
	}

	tbv := fc.Streams()[0].TimeBase()
	_ = fc.SeekFrame(0, int64(600*float64(tbv.Den())/float64(tbv.Num())), astiav.SeekFlags(astiav.SeekFlagBackward))

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()
	tb := ast.TimeBase()
	shown := 0
	for shown < 5 {
		if e := fc.ReadFrame(pkt); e != nil {
			break
		}
		if pkt.StreamIndex() != ast.Index() {
			pkt.Unref()
			continue
		}
		pktPts := pkt.Pts()
		_ = dec.SendPacket(pkt)
		pkt.Unref()
		for dec.ReceiveFrame(frame) == nil {
			fPts := frame.Pts()
			t.Logf("pktPts=%d (%.3fs) decFramePts=%d noPTS=%v", pktPts,
				float64(pktPts)*float64(tb.Num())/float64(tb.Den()), fPts, fPts == noPTS)
			frame.Unref()
			shown++
		}
	}
}
