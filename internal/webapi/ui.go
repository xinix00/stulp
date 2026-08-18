package webapi

import (
	"embed"
	"io/fs"

	"github.com/xinix00/stulp/internal/stulphttp"
)

//go:embed ui/*
var uiFiles embed.FS

func (s *Server) handleUI() {
	root, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err)
	}
	assetHandler := stulphttp.ServeFSPrefix("/assets/", root)
	s.mux.HandleFunc("GET /assets/", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		// Asset URLs intentionally stay simple. Revalidate them so a locally
		// rebuilt binary cannot leave an open browser on yesterday's CSS or JS.
		response.Header().Set("Cache-Control", "no-cache")
		assetHandler(response, request)
	})
	// De service worker en het webmanifest staan met opzet niet onder /assets/.
	//
	// Een service worker mag alleen berichten aannemen voor de map waarin hij
	// zelf staat, dus zou /assets/sw.js alleen over /assets/ gaan en nooit over
	// de pagina zelf. En een iPhone laat pushberichten uitsluitend toe als de
	// pagina als app op het beginscherm staat, wat een manifest met "standalone"
	// vereist; dat leest Safari van de plek waar de pagina hem noemt.
	s.mux.HandleFunc("GET /sw.js", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		response.Header().Set("Cache-Control", "no-cache")
		stulphttp.ServeFS(response, request, root, "sw.js")
	})
	s.mux.HandleFunc("GET /manifest.webmanifest", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		response.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
		response.Header().Set("Cache-Control", "no-cache")
		stulphttp.ServeFS(response, request, root, "manifest.webmanifest")
	})
	s.mux.HandleFunc(stulphttp.RootPattern, func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		response.Header().Set("Cache-Control", "no-cache")
		stulphttp.ServeFS(response, request, root, "index.html")
	})
	s.mux.HandleFunc("GET /app", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		stulphttp.ServeFS(response, request, root, "index.html")
	})
}
