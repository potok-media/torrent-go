// Package catalog remembers the media metadata a torrent was added with (poster, title, TMDB id, …) plus
// its management state (pinned, download mode), keyed by infohash. TorrentGo's engine is otherwise
// metadata-free — it only knows infohashes and files — so the management UI's poster cards and the
// pin/persistence lifecycle both read from here.
//
// Phase 1 keeps this fully in-memory (lost on restart, like the engine itself). Phase 2 adds disk
// persistence behind the same API so pinned torrents can be re-added on startup.
package catalog

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DownloadMode selects how a torrent's data is stored. "stream" is the classic RAM-cache-only path;
// "disk" (Phase 2) persists pieces so playback needs no re-buffering and survives restart.
const (
	ModeStream = "stream"
	ModeDisk   = "disk"
)

// Entry is the remembered metadata + management state for one torrent.
type Entry struct {
	Hash            string    `json:"hash"`
	Source          string    `json:"source,omitempty"` // magnet/link for restart re-add; persisted to disk, not sent to clients (the API builds its own DTO)
	Title           string    `json:"title,omitempty"`
	OriginalTitle   string    `json:"originalTitle,omitempty"`
	MediaType       string    `json:"mediaType,omitempty"` // "movie" | "tv"
	NumberOfSeasons int       `json:"numberOfSeasons,omitempty"`
	TmdbID          int64     `json:"tmdbId,omitempty"`
	Poster          string    `json:"poster,omitempty"`
	Pinned          bool      `json:"pinned"`
	DownloadMode    string    `json:"downloadMode"`
	AddedAt         time.Time `json:"addedAt"`
}

// Catalog is a thread-safe store of entries by infohash. If path is non-empty it is persisted to disk on
// every mutation so metadata + pin/mode survive restart.
type Catalog struct {
	mu      sync.RWMutex
	entries map[string]Entry
	path    string // catalog.json path; empty = in-memory only
}

func New() *Catalog {
	return &Catalog{entries: make(map[string]Entry)}
}

// NewPersistent loads any existing catalog from dir/catalog.json and persists future mutations there.
// A nil/empty dir behaves like New() (in-memory only).
func NewPersistent(dir string) *Catalog {
	c := &Catalog{entries: make(map[string]Entry)}
	if dir == "" {
		return c
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("catalog: cannot create data dir; running in-memory", "dir", dir, "error", err)
		return c
	}
	c.path = filepath.Join(dir, "catalog.json")
	if b, err := os.ReadFile(c.path); err == nil {
		var list []Entry
		if json.Unmarshal(b, &list) == nil {
			for _, e := range list {
				if e.Hash != "" {
					c.entries[e.Hash] = e
				}
			}
			slog.Info("catalog loaded", "entries", len(c.entries), "path", c.path)
		}
	}
	return c
}

// persist snapshots the entries and writes them atomically. Called after a mutation, with no lock held.
func (c *Catalog) persist() {
	if c.path == "" {
		return
	}
	c.mu.RLock()
	list := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		list = append(list, e)
	}
	c.mu.RUnlock()

	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		slog.Warn("catalog: persist failed", "error", err)
		return
	}
	_ = os.Rename(tmp, c.path)
}

// Upsert merges incoming metadata for a hash. Non-empty incoming fields overwrite; management state
// (Pinned, DownloadMode) and AddedAt are preserved from any existing entry so re-adding a torrent (e.g.
// re-opening its detail page) never silently unpins it or resets its mode.
func (c *Catalog) Upsert(in Entry) Entry {
	c.mu.Lock()

	e, ok := c.entries[in.Hash]
	if !ok {
		// Empty DownloadMode = a "saved" library entry (metadata only, not yet downloaded/streamed).
		// It becomes "stream" when played or "disk" when downloaded.
		e = Entry{Hash: in.Hash, AddedAt: time.Now()}
	}
	if in.Source != "" {
		e.Source = in.Source
	}
	if in.Title != "" {
		e.Title = in.Title
	}
	if in.OriginalTitle != "" {
		e.OriginalTitle = in.OriginalTitle
	}
	if in.MediaType != "" {
		e.MediaType = in.MediaType
	}
	if in.NumberOfSeasons != 0 {
		e.NumberOfSeasons = in.NumberOfSeasons
	}
	if in.TmdbID != 0 {
		e.TmdbID = in.TmdbID
	}
	if in.Poster != "" {
		e.Poster = in.Poster
	}
	if in.DownloadMode != "" {
		e.DownloadMode = in.DownloadMode
	}
	c.entries[in.Hash] = e
	c.mu.Unlock()
	c.persist()
	return e
}

func (c *Catalog) Get(hash string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[hash]
	return e, ok
}

func (c *Catalog) All() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	return out
}

// SetPinned toggles pin state, creating a bare entry if the hash was never seen with metadata (so a
// torrent added directly from the UI can still be pinned). Returns the updated entry.
func (c *Catalog) SetPinned(hash string, pinned bool) Entry {
	c.mu.Lock()
	e, ok := c.entries[hash]
	if !ok {
		e = Entry{Hash: hash, AddedAt: time.Now()}
	}
	e.Pinned = pinned
	c.entries[hash] = e
	c.mu.Unlock()
	c.persist()
	return e
}

func (c *Catalog) IsPinned(hash string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[hash].Pinned
}

func (c *Catalog) Remove(hash string) {
	c.mu.Lock()
	delete(c.entries, hash)
	c.mu.Unlock()
	c.persist()
}
