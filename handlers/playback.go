package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Playback lifecycle — the Jellyfin/Plex model, self-contained on this server (no gateway bridge).
//
// A "playback session" = one open player instance. The client mints a sessionId and, while its player is
// open, POSTs a keepalive every few seconds carrying WHAT it is watching: {hash, file, audio}. The TORRENT
// lifetime is then derived purely by REFERENCE COUNT over the live sessions — no idle-timeout guessing.
// Media segments are produced on demand IN-PROCESS by media/, so there is no ffmpeg producer to
// refcount/reposition/reap anymore — only the torrent. All of that state lives under one `lifecycleMu`
// (torrents.go): a single owner, the anacrolix/Jellyfin shape.

type playSession struct {
	hash     string
	file     string
	lastPing time.Time
}

func (h *HandlerContext) sessionTTL() time.Duration {
	if h.Config != nil && h.Config.SessionTTL > 0 {
		return h.Config.SessionTTL
	}
	return 25 * time.Second
}

func (h *HandlerContext) torrentGrace() time.Duration {
	if h.Config != nil && h.Config.TorrentIdleTimeout > 0 {
		return h.Config.TorrentIdleTimeout
	}
	return 60 * time.Second
}

// maxStreams is the admission cap on concurrent playback sessions, DERIVED from the global RAM budget
// (each live session eviction-protects a ~64MB read-ahead window). Scales automatically when the user
// changes the budget in the UI — no separate knob. 0 = unlimited (no storage).
func (h *HandlerContext) maxStreams() int {
	if h.Engine != nil && h.Engine.Storage != nil {
		return h.Engine.Storage.DerivedMaxStreams()
	}
	return 0
}

type playbackKeepalive struct {
	SessionID string `json:"sessionId"`
	Hash      string `json:"hash"`
	File      string `json:"file"`
}

// HandlePlaybackKeepalive upserts the caller's session (what it is watching) so the torrent stays alive
// while a player is open.
func (h *HandlerContext) HandlePlaybackKeepalive(w http.ResponseWriter, r *http.Request) {
	var req playbackKeepalive
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" || req.Hash == "" {
		http.Error(w, "bad keepalive", http.StatusBadRequest)
		return
	}
	h.lifecycleMu.Lock()
	// Admission control: a brand-new session beyond the cap is refused so the un-evictable read-ahead
	// windows can't outgrow the global cache budget. Existing sessions always refresh (never dropped).
	if _, exists := h.playback[req.SessionID]; !exists {
		if max := h.maxStreams(); max > 0 && len(h.playback) >= max {
			h.lifecycleMu.Unlock()
			w.Header().Set("Access-Control-Allow-Origin", "*")
			http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
			return
		}
	}
	h.playback[req.SessionID] = &playSession{hash: req.Hash, file: req.File, lastPing: time.Now()}
	h.lifecycleMu.Unlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusNoContent)
}

// HandlePlaybackStop removes the session (player closed / navigated away). Reads the id from the query so
// it works with navigator.sendBeacon (no readable body on unload).
func (h *HandlerContext) HandlePlaybackStop(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		var req struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		sessionID = req.SessionID
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if sessionID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h.lifecycleMu.Lock()
	delete(h.playback, sessionID)
	h.lifecycleMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ReapPlaybackSessions runs for the process lifetime. Each tick it expires sessions whose keepalive lapsed
// past sessionTTL (the reliable backstop for a missed stop), then drops torrents that have had no session
// for the linger grace. Blocking Drops run AFTER the lock is released.
func (h *HandlerContext) ReapPlaybackSessions() {
	for {
		time.Sleep(5 * time.Second)
		now := time.Now()
		cutoff := now.Add(-h.sessionTTL())

		h.lifecycleMu.Lock()
		expired := 0
		for id, s := range h.playback {
			if s.lastPing.Before(cutoff) {
				delete(h.playback, id)
				expired++
			}
		}
		h.lifecycleMu.Unlock()
		if expired > 0 {
			slog.Info("playback sessions expired", "count", expired)
		}

		// Drop torrents nobody watches, after the linger grace. torrentSeen advances only while a torrent
		// has ≥1 session, so the grace is purely the post-abandonment delay — it can never touch an
		// actively-watched torrent (that always has a live session → refcount > 0).
		grace := h.torrentGrace()
		for _, t := range h.Engine.Client.Torrents() {
			hash := t.InfoHash().HexString()
			// Persistent torrents are never reaped: pinned OR disk-mode (they download to completion in the
			// background with no viewer). Keep their grace clock fresh so a session-less one survives.
			if h.isPersistentTorrent(hash) {
				h.lifecycleMu.Lock()
				h.torrentSeen[hash] = now
				h.lifecycleMu.Unlock()
				continue
			}
			h.lifecycleMu.Lock()
			drop := false
			if h.torrentRefcountLocked(hash) > 0 {
				h.torrentSeen[hash] = now
			} else if seen, ok := h.torrentSeen[hash]; !ok {
				h.torrentSeen[hash] = now // start the clock the first time we see it session-less
			} else if now.Sub(seen) > grace {
				drop = true
			}
			h.lifecycleMu.Unlock()
			if drop {
				h.dropTorrent(t.InfoHash(), hash)
			}
		}
	}
}

// torrentRefcountLocked counts the live sessions on a torrent. Caller MUST hold lifecycleMu.
func (h *HandlerContext) torrentRefcountLocked(hash string) int {
	n := 0
	for _, s := range h.playback {
		if s.hash == hash {
			n++
		}
	}
	return n
}

// Watchers is the anonymous count of live playback sessions on a torrent (a refcount, not identities —
// TorrentGo never sees who is watching). Safe wrapper over torrentRefcountLocked for the management API.
func (h *HandlerContext) Watchers(hash string) int {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	return h.torrentRefcountLocked(hash)
}

// watchersByHash snapshots, in one lock, the live-session count and the current file per torrent for the
// list endpoint (avoids taking lifecycleMu once per torrent).
func (h *HandlerContext) watchersByHash() (counts map[string]int, files map[string]string) {
	counts = make(map[string]int)
	files = make(map[string]string)
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	for _, s := range h.playback {
		counts[s.hash]++
		if _, ok := files[s.hash]; !ok && s.file != "" {
			files[s.hash] = s.file
		}
	}
	return counts, files
}

// sessionStats returns total live sessions and how many distinct torrents are being streamed right now.
func (h *HandlerContext) sessionStats() (sessions, streamingTorrents int) {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	sessions = len(h.playback)
	seen := make(map[string]struct{}, len(h.playback))
	for _, s := range h.playback {
		seen[s.hash] = struct{}{}
	}
	return sessions, len(seen)
}
