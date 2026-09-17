package storage

import (
	"time"

	"github.com/anacrolix/torrent"
)

// ReadClass is the single knob describing WHY a read is happening. It replaces the old opaque
// `background bool`, which silently conflated two unrelated things — piece priority AND wait
// patience — and let them drift out of sync (a background read used to claim only its exact
// current piece at Normal with NO read-ahead, yet wait a patient 10 minutes, while its caller's
// ffmpeg was capped at 45s: a windowed demux into an un-downloaded region could then only freeze
// then 500). One class → one policy, defined here in one place, so that inversion can't recur.
type ReadClass int

const (
	// ClassPlayback — the live player / HLS producer feed. Highest priority, big read-ahead, short
	// wait so a genuinely stalled player surfaces fast.
	ClassPlayback ReadClass = iota
	// ClassAheadDemux — an on-demand extraction of a bounded window the client wants soon (subtitle
	// window, thumbnail). Bounded read-ahead a rung BELOW playback so it pipelines and completes
	// without ever outbidding a live player.
	ClassAheadDemux
	// ClassColdProbe — ffprobe / moov+Cues / header+footer prefetch: jumps to head/foot, tiny, no
	// linear read-ahead.
	ClassColdProbe
	// ClassPatientDemux — the full-file demux fallback (a container that can't be seeked). A linear
	// whole-file read whose caller cap is minutes, so its per-block wait budget is large.
	ClassPatientDemux
	// ClassMKVIndex — the dedicated MKV Cues (keyframe index) read for the copy-vs-transcode decision. Like
	// ClassColdProbe but boosts a WIDE file-tail window so the multi-piece Cues download in time (the plain
	// 2-piece tail boost misses them on a large rip → slow transcode fallback). Runs AFTER the codec probe,
	// which warms the front, so the wide tail boost can't starve the header read.
	ClassMKVIndex
)

// playbackReadaheadBytes is the live player's read-ahead budget in BYTES (not a fixed piece count).
// The effective piece count is derived per-torrent from its piece length, so the protected read-ahead
// window scales with piece size and can never exceed the RAM piece cache: a flat piece count (e.g. 30)
// is ~60MB at 2MB pieces but ~480MB at 16MB pieces — the latter blows the 256MB cache, and because the
// read-ahead window is eviction-protected, the cache would sit permanently over-cap with nothing left to
// evict → stalls/thrash for every reader. ~64MB tracks the old ~30-piece depth on typical 2MB-piece
// torrents while staying safely bounded on large-piece ones.
const playbackReadaheadBytes = 64 << 20 // 64 MiB

// headFootTailBytes is the file-TAIL window (byte-bounded, scales with piece size like the read-ahead) that
// ClassMKVIndex boosts to High. MKV Cues sit near — but not AT — the end (often before trailing Tags/
// Attachments) and span several pieces on a multi-GB rip, so the plain 2-piece tail boost misses them and
// the cold Cues read fails ("invalid varint length") → the whole file falls to the slow transcode grid.
const headFootTailBytes = 32 << 20 // 32 MiB

// classPolicy is the fixed mapping from a ReadClass to piece priority + read-ahead + wait budget.
type classPolicy struct {
	curPrio        torrent.PiecePriority // priority for the piece currently being read
	aheadPrio      torrent.PiecePriority // priority for the read-ahead window
	aheadPieces    int                   // fixed pieces ahead to claim (used when aheadBytes == 0)
	aheadBytes     int64                 // byte-bounded read-ahead (preferred when > 0); scales with piece size
	headFootBoost  bool                  // also boost the file's first/last 2 pieces (container index)
	tailBoostBytes int64                 // when > 0, widen the FOOT boost to this byte window (MKV Cues) instead of 2 pieces
	waitBudget     time.Duration         // max wall time to wait for one block range, before any caller deadline
}

// aheadPiecesFor is the effective read-ahead depth in pieces for a torrent whose pieces are pieceLen
// bytes. A byte budget (aheadBytes) is preferred and converted at runtime so the eviction-protected
// window never outgrows the cache; classes with a fixed small count keep it.
func (p classPolicy) aheadPiecesFor(pieceLen int64) int {
	if p.aheadBytes > 0 && pieceLen > 0 {
		n := int(p.aheadBytes / pieceLen)
		if n < 1 {
			n = 1
		}
		return n
	}
	return p.aheadPieces
}

// anacrolix piece-priority order (low→high): None < Normal < High < Readahead < Next < Now.
// A demux read-ahead therefore uses Normal (NOT Readahead, which outranks the player's High
// read-ahead): the R1 fix is that the window is REQUESTED at all (Normal > None) — the old code left
// every piece but the current one at None, so a wanted window could never pipeline. Playback keeps
// Now for the current piece and High for its read-ahead, so it always outranks a demux under
// contention, while the desired[] max-merge keeps any co-wanted piece at the player's level.
func (c ReadClass) policy() classPolicy {
	switch c {
	case ClassPlayback:
		// Byte-bounded read-ahead (aheadBytes) so the eviction-protected window scales with piece size.
		return classPolicy{curPrio: torrent.PiecePriorityNow, aheadPrio: torrent.PiecePriorityHigh, aheadBytes: playbackReadaheadBytes, headFootBoost: true, waitBudget: 15 * time.Second}
	case ClassAheadDemux:
		// headFootBoost=true: a windowed subtitle / thumbnail extraction input-seeks (-ss), which needs
		// the container index (MKV Cues / MP4 moov) at the file head/foot — boost those so the seek
		// resolves instead of stalling even when the window's own region is already resident.
		return classPolicy{curPrio: torrent.PiecePriorityNormal, aheadPrio: torrent.PiecePriorityNormal, aheadPieces: 12, headFootBoost: true, waitBudget: 30 * time.Second}
	case ClassColdProbe:
		return classPolicy{curPrio: torrent.PiecePriorityNormal, aheadPrio: torrent.PiecePriorityNone, headFootBoost: true, waitBudget: 20 * time.Second}
	case ClassMKVIndex:
		// The Cues read for the copy grid. Same as ColdProbe but with a WIDE tail boost so the multi-piece
		// Cues download in time → COPY instead of transcode. Safe because it runs after the codec probe
		// (front already warm), so the tail boost can't starve the header.
		return classPolicy{curPrio: torrent.PiecePriorityNormal, aheadPrio: torrent.PiecePriorityNone, headFootBoost: true, tailBoostBytes: headFootTailBytes, waitBudget: 20 * time.Second}
	case ClassPatientDemux:
		return classPolicy{curPrio: torrent.PiecePriorityNormal, aheadPrio: torrent.PiecePriorityNormal, aheadPieces: 8, headFootBoost: false, waitBudget: 4 * time.Minute}
	default:
		return classPolicy{curPrio: torrent.PiecePriorityNormal, aheadPrio: torrent.PiecePriorityNone, headFootBoost: true, waitBudget: 20 * time.Second}
	}
}

// ParseReadClass maps the loopback `?class=` query to a ReadClass. Unknown/empty defaults to
// ClassPlayback (a real client stream). The legacy `bg=1` flag maps to ClassAheadDemux for one
// release so an in-flight migration doesn't break.
func ParseReadClass(class string, legacyBg bool) ReadClass {
	switch class {
	case "playback":
		return ClassPlayback
	case "ahead":
		return ClassAheadDemux
	case "cold":
		return ClassColdProbe
	case "patient":
		return ClassPatientDemux
	}
	if legacyBg {
		return ClassAheadDemux
	}
	return ClassPlayback
}
