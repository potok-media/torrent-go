package handlers

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"Potok.Backend.TorrentGo/storage"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-chi/chi/v5"
)

// readClassFromRequest derives the storage read intent (priority + wait policy) and optional caller
// deadline from the loopback URL: &class=playback|ahead|cold|patient (legacy &bg=1 → ahead; missing →
// playback), and &deadline=<unixMillis> (Phase 2) which caps the reader's wait to the caller's ffmpeg
// budget so an inner block-wait can never outlive the outer op cap.
func readClassFromRequest(r *http.Request) (storage.ReadClass, time.Time) {
	q := r.URL.Query()
	class := storage.ParseReadClass(q.Get("class"), q.Get("bg") == "1")
	var deadline time.Time
	if ms := q.Get("deadline"); ms != "" {
		if n, err := strconv.ParseInt(ms, 10, 64); err == nil && n > 0 {
			deadline = time.UnixMilli(n)
		}
	}
	return class, deadline
}

// fileByteInfo resolves the in-RAM cache + a file's byte offset/length within the torrent, for the
// residency gate (does the demux window already live in cache?). ok=false when the torrent/file/cache
// isn't resolvable yet.
func (h *HandlerContext) fileByteInfo(hashHex, fileIndexStr string) (cache *storage.Cache, offset, length int64, ok bool) {
	var infoHash metainfo.Hash
	hb, err := hex.DecodeString(hashHex)
	if err != nil || len(hb) != 20 {
		return nil, 0, 0, false
	}
	copy(infoHash[:], hb)
	t, tok := h.Engine.Client.Torrent(infoHash)
	if !tok || t.Info() == nil {
		return nil, 0, 0, false
	}
	idx, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		return nil, 0, 0, false
	}
	files := t.Files()
	if idx < 1 || idx > len(files) {
		return nil, 0, 0, false
	}
	c, cok := h.Engine.Storage.GetCache(infoHash)
	if !cok {
		return nil, 0, 0, false
	}
	f := files[idx-1]
	return c, f.Offset(), f.Length(), true
}

func (h *HandlerContext) HandleStream(w http.ResponseWriter, r *http.Request) {
	hashHex := chi.URLParam(r, "hash")
	fileIndexStr := chi.URLParam(r, "fileIndex")

	slog.Debug("HandleStream request received", "hash", hashHex, "fileIndex", fileIndexStr, "raw", r.URL.Query().Get("raw"), "userAgent", r.Header.Get("User-Agent"))

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

	if t.Info() == nil {
		slog.Info("Stream waiting for torrent info...", "hash", hashHex)
		select {
		case <-t.GotInfo():
			slog.Info("Stream: Torrent info resolved", "hash", hashHex)
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			http.Error(w, "Timeout waiting for torrent info", http.StatusGatewayTimeout)
			return
		}
	}

	files := t.Files()
	idx := fileIndex - 1
	if idx < 0 || idx >= len(files) {
		http.Error(w, fmt.Sprintf("File index out of bounds. Must be between 1 and %d.", len(files)), http.StatusBadRequest)
		return
	}

	file := files[idx]
	isRaw := r.URL.Query().Get("raw") == "true"
	// Read intent flows in on the URL: &class=playback|ahead|cold|patient (and, from Phase 2,
	// &deadline=<unixMillis>). Legacy &bg=1 → ahead-demux for one release; missing/unknown → playback.
	readClass, readDeadline := readClassFromRequest(r)

	cache, ok := h.Engine.Storage.GetCache(infoHash)
	if !ok {
		http.Error(w, "Storage cache not found for this torrent", http.StatusInternalServerError)
		return
	}

	fileCacheKey := fmt.Sprintf("%s_%s", hashHex, fileIndexStr)
	var fh *FileHeaders
	if val, ok := h.headersCache.Load(fileCacheKey); ok {
		fh = val.(*FileHeaders)
	} else {
		fh = &FileHeaders{}
		h.headersCache.Store(fileCacheKey, fh)
	}

	// 1. Explicit raw request → serve progressive bytes straight from the cache.
	if isRaw {
		slog.Debug("Serving raw stream", "path", file.Path(), "offset", file.Offset(), "length", file.Length())
		rawReader := storage.NewReader(r.Context(), t, cache, file.Offset(), file.Length(), readClass, readDeadline)
		defer rawReader.Close()
		reader := NewCachingReader(rawReader, file.Length(), fh)

		contentType := getMimeType(file.Path())
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		http.ServeContent(w, r, filepath.Base(file.Path()), time.Time{}, reader)
		return
	}

	// 2. Perform Preload (only on initial player requests - range starts at 0 or empty range)
	rangeHeader := r.Header.Get("Range")
	shouldPreload := rangeHeader == "" || strings.HasPrefix(rangeHeader, "bytes=0-")
	if shouldPreload {
		go func() {
			err := cache.Preload(t, file, h.Config.PreloadBytes)
			if err != nil {
				slog.Warn("Preload failed", "error", err)
			}
		}()
		go h.preloadHeadersToCache(hashHex, fileIndexStr, file, cache, t)
	}

	// 3. Serve progressive bytes using the custom storage reader. Adaptive/remuxed playback is handled
	// entirely by the in-process HLS pipeline (see media/ + hlsmedia.go); this endpoint is now just a
	// direct progressive download of the container. Legacy ?audio/?start/?remux params are ignored — the
	// dumb player no longer sends them (it consumes a ready HLS descriptor instead).
	rawReader := storage.NewReader(r.Context(), t, cache, file.Offset(), file.Length(), readClass, readDeadline)
	defer rawReader.Close()
	reader := NewCachingReader(rawReader, file.Length(), fh)

	contentType := getMimeType(file.Path())
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	slog.Info("Streaming file directly", "path", file.Path(), "mime", contentType, "size", file.Length())
	http.ServeContent(w, r, filepath.Base(file.Path()), time.Time{}, reader)
}

