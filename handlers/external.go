package handlers

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"Potok.Backend.TorrentGo/storage"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/asticode/go-astisub"
	"github.com/go-chi/chi/v5"
)

// External (sidecar) track support for "ext" releases: dub audio / subtitle files stored as SEPARATE files in
// the torrent (folders next to the .mkv) instead of muxed in. The plugin does the video↔sidecar matching (it
// owns the Cyrillic-safe parser) and passes a compact list of true torrent file indices on the URL — audio on
// the HLS master (?xa=1,2), subtitles on /metadata (?xs=3,4). This file holds the shared, parser-free glue:
// index-list parsing, folder-derived display labels, and serving an external subtitle file as VTT/ASS.

// maxExternalSubtitleBytes caps a single external subtitle file read — real subs are KB-sized; this only guards
// against a mislabelled huge file.
const maxExternalSubtitleBytes = 32 << 20

// parseIndexList parses a comma-separated list of positive 1-based torrent file indices ("12,15,18"), dropping
// anything non-numeric or ≤0. Deduplicates while preserving order.
func parseIndexList(s string) []int {
	if s == "" {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// torrentFilePaths returns the torrent's file paths indexable 1-based (paths[idx-1]); ok=false if the torrent
// isn't active / has no info yet. Non-blocking: an unresolved torrent yields ok=false rather than waiting.
func (h *HandlerContext) torrentFilePaths(hashHex string) ([]string, bool) {
	var infoHash metainfo.Hash
	hexBytes, err := hex.DecodeString(hashHex)
	if err != nil || len(hexBytes) != 20 {
		return nil, false
	}
	copy(infoHash[:], hexBytes)
	t, ok := h.Engine.Client.Torrent(infoHash)
	if !ok || t.Info() == nil {
		return nil, false
	}
	files := t.Files()
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path()
	}
	return paths, true
}

// externalTrackLabel derives a human display name for an external track from its path (1-based idx). The dub
// studio / sub group is almost always the file's immediate parent folder (e.g. ".../RUS Sound/AniLibria/ep01.mka"
// → "AniLibria"). Falls back to the filename stem for a flat layout where the sidecar sits in the show root.
func externalTrackLabel(paths []string, idx int) string {
	if idx < 1 || idx > len(paths) {
		return fmt.Sprintf("Track %d", idx)
	}
	p := paths[idx-1]
	parts := strings.Split(p, "/")
	base := parts[len(parts)-1]
	stem := strings.TrimSuffix(base, path.Ext(base))
	if len(parts) >= 2 {
		parent := strings.TrimSpace(parts[len(parts)-2])
		// Use the parent folder unless it's the torrent's top-level folder (flat layout → the folder is the
		// show name, not a studio); then the filename stem is the better signal.
		if parent != "" && parent != strings.TrimSpace(parts[0]) {
			return cleanExternalLabel(parent)
		}
	}
	return cleanExternalLabel(stem)
}

// cleanExternalLabel tidies a folder/file name into a menu label: drop surrounding brackets and squeeze space.
func cleanExternalLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[](){}._-")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "Audio"
	}
	return s
}

// HandleGetExternalSubtitleFile serves a WHOLE external subtitle FILE (a separate file in the torrent) as VTT
// or ASS. Unlike HandleGetSubtitles (which demuxes an embedded track out of the video container), this reads
// the sidecar file directly by its torrent index and converts SRT→WebVTT (ASS/VTT pass through). It ignores
// ?start= (sub files are tiny — the whole doc is returned; the player's windowed feeder dedupes cues), so the
// player needs no change to consume it.
func (h *HandlerContext) HandleGetExternalSubtitleFile(w http.ResponseWriter, r *http.Request) {
	hashHex := chi.URLParam(r, "hash")
	fileIndexStr := chi.URLParam(r, "fileIndex")

	w.Header().Set("Access-Control-Allow-Origin", "*")

	rs, ext, err := h.openTorrentFileReader(r.Context(), hashHex, fileIndexStr, storage.ClassColdProbe)
	if err != nil {
		http.Error(w, "subtitle file unavailable", http.StatusInternalServerError)
		return
	}
	defer rs.Close()

	raw, err := io.ReadAll(io.LimitReader(rs, maxExternalSubtitleBytes))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "subtitle read failed", http.StatusInternalServerError)
		return
	}

	body, contentType := convertExternalSubtitle(raw, ext)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	_, _ = w.Write(body)
}

// convertExternalSubtitle returns the bytes to serve for an external subtitle file and its Content-Type. SRT is
// converted to WebVTT (astisub); ASS/SSA/VTT pass through unchanged. Anything else is served as-is text.
func convertExternalSubtitle(raw []byte, ext string) ([]byte, string) {
	switch strings.ToLower(ext) {
	case ".ass", ".ssa":
		return raw, "text/x-ssa; charset=utf-8"
	case ".vtt":
		return raw, "text/vtt; charset=utf-8"
	case ".srt":
		if subs, err := astisub.ReadFromSRT(bytes.NewReader(raw)); err == nil {
			var buf bytes.Buffer
			if werr := subs.WriteToWebVTT(&buf); werr == nil && buf.Len() > 0 {
				return buf.Bytes(), "text/vtt; charset=utf-8"
			}
		}
		// Fall through: serve the raw SRT (the player can still parse SubRip).
		return raw, "text/plain; charset=utf-8"
	default:
		return raw, "text/plain; charset=utf-8"
	}
}

// externalSubtitleFormat maps a sidecar subtitle file extension to the codec token the plugin uses to pick a
// player format ("ass" → octopus, else webvtt). Mirrors convertExternalSubtitle's output.
func externalSubtitleFormat(ext string) string {
	switch strings.ToLower(ext) {
	case ".ass", ".ssa":
		return "ass"
	default:
		return "webvtt"
	}
}

// appendExternalSubtitleTracks returns synthetic ClientTracks for the external subtitle files named by xs (a
// comma-separated index list), each carrying SourceFile=<torrent index> so the plugin builds its src against
// the external-file endpoint instead of the embedded-track path. relStart is the next subtitle RelIndex after
// the embedded ones. Parser-free: label from folder, codec/format from extension.
func (h *HandlerContext) appendExternalSubtitleTracks(hashHex, xs string, relStart int) []ClientTrack {
	idxs := parseIndexList(xs)
	if len(idxs) == 0 {
		return nil
	}
	paths, ok := h.torrentFilePaths(hashHex)
	if !ok {
		return nil
	}
	var out []ClientTrack
	rel := relStart
	for _, idx := range idxs {
		if idx < 1 || idx > len(paths) {
			continue
		}
		ext := strings.ToLower(path.Ext(paths[idx-1]))
		// Codec is only meaningful to the plugin as "ass → octopus, else webvtt"; leave it empty for VTT so the
		// menu label doesn't get a "(WEBVTT)" suffix.
		codec := ""
		if externalSubtitleFormat(ext) == "ass" {
			codec = "ass"
		}
		out = append(out, ClientTrack{
			Index:      idx,
			Type:       "subtitle",
			Codec:      codec,
			Language:   "",
			Title:      externalTrackLabel(paths, idx),
			RelIndex:   rel,
			SourceFile: idx,
		})
		rel++
	}
	return out
}
