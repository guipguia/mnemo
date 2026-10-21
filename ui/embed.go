// Package ui embeds the built web UI. Build it with `npm ci && npm run build` in ui/
// before `go build`; without that, the hub serves a hint and the API still works.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	return handler(sub)
}

func handler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	_, err := fs.Stat(fsys, "index.html")
	built := err == nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !built {
			http.Error(w, "mnemo UI not built: run `npm ci && npm run build` in ui/, then rebuild. The API is at /api/v1/.", http.StatusNotFound)
			return
		}
		files.ServeHTTP(w, r)
	})
}
