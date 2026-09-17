package handlers

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"runtime"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
)

// Management API for the standalone TorrentGo web UI. Everything here is READ/CONTROL over the live
// in-memory torrent set — the pieces the per-hash plugin API never exposed: a list of ALL torrents,
// aggregate stats, and pin control. Mounted under /api/manage/* behind BasicAuth (main.go); the plugin's
// own endpoints stay open. Named /api/manage/* so "stats"/"torrents" can't be parsed as a {hash}.

type manageTorrent struct {
	Hash           string  `json:"hash"`
	Name           string  `json:"name"`
	Poster         string  `json:"poster,omitempty"`
	MediaType      string  `json:"mediaType,omitempty"`
	State          string  `json:"state"`
	Progress       float64 `json:"progress"`
	SizeBytes      int64   `json:"sizeBytes"`
	CompletedBytes int64   `json:"completedBytes"`
	DownloadSpeed  int64   `json:"downloadSpeed"`
	UploadSpeed    int64   `json:"uploadSpeed"`
	ActivePeers    int     `json:"activePeers"`
	Seeders        int     `json:"seeders"`
	Watchers       int     `json:"watchers"`
	Pinned         bool    `json:"pinned"`
	DownloadMode   string  `json:"downloadMode"`
	CurrentFile    string  `json:"currentFile,omitempty"`
}

// HandleListTorrents returns every live torrent joined with its remembered metadata, speeds, peers and
// watcher count — the dashboard's main feed.
func (h *HandlerContext) HandleListTorrents(w http.ResponseWriter, r *http.Request) {
	watchers, files := h.watchersByHash()

	torrents := h.Engine.Client.Torrents()
	live := make(map[string]bool, len(torrents))
	out := make([]manageTorrent, 0, len(torrents))
	for _, t := range torrents {
		hash := t.InfoHash().HexString()
		live[hash] = true
		stats := t.Stats()
		sp := h.SpeedMonitor.GetSpeed(hash)

		length := t.Length()
		completed := t.BytesCompleted()
		progress := 0.0
		if length > 0 {
			progress = float64(completed) / float64(length)
		}
		state := "Downloading"
		if t.Info() == nil {
			state = "Metadata"
		} else if completed == length {
			state = "Seeding"
		}

		item := manageTorrent{
			Hash:           hash,
			Name:           t.Name(),
			State:          state,
			Progress:       progress,
			SizeBytes:      length,
			CompletedBytes: completed,
			DownloadSpeed:  sp.DownloadSpeed,
			UploadSpeed:    sp.UploadSpeed,
			ActivePeers:    stats.ActivePeers,
			Seeders:        stats.ConnectedSeeders,
			Watchers:       watchers[hash],
			DownloadMode:   "stream",
			CurrentFile:    files[hash],
		}
		if h.Catalog != nil {
			if e, ok := h.Catalog.Get(hash); ok {
				if e.Title != "" {
					item.Name = e.Title
				}
				item.Poster = e.Poster
				item.MediaType = e.MediaType
				item.Pinned = e.Pinned
				if e.DownloadMode != "" {
					item.DownloadMode = e.DownloadMode
				}
			}
		}
		out = append(out, item)
	}

	// Saved library entries (metadata only — not yet engaged): shown so the user can browse and later
	// download/play them. State "Saved", no live stats.
	if h.Catalog != nil {
		for _, e := range h.Catalog.All() {
			if live[e.Hash] {
				continue
			}
			out = append(out, manageTorrent{
				Hash: e.Hash, Name: e.Title, Poster: e.Poster, MediaType: e.MediaType,
				State: "Saved", DownloadMode: e.DownloadMode, Pinned: e.Pinned,
			})
		}
	}

	writeJSON(w, http.StatusOK, out)
}

type manageStats struct {
	TotalDownload int64 `json:"totalDownload"`
	TotalUpload   int64 `json:"totalUpload"`
	Active        int   `json:"active"`
	Streaming     int   `json:"streaming"`
	Sessions      int   `json:"sessions"`
	TotalPeers    int   `json:"totalPeers"`
	CacheFilled   int64 `json:"cacheFilled"`
	CacheCapacity int64 `json:"cacheCapacity"`
	HeapBytes     int64 `json:"heapBytes"`
	SysBytes      int64 `json:"sysBytes"`
	ActiveStreams int   `json:"activeStreams"`
	MaxStreams    int   `json:"maxStreams"`
}

