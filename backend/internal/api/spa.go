package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// spaHandler serves the embedded frontend build (web/dist). Requests for real
// files are served directly; everything else falls back to index.html so the
// React client (T2) can handle routing. API paths never reach here — they're
// matched by more specific mux patterns first.
func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			httpx.Fail(w, http.StatusNotFound, "not_found", "API endpoint not found")
			return
		}

		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "" {
			s.serveIndex(w, r)
			return
		}
		if _, err := fs.Stat(s.webFS, clean); err != nil {
			// Not a real asset → SPA fallback.
			s.serveIndex(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.webFS, "index.html")
	if err != nil {
		http.Error(w, "frontend not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}
