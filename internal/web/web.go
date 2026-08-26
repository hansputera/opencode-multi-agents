package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// IndexHTML returns the embedded index.html content for SSR routes.
func IndexHTML() []byte {
	sub, _ := fs.Sub(dist, "dist")
	data, _ := fs.ReadFile(sub, "index.html")
	return data
}

// ManageHTML returns the embedded manage.html content.
// This is a standalone page with no SPA scripts — fully server-isolated.
func ManageHTML() []byte {
	sub, _ := fs.Sub(dist, "dist")
	data, _ := fs.ReadFile(sub, "manage.html")
	return data
}

// Handler returns an http.Handler serving the embedded web UI.
// Static assets are served with Cache-Control: no-cache so browsers
// revalidate (304) instead of heuristically caching stale JS/CSS after
// upgrades — the dashboard/chat code ships inside the binary and changes
// with every release.
//
// SPA fallback: requests for paths that don't match a real file in dist/
// serve index.html so the client-side router can handle them.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	indexHTML, _ := fs.ReadFile(sub, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Cache headers for known static assets
		if strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".css") || path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}

		// Try to open the file; if it doesn't exist, serve index.html (SPA fallback)
		f, err := sub.Open(strings.TrimPrefix(path, "/"))
		if err != nil {
			// File not found — serve index.html for client-side routing
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(indexHTML)
			return
		}
		f.Close()
		fileServer.ServeHTTP(w, r)
	})
}
