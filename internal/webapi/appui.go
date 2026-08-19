package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"

	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/stulphttp"
)

type appUIContext struct {
	AppID     string `json:"appId"`
	DriverID  string `json:"driverId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Origin    string `json:"origin"`
}

func (s *Server) handleAppUI() {
	s.mux.HandleFunc("GET /stulp.js", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		data, err := uiFiles.ReadFile("ui/stulp.js")
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		response.Header().Set("Cache-Control", "no-cache")
		_, _ = response.Write(data)
	})
	// Alleen de rest-vorm: {path...} matcht óók de lege rest (de wortel mét
	// slash), op beide gedaanten, en de handler maakt van "" al index.html —
	// de {$}-regel die hiernaast stond was dubbel (en de leanhttp-mux draagt
	// {$} per KAM niet).
	s.mux.HandleFunc("GET /app-ui/{app}/settings/{path...}", s.serveSettingsUI)
	s.mux.HandleFunc("GET /app-ui/{app}/pair/{driver}/{path...}", s.servePairUI)
}

func (s *Server) serveSettingsUI(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	app, err := s.store.App(stulphttp.Context(request), request.PathValue("app"))
	if err != nil {
		stulphttp.NotFound(response, request)
		return
	}
	relative := request.PathValue("path")
	if relative == "" {
		relative = "index.html"
	}
	s.serveAppAsset(response, request, app, "settings", relative, appUIContext{
		AppID: app.ID, Origin: "settings",
	})
}

func (s *Server) servePairUI(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	app, err := s.store.App(stulphttp.Context(request), request.PathValue("app"))
	if err != nil {
		stulphttp.NotFound(response, request)
		return
	}
	relative := request.PathValue("path")
	if relative == "" {
		relative = "validate.html"
	}
	s.serveAppAsset(response, request, app, path.Join("drivers", request.PathValue("driver"), "pair"), relative, appUIContext{
		AppID: app.ID, DriverID: request.PathValue("driver"), SessionID: stulphttp.Query(request).Get("session"), Origin: "pair",
	})
}

func (s *Server) serveAppAsset(response stulphttp.ResponseWriter, request *stulphttp.Request, app store.App, base, relative string, context appUIContext) {
	clean, ok := cleanAppAssetPath(relative)
	if !ok {
		stulphttp.NotFound(response, request)
		return
	}
	data, found, err := s.readAppAsset(stulphttp.Context(request), app, base, clean)
	if !found && err == nil {
		stulphttp.NotFound(response, request)
		return
	}
	if err != nil {
		writeError(response, stulphttp.StatusBadGateway, err)
		return
	}
	if strings.EqualFold(path.Ext(clean), ".html") {
		data, err = s.decorateAppHTML(stulphttp.Context(request), data, context, app)
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else if contentType := mime.TypeByExtension(path.Ext(clean)); contentType != "" {
		response.Header().Set("Content-Type", contentType)
	}
	response.Header().Set("Cache-Control", "no-cache")
	_, _ = response.Write(data)
}

// readAppAsset houdt de twee gedaanten van een app achter één kleine naad. Een
// klassieke bundel blijft rechtstreeks van schijf komen; een slot-app levert
// alleen het gevraagde embedded bestand over zijn bestaande runtimeverbinding.
func (s *Server) readAppAsset(ctx context.Context, app store.App, base, relative string) ([]byte, bool, error) {
	name := path.Join(base, relative)
	if app.Root == "" {
		asset, err := s.supervisor.ReadUIAsset(ctx, app.ID, name)
		return asset.Data, asset.Found, err
	}

	root := filepath.Join(app.Root, filepath.FromSlash(base))
	target := filepath.Join(root, filepath.FromSlash(relative))
	resolved, err := filepath.Abs(target)
	if err != nil {
		return nil, false, err
	}
	allowed, err := filepath.Abs(root)
	if err != nil {
		return nil, false, err
	}
	if resolved != allowed && !strings.HasPrefix(resolved, allowed+string(filepath.Separator)) {
		return nil, false, nil
	}
	realPath, pathErr := filepath.EvalSymlinks(resolved)
	realRoot, rootErr := filepath.EvalSymlinks(allowed)
	if errors.Is(pathErr, os.ErrNotExist) || errors.Is(rootErr, os.ErrNotExist) {
		return nil, false, nil
	}
	if pathErr != nil {
		return nil, false, pathErr
	}
	if rootErr != nil {
		return nil, false, rootErr
	}
	if realPath != realRoot && !strings.HasPrefix(realPath, realRoot+string(filepath.Separator)) {
		return nil, false, nil
	}
	data, err := os.ReadFile(realPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func cleanAppAssetPath(name string) (string, bool) {
	if strings.Contains(name, `\`) {
		return "", false
	}
	clean := path.Clean(name)
	return clean, clean != "." && clean == name && fs.ValidPath(clean)
}

// hasBridgeScript zoekt een echt scripttag naar de brug, en niet elke vermelding
// van dat pad.
func hasBridgeScript(html string) bool {
	for rest := html; ; {
		start := strings.Index(rest, "<script")
		if start < 0 {
			return false
		}
		rest = rest[start:]
		end := strings.Index(rest, ">")
		if end < 0 {
			return false
		}
		tag := rest[:end]
		if strings.Contains(tag, "src=") && strings.Contains(tag, "stulp.js") {
			return true
		}
		rest = rest[end:]
	}
}

func (s *Server) decorateAppHTML(ctx context.Context, data []byte, context appUIContext, app store.App) ([]byte, error) {
	contextJSON, err := json.Marshal(context)
	if err != nil {
		return nil, err
	}
	localeJSON, err := json.Marshal(s.loadAppLocale(ctx, app, s.options.Language))
	if err != nil {
		return nil, err
	}
	bootstrap := fmt.Sprintf(`<script>window.__STULP_CONTEXT__=%s;window.__STULP_LOCALE__=%s;</script>`, contextJSON, localeJSON)
	html := string(data)
	injection := `<meta name="color-scheme" content="dark"><link rel="stylesheet" href="/assets/app-frame.css">` + bootstrap
	// De brug hoort er altijd bij, tenzij de pagina hem zelf al binnenhaalt.
	// Zoeken op het pad alleen was te ruim: een pagina die "/stulp.js" ergens in
	// een commentaar of een fetch noemt kreeg geen brug, en dan meldt de eerste
	// regel van zijn eigen script "Stulp is not defined".
	if !hasBridgeScript(html) {
		injection += `<script src="/stulp.js" data-origin="` + context.Origin + `"></script>`
	}
	if strings.Contains(html, "<head>") {
		html = strings.Replace(html, "<head>", "<head>"+injection, 1)
	} else {
		html = injection + html
	}
	return []byte(html), nil
}

func (s *Server) loadAppLocale(ctx context.Context, app store.App, language string) map[string]any {
	for _, candidate := range []string{language, "en"} {
		data, found, err := s.readAppAsset(ctx, app, "locales", candidate+".json")
		if err != nil || !found {
			continue
		}
		var locale map[string]any
		if json.Unmarshal(data, &locale) == nil {
			return locale
		}
	}
	return map[string]any{}
}

// appHasUIAsset gebruikt dezelfde statische beschrijving die de slot-app bij
// zijn manifest aanmeldt. Zo hoeft de Apps-/Drivers-lijst geen RPC per knop te
// doen; het bestand zelf wordt pas opgehaald als de gebruiker erop klikt.
func appHasUIAsset(app store.App, name string) bool {
	if app.Root != "" {
		_, err := os.Stat(filepath.Join(app.Root, filepath.FromSlash(name)))
		return err == nil
	}
	ui, _ := app.Manifest["ui"].(map[string]any)
	switch assets := ui["assets"].(type) {
	case []any:
		for _, candidate := range assets {
			if candidate == name {
				return true
			}
		}
	case []string:
		for _, candidate := range assets {
			if candidate == name {
				return true
			}
		}
	}
	return false
}
