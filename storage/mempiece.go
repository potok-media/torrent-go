package storage

import (
	"errors"
	"io"
	"sync"
	"time"
)

var ErrPieceEvicted = errors.New("piece data evicted from cache")

type MemPiece struct {
	mu            sync.Mutex
	size          int64
	data          []byte
	writtenBlocks []bool
	complete      bool
	accessed      time.Time
	accounted     int64 // bytes this piece has added to Cache.filled (so release subtracts exactly that)
	doneCh        chan struct{}
	closeOnce     sync.Once
	notifyCh      chan struct{}
}

func NewMemPiece(size int64) *MemPiece {
	numBlocks := (size + 16383) / 16384
	return &MemPiece{
		size:          size,
		writtenBlocks: make([]bool, numBlocks),
		accessed:      time.Now(),
		doneCh:        make(chan struct{}),
		notifyCh:      make(chan struct{}),
	}
}

// wake closes the current notify channel (waking every reader parked on it) and arms a fresh one.
// Callers must hold m.mu.
func (m *MemPiece) wake() {
	if m.notifyCh != nil {
		select {
		case <-m.notifyCh:
			// already closed
		default:
			close(m.notifyCh)
		}
	}
	m.notifyCh = make(chan struct{})
}

// Release drops the piece's buffer. Kept for the Close() path; ReleaseAndSize is the accounting form.
func (m *MemPiece) Release() { m.ReleaseAndSize() }

// ReleaseAndSize drops the buffer and returns exactly how many bytes this piece had contributed to
// Cache.filled, so the cache can subtract that with no read-then-release race (len(data) is the full
// piece size once allocated, and MarkNotComplete used to subtract nothing — both drifted `filled`).
func (m *MemPiece) ReleaseAndSize() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	sz := m.accounted
	m.accounted = 0
	m.data = nil
	m.writtenBlocks = nil
	m.complete = false
	m.wake()
	return sz
}

func (m *MemPiece) ReadAt(p []byte, off int64) (n int, err error) {
	// Single critical section: the old code checked data==nil under Lock, unlocked, then re-RLocked
	// to copy — a Release() in that gap turned an evicted piece into a truncated read reported as a
	// clean io.EOF (Reader.Read then returned short with a nil error).
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		return 0, ErrPieceEvicted
	}
	m.accessed = time.Now()
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n = copy(p, m.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	return
}

func (m *MemPiece) WriteAt(p []byte, off int64) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.accessed = time.Now()
	if m.data == nil {
		m.data = make([]byte, m.size)
	}
	if off >= int64(len(m.data)) {
		return 0, io.ErrShortWrite
	}
	n = copy(m.data[off:], p)
	if n < len(p) {
		err = io.ErrShortWrite
	}
	m.accounted += int64(n) // mirrors Cache.UpdateFilled(n) so release nets out to zero

	startBlock := off / 16384
	endBlock := (off + int64(n) - 1) / 16384
	for i := startBlock; i <= endBlock && i < int64(len(m.writtenBlocks)); i++ {
		m.writtenBlocks[i] = true
	}

	m.wake()
	return
}

func (m *MemPiece) MarkComplete() {
	m.mu.Lock()
	m.complete = true
	if m.data != nil {
		for i := range m.writtenBlocks {
			m.writtenBlocks[i] = true
		}
	}
	m.accessed = time.Now()
	m.wake()
	m.mu.Unlock()

	m.closeOnce.Do(func() {
		close(m.doneCh)
	})
}

func (m *MemPiece) IsComplete() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.complete && m.data != nil
}

func (m *MemPiece) HasRange(off int64, length int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasRangeLocked(off, length)
}

// WaitRange atomically reports whether [off, off+length) is available now and, if not, returns the
// notify channel that the NEXT write/complete/release will close — captured under the same lock as
// the check, so a wakeup can never be lost between the check and parking on the channel (the R4
// lost-wakeup). Reader.Read re-fetches the MemPiece each iteration, so an eviction+re-download that
// swaps in a fresh piece can't strand a waiter on the released piece's orphaned channel either.
func (m *MemPiece) WaitRange(off, length int64) (ready bool, ch <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasRangeLocked(off, length) {
		return true, nil
	}
	return false, m.notifyCh
}

func (m *MemPiece) hasRangeLocked(off int64, length int64) bool {
	if m.complete && m.data != nil {
		return true
	}
	if m.data == nil {
		return false
	}

	startBlock := off / 16384
	endBlock := (off + length - 1) / 16384
	if startBlock < 0 || endBlock >= int64(len(m.writtenBlocks)) {
		return false
	}
	for i := startBlock; i <= endBlock; i++ {
		if !m.writtenBlocks[i] {
			return false
		}
	}
	return true
}

func (m *MemPiece) Done() <-chan struct{} {
	return m.doneCh
}

func (m *MemPiece) Accessed() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accessed
}

// HasData reports whether the piece currently holds any buffer (resident, possibly partial). Used to
// decide whether a disk-mode piece needs hydration from disk.
func (m *MemPiece) HasData() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data != nil
}

// Snapshot returns a copy of the full piece bytes if complete, else nil. Used to persist a verified piece
// to disk without holding the lock across disk I/O.
func (m *MemPiece) Snapshot() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.complete || m.data == nil {
		return nil
	}
	out := make([]byte, len(m.data))
	copy(out, m.data)
	return out
}

// LoadComplete hydrates an empty piece from disk-loaded bytes and marks it complete, atomically. Returns
// the number of bytes it actually installed (0 if the piece already had data — i.e. a concurrent writer
// or hydrator won the race), so the caller accounts exactly one contribution to Cache.filled.
func (m *MemPiece) LoadComplete(src []byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data != nil {
		return 0 // lost the race; someone else is filling this piece
	}
	m.data = make([]byte, len(src))
	copy(m.data, src)
	for i := range m.writtenBlocks {
		m.writtenBlocks[i] = true
	}
	m.accounted += int64(len(src))
	m.complete = true
	m.accessed = time.Now()
	m.wake()
	m.closeOnce.Do(func() { close(m.doneCh) })
	return len(src)
}
