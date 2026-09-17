package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type Reader struct {
	mu           sync.Mutex
	torrent      *torrent.Torrent
	cache        *Cache
	fileOffset   int64
	fileSize     int64
	pos          int64
	closed       bool
	lastPieceIdx int
	closeCh      chan struct{}
	closeOnce    sync.Once
	ctx          context.Context
	// class describes WHY this read happens (playback / ahead-demux / probe / patient) and maps to a
	// single policy: piece priority, read-ahead window, and per-block wait budget. waitDeadline is the
	// caller's hard op deadline (zero = none); the reader waits min(policy budget, until(deadline)) so
	// an inner wait can never outlive its caller's outer ffmpeg cap. Both immutable after construction.
	class        ReadClass
	waitDeadline time.Time
}

func NewReader(ctx context.Context, t *torrent.Torrent, cache *Cache, fileOffset, fileSize int64, class ReadClass, waitDeadline time.Time) *Reader {
	r := &Reader{
		torrent:      t,
		cache:        cache,
		fileOffset:   fileOffset,
		fileSize:     fileSize,
		lastPieceIdx: -1,
		closeCh:      make(chan struct{}),
		ctx:          ctx,
		class:        class,
		waitDeadline: waitDeadline,
	}
	cache.RegisterReader(r)
	return r
}

func (r *Reader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, errors.New("reader closed")
	}

	if r.pos >= r.fileSize {
		r.mu.Unlock()
		return 0, io.EOF
	}

	pos := r.pos
	fileOffset := r.fileOffset
	fileSize := r.fileSize
	r.mu.Unlock()

	// Per-class wait policy. The per-block wait budget is capped by the caller's hard deadline (if
	// any) inside the wait loop, so an inner block-wait can never outlive the caller's outer op cap
	// (the old fixed 10-min background wait vs a 45s caller ffmpeg cap was exactly the freeze).
	pol := r.class.policy()

	slog.Debug("Reader Read called (unlocked)", "len(p)", len(p), "pos", pos, "fileSize", fileSize)

	limit := fileSize - pos
	if int64(len(p)) > limit {
		p = p[:limit]
	}
	if len(p) == 0 {
		return 0, nil
	}

	totalRead := 0
	for len(p) > 0 {
		absOffset := fileOffset + pos
		pieceIdx := int(absOffset / r.cache.pieceLen)
		pieceOffset := absOffset % r.cache.pieceLen
		pieceRemaining := r.cache.pieceLen - pieceOffset

		pieceSize := r.cache.pieceLen
		if pieceIdx == r.cache.pieceCount-1 {
			totalLen := r.torrent.Length()
			pieceSize = totalLen - int64(pieceIdx)*r.cache.pieceLen
			pieceRemaining = pieceSize - pieceOffset
		}

		if pieceRemaining <= 0 {
			break
		}

		toRead := int64(len(p))
		if toRead > pieceRemaining {
			toRead = pieceRemaining
		}

		mp := r.cache.GetOrCreateMemPiece(pieceIdx, pieceSize)
		// Disk mode: reload an evicted-but-persisted piece from disk so the read resolves without a
		// re-download. No-op in stream mode / when already resident.
		r.cache.hydrateIfNeeded(pieceIdx, mp, pieceSize)
		if !mp.IsComplete() && r.torrent != nil {
			r.torrent.Piece(pieceIdx).UpdateCompletion()
			r.torrent.Piece(pieceIdx).SetPriority(pol.curPrio)
		}

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return totalRead, errors.New("reader closed")
		}
		lastPieceIdx := r.lastPieceIdx
		r.mu.Unlock()

		if pieceIdx != lastPieceIdx {
			r.mu.Lock()
			r.lastPieceIdx = pieceIdx
			r.mu.Unlock()

			fileStartPiece := int(fileOffset / r.cache.pieceLen)
			fileEndPiece := int((fileOffset + fileSize - 1) / r.cache.pieceLen)
			r.cache.UpdatePriorities(fileStartPiece, fileEndPiece)
		}

		// Wait until the block range is available. Re-fetch the piece each iteration and capture its
		// notify channel atomically with the readiness check (WaitRange), so neither a lost wakeup nor
		// an eviction+re-download (which swaps in a fresh MemPiece) can strand us.
		for {
			mp = r.cache.GetOrCreateMemPiece(pieceIdx, pieceSize)
			ready, watchCh := mp.WaitRange(pieceOffset, toRead)
			if ready {
				break
			}
			budget := pol.waitBudget
			if !r.waitDeadline.IsZero() {
				if d := time.Until(r.waitDeadline); d < budget {
					budget = d
				}
			}
			if budget <= 0 {
				return totalRead, context.DeadlineExceeded
			}
			slog.Debug("Reader waiting for piece block range", "piece", pieceIdx, "offset", pieceOffset, "toRead", toRead)
			select {
			case <-watchCh:
			case <-mp.Done():
			case <-r.closeCh:
				return totalRead, errors.New("reader closed")
			case <-r.ctx.Done():
				return totalRead, r.ctx.Err()
			case <-time.After(budget):
				slog.Warn("Reader timeout waiting for piece block range", "piece", pieceIdx, "offset", pieceOffset, "toRead", toRead, "class", int(r.class))
				return totalRead, errors.New("timeout waiting for piece data")
			}
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				return totalRead, errors.New("reader closed during read")
			}
		}

		nr, err := mp.ReadAt(p[:toRead], pieceOffset)
		if err == ErrPieceEvicted {
			slog.Warn("Reader detected piece eviction, retrying download", "piece", pieceIdx)
			continue
		}
		if nr > 0 {
			totalRead += nr
			pos += int64(nr)
			r.mu.Lock()
			r.pos = pos
			r.mu.Unlock()
			p = p[nr:]
		}
		if err != nil && err != io.EOF {
			return totalRead, err
		}
		if nr == 0 {
			break
		}
	}

	if totalRead == 0 && pos >= fileSize {
		return 0, io.EOF
	}

	return totalRead, nil
}

