package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSegmentRoundtrip validates the in-process copy path end-to-end against a real local file.
//
//	POTOK_TEST_MEDIA=/path/to/h264.mkv go test ./media -run Segment -v
//
// It probes the file, writes the fMP4 init segment + the first media segment, and also writes
// check.mp4 = init + seg0 (a moof-only segment appended to its init IS a playable fragmented MP4). Verify:
//
//	ffprobe /tmp/potok-media-test/check.mp4      # streams/duration sane, no errors
//	ffplay  /tmp/potok-media-test/check.mp4      # plays the first ~6s
//
// Output dir override: POTOK_TEST_OUT=/some/dir.
func TestSegmentRoundtrip(t *testing.T) {
	in := os.Getenv("POTOK_TEST_MEDIA")
	if in == "" {
		t.Skip("set POTOK_TEST_MEDIA=/path/to/media to run the copy-path roundtrip")
	}

	outDir := os.Getenv("POTOK_TEST_OUT")
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "potok-media-test")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}

	f, err := os.Open(in)
	if err != nil {
		t.Fatalf("open %s: %v", in, err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	probe, err := ProbeTracks(ctx, f)
	if err != nil {
		t.Fatalf("ProbeTracks: %v", err)
	}
	t.Logf("duration=%.2fs tracks=%d", probe.DurationSec, len(probe.Tracks))
	for _, tr := range probe.Tracks {
		t.Logf("  #%d %-9s %s", tr.Index, tr.Kind, tr.Codec)
	}

	video, audio := -1, -1
	for _, tr := range probe.Tracks {
		if tr.Kind == "video" && video < 0 {
			video = tr.Index
		}
		if tr.Kind == "audio" && audio < 0 {
			audio = tr.Index
		}
	}
	if video < 0 {
		t.Fatalf("no video stream found")
	}
	streams := []int{video}
	if audio >= 0 {
		streams = append(streams, audio)
	}

	// Each media/ call gets a fresh reader — a demux context owns its own seek cursor (this is why the
	// pool, step 2b, needs a reader factory rather than a single shared reader).
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek 0: %v", err)
	}
	initSeg, err := InitSegment(ctx, f, streams, false)
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	initPath := filepath.Join(outDir, "init.mp4")
	if err := os.WriteFile(initPath, initSeg, 0o644); err != nil {
		t.Fatalf("write init: %v", err)
	}
	t.Logf("init.mp4: %d bytes", len(initSeg))

	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek 0: %v", err)
	}
	seg, err := Segment(ctx, f, 0, 6, streams, false)
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	segPath := filepath.Join(outDir, "seg0.m4s")
	if err := os.WriteFile(segPath, seg, 0o644); err != nil {
		t.Fatalf("write seg: %v", err)
	}
	t.Logf("seg0.m4s: %d bytes", len(seg))

	// init + seg0 concatenated = a self-contained fragmented MP4, playable directly.
	check := append(append([]byte{}, initSeg...), seg...)
	checkPath := filepath.Join(outDir, "check.mp4")
	if err := os.WriteFile(checkPath, check, 0o644); err != nil {
		t.Fatalf("write check: %v", err)
	}
	t.Logf("check.mp4: %d bytes → ffprobe/ffplay %s", len(check), checkPath)
}
