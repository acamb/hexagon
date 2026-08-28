package httpapi

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves the embedded frontend. Requests that match a real file are
// served as-is; everything else falls back to index.html so client-side routes
// survive a reload.
func (s *Server) spaHandler() http.Handler {
	if _, err := fs.Stat(s.frontend, "index.html"); err != nil {
		s.log.Warn("frontend not built, serving a placeholder page; run `make build-web`")
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "frontend not built: run `make build-web`")
		})
	}

	files := http.FileServerFS(s.frontend)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		if _, err := fs.Stat(s.frontend, name); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				s.log.Error("stat frontend asset", "name", name, "err", err)
			}
			s.serveIndex(w, r)
			return
		}

		// Vite emits content-hashed filenames under /assets, so they can be
		// cached forever. index.html deliberately is not.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.frontend.Open("index.html")
	if err != nil {
		s.log.Error("open index.html", "err", err)
		writeError(w, http.StatusInternalServerError, "frontend unavailable")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := io.Copy(w, f); err != nil {
		s.log.Error("write index.html", "err", err)
	}
}
