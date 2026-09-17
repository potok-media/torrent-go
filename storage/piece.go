package storage

import (
	"log/slog"
	"sync"

	"github.com/anacrolix/torrent/storage"
)

type Piece struct {
	mu    sync.Mutex
	index int
	size  int64
	cache *Cache
}

func NewPiece(index int, size int64, cache *Cache) *Piece {
	return &Piece{
		index: index,
		size:  size,
		cache: cache,
	}
}

func (p *Piece) ReadAt(buf []byte, off int64) (n int, err error) {
	slog.Debug("Piece ReadAt", "index", p.index, "off", off, "len", len(buf))
	mp := p.cache.GetOrCreateMemPiece(p.index, p.size)
	// Disk mode: an evicted-but-persisted piece is reloaded from disk here (covers anacrolix seeding reads
	// too, not just the player Reader). No-op in stream mode / when already resident.
	p.cache.hydrateIfNeeded(p.index, mp, p.size)
	return mp.ReadAt(buf, off)
}

func (p *Piece) WriteAt(buf []byte, off int64) (n int, err error) {
	slog.Debug("Piece WriteAt", "index", p.index, "off", off, "len", len(buf))
	mp := p.cache.GetOrCreateMemPiece(p.index, p.size)
	n, err = mp.WriteAt(buf, off)
	if err == nil && n > 0 {
		p.cache.UpdateFilled(int64(n))
	}
	return n, err
}

func (p *Piece) MarkComplete() error {
	slog.Debug("Piece MarkComplete", "index", p.index)
	mp := p.cache.GetOrCreateMemPiece(p.index, p.size)
	mp.MarkComplete()
	// Disk mode: persist the verified piece to disk (once) before it can be evicted from RAM.
	p.cache.persistIfDisk(p.index, mp)
	p.cache.evictAfterComplete()
	return nil
}

func (p *Piece) MarkNotComplete() error {
	slog.Debug("Piece MarkNotComplete", "index", p.index)
	p.cache.clearDisk(p.index) // hash failure → drop any stale on-disk copy so it re-downloads
	p.cache.MarkNotComplete(p.index)
	return nil
}

func (p *Piece) Done() <-chan struct{} {
	mp := p.cache.GetOrCreateMemPiece(p.index, p.size)
	return mp.Done()
}

func (p *Piece) Completion() storage.Completion {
	// RAM-resident-complete OR persisted on disk. The disk check is what keeps an evicted disk-mode piece
	// "complete" so anacrolix serves it from disk instead of re-downloading. Does not hydrate (hot path).
	complete := p.cache.pieceComplete(p.index)
	slog.Debug("Piece Completion check", "index", p.index, "complete", complete)
	return storage.Completion{
		Complete: complete,
		Ok:       true,
		Err:      nil,
	}
}

func (p *Piece) Size() int64 {
	return p.size
}

func (p *Piece) Index() int {
	return p.index
}
