package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"Potok.Backend.TorrentGo/storage"
)

// The management UI exposes exactly ONE tunable: the global RAM budget for torrents. Everything else
// (max concurrent streams, transcoder cap, per-torrent cache ceiling) is derived from it in storage, so
// there is no pile of knobs to expose. The chosen budget is persisted to DataDir/settings.json and
// re-applied on boot.

type settingsDTO struct {
	CacheBudgetBytes    int64 `json:"cacheBudgetBytes"`
	MinBudgetBytes      int64 `json:"minBudgetBytes"`
	PerStreamBytes      int64 `json:"perStreamBytes"`
	MaxStreams          int   `json:"maxStreams"`
	MaxAudioTranscoders int   `json:"maxAudioTranscoders"`
	CacheFilled         int64 `json:"cacheFilled"`
}

type settingsFile struct {
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
	var s settingsFile
	if json.Unmarshal(b, &s) == nil && s.CacheBudgetBytes > 0 {
		h.Engine.Storage.SetGlobalCapacity(s.CacheBudgetBytes)
		slog.Info("applied persisted cache budget", "bytes", h.Engine.Storage.GlobalCapacity())
	}
}

func (h *HandlerContext) currentSettings() settingsDTO {
	st := h.Engine.Storage
	return settingsDTO{
		CacheBudgetBytes:    st.GlobalCapacity(),
		MinBudgetBytes:      storage.PerStreamWindowBytes(),
		PerStreamBytes:      storage.PerStreamWindowBytes(),
		MaxStreams:          st.DerivedMaxStreams(),
		MaxAudioTranscoders: st.DerivedMaxAudioTranscoders(),
		CacheFilled:         st.GlobalFilled(),
	}
}

func (h *HandlerContext) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.currentSettings())
}

func (h *HandlerContext) HandleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsFile
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CacheBudgetBytes <= 0 {
		http.Error(w, "cacheBudgetBytes (positive) required", http.StatusBadRequest)
		return
	}
	h.Engine.Storage.SetGlobalCapacity(req.CacheBudgetBytes) // clamps to the minimum + evicts down

	if p := h.settingsPath(); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			b, _ := json.MarshalIndent(settingsFile{CacheBudgetBytes: h.Engine.Storage.GlobalCapacity()}, "", "  ")
			tmp := p + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, p)
			}
		}
	}

	writeJSON(w, http.StatusOK, h.currentSettings())
}
