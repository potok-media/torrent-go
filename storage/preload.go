package storage

import (
	"log/slog"
	"time"

	"github.com/anacrolix/torrent"
)

func (c *Cache) Preload(t *torrent.Torrent, file *torrent.File, size int64) error {
	totalLength := t.Length()

	fileStartOffset := file.Offset()
	preloadEndOffset := fileStartOffset + size
	if preloadEndOffset > fileStartOffset+file.Length() {
		preloadEndOffset = fileStartOffset + file.Length()
	}

	startPiece := int(fileStartOffset / c.pieceLen)
	endPiece := int((preloadEndOffset - 1) / c.pieceLen)

	fileEndOffset := fileStartOffset + file.Length()
	lastPiece := int((fileEndOffset - 1) / c.pieceLen)

	slog.Info("Starting preload",
		"file", file.Path(),
		"startPiece", startPiece,
		"endPiece", endPiece,
		"lastPiece", lastPiece,
		"preloadSizeMB", float64(size)/(1024*1024),
	)

	// Prioritize start pieces
	for i := startPiece; i <= endPiece && i < c.pieceCount; i++ {
		t.Piece(i).SetPriority(torrent.PiecePriorityNow)
	}

	// Prioritize last 2 pieces of the file (critical for ffprobe/ffmpeg MKV/MP4 seeking)
	lastPieces := []int{}
	if lastPiece >= 0 && lastPiece < c.pieceCount {
		t.Piece(lastPiece).SetPriority(torrent.PiecePriorityNow)
		lastPieces = append(lastPieces, lastPiece)
		if lastPiece-1 >= startPiece {
			t.Piece(lastPiece - 1).SetPriority(torrent.PiecePriorityNow)
			lastPieces = append(lastPieces, lastPiece-1)
		}
	}

	// Wait for start pieces to complete
	for i := startPiece; i <= endPiece && i < c.pieceCount; i++ {
		pSize := c.getPieceSize(i, totalLength)
		mp := c.GetOrCreateMemPiece(i, pSize)

		select {
		case <-mp.Done():
			// piece downloaded
		case <-time.After(15 * time.Second):
			slog.Warn("Preload timeout waiting for start piece", "piece", i)
		}
	}

	// Wait for last pieces to complete
	for _, lp := range lastPieces {
		pSize := c.getPieceSize(lp, totalLength)
		mp := c.GetOrCreateMemPiece(lp, pSize)

		select {
		case <-mp.Done():
			// piece downloaded
		case <-time.After(15 * time.Second):
			slog.Warn("Preload timeout waiting for last piece", "piece", lp)
		}
	}

	slog.Info("Preload finished", "file", file.Path())
	return nil
}

func (c *Cache) getPieceSize(idx int, totalLength int64) int64 {
	if idx < 0 || idx >= c.pieceCount {
		return 0
	}
	if idx == c.pieceCount-1 {
		return totalLength - int64(idx)*c.pieceLen
	}
	return c.pieceLen
}
