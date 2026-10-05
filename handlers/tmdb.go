package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// TmdbLookupResponse is the pre-fill data for the Add-torrent dialog resolved from a TMDB id.
type TmdbLookupResponse struct {
	Title     string `json:"title"`
	Poster    string `json:"poster"`
	Year      string `json:"year"`
	MediaType string `json:"mediaType" enums:"movie,tv"`
	TmdbID    string `json:"tmdbId"`
}

// HandleTmdbLookup godoc
//	@ID			tmdbLookup
//
//	@Summary		Look up title/poster by TMDB id
//	@Description	Resolves a TMDB id to a title + poster URL + year for the Add-torrent dialog. Proxies TMDB (v3 api_key from TMDB_API_KEY) so no key is exposed to the browser. Returns 501 if no key is set — manual title/poster entry still works without it.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			id		query		string	true	"TMDB id"
//	@Param			type	query		string	false	"movie (default) | tv"	Enums(movie, tv)
//	@Success		200		{object}	handlers.TmdbLookupResponse
//	@Failure		400		{string}	string	"id required"
//	@Failure		501		{string}	string	"TMDB not configured (set TMDB_API_KEY)"
//	@Failure		502		{string}	string	"TMDB request/lookup failed"
//	@x-scalar-ignore	true
//	@Router			/api/manage/tmdb [get]
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

	writeJSON(w, http.StatusOK, TmdbLookupResponse{
		Title: title, Poster: poster, Year: year, MediaType: mediaType, TmdbID: id,
	})
}
