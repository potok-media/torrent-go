package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/potok-media/potok-torrentgo/storage"
)

// The management UI exposes exactly ONE tunable: the global RAM budget for torrents. Everything else
// (max concurrent streams, transcoder cap, per-torrent cache ceiling) is derived from it in storage, so
// there is no pile of knobs to expose. The chosen budget is persisted to DataDir/settings.json and
// re-applied on boot.

// Settings is the management view of the RAM budget: the one tunable (cacheBudgetBytes) plus the limits
// derived from it (max streams, audio transcoders) and the current fill.
type Settings struct {
	CacheBudgetBytes    int64 `json:"cacheBudgetBytes"`
	MinBudgetBytes      int64 `json:"minBudgetBytes"`
	PerStreamBytes      int64 `json:"perStreamBytes"`
	MaxStreams          int   `json:"maxStreams"`
	MaxAudioTranscoders int   `json:"maxAudioTranscoders"`
	CacheFilled         int64 `json:"cacheFilled"`
}

// SettingsUpdate is the settings write body: the global RAM budget in bytes (positive).
type SettingsUpdate struct {
	CacheBudgetBytes int64 `json:"cacheBudgetBytes"`
}

func (h *HandlerContext) settingsPath() string {
	if h.Config == nil || h.Config.DataDir == "" {
		return ""
	}
	return filepath.Join(h.Config.DataDir, "settings.json")
}

// ApplyPersistedSettings re-applies a saved RAM budget at startup (before serving), so a value set in the
// UI survives restart. No-op when there is no DataDir or no saved value (falls back to POTOK_CACHE_SIZE_MB).
func (h *HandlerContext) ApplyPersistedSettings() {
	p := h.settingsPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var s SettingsUpdate
	if json.Unmarshal(b, &s) == nil && s.CacheBudgetBytes > 0 {
		h.Engine.Storage.SetGlobalCapacity(s.CacheBudgetBytes)
		slog.Info("applied persisted cache budget", "bytes", h.Engine.Storage.GlobalCapacity())
	}
}

func (h *HandlerContext) currentSettings() Settings {
	st := h.Engine.Storage
	return Settings{
		CacheBudgetBytes:    st.GlobalCapacity(),
		MinBudgetBytes:      storage.PerStreamWindowBytes(),
		PerStreamBytes:      storage.PerStreamWindowBytes(),
		MaxStreams:          st.DerivedMaxStreams(),
		MaxAudioTranscoders: st.DerivedMaxAudioTranscoders(),
		CacheFilled:         st.GlobalFilled(),
	}
}

// HandleGetSettings godoc
//	@ID			getSettings
//
//	@Summary		Get cache settings
//	@Description	The global RAM budget plus the derived limits (max concurrent streams, audio transcoder cap) and the current cache fill.
//	@Tags			Management
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Success		200	{object}	handlers.Settings
//	@x-scalar-ignore	true
//	@Router			/api/manage/settings [get]
func (h *HandlerContext) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.currentSettings())
}

// HandleSetSettings godoc
//	@ID			setSettings
//
//	@Summary		Set the global RAM budget
//	@Description	Updates the global piece-cache budget (bytes, positive; clamped to the minimum, evicts down), persists it to DataDir/settings.json, and returns the effective settings.
//	@Tags			Management
//	@Accept			json
//	@Produce		json
//	@Security		BasicAuth
//	@Failure		401		{string}	string	"unauthorized (BasicAuth)"
//	@Param			request	body		handlers.SettingsUpdate	true	"New RAM budget"
//	@Success		200		{object}	handlers.Settings
//	@Failure		400		{string}	string	"cacheBudgetBytes (positive) required"
//	@x-scalar-ignore	true
//	@Router			/api/manage/settings [post]
func (h *HandlerContext) HandleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req SettingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CacheBudgetBytes <= 0 {
		http.Error(w, "cacheBudgetBytes (positive) required", http.StatusBadRequest)
		return
	}
	h.Engine.Storage.SetGlobalCapacity(req.CacheBudgetBytes) // clamps to the minimum + evicts down

	if p := h.settingsPath(); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			b, _ := json.MarshalIndent(SettingsUpdate{CacheBudgetBytes: h.Engine.Storage.GlobalCapacity()}, "", "  ")
			tmp := p + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, p)
			}
		}
	}

	writeJSON(w, http.StatusOK, h.currentSettings())
}
