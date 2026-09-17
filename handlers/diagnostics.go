package handlers

import (
	"net/http"
	"runtime"
	"syscall"
	"time"

	"Potok.Backend.TorrentGo/storage"
)

// HandleDiagnostics answers the diagnostics page: a full accounting of where RAM (and disk) goes — the Go
// runtime heap, the global piece cache (with per-torrent breakdown), the HLS segment cache, the thumbnail
// cache, live AAC transcoders, playback sessions, and disk usage. Everything the operator needs to see
// "what is eating memory" in one place.
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
	list := make([]map[string]any, 0, len(torrents))
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
		list = append(list, map[string]any{
			"hash": hash, "name": name,
			"cacheBytes": ci.CacheBytes, "diskBytes": ci.DiskBytes,
			"activePeers": stats.ActivePeers, "piecesComplete": stats.PiecesComplete, "numPieces": numPieces,
			"watchers": watchers[hash], "downloadMode": mode, "pinned": pinned,
		})
	}

	sessions, streaming := h.sessionStats()
	memLimit := int64(0)
	if h.Config != nil {
		memLimit = h.Config.MemLimitBytes
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"runtime": map[string]any{
			"heapAlloc": ms.HeapAlloc, "heapSys": ms.HeapSys, "stackSys": ms.StackSys, "sys": ms.Sys,
			"numGoroutine": runtime.NumGoroutine(), "numGC": ms.NumGC, "gcCPUFraction": ms.GCCPUFraction,
			"memLimitBytes": memLimit, "uptimeSec": int64(time.Since(h.StartedAt).Seconds()),
		},
		"pieceCache":  map[string]any{"filled": st.GlobalFilled(), "capacity": st.GlobalCapacity()},
		"hlsCache":    map[string]any{"bytes": segBytes, "count": segCount, "max": segMax},
		"thumbCache":  map[string]any{"count": thCount, "bytes": thBytes, "max": thMax},
		"transcoders": map[string]any{"count": transcoders, "estBytes": int64(transcoders) * (100 << 20), "max": st.DerivedMaxAudioTranscoders()},
		"sessions":    map[string]any{"active": sessions, "streaming": streaming, "maxStreams": st.DerivedMaxStreams(), "perStreamBytes": storage.PerStreamWindowBytes()},
		"disk":        map[string]any{"dir": st.DownloadDir(), "used": diskUsed, "free": diskFree(st.DownloadDir())},
		"torrents":    list,
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
