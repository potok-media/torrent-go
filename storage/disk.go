package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// diskBacking persists a disk-mode torrent's pieces so they survive RAM eviction and process restart.
// Layout: one sparse data file <hash>.dat holding piece bytes at their natural offsets, plus a packed
// bitmap sidecar <hash>.bitmap recording which pieces are present. On restart the bitmap is reloaded and
// those pieces are reported complete (see Cache.pieceComplete), so anacrolix serves them from disk
// instead of re-downloading — the whole point of disk mode.
//
// It is deliberately NOT a replacement for the custom RAM Cache: the media read path reads through the
// Cache (GetCache → Reader → AVIO). This just lets the Cache spill to / reload from disk. Bytes are only
// written after a piece is hash-verified complete (Piece.MarkComplete), so a set bit means good data.
type diskBacking struct {
	mu         sync.Mutex
	data       *os.File
	dataPath   string
	bitmapPath string
	pieceLen   int64
	pieceCount int
	have       []bool
}

var errNotOnDisk = errors.New("piece not persisted on disk")

func newDiskBacking(dir, hashHex string, pieceLen, totalSize int64, pieceCount int) (*diskBacking, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dataPath := filepath.Join(dir, hashHex+".dat")
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	// Sparse-size the file to the full torrent length so piece offsets are always in range.
	if err := f.Truncate(totalSize); err != nil {
		_ = f.Close()
		return nil, err
	}
	d := &diskBacking{
		data:       f,
		dataPath:   dataPath,
		bitmapPath: filepath.Join(dir, hashHex+".bitmap"),
		pieceLen:   pieceLen,
		pieceCount: pieceCount,
		have:       make([]bool, pieceCount),
	}
	d.loadBitmap() // best-effort; restores which pieces are already on disk from a previous run
	return d, nil
}

// HaveCount reports how many pieces are already persisted (used to decide whether a restored torrent has
// data worth reusing).
func (d *diskBacking) HaveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, h := range d.have {
		if h {
			n++
		}
	}
	return n
}

func (d *diskBacking) Has(idx int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return idx >= 0 && idx < len(d.have) && d.have[idx]
}

// WritePiece persists a verified complete piece and records it in the bitmap.
func (d *diskBacking) WritePiece(idx int, data []byte) error {
	off := int64(idx) * d.pieceLen
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.data.WriteAt(data, off); err != nil {
		return err
	}
	if idx >= 0 && idx < len(d.have) {
		d.have[idx] = true
	}
	d.saveBitmapLocked()
	return nil
}

// ReadPiece loads a persisted piece into buf (len(buf) must be the piece size).
func (d *diskBacking) ReadPiece(idx int, buf []byte) (int, error) {
	d.mu.Lock()
	present := idx >= 0 && idx < len(d.have) && d.have[idx]
	d.mu.Unlock()
	if !present {
		return 0, errNotOnDisk
	}
	return d.data.ReadAt(buf, int64(idx)*d.pieceLen)
}

// Clear drops a piece from the bitmap (hash failure / MarkNotComplete) so it is re-downloaded.
func (d *diskBacking) Clear(idx int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if idx >= 0 && idx < len(d.have) && d.have[idx] {
		d.have[idx] = false
		d.saveBitmapLocked()
	}
}

func (d *diskBacking) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.data.Close()
}

// Remove closes and deletes the data file and bitmap (torrent dropped / freed).
func (d *diskBacking) Remove() {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.data.Close()
	_ = os.Remove(d.dataPath)
	_ = os.Remove(d.bitmapPath)
}

// saveBitmapLocked writes the packed have-bitmap sidecar (1 bit per piece). Caller holds d.mu. Small
// (pieceCount/8 bytes) so rewriting it on every completed piece is cheap.
func (d *diskBacking) saveBitmapLocked() {
	packed := make([]byte, (d.pieceCount+7)/8)
	for i, h := range d.have {
		if h {
			packed[i/8] |= 1 << (uint(i) % 8)
		}
	}
	tmp := d.bitmapPath + ".tmp"
	if err := os.WriteFile(tmp, packed, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, d.bitmapPath) // atomic replace
}

func (d *diskBacking) loadBitmap() {
	packed, err := os.ReadFile(d.bitmapPath)
	if err != nil {
		return
	}
	for i := 0; i < d.pieceCount && i/8 < len(packed); i++ {
		if packed[i/8]&(1<<(uint(i)%8)) != 0 {
			d.have[i] = true
		}
	}
}
