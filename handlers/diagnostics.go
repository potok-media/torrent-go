package handlers

import (
	"net/http"
	"runtime"
	"syscall"
	"time"

	"github.com/potok-media/potok-torrentgo/storage"
)

// RuntimeDiagnostics reports the Go runtime memory/goroutine state and process uptime.
type RuntimeDiagnostics struct {
	HeapAlloc     uint64  `json:"heapAlloc"`
	HeapSys       uint64  `json:"heapSys"`
	StackSys      uint64  `json:"stackSys"`
	Sys           uint64  `json:"sys"`
	NumGoroutine  int     `json:"numGoroutine"`
	NumGC         uint32  `json:"numGC"`
	GCCPUFraction float64 `json:"gcCPUFraction"`
	MemLimitBytes int64   `json:"memLimitBytes"`
	UptimeSec     int64   `json:"uptimeSec"`
}

// PieceCacheDiagnostics reports the global torrent piece cache (RAM) fill vs capacity.
type PieceCacheDiagnostics struct {
	Filled   int64 `json:"filled"`
	Capacity int64 `json:"capacity"`
}

// HlsCacheDiagnostics reports the HLS segment LRU: resident bytes, entry count, ceiling.
type HlsCacheDiagnostics struct {
	Bytes int64 `json:"bytes"`
	Count int   `json:"count"`
	Max   int64 `json:"max"`
}

// ThumbCacheDiagnostics reports the thumbnail LRU: entry count, resident bytes, entry ceiling.
type ThumbCacheDiagnostics struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
	Max   int   `json:"max"`
}

// TranscoderDiagnostics reports the live continuous-AAC transcoders and their estimated RAM cost.
type TranscoderDiagnostics struct {
	Count    int   `json:"count"`
	EstBytes int64 `json:"estBytes"`
	Max      int   `json:"max"`
}

// SessionDiagnostics reports playback-session counts and the derived concurrent-stream cap.
type SessionDiagnostics struct {
	Active         int   `json:"active"`
	Streaming      int   `json:"streaming"`
	MaxStreams     int   `json:"maxStreams"`
	PerStreamBytes int64 `json:"perStreamBytes"`
}

// DiskDiagnostics reports the disk-mode download directory usage.
type DiskDiagnostics struct {
	Dir  string `json:"dir"`
	Used int64  `json:"used"`
	Free int64  `json:"free"`
}

// DiagnosticsTorrentEntry is one torrent's footprint on the diagnostics page.
type DiagnosticsTorrentEntry struct {
	Hash           string `json:"hash"`
	Name           string `json:"name"`
	CacheBytes     int64  `json:"cacheBytes"`
	DiskBytes      int64  `json:"diskBytes"`
	ActivePeers    int    `json:"activePeers"`
	PiecesComplete int    `json:"piecesComplete"`
	NumPieces      int    `json:"numPieces"`
	Watchers       int    `json:"watchers"`
	DownloadMode   string `json:"downloadMode"`
	Pinned         bool   `json:"pinned"`
}

// DiagnosticsResponse is a full accounting of where RAM (and disk) goes: the Go runtime heap, the global
// piece cache (with per-torrent breakdown), the HLS segment cache, the thumbnail cache, live AAC
// transcoders, playback sessions, and disk usage.
type DiagnosticsResponse struct {
	Runtime     RuntimeDiagnostics        `json:"runtime"`
	PieceCache  PieceCacheDiagnostics     `json:"pieceCache"`
	HlsCache    HlsCacheDiagnostics       `json:"hlsCache"`
	ThumbCache  ThumbCacheDiagnostics     `json:"thumbCache"`
	Transcoders TranscoderDiagnostics     `json:"transcoders"`
	Sessions    SessionDiagnostics        `json:"sessions"`
	Disk        DiskDiagnostics           `json:"disk"`
	Torrents    []DiagnosticsTorrentEntry `json:"torrents"`
}

