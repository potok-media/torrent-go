package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// HandleTmdbLookup resolves a TMDB id to a title + poster URL + year for the Add-torrent dialog, so the
// user can pre-fill a torrent's media card from a TMDB id instead of typing everything. It proxies TMDB
// (v3 api_key from TMDB_API_KEY) so no key is exposed to the browser. Returns 501 if no key is set —
// manual title/poster entry still works without it.
func (h *HandlerContext) HandleTmdbLookup(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	mediaType := r.URL.Query().Get("type")
	if mediaType != "tv" {
		mediaType = "movie"
	}
	key := ""
	if h.Config != nil {
		key = h.Config.TmdbKey
	}
	if key == "" {
		http.Error(w, "TMDB not configured (set TMDB_API_KEY)", http.StatusNotImplemented)
		return
	}

	api := fmt.Sprintf("https://api.themoviedb.org/3/%s/%s?api_key=%s&language=ru-RU",
		mediaType, url.PathEscape(id), url.QueryEscape(key))
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(api)
	if err != nil {
		http.Error(w, "TMDB request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "TMDB lookup failed", resp.StatusCode)
		return
	}

	var m struct {
		Title       string `json:"title"`
		Name        string `json:"name"`
		PosterPath  string `json:"poster_path"`
		ReleaseDate string `json:"release_date"`
		FirstAir    string `json:"first_air_date"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		http.Error(w, "bad TMDB response", http.StatusBadGateway)
		return
	}
	title := m.Title
	if title == "" {
		title = m.Name
	}
	date := m.ReleaseDate
	if date == "" {
		date = m.FirstAir
	}
	year := ""
	if len(date) >= 4 {
		year = date[:4]
	}
	poster := ""
	if m.PosterPath != "" {
		poster = "https://image.tmdb.org/t/p/w500" + m.PosterPath
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"title": title, "poster": poster, "year": year, "mediaType": mediaType, "tmdbId": id,
	})
}
