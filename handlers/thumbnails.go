package handlers

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"Potok.Backend.TorrentGo/media"
	"Potok.Backend.TorrentGo/storage"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"
)

type thumbCacheEntry struct {
	Data      []byte
	CreatedAt time.Time
	Key       string
}

type ThumbnailCache struct {
	mu      sync.Mutex
	items   map[string]*thumbCacheEntry
	order   []string
	maxSize int
	ttl     time.Duration
}

func NewThumbnailCache(maxSize int, ttl time.Duration) *ThumbnailCache {
	return &ThumbnailCache{
		items:   make(map[string]*thumbCacheEntry),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Stats reports the thumbnail cache's entry count, total resident bytes and entry ceiling (diagnostics).
func (c *ThumbnailCache) Stats() (count int, bytes int64, max int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b int64
	for _, e := range c.items {
		b += int64(len(e.Data))
	}
	return len(c.items), b, c.maxSize
}

func (c *ThumbnailCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.items[key]
	if !ok {
		return nil, false
	}

	if time.Since(entry.CreatedAt) > c.ttl {
		c.remove(key)
		return nil, false
	}

	c.moveToMRU(key)
	return entry.Data, true
}

func (c *ThumbnailCache) Set(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.items[key]; ok {
		entry.Data = data
		entry.CreatedAt = time.Now()
		c.moveToMRU(key)
		return
	}

	if len(c.items) >= c.maxSize && len(c.order) > 0 {
		oldest := c.order[0]
		c.remove(oldest)
	}

	entry := &thumbCacheEntry{
		Data:      data,
		CreatedAt: time.Now(),
		Key:       key,
	}
	c.items[key] = entry
	c.order = append(c.order, key)
}

func (c *ThumbnailCache) remove(key string) {
	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *ThumbnailCache) moveToMRU(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, key)
}

type ThumbnailService struct {
	cache *ThumbnailCache
	sfg   singleflight.Group
}

func NewThumbnailService(maxSize int, ttl time.Duration) *ThumbnailService {
	return &ThumbnailService{
		cache: NewThumbnailCache(maxSize, ttl),
	}
}

func (h *HandlerContext) HandleGetThumbnail(w http.ResponseWriter, r *http.Request) {
	hashHex := chi.URLParam(r, "hash")
	fileIndexStr := chi.URLParam(r, "fileIndex")
	timeStr := r.URL.Query().Get("time")

	var infoHash metainfo.Hash
	hexBytes, err := hex.DecodeString(hashHex)
	if err != nil || len(hexBytes) != 20 {
		http.Error(w, "Invalid torrent hash format", http.StatusBadRequest)
		return
	}
	copy(infoHash[:], hexBytes)

	t, ok := h.Engine.Client.Torrent(infoHash)
	if !ok {
		http.Error(w, "Torrent not active. Please add it first.", http.StatusNotFound)
		return
	}

	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil || fileIndex < 1 {
		http.Error(w, "Invalid file index. Must be 1-based.", http.StatusBadRequest)
		return
	}

	files := t.Files()
	idx := fileIndex - 1
	if idx < 0 || idx >= len(files) {
		http.Error(w, fmt.Sprintf("File index out of bounds. Must be between 1 and %d.", len(files)), http.StatusBadRequest)
		return
	}

	timeVal, err := strconv.ParseFloat(timeStr, 64)
	if err != nil || timeVal < 0 {
		timeVal = 0
	}

	roundedTime := int(math.Round(timeVal/5.0) * 5)
	if roundedTime < 0 {
		roundedTime = 0
	}

	cacheKey := fmt.Sprintf("%s_%d_%d", hashHex, fileIndex, roundedTime)

	// Try reading from LRU cache first
	if data, found := h.ThumbService.cache.Get(cacheKey); found {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
		return
	}

	// Execute through singleflight (coalesce concurrent requests for the same rounded time).
	resultVal, err, _ := h.ThumbService.sfg.Do(cacheKey, func() (interface{}, error) {
		if data, found := h.ThumbService.cache.Get(cacheKey); found {
			return data, nil
		}

		// Admit as a window-class extraction; a scrub abandons most hovered positions, so they cancel here
		// (while QUEUED) instead of piling up in-process decode/encode work.
		release, acqErr := h.extExec.Acquire(r.Context(), extWindow)
		if acqErr != nil {
			return nil, acqErr
		}
		defer release()

		// Detached from the leader's ctx (singleflight shares it) + time-capped so a cold seek can't pin a slot.
		extractCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		rs, _, oerr := h.openTorrentFileReader(extractCtx, hashHex, fileIndexStr, storage.ClassAheadDemux)
		if oerr != nil {
			return nil, oerr
		}
		defer rs.Close()

		data, terr := media.Thumbnail(extractCtx, rs, float64(roundedTime), 160, 90)
		if terr != nil {
			slog.Error("thumbnail extraction failed", "hash", hashHex, "file", fileIndexStr, "error", terr)
			return nil, terr
		}
		h.ThumbService.cache.Set(cacheKey, data)
		return data, nil
	})

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	thumbnailData, ok := resultVal.([]byte)
	if !ok {
		http.Error(w, "invalid thumbnail data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(thumbnailData)
}