// HandleDiagnostics godoc
//	@ID			getDiagnostics
//
//	@Summary		Full memory/disk diagnostics
//	@Description	A full accounting of where RAM (and disk) goes — the Go runtime heap, the global piece cache (with per-torrent breakdown), the HLS segment cache, the thumbnail cache, live AAC transcoders, playback sessions, and disk usage. Everything the operator needs to see "what is eating memory" in one place.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Success		200	{object}	handlers.DiagnosticsResponse
//	@x-scalar-ignore	true
//	@Router			/api/manage/diagnostics [get]
func (h *HandlerContext) HandleDiagnostics(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := h.Engine.Storage

	segBytes, segCount, segMax := h.hlsSegCache.stats()
	thCount, thBytes, thMax := h.ThumbService.cache.Stats()
	transcoders := int(h.audioContCount.Load())

	watchers, _ := h.watchersByHash()
	byHash := make(map[string]storage.CacheInfo)
	for _, ci := range st.CacheInfos() {
		byHash[ci.Hash] = ci
	}

	var diskUsed int64
	torrents := h.Engine.Client.Torrents()
	list := make([]DiagnosticsTorrentEntry, 0, len(torrents))
	for _, t := range torrents {
		hash := t.InfoHash().HexString()
		ci := byHash[hash]
		diskUsed += ci.DiskBytes
		stats := t.Stats()
		numPieces := 0
		if t.Info() != nil { // NumPieces derefs Info — panics before metadata resolves
			numPieces = t.NumPieces()
		}
		name := t.Name()
		mode, pinned := "stream", false
		if h.Catalog != nil {
			if e, ok := h.Catalog.Get(hash); ok {
				if e.Title != "" {
					name = e.Title
				}
				if e.DownloadMode != "" {
					mode = e.DownloadMode
				}
				pinned = e.Pinned
			}
		}
		list = append(list, DiagnosticsTorrentEntry{
			Hash: hash, Name: name,
			CacheBytes: ci.CacheBytes, DiskBytes: ci.DiskBytes,
			ActivePeers: stats.ActivePeers, PiecesComplete: stats.PiecesComplete, NumPieces: numPieces,
			Watchers: watchers[hash], DownloadMode: mode, Pinned: pinned,
		})
	}

	sessions, streaming := h.sessionStats()
	memLimit := int64(0)
	if h.Config != nil {
		memLimit = h.Config.MemLimitBytes
	}

	writeJSON(w, http.StatusOK, DiagnosticsResponse{
		Runtime: RuntimeDiagnostics{
			HeapAlloc: ms.HeapAlloc, HeapSys: ms.HeapSys, StackSys: ms.StackSys, Sys: ms.Sys,
			NumGoroutine: runtime.NumGoroutine(), NumGC: ms.NumGC, GCCPUFraction: ms.GCCPUFraction,
			MemLimitBytes: memLimit, UptimeSec: int64(time.Since(h.StartedAt).Seconds()),
		},
		PieceCache:  PieceCacheDiagnostics{Filled: st.GlobalFilled(), Capacity: st.GlobalCapacity()},
		HlsCache:    HlsCacheDiagnostics{Bytes: segBytes, Count: segCount, Max: segMax},
		ThumbCache:  ThumbCacheDiagnostics{Count: thCount, Bytes: thBytes, Max: thMax},
		Transcoders: TranscoderDiagnostics{Count: transcoders, EstBytes: int64(transcoders) * (100 << 20), Max: st.DerivedMaxAudioTranscoders()},
		Sessions:    SessionDiagnostics{Active: sessions, Streaming: streaming, MaxStreams: st.DerivedMaxStreams(), PerStreamBytes: storage.PerStreamWindowBytes()},
		Disk:        DiskDiagnostics{Dir: st.DownloadDir(), Used: diskUsed, Free: diskFree(st.DownloadDir())},
		Torrents:    list,
	})
}

// diskFree returns the free bytes on the filesystem backing dir (0 if unavailable).
func diskFree(dir string) int64 {
	if dir == "" {
		return 0
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0
	}
	return int64(s.Bavail) * int64(s.Bsize)
}