func getMimeType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mkv":
		return "video/x-matroska"
	case ".mp4":
		return "video/mp4"
	case ".avi":
		return "video/x-msvideo"
	case ".ts":
		return "video/MP2T"
	case ".mov":
		return "video/quicktime"
	default:
		t := mime.TypeByExtension(ext)
		if t != "" {
			return t
		}
		return "application/octet-stream"
	}
}

type FileHeaders struct {
	mu         sync.RWMutex
	StartBytes []byte
	EndBytes   []byte
	EndOffset  int64
}

type CachingReader struct {
	reader       *storage.Reader
	fileSize     int64
	pos          int64
	headersCache *FileHeaders
}

func NewCachingReader(r *storage.Reader, fileSize int64, headersCache *FileHeaders) *CachingReader {
	return &CachingReader{
		reader:       r,
		fileSize:     fileSize,
		headersCache: headersCache,
	}
}

func (cr *CachingReader) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = cr.pos + offset
	case io.SeekEnd:
		newPos = cr.fileSize + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("negative position: %d", newPos)
	}
	cr.pos = newPos
	return newPos, nil
}

func (cr *CachingReader) Read(p []byte) (int, error) {
	if cr.pos >= cr.fileSize {
		return 0, io.EOF
	}

	limit := cr.fileSize - cr.pos
	if int64(len(p)) > limit {
		p = p[:limit]
	}
	if len(p) == 0 {
		return 0, nil
	}

	if cr.headersCache != nil {
		cr.headersCache.mu.RLock()
		startLen := int64(len(cr.headersCache.StartBytes))
		endLen := int64(len(cr.headersCache.EndBytes))
		endOffset := cr.headersCache.EndOffset

		// Case 1: Read falls entirely within StartBytes
		if cr.pos+int64(len(p)) <= startLen {
			copy(p, cr.headersCache.StartBytes[cr.pos:cr.pos+int64(len(p))])
			cr.pos += int64(len(p))
			cr.headersCache.mu.RUnlock()
			return len(p), nil
		}

		// Case 2: Read falls entirely within EndBytes
		if cr.pos >= endOffset && cr.pos+int64(len(p)) <= endOffset+endLen {
			localStart := cr.pos - endOffset
			copy(p, cr.headersCache.EndBytes[localStart:localStart+int64(len(p))])
			cr.pos += int64(len(p))
			cr.headersCache.mu.RUnlock()
			return len(p), nil
		}
		cr.headersCache.mu.RUnlock()
	}

	// Fallback to underlying storage reader
	_, err := cr.reader.Seek(cr.pos, io.SeekStart)
	if err != nil {
		return 0, err
	}

	n, err := cr.reader.Read(p)
	if n > 0 {
		cr.pos += int64(n)
	}

	return n, err
}

func (cr *CachingReader) Close() error {
	return cr.reader.Close()
}

func (h *HandlerContext) preloadHeadersToCache(hashHex, fileIndexStr string, file *torrent.File, cache *storage.Cache, t *torrent.Torrent) {
	cacheKey := fmt.Sprintf("%s_%s", hashHex, fileIndexStr)
	var fh *FileHeaders
	if val, ok := h.headersCache.Load(cacheKey); ok {
		fh = val.(*FileHeaders)
	} else {
		fh = &FileHeaders{}
		h.headersCache.Store(cacheKey, fh)
	}

	fileSize := file.Length()
	startMax := int64(8 * 1024 * 1024)
	if startMax > fileSize {
		startMax = fileSize
	}
	endMin := fileSize - int64(8*1024*1024)
	if endMin < 0 {
		endMin = 0
	}

	// Read lock to check if already fully cached to avoid redundant operations
	fh.mu.RLock()
	hasStart := int64(len(fh.StartBytes)) >= startMax
	hasEnd := endMin >= fileSize || int64(len(fh.EndBytes)) >= (fileSize-endMin)
	fh.mu.RUnlock()

	if hasStart && hasEnd {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 1. Read first 8MB without lock
	if !hasStart {
		reader := storage.NewReader(ctx, t, cache, file.Offset(), fileSize, storage.ClassColdProbe, time.Time{}) // header prefetch: probe-shaped, yields to the player
		buf := make([]byte, startMax)
		n, err := io.ReadFull(reader, buf)
		reader.Close()

		if n > 0 {
			fh.mu.Lock()
			if int64(len(fh.StartBytes)) < int64(n) {
				fh.StartBytes = buf[:n]
			}
			fh.mu.Unlock()
		}
		slog.Info("Proactively cached start headers in RAM", "size", n, "key", cacheKey, "err", err)
	}

	// 2. Read last 8MB without lock
	if !hasEnd {
		reader := storage.NewReader(ctx, t, cache, file.Offset(), fileSize, storage.ClassColdProbe, time.Time{}) // footer prefetch: probe-shaped, yields to the player
		_, err := reader.Seek(endMin, io.SeekStart)
		if err == nil {
			buf := make([]byte, fileSize-endMin)
			n, err := io.ReadFull(reader, buf)
			reader.Close()

			if n > 0 {
				fh.mu.Lock()
				if int64(len(fh.EndBytes)) < int64(n) {
					fh.EndOffset = endMin
					fh.EndBytes = buf[:n]
				}
				fh.mu.Unlock()
			}
			slog.Info("Proactively cached end headers in RAM", "size", n, "key", cacheKey, "err", err)
		} else {
			reader.Close()
		}
	}
}
