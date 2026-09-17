package catalog

import "testing"

// TestPersistRoundTrip verifies the catalog survives "restart": metadata, pin state, download mode, and
// the Source magnet (needed to re-add pinned torrents) all reload from disk.
func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()

	c := NewPersistent(dir)
	c.Upsert(Entry{
		Hash:         "abc123",
		Source:       "magnet:?xt=urn:btih:abc123",
		Title:        "Dune",
		MediaType:    "movie",
		Poster:       "http://img/poster.jpg",
		DownloadMode: ModeDisk,
	})
	c.SetPinned("abc123", true)

	// Reload from the same dir (fresh instance = a restart).
	c2 := NewPersistent(dir)
	e, ok := c2.Get("abc123")
	if !ok {
		t.Fatal("entry missing after reload")
	}
	if !e.Pinned {
		t.Error("Pinned not persisted")
	}
	if e.Source != "magnet:?xt=urn:btih:abc123" {
		t.Errorf("Source not persisted: %q", e.Source)
	}
	if e.DownloadMode != ModeDisk {
		t.Errorf("DownloadMode = %q, want %q", e.DownloadMode, ModeDisk)
	}
	if e.Title != "Dune" || e.Poster == "" {
		t.Errorf("metadata not persisted: %+v", e)
	}

	// Remove must persist too.
	c2.Remove("abc123")
	c3 := NewPersistent(dir)
	if _, ok := c3.Get("abc123"); ok {
		t.Error("entry should be gone after Remove + reload")
	}
}
