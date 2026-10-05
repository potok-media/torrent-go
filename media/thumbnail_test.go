package media

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestThumbnailRoundtrip validates the in-process thumbnail pipeline (demux → decode → scale → MJPEG)
// against a real local file.
//
//	POTOK_TEST_MEDIA=/path/to/video.mkv go test ./media -run Thumbnail -v
//
// It extracts a frame at several timestamps and writes check-N.jpg files. The MJPEG encoder requires
// full-range yuvj420p — this test is the regression guard for the "Non full-range YUV is non-standard"
// encoder rejection.
//
// Output dir override: POTOK_TEST_OUT=/some/dir.
func TestThumbnailRoundtrip(t *testing.T) {
	in := os.Getenv("POTOK_TEST_MEDIA")
	if in == "" {
		t.Skip("set POTOK_TEST_MEDIA=/path/to/media to run the thumbnail roundtrip")
	}

	outDir := os.Getenv("POTOK_TEST_OUT")
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "potok-media-test")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}

	for _, ts := range []float64{0, 1.5, 3} {
		// Fresh reader per call — a demux context owns its own seek cursor.
		f, err := os.Open(in)
		if err != nil {
			t.Fatalf("open %s: %v", in, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		jpg, err := Thumbnail(ctx, f, ts, 160, 90)
		cancel()
		f.Close()
		if err != nil {
			t.Fatalf("Thumbnail(t=%v): %v", ts, err)
		}
		if len(jpg) < 3 || jpg[0] != 0xFF || jpg[1] != 0xD8 || jpg[2] != 0xFF {
			t.Fatalf("Thumbnail(t=%v): not a JPEG (first bytes %x)", ts, jpg[:min(8, len(jpg))])
		}
		out := filepath.Join(outDir, "thumb-"+strconv.Itoa(int(ts*10))+".jpg")
		if err := os.WriteFile(out, jpg, 0o644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
		t.Logf("t=%.1fs → %s (%d bytes)", ts, out, len(jpg))
	}
}
