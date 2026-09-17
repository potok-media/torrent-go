// Package webui serves the standalone TorrentGo management dashboard as a self-contained static bundle
// baked into the binary via embed.FS. No node build, no external assets — just index.html + styles.css +
// app.js talking to /api/manage/* and the existing torrent endpoints. Mounted at / behind BasicAuth.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Handler serves the embedded single-page UI. Any request that isn't a real asset falls back to
// index.html so refreshes and client-side routes resolve.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r.URL.Path = "/" // SPA fallback → index.html
		}
		fileServer.ServeHTTP(w, r)
	})
}
