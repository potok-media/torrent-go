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

	"github.com/go-chi/chi/v5"
	"github.com/potok-media/potok-torrentgo/bt"
	"github.com/potok-media/potok-torrentgo/catalog"
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

// SavedLibraryResponse acknowledges saving a library entry.
type SavedLibraryResponse struct {
	Hash  string `json:"hash"`
	Saved bool   `json:"saved"`
}

// DownloadStartedResponse acknowledges engaging a saved library entry for disk download.
type DownloadStartedResponse struct {
	Hash        string `json:"hash"`
	Downloading bool   `json:"downloading"`
}

// HandleSaveLibrary godoc
//	@ID			saveLibrary
//
//	@Summary		Save a torrent to the library
//	@Description	Saves ONLY a torrent's metadata (a library entry) — no resolve, no download, no stream. Requires a magnet link (the infohash is parsed locally); .torrent URLs must go through the download path since they need fetching.
//	@Tags			Management
//	@Accept			json
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			request	body		handlers.TorrentFilesRequest	true	"Magnet link + optional media metadata"
//	@Success		200		{object}	handlers.SavedLibraryResponse
//	@Failure		400		{string}	string	"bad body / magnet required / unreadable infohash"
//	@Failure		500		{string}	string	"catalog unavailable"
//	@x-scalar-ignore	true
//	@Router			/api/manage/library [post]
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
	writeJSON(w, http.StatusOK, SavedLibraryResponse{Hash: hash, Saved: true})
}

// HandleDownloadSaved godoc
//	@ID			downloadSaved
//
//	@Summary		Download a saved torrent to disk
//	@Description	Engages a saved library entry: resolves its magnet, attaches disk storage, and downloads the whole file to disk in the background. Idempotent-ish — re-calling just re-prioritises.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			hash	path		string	true	"Infohash (40-char hex)"
//	@Success		200		{object}	handlers.DownloadStartedResponse
//	@Failure		400		{string}	string	"invalid torrent hash format"
//	@Failure		404		{string}	string	"no saved source for this torrent"
//	@Failure		500		{string}	string	"catalog unavailable"
//	@Failure		502		{string}	string	"resolve failed"
//	@x-scalar-ignore	true
//	@Router			/api/manage/torrents/{hash}/download [post]
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
	writeJSON(w, http.StatusOK, DownloadStartedResponse{Hash: hashHex, Downloading: true})
}
