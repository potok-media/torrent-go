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

// ManageTorrent is one row of the management dashboard: a live torrent (or a saved library entry with
// State "Saved") joined with its remembered catalog metadata, speeds, peers and watcher count.
type ManageTorrent struct {
	Hash           string  `json:"hash"`
	Name           string  `json:"name"`
	Poster         string  `json:"poster,omitempty"`
	MediaType      string  `json:"mediaType,omitempty"`
	State          string  `json:"state" enums:"Metadata,Downloading,Seeding,Saved"`
	Progress       float64 `json:"progress"`
	SizeBytes      int64   `json:"sizeBytes"`
	CompletedBytes int64   `json:"completedBytes"`
	DownloadSpeed  int64   `json:"downloadSpeed"`
	UploadSpeed    int64   `json:"uploadSpeed"`
	ActivePeers    int     `json:"activePeers"`
	Seeders        int     `json:"seeders"`
	Watchers       int     `json:"watchers"`
	Pinned         bool    `json:"pinned"`
	DownloadMode   string  `json:"downloadMode" enums:"stream,disk"`
	CurrentFile    string  `json:"currentFile,omitempty"`
}

// PinResponse acknowledges a pin/unpin toggle.
type PinResponse struct {
	Hash   string `json:"hash"`
	Pinned bool   `json:"pinned"`
}

// ManageTorrentFilesResponse is the torrent detail view: flat file list plus catalog metadata for the
// page header.
type ManageTorrentFilesResponse struct {
	Hash           string              `json:"hash"`
	Name           string              `json:"name"`
	Ready          bool                `json:"ready"`
	Poster         string              `json:"poster,omitempty"`
	MediaType      string              `json:"mediaType,omitempty"`
	Pinned         bool                `json:"pinned,omitempty"`
	DownloadMode   string              `json:"downloadMode,omitempty"`
	Files          []ManageTorrentFile `json:"files"`
}

// HandleListTorrents godoc
//	@ID			listTorrents
//
//	@Summary		List all torrents
//	@Description	Every live torrent joined with its remembered metadata, speeds, peers and watcher count, plus saved library entries (State "Saved") — the dashboard's main feed.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Success		200	{array}	handlers.ManageTorrent
//	@x-scalar-ignore	true
//	@Router			/api/manage/torrents [get]
func (h *HandlerContext) HandleListTorrents(w http.ResponseWriter, r *http.Request) {
	watchers, files := h.watchersByHash()

	torrents := h.Engine.Client.Torrents()
	live := make(map[string]bool, len(torrents))
	out := make([]ManageTorrent, 0, len(torrents))
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

		item := ManageTorrent{
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
			out = append(out, ManageTorrent{
				Hash: e.Hash, Name: e.Title, Poster: e.Poster, MediaType: e.MediaType,
				State: "Saved", DownloadMode: e.DownloadMode, Pinned: e.Pinned,
			})
		}
	}

	writeJSON(w, http.StatusOK, out)
}

type ManageStats struct {
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

// HandleManageStats godoc
//	@ID			getManageStats
//
//	@Summary		Aggregate engine KPIs
//	@Description	Summed speeds, torrent/session counts, the GLOBAL piece-cache fill, Go heap/sys memory, and the concurrent-stream cap.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Success		200	{object}	handlers.ManageStats
//	@x-scalar-ignore	true
//	@Router			/api/manage/stats [get]
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

	writeJSON(w, http.StatusOK, ManageStats{
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

// HandlePinTorrent godoc
//	@ID			pinTorrent
//
//	@Summary		Pin a torrent
//	@Description	Marks the torrent as pinned: never reaped, survives restart. Creates a bare catalog entry if the hash had no metadata, so a UI-added magnet can still be pinned.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			hash	path		string	true	"Infohash (40-char hex)"
//	@Success		200		{object}	handlers.PinResponse
//	@Failure		400		{string}	string	"invalid torrent hash format"
//	@Failure		500		{string}	string	"catalog unavailable"
//	@x-scalar-ignore	true
//	@Router			/api/manage/torrents/{hash}/pin [post]
func (h *HandlerContext) HandlePinTorrent(w http.ResponseWriter, r *http.Request) {
	h.setPinned(w, r, true)
}

// HandleUnpinTorrent godoc
//	@ID			unpinTorrent
//
//	@Summary		Unpin a torrent
//	@Description	Clears the pinned state; the torrent becomes reapable by the idle sweeper again.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			hash	path		string	true	"Infohash (40-char hex)"
//	@Success		200		{object}	handlers.PinResponse
//	@Failure		400		{string}	string	"invalid torrent hash format"
//	@Failure		500		{string}	string	"catalog unavailable"
//	@x-scalar-ignore	true
//	@Router			/api/manage/torrents/{hash}/pin [delete]
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
	writeJSON(w, http.StatusOK, PinResponse{Hash: hashHex, Pinned: e.Pinned})
}

type ManageTorrentFile struct {
	Path           string `json:"path"`
	SizeBytes      int64  `json:"sizeBytes"`
	CompletedBytes int64  `json:"completedBytes"`
}

// HandleTorrentFiles godoc
//	@ID			getManageTorrentFiles
//
//	@Summary		Get a torrent's file list
//	@Description	Flat file list of a live torrent (path/size/completed), which the UI's torrent detail page folds into a folder tree. Metadata (name/poster/mediaType) is joined from the catalog so the page has a proper header even for a bare magnet.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			hash	path		string	true	"Infohash (40-char hex)"
//	@Success		200		{object}	handlers.ManageTorrentFilesResponse
//	@Failure		400		{string}	string	"invalid torrent hash format"
//	@Failure		404		{string}	string	"torrent not found"
//	@x-scalar-ignore	true
//	@Router			/api/manage/torrents/{hash}/files [get]
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

	resp := ManageTorrentFilesResponse{Hash: hashHex, Name: t.Name(), Ready: t.Info() != nil}
	if h.Catalog != nil {
		if e, ok := h.Catalog.Get(hashHex); ok {
			if e.Title != "" {
				resp.Name = e.Title
			}
			resp.Poster = e.Poster
			resp.MediaType = e.MediaType
			resp.Pinned = e.Pinned
			resp.DownloadMode = e.DownloadMode
		}
	}

	resp.Files = []ManageTorrentFile{}
	if t.Info() != nil {
		for _, f := range t.Files() {
			resp.Files = append(resp.Files, ManageTorrentFile{
				Path:           f.Path(),
				SizeBytes:      f.Length(),
				CompletedBytes: f.BytesCompleted(),
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
