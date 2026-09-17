package handlers

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"Potok.Backend.TorrentGo/bt"
	"Potok.Backend.TorrentGo/catalog"
	"github.com/go-chi/chi/v5"
)

var btihRe = regexp.MustCompile(`(?i)xt=urn:btih:([a-z2-7]{32}|[a-f0-9]{40})`)

// infohashFromMagnet extracts the v1 infohash (hex) from a magnet URI without touching the engine, so a
// torrent can be saved to the library by metadata alone. Handles both 40-char hex and 32-char base32.
func infohashFromMagnet(magnet string) (string, error) {
	m := btihRe.FindStringSubmatch(magnet)
	if m == nil {
		return "", errors.New("no btih infohash in magnet")
	}
	h := m[1]
	switch len(h) {
	case 40:
		if _, err := hex.DecodeString(h); err != nil {
			return "", err
		}
		return strings.ToLower(h), nil
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
		if err != nil || len(b) != 20 {
			return "", errors.New("bad base32 infohash")
		}
		return hex.EncodeToString(b), nil
	}
	return "", errors.New("unsupported infohash length")
}

// HandleSaveLibrary saves ONLY a torrent's metadata (a library entry) — no resolve, no download, no
// stream. The Add button uses this; downloading to disk is a separate explicit action. Requires a magnet
// (infohash parsed locally); .torrent URLs must go through the download path since they need fetching.
func (h *HandlerContext) HandleSaveLibrary(w http.ResponseWriter, r *http.Request) {
	var req TorrentFilesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	magnet := ""
	if req.MagnetUri != nil && *req.MagnetUri != "" {
		magnet = *req.MagnetUri
	} else if req.Link != nil {
		magnet = *req.Link
	}
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		http.Error(w, "a magnet link is required to save without downloading", http.StatusBadRequest)
		return
	}
	hash, err := infohashFromMagnet(magnet)
	if err != nil {
		http.Error(w, "could not read infohash from magnet: "+err.Error(), http.StatusBadRequest)
		return
	}
	if h.Catalog == nil {
		http.Error(w, "catalog unavailable", http.StatusInternalServerError)
		return
	}

	e := catalog.Entry{Hash: hash, Source: magnet, Title: req.Title}
	if req.OriginalTitle != nil {
		e.OriginalTitle = *req.OriginalTitle
	}
	if req.MediaType != nil {
		e.MediaType = *req.MediaType
	}
	if req.NumberOfSeasons != nil {
		e.NumberOfSeasons = *req.NumberOfSeasons
	}
	if req.TmdbId != nil {
		e.TmdbID = *req.TmdbId
	}
	if req.Poster != nil {
		e.Poster = *req.Poster
	}
	h.Catalog.Upsert(e) // DownloadMode stays empty → "saved"
	writeJSON(w, http.StatusOK, map[string]any{"hash": hash, "saved": true})
}

// HandleDownloadSaved engages a saved library entry: resolves its magnet, attaches disk storage, and
// downloads the whole file to disk in the background. This is the "Download to disk" action for a saved
// torrent (or from the Add dialog). Idempotent-ish — re-calling just re-prioritises.
func (h *HandlerContext) HandleDownloadSaved(w http.ResponseWriter, r *http.Request) {
	hashHex := chi.URLParam(r, "hash")
	if b, err := hex.DecodeString(hashHex); err != nil || len(b) != 20 {
		http.Error(w, "Invalid torrent hash format", http.StatusBadRequest)
		return
	}
	if h.Catalog == nil {
		http.Error(w, "catalog unavailable", http.StatusInternalServerError)
		return
	}
	e, ok := h.Catalog.Get(hashHex)
	if !ok || e.Source == "" {
		http.Error(w, "no saved source for this torrent", http.StatusNotFound)
		return
	}

	t, err := bt.ResolveTorrent(context.Background(), h.Engine.Client, e.Source)
	if err != nil {
		http.Error(w, "resolve failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.Engine.Storage.SetMode(t.InfoHash(), true) // disk
	h.Catalog.Upsert(catalog.Entry{Hash: hashHex, DownloadMode: catalog.ModeDisk})

	dl := h.Config != nil && h.Config.DownloadDir != ""
	tt := t
	go func() {
		select {
		case <-tt.GotInfo():
			if dl {
				tt.DownloadAll()
			}
		case <-time.After(90 * time.Second):
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"hash": hashHex, "downloading": true})
}