// HandleManageStats returns the aggregate KPIs: summed speeds, torrent/session counts, the GLOBAL cache
// fill (from the Phase-0 accountant), Go heap/sys memory, and the concurrent-stream cap.
func (h *HandlerContext) HandleManageStats(w http.ResponseWriter, r *http.Request) {
	torrents := h.Engine.Client.Torrents()
	totalPeers := 0
	for _, t := range torrents {
		totalPeers += t.Stats().ActivePeers
	}

	sessions, streaming := h.sessionStats()
	total := h.SpeedMonitor.Total()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	writeJSON(w, http.StatusOK, manageStats{
		TotalDownload: total.DownloadSpeed,
		TotalUpload:   total.UploadSpeed,
		Active:        len(torrents),
		Streaming:     streaming,
		Sessions:      sessions,
		TotalPeers:    totalPeers,
		CacheFilled:   h.Engine.Storage.GlobalFilled(),
		CacheCapacity: h.Engine.Storage.GlobalCapacity(),
		HeapBytes:     int64(ms.HeapAlloc),
		SysBytes:      int64(ms.Sys),
		ActiveStreams: sessions,
		MaxStreams:    h.maxStreams(),
	})
}

// HandlePinTorrent / HandleUnpinTorrent toggle a torrent's pin (never-reaped, persistent) state. Pinning
// creates a bare catalog entry if the hash had no metadata, so a UI-added magnet can still be pinned.
func (h *HandlerContext) HandlePinTorrent(w http.ResponseWriter, r *http.Request) {
	h.setPinned(w, r, true)
}
func (h *HandlerContext) HandleUnpinTorrent(w http.ResponseWriter, r *http.Request) {
	h.setPinned(w, r, false)
}

func (h *HandlerContext) setPinned(w http.ResponseWriter, r *http.Request, pinned bool) {
	hashHex := chi.URLParam(r, "hash")
	if b, err := hex.DecodeString(hashHex); err != nil || len(b) != 20 {
		http.Error(w, "Invalid torrent hash format", http.StatusBadRequest)
		return
	}

	if h.Catalog == nil {
		http.Error(w, "catalog unavailable", http.StatusInternalServerError)
		return
	}
	e := h.Catalog.SetPinned(hashHex, pinned)
	writeJSON(w, http.StatusOK, map[string]any{"hash": hashHex, "pinned": e.Pinned})
}

type torrentFileNode struct {
	Path           string `json:"path"`
	SizeBytes      int64  `json:"sizeBytes"`
	CompletedBytes int64  `json:"completedBytes"`
}

// HandleTorrentFiles returns the flat file list of a live torrent (path/size/completed), which the UI's
// torrent detail page folds into a folder tree. Metadata (name/poster/mediaType) is joined from the
// catalog so the page has a proper header even for a bare magnet.
func (h *HandlerContext) HandleTorrentFiles(w http.ResponseWriter, r *http.Request) {
	hashHex := chi.URLParam(r, "hash")
	var ih metainfo.Hash
	b, err := hex.DecodeString(hashHex)
	if err != nil || len(b) != 20 {
		http.Error(w, "Invalid torrent hash format", http.StatusBadRequest)
		return
	}
	copy(ih[:], b)

	t, ok := h.Engine.Client.Torrent(ih)
	if !ok {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}

	resp := map[string]any{"hash": hashHex, "name": t.Name(), "ready": t.Info() != nil}
	if h.Catalog != nil {
		if e, ok := h.Catalog.Get(hashHex); ok {
			if e.Title != "" {
				resp["name"] = e.Title
			}
			resp["poster"] = e.Poster
			resp["mediaType"] = e.MediaType
			resp["pinned"] = e.Pinned
			resp["downloadMode"] = e.DownloadMode
		}
	}

	files := []torrentFileNode{}
	if t.Info() != nil {
		for _, f := range t.Files() {
			files = append(files, torrentFileNode{
				Path:           f.Path(),
				SizeBytes:      f.Length(),
				CompletedBytes: f.BytesCompleted(),
			})
		}
	}
	resp["files"] = files
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