func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()

	slog.Debug("Reader Seek called", "offset", offset, "whence", whence, "pos", r.pos)

	if r.closed {
		r.mu.Unlock()
		return 0, errors.New("reader closed")
	}

	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = r.fileSize + offset
	default:
		r.mu.Unlock()
		return 0, errors.New("invalid whence")
	}

	if newPos < 0 {
		r.mu.Unlock()
		return 0, errors.New("negative position")
	}
	r.pos = newPos
	r.mu.Unlock()

	// Trigger priority update on seek
	if r.torrent != nil && r.cache != nil {
		fileStartPiece := int(r.fileOffset / r.cache.pieceLen)
		fileEndPiece := int((r.fileOffset + r.fileSize - 1) / r.cache.pieceLen)
		r.cache.UpdatePriorities(fileStartPiece, fileEndPiece)
	}

	return newPos, nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	wasClosed := r.closed
	if !wasClosed {
		r.closed = true
		r.closeOnce.Do(func() {
			close(r.closeCh)
		})
	}
	r.mu.Unlock()

	if !wasClosed {
		r.cache.UnregisterReader(r)
		fileStartPiece := int(r.fileOffset / r.cache.pieceLen)
		fileEndPiece := int((r.fileOffset + r.fileSize - 1) / r.cache.pieceLen)
		r.cache.UpdatePriorities(fileStartPiece, fileEndPiece)
	}
	return nil
}

func (r *Reader) GetActiveWindow() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, 0
	}

	absOffset := r.fileOffset + r.pos
	currPiece := int(absOffset / r.cache.pieceLen)

	// Protect from eviction the window this reader will actually consume next — its class's read-ahead
	// span (byte-bounded per piece size so it can't outgrow the cache; min 2 so an in-flight read isn't
	// evicted out from under us).
	ahead := r.class.policy().aheadPiecesFor(r.cache.pieceLen)
	if ahead < 2 {
		ahead = 2
	}
	start := currPiece - 2
	if start < 0 {
		start = 0
	}
	end := currPiece + ahead
	if end >= r.cache.pieceCount {
		end = r.cache.pieceCount - 1
	}
	return start, end
}
