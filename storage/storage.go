package storage

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"Potok.Backend.TorrentGo/config"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type Storage struct {
	mu     sync.RWMutex
	caches map[metainfo.Hash]*Cache
	config *config.Config

	// Global piece-cache accountant. `globalFilled` tracks resident piece bytes across ALL torrents;
	// eviction is driven by this total against `globalCapacity` (= POTOK_CACHE_SIZE_MB), so N torrents
	// can't each independently grow to the old per-torrent cap → N × 256MB OOM. Per-`Cache.capacity`
	// stays as a soft ceiling but the global budget is authoritative.
	globalFilled   atomic.Int64
	globalCapacity atomic.Int64 // runtime-adjustable (SetGlobalCapacity); the single knob everything derives from
	evictMu        sync.Mutex   // serializes global eviction passes so two writers can't double-evict

	// diskModes records which torrents want disk persistence (set via SetMode before/around add). OpenTorrent
	// consults it to attach a diskBacking. Empty / false → RAM-only stream cache (default).
	diskModes map[metainfo.Hash]bool
}

// perStreamWindowBytes is the un-evictable RAM a single live stream protects (its ~64MB read-ahead window
// plus container head/foot + in-flight slack). Every derived limit is computed against this so the whole
// system scales from ONE user-facing knob — the global cache budget — instead of a pile of env vars.
const perStreamWindowBytes = playbackReadaheadBytes + (16 << 20)

// minGlobalCapacity keeps the budget large enough for at least one comfortable stream.
const minGlobalCapacity = perStreamWindowBytes

// PerStreamWindowBytes exposes the per-stream RAM estimate so the UI can derive/preview limits identically.
func PerStreamWindowBytes() int64 { return perStreamWindowBytes }

func NewStorage(cfg *config.Config) *Storage {
	s := &Storage{
		caches:    make(map[metainfo.Hash]*Cache),
		config:    cfg,
		diskModes: make(map[metainfo.Hash]bool),
	}
	cap := cfg.CacheSizeBytes
	if cap < minGlobalCapacity {
		cap = minGlobalCapacity
	}
	s.globalCapacity.Store(cap)
	return s
}

func (s *Storage) addGlobalFilled(n int64) { s.globalFilled.Add(n) }

// DownloadDir is the disk-mode download directory (empty = disk mode off).
func (s *Storage) DownloadDir() string {
	if s.config != nil {
		return s.config.DownloadDir
	}
	return ""
}

// CacheInfo is one torrent's memory footprint for diagnostics.
type CacheInfo struct {
	Hash       string
	CacheBytes int64
	DiskBytes  int64
}

// CacheInfos snapshots per-torrent RAM cache + disk bytes for the diagnostics page.
func (s *Storage) CacheInfos() []CacheInfo {
	s.mu.RLock()
	caches := make(map[metainfo.Hash]*Cache, len(s.caches))
	for h, c := range s.caches {
		caches[h] = c
	}
	s.mu.RUnlock()
	out := make([]CacheInfo, 0, len(caches))
	for h, c := range caches {
		out = append(out, CacheInfo{Hash: h.HexString(), CacheBytes: c.Filled(), DiskBytes: c.DiskBytes()})
	}
	return out
}

// GlobalFilled / GlobalCapacity expose the global accountant for management stats.
func (s *Storage) GlobalFilled() int64   { return s.globalFilled.Load() }
func (s *Storage) GlobalCapacity() int64 { return s.globalCapacity.Load() }

// SetGlobalCapacity adjusts the RAM budget at runtime (from the UI settings) and immediately evicts down
// to the new ceiling. Clamped to a sane minimum so at least one stream always fits.
func (s *Storage) SetGlobalCapacity(bytes int64) {
	if bytes < minGlobalCapacity {
		bytes = minGlobalCapacity
	}
	s.globalCapacity.Store(bytes)
	s.evictGlobalIfNeeded()
}

// DerivedMaxStreams is how many concurrent streams the current budget safely allows (their protected
// read-ahead windows must fit under it). This replaces the old POTOK_MAX_STREAMS env knob.
func (s *Storage) DerivedMaxStreams() int {
	n := int(s.globalCapacity.Load() / perStreamWindowBytes)
	if n < 1 {
		n = 1
	}
	return n
}

// DerivedMaxAudioTranscoders scales the ~100MB-each continuous-AAC transcoder cap with the budget, floored
// so mixed-audio playback isn't starved. Replaces the old POTOK_MAX_AUDIO_TRANSCODERS env knob.
func (s *Storage) DerivedMaxAudioTranscoders() int {
	n := int(s.globalCapacity.Load() / (128 << 20))
	if n < 2 {
		n = 2
	}
	return n
}

