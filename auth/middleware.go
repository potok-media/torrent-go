package auth

import (
	"crypto/subtle"
	"net/http"

	"Potok.Backend.TorrentGo/config"
)

func BasicAuth(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.AuthUser == "" {
				next.ServeHTTP(w, r)
				return
			}

			user, pass, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(user), []byte(cfg.AuthUser)) != 1 ||
				subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.AuthPass)) != 1 {
				
				w.Header().Set("WWW-Authenticate", `Basic realm="Potok TorrentGo v2"`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("Unauthorized\n"))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
