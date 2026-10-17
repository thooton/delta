package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// The website: Delta in a browser, for a computer, alongside the phone app.
// It is plain HTML, CSS and JavaScript (web/), embedded here and served
// without the token, which the page asks for itself and sends with every
// request it makes to the API on this same origin. Cloud sessions go
// through the cloud relay (cloud.go), because the provider's session routes
// can't be called from a browser.
//
//	GET /        the page
//	GET /web/... its scripts and styles
//
//go:embed web
var webFiles embed.FS

// withWebsite serves the website, and hands everything else to api.
func withWebsite(api http.Handler) http.Handler {
	files, _ := fs.Sub(webFiles, "web")
	static := http.StripPrefix("/web/", http.FileServerFS(files))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, files, "index.html")
	})
	mux.Handle("GET /web/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Revalidated each time, so an update reaches open browsers on reload.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	mux.Handle("/", api)
	return mux
}
