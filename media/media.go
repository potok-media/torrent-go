// Package media is TorrentGo v2's in-process media core. It drives ffmpeg's libav* libraries directly
// (via go-astiav, cgo) instead of forking the ffmpeg BINARY — so demux/seek/transcode/mux become
// in-process function calls fed by a custom AVIO reading our RAM torrent cache. This dissolves the
// ffmpeg-process orchestration (spawn/reposition/reap/loopback/watchdog) and makes a hang impossible by
// construction: WE own the AVIO read callback, so a non-resident byte returns "retry" instead of blocking.
//
// This file is only the STEP-1 toolchain smoke test (prove cgo → libav links + the asticode deps
// resolve). The real core — a per-(hash,file) context pool, `Segment()` (copy for H.264 / HW-transcode
// for HEVC → one fMP4 fragment), `InitSegment()`, `ProbeTracks()`, windowed subtitles via go-astisub,
// and thumbnails — lands in subsequent steps once the toolchain builds green in Docker.
package media

import (
	"github.com/asticode/go-astiav"

	// Pinned into the build now so the whole asticode stack is wired from the start:
	//   go-astisub — text subtitles (SRT/ASS/SSA/VTT…): demux packets → parse → Fragment() the window.
	//   go-astikit — utils (GoroutineLimiter for transcode concurrency, FIFOMutex, PCM for the analyzer).
	_ "github.com/asticode/go-astikit"
	_ "github.com/asticode/go-astisub"
)

// LibavLinked reports whether the go-astiav (libav) cgo layer is available at runtime. It allocates and
// frees a format context — the cheapest call that actually exercises the linked libavformat. Used as a
// startup self-check while the media core is being built out.
func LibavLinked() bool {
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return false
	}
	fc.Free()
	return true
}
