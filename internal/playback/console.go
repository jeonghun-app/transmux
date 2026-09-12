package playback

import (
	"embed"
	"io/fs"
	"net/http"
)

// Assets are served locally: operation does not depend on a public CDN, and
// no viewer's camera name or playback capability is sent to a third party.
//
//go:embed web/*
var web embed.FS

func (s *Server) console(mux *http.ServeMux) {
	assets, _ := fs.Sub(web, "web")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		body, err := web.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "Console unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	})
}