// SetMode selects a torrent's storage mode BEFORE its OpenTorrent runs (call right after adding it, while
// metadata is still resolving). If the cache already opened (info was instant), the disk backing is
// attached retroactively — safe because no piece has completed yet at that point.
func (s *Storage) SetMode(hash metainfo.Hash, disk bool) {
	s.mu.Lock()
	s.diskModes[hash] = disk
	c := s.caches[hash]
	s.mu.Unlock()
	if disk && c != nil {
		s.attachDisk(c, hash)
	}
}

func (s *Storage) attachDisk(c *Cache, hash metainfo.Hash) {
	if s.config == nil || s.config.DownloadDir == "" {
		return
	}
	c.mu.Lock()
	need := c.disk == nil && !c.closed && c.pieceLen > 0
	pieceLen, pieceCount, totalSize := c.pieceLen, c.pieceCount, c.totalSize
	c.mu.Unlock()
	if !need {
		return
	}
	db, err := newDiskBacking(s.config.DownloadDir, hash.HexString(), pieceLen, totalSize, pieceCount)
	if err != nil {
		slog.Warn("disk backing init failed (retroactive)", "hash", hash.HexString(), "error", err)
		return
	}
	c.mu.Lock()
	if c.disk == nil && !c.closed {
		c.disk = db
		c.mu.Unlock()
		slog.Info("disk-mode attached", "hash", hash.HexString(), "piecesOnDisk", db.HaveCount())
		return
	}
	c.mu.Unlock()
	_ = db.Close() // lost a race; another attach won
}

// evictGlobalIfNeeded runs a cross-torrent LRU eviction when total resident bytes exceed the global
// budget. Called after a piece completes. Candidates are gathered from every cache (excluding pieces in
// an active reader's protected window), sorted globally by last-access, and evicted oldest-first until
// back under budget. Each eviction re-validates the piece under its cache lock, so a concurrent
// access/eviction can't corrupt the accounting.
func (s *Storage) evictGlobalIfNeeded() {
	capacity := s.globalCapacity.Load()
	if capacity <= 0 || s.globalFilled.Load() <= capacity {
		return
	}
	s.evictMu.Lock()
	defer s.evictMu.Unlock()
	capacity = s.globalCapacity.Load()
	if s.globalFilled.Load() <= capacity {
		return
	}

	s.mu.RLock()
	caches := make([]*Cache, 0, len(s.caches))
	for _, c := range s.caches {
		caches = append(caches, c)
	}
	s.mu.RUnlock()

	type cand struct {
		c        *Cache
		idx      int
		mp       *MemPiece
		accessed time.Time
	}
	var cands []cand
	for _, c := range caches {
		for _, e := range c.collectEvictCandidates() {
			cands = append(cands, cand{c: c, idx: e.index, mp: e.mp, accessed: e.accessed})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].accessed.Before(cands[j].accessed) })

	for _, cd := range cands {
		if s.globalFilled.Load() <= capacity {
			break
		}
		cd.c.evictPiece(cd.idx, cd.mp)
	}
}

// OpenTorrent implements storage.ClientImpl
func (s *Storage) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pieceCount := len(info.Pieces) / 20

	// Per-torrent soft cap = the global budget, so a single torrent may use the whole cache; the global
	// accountant (globalFilled vs globalCapacity) is what actually bounds total memory across torrents.
	cache := NewCache(hash, s.globalCapacity.Load(), info.PieceLength, pieceCount)
	cache.store = s
	cache.totalSize = info.TotalLength()

	// Disk mode: attach a diskBacking so pieces spill to / reload from disk (and survive restart). A
	// reused .dat+.bitmap from a previous run makes those pieces immediately complete (no re-download).
	if s.diskModes[hash] && s.config != nil && s.config.DownloadDir != "" {
		if db, err := newDiskBacking(s.config.DownloadDir, hash.HexString(), info.PieceLength, info.TotalLength(), pieceCount); err == nil {
			cache.disk = db
			slog.Info("disk-mode torrent opened", "hash", hash.HexString(), "piecesOnDisk", db.HaveCount(), "totalPieces", pieceCount)
		} else {
			slog.Warn("disk backing init failed; falling back to stream", "hash", hash.HexString(), "error", err)
		}
	}

	s.caches[hash] = cache

	impl := storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl {
			return cache.Piece(p)
		},
		Close: func() error {
			return cache.Close()
		},
	}
	return impl, nil
}

func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, cache := range s.caches {
		_ = cache.Close()
	}
	s.caches = make(map[metainfo.Hash]*Cache)
	return nil
}

func (s *Storage) GetCache(hash metainfo.Hash) (*Cache, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cache, ok := s.caches[hash]
	return cache, ok
}

func (s *Storage) DeleteCache(hash metainfo.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cache, ok := s.caches[hash]; ok {
		_ = cache.Close()
		delete(s.caches, hash)
	}
}
