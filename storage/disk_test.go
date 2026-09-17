package storage

import (
	"bytes"
	"testing"
)

// TestDiskBackingRoundTrip covers the core disk-mode invariants: a written piece reads back byte-exact,
// Has reflects presence, Clear drops it, and — critically for restart survival — a fresh diskBacking over
// the same directory reloads the persisted bitmap so the piece is still "present" without re-download.
func TestDiskBackingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	const (
		hash       = "aabbccddeeff00112233445566778899aabbccdd"
		pieceLen   = int64(1024)
		pieceCount = 4
		totalSize  = pieceLen * pieceCount
	)

	db, err := newDiskBacking(dir, hash, pieceLen, totalSize, pieceCount)
	if err != nil {
		t.Fatalf("newDiskBacking: %v", err)
	}

	p2 := bytes.Repeat([]byte{0xAB}, int(pieceLen))
	if err := db.WritePiece(2, p2); err != nil {
		t.Fatalf("WritePiece: %v", err)
	}
	if !db.Has(2) {
		t.Fatal("Has(2) should be true after write")
	}
	if db.Has(1) {
		t.Fatal("Has(1) should be false (never written)")
	}
	if db.HaveCount() != 1 {
		t.Fatalf("HaveCount = %d, want 1", db.HaveCount())
	}

	buf := make([]byte, pieceLen)
	n, err := db.ReadPiece(2, buf)
	if err != nil || int64(n) != pieceLen || !bytes.Equal(buf, p2) {
		t.Fatalf("ReadPiece(2): n=%d err=%v equal=%v", n, err, bytes.Equal(buf, p2))
	}
	if _, err := db.ReadPiece(1, buf); err != errNotOnDisk {
		t.Fatalf("ReadPiece(1) err = %v, want errNotOnDisk", err)
	}
	_ = db.Close()

	// Simulate a restart: a new backing over the same dir must reload the bitmap and still serve piece 2.
	db2, err := newDiskBacking(dir, hash, pieceLen, totalSize, pieceCount)
	if err != nil {
		t.Fatalf("re-open newDiskBacking: %v", err)
	}
	defer db2.Close()
	if !db2.Has(2) {
		t.Fatal("piece 2 should survive restart via the persisted bitmap")
	}
	n, err = db2.ReadPiece(2, buf)
	if err != nil || !bytes.Equal(buf, p2) {
		t.Fatalf("post-restart ReadPiece(2): err=%v equal=%v", err, bytes.Equal(buf, p2))
	}

	// Clear must drop it from the bitmap (hash failure path).
	db2.Clear(2)
	if db2.Has(2) {
		t.Fatal("Has(2) should be false after Clear")
	}
}
