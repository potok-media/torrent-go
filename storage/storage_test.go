package storage

import (
	"testing"
	"time"

	"Potok.Backend.TorrentGo/config"
	"github.com/anacrolix/torrent/metainfo"
)

func TestMemPieceReadWriteComplete(t *testing.T) {
	mp := NewMemPiece(10)

	// Test basic write
	data := []byte("hello")
	n, err := mp.WriteAt(data, 0)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Expected to write 5 bytes, wrote %d", n)
	}

	// Test basic read
	buf := make([]byte, 5)
	n, err = mp.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("Expected 'hello', got %s", string(buf))
	}

	// Test accessed time updating
	initialAccess := mp.Accessed()
	time.Sleep(10 * time.Millisecond)

	_, _ = mp.ReadAt(buf, 0)
	if !mp.Accessed().After(initialAccess) {
		t.Errorf("Accessed time did not update after read")
	}

	// Test completion channel
	select {
	case <-mp.Done():
		t.Errorf("Done channel closed before completion")
	default:
	}

	mp.MarkComplete()
	if !mp.IsComplete() {
		t.Errorf("Expected complete to be true")
	}

	select {
	case <-mp.Done():
		// Success
	default:
		t.Errorf("Done channel not closed after completion")
	}

	// Test release
	mp.Release()
	if mp.IsComplete() {
		t.Errorf("Expected complete to be false after release")
	}
}

func TestCacheEviction(t *testing.T) {
	cfg := &config.Config{
		CacheSizeBytes: 30, // Max 30 bytes (3 pieces of size 10)
	}

	hash := metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
	cache := NewCache(hash, cfg.CacheSizeBytes, 10, 5)

	mp0 := cache.GetOrCreateMemPiece(0, 10)
	mp1 := cache.GetOrCreateMemPiece(1, 10)
	mp2 := cache.GetOrCreateMemPiece(2, 10)
	mp3 := cache.GetOrCreateMemPiece(3, 10)

	// Write to them and update filled
	_, _ = mp0.WriteAt([]byte("0123456789"), 0)
	mp0.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	time.Sleep(10 * time.Millisecond)
	_, _ = mp1.WriteAt([]byte("0123456789"), 0)
	mp1.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	time.Sleep(10 * time.Millisecond)
	_, _ = mp2.WriteAt([]byte("0123456789"), 0)
	mp2.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	cache.mu.RLock()
	filled := cache.filled
	cache.mu.RUnlock()
	if filled != 30 {
		t.Errorf("Expected filled size 30, got %d", filled)
	}

	// Exceed capacity
	time.Sleep(10 * time.Millisecond)
	_, _ = mp3.WriteAt([]byte("0123456789"), 0)
	mp3.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	// Eviction should have run during UpdateFilled.
	// Since 0 was written first (oldest access), it should have been evicted.
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	if _, ok := cache.pieces[0]; ok {
		t.Errorf("Expected piece 0 to be evicted")
	}
	if _, ok := cache.pieces[1]; !ok {
		t.Errorf("Expected piece 1 to be retained")
	}
	if _, ok := cache.pieces[2]; !ok {
		t.Errorf("Expected piece 2 to be retained")
	}
	if _, ok := cache.pieces[3]; !ok {
		t.Errorf("Expected piece 3 to be retained")
	}
	if cache.filled != 30 {
		t.Errorf("Expected cache filled size to be reset to 30, got %d", cache.filled)
	}
}

func TestCacheEvictionWithReaderProtection(t *testing.T) {
	cfg := &config.Config{
		CacheSizeBytes: 20, // Capacity 20 bytes (2 pieces)
	}

	hash := metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
	// pieceLen must be realistic: the playback read-ahead window is byte-bounded (~64MB), so a 2MB piece
	// gives a ~32-piece protected window (0..32) — piece 40 is outside it and stays evictable. (The old
	// 10-byte pieceLen made 64MB/10 protect the whole cache.) Byte accounting below still uses 10-byte
	// writes; only the reader's window math reads pieceLen.
	cache := NewCache(hash, cfg.CacheSizeBytes, 2<<20, 50)

	mp0 := cache.GetOrCreateMemPiece(0, 10)
	mp40 := cache.GetOrCreateMemPiece(40, 10)
	mp41 := cache.GetOrCreateMemPiece(41, 10)

	_, _ = mp0.WriteAt([]byte("0123456789"), 0)
	mp0.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	time.Sleep(5 * time.Millisecond)

	_, _ = mp40.WriteAt([]byte("0123456789"), 0)
	mp40.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	// Register reader to protect piece 0
	r := &Reader{
		cache:      cache,
		fileOffset: 0,
		fileSize:   500,
		pos:        0, // will protect piece 0 (byte-bounded window 0 to 32 at 2MB pieces)
	}
	cache.RegisterReader(r)

	// Write to piece 41, exceeding capacity of 20
	time.Sleep(5 * time.Millisecond)
	_, _ = mp41.WriteAt([]byte("0123456789"), 0)
	mp41.MarkComplete()
	cache.UpdateFilled(10)
	cache.EvictIfNeeded()

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	if _, ok := cache.pieces[0]; !ok {
		t.Errorf("Expected piece 0 to be protected by reader and NOT evicted")
	}
	if _, ok := cache.pieces[40]; ok {
		t.Errorf("Expected piece 40 to be evicted because it was not protected")
	}
	if _, ok := cache.pieces[41]; !ok {
		t.Errorf("Expected piece 41 to be retained")
	}
}

func TestUpdatePriorities(t *testing.T) {
	hash := metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
	cache := NewCache(hash, 100, 10, 50)

	r := &Reader{
		cache:      cache,
		fileOffset: 0,
		fileSize:   500,
		pos:        20, // currPiece is 2
	}
	cache.RegisterReader(r)

	// UpdatePriorities with c.torrent == nil should return immediately without panicking
	cache.UpdatePriorities(0, 49)
}
