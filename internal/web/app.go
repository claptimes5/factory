package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// The dashboard: a React app built from web/ by `make web` into app/, and
// embedded here so Node is only needed to change it.
//
//go:embed all:app
var appFS embed.FS

// OpenAPI describes the JSON API; web/ generates its types from it.
//
//go:embed openapi.yaml
var OpenAPI []byte

func (s *Server) routesApp(mux *http.ServeMux) error {
	dist, err := fs.Sub(appFS, "app")
	if err != nil {
		return err
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return err
	}
	files := http.FileServer(http.FS(dist))
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /app/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/app")
		p = strings.TrimPrefix(p, "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(dist, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					// Vite puts a content hash in every asset name.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + p
				files.ServeHTTP(w, r2)
				return
			}
			if strings.HasPrefix(p, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		// Any other path is a client-side route.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Diffs and transcripts come from untrusted agents: even if one got
		// past React's escaping it could not load or send anything.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
	return nil
}
