package main

import (
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
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
	page := versionAssets(files)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.Handle("GET /web/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Revalidated each time, so an update reaches open browsers on reload.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	mux.Handle("/", api)
	return mux
}

// versionAssets is index.html with this build's version on every web/
// script and stylesheet it loads (web/app.js?v=<version>). Cloudflare, in
// front of the command center, keeps .js and .css for hours whatever
// Cache-Control says, so without it an update's page could run the last
// update's script; a new version is a new address, which nothing has cached.
func versionAssets(files fs.FS) []byte {
	page, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return []byte("index.html is missing from this build")
	}
	stamp := []byte(`${1}web/${2}?v=` + url.QueryEscape(version()) + `"`)
	return assetLink.ReplaceAll(page, stamp)
}

var assetLink = regexp.MustCompile(`((?:src|href)=")web/([^"?]+\.(?:js|css))"`)
