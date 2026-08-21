package webapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appinstall"
	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/backup"
	flowengine "github.com/xinix00/stulp/internal/flow"
	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/scene"
	"github.com/xinix00/stulp/internal/stats"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/stulphttp"
	"github.com/xinix00/stulp/internal/supervisor"
	"github.com/xinix00/stulp/internal/units"
	"github.com/xinix00/stulp/internal/valueutil"
)

type Options struct {
	StulpID      string
	StulpVersion string
	Language     string
	Timezone     string
	Token        string
	Logger       *slog.Logger
}

type Server struct {
	// appsRoot is de map waar Stulp bundels neerzet; alleen een uninstall doet er
	// nog iets mee.
	appsRoot string

	store      *store.Store
	supervisor *supervisor.Supervisor
	options    Options
	mux        *stulphttp.Mux
	flows      *flowengine.Engine
	scenes     *scene.Activator
	stats      *stats.Collector
	images     *imageshare.Store
	mcpLimit   mcpLimiter
	installMu  sync.Mutex
	pairMu     sync.Mutex
	pairs      map[string]pairSession
	// unitsSet is de eenhedenkeuze van dit huis. Hij staat in het document, maar
	// hij wordt bij elke tegel gelezen -- dus hier een kopie, in plaats van een
	// slot op de store per capability.
	unitsMu  sync.RWMutex
	unitsSet units.Set
}

type pairSession struct {
	AppID    string   `json:"appId"`
	DriverID string   `json:"driverId"`
	Handlers []string `json:"handlers"`
}

func New(database *store.Store, apps *supervisor.Supervisor, options Options) *Server {
	if options.StulpID == "" {
		options.StulpID = "stulp-local"
	}
	if options.StulpVersion == "" {
		options.StulpVersion = "12.0.0-stulp"
	}
	if options.Language == "" {
		options.Language = "en"
	}
	if options.Timezone == "" {
		options.Timezone = "UTC"
	}
	// appsRoot is alleen nog nodig om bij een uninstall de bundel op te ruimen die
	// Stulp zelf neerzette. Een fout hier is geen reden om niet te starten: dan
	// blijven er bij een uninstall bestanden staan, en dat meldt hij daar.
	appsRoot, _ := database.AppsRoot()
	s := &Server{
		store: database, supervisor: apps, options: options, appsRoot: appsRoot, mux: stulphttp.NewServeMux(),
		pairs: make(map[string]pairSession),
	}
	// De eenhedenkeuze staat in het document en niet in een vlag: het is een
	// keuze die je maakt terwijl Stulp draait. Gaat het lezen mis, dan blijft de
	// canonieke eenheid staan -- dat is wat elke installatie zonder keuze doet.
	if system, err := database.System(context.Background()); err == nil {
		s.unitsSet = system.Units
	}
	s.scenes = scene.New(database, s.invokeCapability)
	s.flows = flowengine.NewWithOptions(database, apps, flowengine.Options{
		Timezone: options.Timezone, InvokeCapability: s.invokeCapability,
		// Een token in een pushbericht leest in de eenheid van dit huis; een token
		// in een getalveld blijft de meting. De motor rekent niet zelf -- hij vraagt
		// het hier, zodat de formules op één plek staan.
		ReadToken: s.readFlowToken, ArgumentWantsNumber: s.flowArgumentWantsNumber,
	})
	s.routes()
	return s
}

const accessCookie = "stulp-session"

// Handler is de hele interface plus de API en MCP, met één toegangssleutel
// eromheen. De sleutel zelf blijft in de URL waarmee iemand voor het eerst
// binnenkomt. Daarna krijgt de browser een HttpOnly bewijs, zodat de sleutel
// niet in localStorage en niet in elke API-aanroep terugkomt.
func (s *Server) Handler() stulphttp.Handler {
	return func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		response.Header().Set("X-Stulp-ID", s.options.StulpID)
		response.Header().Set("X-Stulp-Version", s.options.StulpVersion)
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")

		requestPath := stulphttp.Path(request)
		// MCP bestaat nooit zonder expliciete sleutel. Ook de key-loze lokale
		// ontwikkelmodus mag niet per ongeluk een schrijfserver publiceren.
		if strings.HasPrefix(requestPath, "/mcp/") {
			if s.options.Token == "" || !s.accessKeyMatches(strings.TrimPrefix(requestPath, "/mcp/")) {
				stulphttp.NotFound(response, request)
				return
			}
			s.mux.ServeHTTP(response, request)
			return
		}
		if s.options.Token != "" {
			// Dit is de enige URL waarin de browser de echte sleutel gebruikt.
			// De pagina zelf krijgt no-store mee, zodat een gedeelde browsercache
			// nooit een sleutel-URL als bruikbare ingang bewaart.
			if request.Method == stulphttp.MethodGet && strings.HasPrefix(requestPath, "/") &&
				s.accessKeyMatches(strings.TrimPrefix(requestPath, "/")) {
				response.Header().Set("Set-Cookie", accessCookie+"="+s.accessProof()+"; Path=/; HttpOnly; SameSite=Strict; Max-Age=31536000")
				response.Header().Set("Cache-Control", "no-store")
				response.Header().Set("X-Robots-Tag", "noindex, nofollow")
				s.serveUI(response, request)
				return
			}

			// CSS, JS, iconen en de PWA-hulpen vertellen niets over het huis en
			// moeten al kunnen laden terwijl de zojuist gezette cookie landt.
			//
			// /stulp.js en /app-ui/ horen in dezelfde rij, en niet uit gemak: een
			// app-pagina draait in een iframe met sandbox="allow-scripts
			// allow-forms allow-modals" — ZONDER allow-same-origin, dus met een
			// opaque origin. Zo'n document krijgt bij het laden van een
			// subresource geen SameSite=Strict-cookie mee, hoe geldig de sessie
			// ook is. Achter de sleutel stond dus élk script van zo'n pagina op
			// 404: eerst de brug ("Stulp is not defined" op de koppelpagina), en
			// na alleen díe fix nog steeds page.js — een instelpagina waarvan het
			// document laadt (de navigatie komt van de ouder, mét cookie) maar het
			// eigen script niet, en die dus leeg blijft staan (gemeten 19-08 in een
			// echte browser tegen de node: page.js mét cookie 200, zonder 404).
			//
			// Wat er dan publiek staat is statische plugin-code plus HTML met
			// vertaling en een context die alleen echoot wat al in de URL zit
			// (app-id, driver, pair-sessie uit de query). Huisdata komt daar per
			// ontwerp niet: het iframe is credential-loos en alles wat bevoegdheid
			// vraagt loopt via de brug door de óuder, die de cookie wél draagt. De
			// sleutel blijft staan waar hij hoort: op /api/ en op Manage zelf.
			public := strings.HasPrefix(requestPath, "/assets/") || strings.HasPrefix(requestPath, "/image/") ||
				strings.HasPrefix(requestPath, "/app-ui/") ||
				requestPath == "/sw.js" || requestPath == "/manifest.webmanifest" ||
				requestPath == "/stulp.js"
			if !public && !s.hasAccessCookie(request) {
				if strings.HasPrefix(requestPath, "/api/") {
					writeError(response, stulphttp.StatusUnauthorized, errors.New("open /<key> before using Stulp"))
				} else {
					stulphttp.NotFound(response, request)
				}
				return
			}
		}
		if request.Method == stulphttp.MethodOptions {
			response.WriteHeader(stulphttp.StatusNoContent)
			return
		}
		s.mux.ServeHTTP(response, request)
	}
}

func (s *Server) accessKeyMatches(candidate string) bool {
	want := sha256.Sum256([]byte(s.options.Token))
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func (s *Server) accessProof() string {
	sum := sha256.Sum256([]byte("stulp web session\x00" + s.options.Token))
	return fmt.Sprintf("%x", sum[:])
}

func (s *Server) hasAccessCookie(request *stulphttp.Request) bool {
	for _, part := range strings.Split(request.Header.Get("Cookie"), ";") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && name == accessCookie {
			want := sha256.Sum256([]byte(s.accessProof()))
			got := sha256.Sum256([]byte(value))
			return subtle.ConstantTimeCompare(want[:], got[:]) == 1
		}
	}
	return false
}

// invokeCapability is the single command path for Manage and Flows. Native
// Matter devices do not have a JavaScript app runtime, so sending them through
// the supervisor would incorrectly report com.stulp.matter as stopped.
func (s *Server) invokeCapability(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) error {
	_, err := s.invokeCapabilityDetailed(ctx, deviceID, capabilityID, value, options)
	return err
}

// invokeCapabilityDetailed keeps the normal capability boundary while letting
// callers that can present structured results (currently MCP) retain the
// durable outcome of a Scene. Manage and Flows intentionally use the simpler
// error-only wrapper above, just like they do for a physical device command.
func (s *Server) invokeCapabilityDetailed(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) (*scene.ActivationResult, error) {
	device, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if sceneID, sceneDevice := store.SceneIDFromDeviceID(device.ID); sceneDevice && device.AppID == store.NativeSceneAppID {
		if capabilityID != "onoff" {
			return nil, fmt.Errorf("scene-apparaat heeft geen capability %q", capabilityID)
		}
		on, ok := value.(bool)
		if !ok {
			return nil, errors.New("de onoff-status van een scene moet true of false zijn")
		}
		result, activationErr := s.scenes.Set(ctx, sceneID, on)
		return &result, activationErr
	}
	return nil, s.supervisor.InvokeCapability(ctx, device.ID, capabilityID, value, options)
}

// UseStatistics hangt een verzamelaar aan de API. Zonder is de route er wel en
// zegt hij dat er niets verzameld wordt -- dat is duidelijker dan een route die
// niet bestaat.
func (s *Server) UseStatistics(collector *stats.Collector) { s.stats = collector }

func (s *Server) Close() {
	s.flows.Close()
	s.pairMu.Lock()
	pairs := s.pairs
	s.pairs = make(map[string]pairSession)
	s.pairMu.Unlock()
	for id, session := range pairs {
		_ = s.supervisor.ClosePairSession(context.Background(), session.AppID, id)
	}
}

func (s *Server) routes() {
	s.handleMCP()
	s.handleSystem()
	s.handleStatistics()
	s.handleSystemSettings()
	s.handleApps()
	s.handleDrivers()
	s.handleScenes()
	s.handleFlows()
	s.handleNotifications()
	s.handleImages()
	s.handleBackup()
	s.handleDevices()
	s.handleDeviceGroups()
	s.handleMedia()
	s.handlePairing()
	s.handleAppAPI()
	s.handleAppUI()
	s.handleUI()
	s.mux.HandleFunc("GET /api/stulp/health", s.health)
	s.mux.HandleFunc("GET /api/stulp/events", s.events)
	s.mux.HandleFunc("GET /api/stulp/apps/{id}/registrations", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		registrations, err := s.supervisor.Registrations(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, registrations)
	})
}

func (s *Server) handleBackup() {
	s.mux.HandleFunc("GET /api/stulp/backup", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		response.Header().Set("Content-Disposition", `attachment; filename="stulp-backup.zip"`)
		response.Header().Set("Content-Type", "application/zip")
		// ZIP is a streaming format. Writing it straight to the response also
		// works on HopOS, where there is deliberately no process-local temp dir.
		if err := backup.Write(stulphttp.Context(request), s.store, response); err != nil {
			logger := s.options.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Error("backup download failed", "error", err)
		}
	})

	s.mux.HandleFunc("POST /api/stulp/restore", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		defer stulphttp.CloseBody(request)
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0]))
		if contentType != "application/zip" && contentType != "application/octet-stream" {
			writeError(response, stulphttp.StatusUnsupportedMediaType, errors.New("upload a Stulp .zip backup"))
			return
		}
		data, err := io.ReadAll(stulphttp.LimitBody(response, request, maxRestoreUploadBytes+1))
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "body too large") {
				writeError(response, stulphttp.StatusRequestEntityTooLarge,
					fmt.Errorf("backup is larger than the %d MiB upload limit", maxRestoreUploadBytes>>20))
				return
			}
			writeError(response, stulphttp.StatusBadRequest, fmt.Errorf("read backup upload: %w", err))
			return
		}
		if int64(len(data)) > maxRestoreUploadBytes {
			writeError(response, stulphttp.StatusRequestEntityTooLarge,
				fmt.Errorf("backup is larger than the %d MiB upload limit", maxRestoreUploadBytes>>20))
			return
		}

		// Install/uninstall and restore all replace app ownership. One lock keeps
		// those filesystem operations from crossing each other.
		s.installMu.Lock()
		defer s.installMu.Unlock()
		if err := backup.ValidateBytes(stulphttp.Context(request), data, s.store); err != nil {
			writeError(response, stulphttp.StatusUnprocessableEntity, err)
			return
		}
		s.supervisor.Pause()
		result, restoreErr := backup.RestoreBytes(stulphttp.Context(request), data, s.store)

		// Runtime state belongs to the old document. Pair sessions and in-memory
		// statistics must not leak across the boundary either.
		s.pairMu.Lock()
		s.pairs = make(map[string]pairSession)
		s.pairMu.Unlock()
		if restoreErr == nil {
			s.flows.Reset()
			if s.stats != nil {
				s.stats.Forget()
			}
			if s.images != nil {
				s.images.Forget()
			}
			if system, systemErr := s.store.System(context.Background()); systemErr == nil {
				s.applySystem(system)
			}
		}
		resumeErr := s.supervisor.Resume(context.Background())
		if restoreErr != nil {
			writeError(response, stulphttp.StatusUnprocessableEntity, restoreErr)
			return
		}
		answer := map[string]any{
			"restored": true, "document": result.Document, "appsRoot": result.AppsRoot,
			"previousDocument": result.PreviousDocument, "previousAppsRoot": result.PreviousAppsRoot,
		}
		if resumeErr != nil {
			answer["warning"] = "De backup is hersteld, maar niet iedere app kon meteen starten: " + resumeErr.Error()
		}
		writeJSON(response, stulphttp.StatusOK, answer)
	})
}

func (s *Server) handleNotifications() {
	s.mux.HandleFunc("GET /api/manager/notifications/notification", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		notifications, err := s.store.Notifications(stulphttp.Context(request), 50)
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		result := make(map[string]store.Notification, len(notifications))
		for _, notification := range notifications {
			result[notification.ID] = notification
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
	s.mux.HandleFunc("DELETE /api/manager/notifications/notification/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.store.DeleteNotification(stulphttp.Context(request), request.PathValue("id")); err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
}

func (s *Server) handleAppAPI() {
	s.mux.HandleFunc("/api/stulp/apps/{id}/api/{path...}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		app, err := s.store.App(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		handler, ok := appAPIHandler(app.Manifest, request.Method, "/"+request.PathValue("path"))
		if !ok {
			writeError(response, stulphttp.StatusNotFound, errors.New("app API route does not exist"))
			return
		}
		query := make(map[string]any)
		for key, values := range stulphttp.Query(request) {
			if len(values) == 1 {
				query[key] = values[0]
			} else {
				query[key] = values
			}
		}
		body := make(map[string]any)
		if request.Method != stulphttp.MethodGet && request.ContentLength != 0 {
			if err := decodeJSON(request, &body); err != nil {
				writeError(response, stulphttp.StatusBadRequest, err)
				return
			}
		}
		result, err := s.supervisor.InvokeAppAPI(stulphttp.Context(request), app.ID, handler, query, body)
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, err)
			return
		}
		// Een pagina van een app leest in dezelfde eenheid als de rest van het
		// huis. Een plugin zegt bij een getal welke eenheid het is -- met
		// appsdk.Measure -- en dit rekent het om. Zo staat de omrekening ook voor
		// een pagina op één plek en niet nog eens in de browser.
		writeJSON(response, stulphttp.StatusOK, s.showMeasures(result))
	})
}

func appAPIHandler(raw map[string]any, method, path string) (string, bool) {
	api, _ := raw["api"].(map[string]any)
	for handler, value := range api {
		route, _ := value.(map[string]any)
		routeMethod, _ := route["method"].(string)
		routePath, _ := route["path"].(string)
		if strings.EqualFold(routeMethod, method) && routePath == path {
			return handler, true
		}
	}
	return "", false
}

func (s *Server) handlePairing() {
	s.mux.HandleFunc("GET /api/stulp/apps/{id}/drivers/{driver}/pair/devices", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		devices, err := s.supervisor.PairListDevices(stulphttp.Context(request), request.PathValue("id"), request.PathValue("driver"))
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, devices)
	})
	s.mux.HandleFunc("POST /api/stulp/apps/{id}/drivers/{driver}/pair/devices", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var candidate map[string]any
		if err := decodeJSON(request, &candidate); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		device, err := s.supervisor.AddPairedDevice(stulphttp.Context(request), request.PathValue("id"), request.PathValue("driver"), candidate)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusCreated, s.deviceObject(device))
	})
	s.mux.HandleFunc("POST /api/stulp/pair", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			AppID    string `json:"appId"`
			DriverID string `json:"driverId"`
		}
		if err := decodeJSON(request, &body); err != nil || body.AppID == "" || body.DriverID == "" {
			if err == nil {
				err = errors.New("appId and driverId are required")
			}
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		id, err := randomID()
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		handlers, err := s.supervisor.StartPairSession(stulphttp.Context(request), body.AppID, body.DriverID, id)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		session := pairSession{AppID: body.AppID, DriverID: body.DriverID, Handlers: handlers}
		s.pairMu.Lock()
		s.pairs[id] = session
		s.pairMu.Unlock()
		writeJSON(response, stulphttp.StatusCreated, map[string]any{"id": id, "appId": body.AppID, "driverId": body.DriverID, "handlers": handlers})
	})
	s.mux.HandleFunc("POST /api/stulp/pair/{id}/emit/{event}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		session, ok := s.pair(request.PathValue("id"))
		if !ok {
			writeError(response, stulphttp.StatusNotFound, errors.New("pair session does not exist"))
			return
		}
		var data any
		if request.ContentLength != 0 {
			if err := decodeJSON(request, &data); err != nil {
				writeError(response, stulphttp.StatusBadRequest, err)
				return
			}
		}
		result, err := s.supervisor.PairEmit(stulphttp.Context(request), session.AppID, request.PathValue("id"), request.PathValue("event"), data)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
	s.mux.HandleFunc("DELETE /api/stulp/pair/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		s.pairMu.Lock()
		session, ok := s.pairs[request.PathValue("id")]
		delete(s.pairs, request.PathValue("id"))
		s.pairMu.Unlock()
		if ok {
			_ = s.supervisor.ClosePairSession(stulphttp.Context(request), session.AppID, request.PathValue("id"))
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
}

func (s *Server) pair(id string) (pairSession, bool) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	session, ok := s.pairs[id]
	return session, ok
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
}

func (s *Server) handleMedia() {
	s.mux.HandleFunc("GET /api/stulp/devices/{id}/media", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		registrations, err := s.supervisor.DeviceMedia(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, registrations)
	})
	// De stream doorgeven, niet omzetten.
	//
	// De plugin bedient hem zelf en zegt alleen waar; Stulp kopieert de bytes
	// door. Dat is met opzet geen redirect: de luisteraar van een plugin hoeft
	// niet vanaf de browser bereikbaar te zijn, en het adres erheen -- vaak met
	// een token erin -- hoort het huis niet uit.
	s.mux.HandleFunc("GET /api/stulp/devices/{id}/media/{slot}/stream", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		stream, err := s.supervisor.ResolveMedia(stulphttp.Context(request), request.PathValue("id"), request.PathValue("slot"), stulphttp.Query(request).Get("kind"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		s.pipeStream(response, request, stream)
	})
}

func (s *Server) handleSystem() {
	info := func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		writeJSON(response, stulphttp.StatusOK, map[string]any{
			"apiVersion": 3, "stulpVersion": s.options.StulpVersion,
			"language": s.options.Language, "timezone": s.options.Timezone,
			"stulpId": s.options.StulpID, "cloudId": s.options.StulpID,
			"productId": "stulp", "productName": "Stulp", "platform": runtime.GOOS,
		})
	}
	// Alleen de kale vorm: de {$}-slash-tolerantie die hiernaast stond is met
	// de leanhttp-mux mee gesneuveld (KAM: geen {$}) — nul aanroepers geteld,
	// en één vorm op beide gedaanten is eerlijker dan tolerantie op één.
	s.mux.HandleFunc("GET /api/manager/system", info)
	s.mux.HandleFunc("GET /api/manager/system/ping", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("GET /api/manager/system/name", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		writeJSON(response, stulphttp.StatusOK, "Stulp")
	})
}

func (s *Server) handleApps() {
	// Het token waarmee een app zich mag melden.
	//
	// Zonder dit endpoint is een headless Stulp niet in te richten: het token is
	// wat je een app MEEGEEFT voordat je hem neerzet (in de jobspec, in de
	// container-env), en op een node is er geen shell om `stulp attach-token` te
	// draaien. Het is per app-id afgeleid uit één geheim, dus het bestaat ook voor
	// een app die Stulp nog niet kent -- en dat is precies de app waarvoor je hem
	// komt halen.
	//
	// Wie dit mag opvragen, mag alles: het zit achter dezelfde sessie als de rest
	// van Manage. Dat is geen versoepeling, want via die sessie kun je al elk
	// apparaat in het huis bedienen.
	s.mux.HandleFunc("GET /api/stulp/attach-token/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		id := request.PathValue("id")
		secret, err := s.store.AttachSecret(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		known := true
		if _, err := s.store.App(stulphttp.Context(request), id); err != nil {
			known = false
		}
		writeJSON(response, stulphttp.StatusOK, map[string]any{
			"appId": id, "token": appproto.Token(secret, id), "known": known,
		})
	})

	// Installeren is accepteren wat zich gemeld heeft.
	//
	// Er valt hier niets te downloaden: een app komt binnen doordat iemand hem
	// neerzet (HOP plaatst een slot-image, docker start een container), waarna
	// hij zich meldt met zijn manifest en in het document staat als aangeboden.
	// Deze knop is de handeling die hij bewust NIET zelf mag doen -- anders zou
	// een gelekt token genoeg zijn om binnen te komen.
	s.mux.HandleFunc("POST /api/stulp/apps/{id}/install", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		id := request.PathValue("id")
		app, err := s.store.AcceptApp(stulphttp.Context(request), id)
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		// Niet starten. Deze app is er al -- iemand heeft hem neergezet -- en Stulp
		// heeft geen binary om te starten: er is geen bundel op schijf. Hij komt
		// terug bij zijn volgende aanmelding, en die doet hij zelf, want een
		// afgewezen app blijft het proberen (restart: always, of de retry in de
		// SDK). Wat deze knop verandert is dat hij dan wél binnenkomt.
		writeJSON(response, stulphttp.StatusOK, s.appObject(app))
	})
	s.mux.HandleFunc("GET /api/manager/apps/app", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		apps, err := s.store.Apps(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		result := make(map[string]any, len(apps))
		for _, app := range apps {
			result[app.ID] = s.appObject(app)
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
	s.mux.HandleFunc("GET /api/manager/apps/app/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		app, err := s.store.App(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.appObject(app))
	})
	// Er is geen bron meer om een nieuwere versie aan te vragen. Een app komt
	// binnen doordat iemand hem neerzet, dus IS de versie die zich meldt de
	// nieuwste die er is: updaten is een nieuw image plaatsen, en dat gebeurt
	// buiten Stulp. Dit endpoint zegt dat, in plaats van te verdwijnen en Manage
	// op een 404 te laten lopen.
	s.mux.HandleFunc("GET /api/manager/apps/app/{id}/update", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		app, err := s.store.App(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, map[string]any{
			"version":         app.Version,
			"updateAvailable": false,
			"reason":          "an app is updated by placing a new image; it announces its version when it attaches",
		})
	})
	s.mux.HandleFunc("DELETE /api/manager/apps/app/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		// The same lock the installer holds: an uninstall racing an install of
		// the same app would leave a registered app without its sources.
		s.installMu.Lock()
		defer s.installMu.Unlock()
		app, devices, err := s.supervisor.Uninstall(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		deviceIDs := make([]string, 0, len(devices))
		for _, device := range devices {
			deviceIDs = append(deviceIDs, device.ID)
		}
		result := map[string]any{
			"id": app.ID, "name": localized(app.Manifest["name"], s.options.Language),
			"devices": len(devices), "flows": 0,
		}
		// The app is gone from the database either way. What follows is
		// cleanup, so a failure is reported as a warning instead of an error
		// that would suggest nothing happened.
		var warnings []string
		disabled, flowErr := s.store.DisableFlowsFor(stulphttp.Context(request), app, deviceIDs)
		result["flows"] = len(disabled)
		if flowErr != nil {
			warnings = append(warnings, "niet alle Flows konden worden uitgeschakeld: "+flowErr.Error())
		}
		if _, removeErr := appinstall.RemoveBundle(s.appsRoot, app.Root); removeErr != nil {
			warnings = append(warnings, "de app is verwijderd, maar de bestanden bleven staan: "+removeErr.Error())
		}
		if len(warnings) > 0 {
			result["warning"] = strings.Join(warnings, " · ")
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
	s.mux.HandleFunc("GET /api/manager/apps/app/{id}/setting", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		settings, err := s.store.Settings(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, settings)
	})
	s.mux.HandleFunc("GET /api/manager/apps/app/{id}/setting/{name}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		value, exists, err := s.store.Setting(stulphttp.Context(request), request.PathValue("id"), request.PathValue("name"))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		if !exists {
			writeError(response, stulphttp.StatusNotFound, errors.New("setting does not exist"))
			return
		}
		writeJSON(response, stulphttp.StatusOK, value)
	})
	s.mux.HandleFunc("PUT /api/manager/apps/app/{id}/setting/{name}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body map[string]any
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		value, exists := body["value"]
		if !exists {
			writeError(response, stulphttp.StatusBadRequest, errors.New("value is required"))
			return
		}
		if err := s.supervisor.SetAppSetting(stulphttp.Context(request), request.PathValue("id"), request.PathValue("name"), value); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("DELETE /api/manager/apps/app/{id}/setting/{name}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.supervisor.UnsetAppSetting(stulphttp.Context(request), request.PathValue("id"), request.PathValue("name")); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("PUT /api/manager/apps/app/{id}/enable", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.supervisor.Enable(stulphttp.Context(request), request.PathValue("id")); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("PUT /api/manager/apps/app/{id}/disable", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.supervisor.Disable(stulphttp.Context(request), request.PathValue("id")); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("POST /api/manager/apps/app/{id}/restart", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.supervisor.Restart(stulphttp.Context(request), request.PathValue("id")); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("GET /api/manager/apps/app/{id}/locale", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		app, err := s.store.App(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, map[string]any{
			"language": s.options.Language,
			"name":     localized(app.Manifest["name"], s.options.Language),
			"manifest": app.Manifest,
		})
	})
}

func (s *Server) restoreApp(ctx context.Context, previous store.App) error {
	previousManifest, _, err := manifest.Load(previous.Root)
	if err != nil {
		return err
	}
	if err := s.store.InstallApp(ctx, previousManifest, previous.Root, previous.Source); err != nil {
		return err
	}
	if previous.Enabled {
		return s.supervisor.Restart(ctx, previous.ID)
	}
	if err := s.supervisor.Stop(previous.ID); err != nil {
		return err
	}
	return s.store.SetAppEnabled(ctx, previous.ID, false)
}

func (s *Server) handleDrivers() {
	s.mux.HandleFunc("GET /api/manager/drivers/driver", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		apps, err := s.store.Apps(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		result := make(map[string]any)
		for _, app := range apps {
			// De store heeft het manifest al uit zijn echte bron gelezen: uit
			// app.json bij een bundel, uit het document bij een app die zichzelf
			// aanmeldde. De weblaag hoort daar niet alsnog een gedeelde map naast
			// te veronderstellen.
			appManifest, loadErr := manifest.FromRaw(app.Manifest)
			if loadErr != nil {
				continue
			}
			for _, driver := range appManifest.Drivers {
				id := driverID(app.ID, driver.ID)
				result[id] = s.driverObject(app, driver)
			}
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
	s.mux.HandleFunc("GET /api/manager/drivers/driver/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		app, driver, err := s.findDriver(request.PathValue("id"), stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.driverObject(app, driver))
	})
}

func (s *Server) handleDevices() {
	s.mux.HandleFunc("GET /api/manager/devices/device", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		devices, err := s.store.Devices(stulphttp.Context(request), "")
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		result := make(map[string]any, len(devices))
		for _, device := range devices {
			result[device.ID] = s.deviceObject(device)
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
	s.mux.HandleFunc("GET /api/manager/devices/device/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		device, err := s.store.Device(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.deviceObject(device))
	})
	s.mux.HandleFunc("GET /api/manager/devices/device/{id}/capability/{capability}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		device, err := s.store.Device(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		capability := request.PathValue("capability")
		for _, id := range device.Capabilities {
			if id == capability {
				writeJSON(response, stulphttp.StatusOK, device.State[capability])
				return
			}
		}
		writeError(response, stulphttp.StatusNotFound, fmt.Errorf("device has no capability %q", capability))
	})
	s.mux.HandleFunc("PUT /api/manager/devices/device/{id}/capability/{capability}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body map[string]any
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		value, exists := body["value"]
		if !exists {
			writeError(response, stulphttp.StatusBadRequest, errors.New("value is required"))
			return
		}
		options, _ := body["opts"].(map[string]any)
		if options == nil {
			options = map[string]any{}
		}
		device, err := s.store.Device(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		// Wat hier binnenkomt is getypt in de eenheid van de gebruiker: 70 in een
		// huis dat Fahrenheit leest is 21,1 °C voor het apparaat.
		capability := request.PathValue("capability")
		value = s.canonicalCapabilityValue(stulphttp.Context(request), device, capability, value)
		err = s.invokeCapability(stulphttp.Context(request), device.ID, capability, value, options)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("PUT /api/manager/devices/device/{id}/settings", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var patch map[string]any
		if err := decodeJSON(request, &patch); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		storedDevice, err := s.store.Device(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		device, err := s.supervisor.UpdateDeviceSettings(stulphttp.Context(request), storedDevice.ID, patch)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.deviceObject(device))
	})
	s.mux.HandleFunc("PUT /api/manager/devices/device/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var patch map[string]any
		if err := decodeJSON(request, &patch); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		device, err := s.store.Device(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		if name, ok := patch["name"].(string); ok {
			name = strings.TrimSpace(name)
			if name == "" {
				writeError(response, stulphttp.StatusBadRequest, errors.New("device name is required"))
				return
			}
			if name != device.Name {
				if sceneID, sceneDevice := store.SceneIDFromDeviceID(device.ID); sceneDevice && device.AppID == store.NativeSceneAppID {
					definition, sceneErr := s.store.Scene(stulphttp.Context(request), sceneID)
					if sceneErr == nil {
						definition.Name = name
						_, sceneErr = s.store.UpdateScene(stulphttp.Context(request), definition)
					}
					if sceneErr == nil {
						device, sceneErr = s.store.Device(stulphttp.Context(request), device.ID)
					}
					err = sceneErr
				} else {
					device, err = s.supervisor.RenameDevice(stulphttp.Context(request), device.ID, name)
				}
				if err != nil {
					status := stulphttp.StatusBadRequest
					if errors.Is(err, store.ErrSceneActive) {
						status = stulphttp.StatusConflict
					}
					writeError(response, status, err)
					return
				}
			}
		}
		metadataChanged := false
		for _, property := range []string{"zone", "note", "iconOverride", "virtualClass", "uiIndicator", "hidden"} {
			if value, exists := patch[property]; exists {
				device.Store["__stulp.api."+property] = value
				metadataChanged = true
			}
		}
		if metadataChanged {
			if err := s.store.UpdateDevice(stulphttp.Context(request), device); err != nil {
				writeError(response, stulphttp.StatusBadRequest, err)
				return
			}
		}
		writeJSON(response, stulphttp.StatusOK, s.deviceObject(device))
	})
	s.mux.HandleFunc("DELETE /api/manager/devices/device/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		device, err := s.store.Device(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		if sceneID, sceneDevice := store.SceneIDFromDeviceID(device.ID); sceneDevice && device.AppID == store.NativeSceneAppID {
			err = s.deleteScene(stulphttp.Context(request), sceneID)
		} else {
			err = s.supervisor.DeleteDevice(stulphttp.Context(request), device.ID)
		}
		if err != nil {
			status := stulphttp.StatusBadRequest
			if errors.Is(err, store.ErrSceneActive) || errors.Is(err, store.ErrSceneInUse) {
				status = stulphttp.StatusConflict
			}
			writeError(response, status, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("GET /api/manager/devices/capability", s.capabilities)
}

func (s *Server) health(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	// stulpVersion: Manage leest dit veld al sinds de eerste dag ("de
	// controller zelf · …") maar het werd nooit meegegeven — de regel bleef
	// leeg staan. Dezelfde waarde als de X-Stulp-Version-kop, maar de UI hoort
	// niet in koppen te hoeven graven.
	writeJSON(response, stulphttp.StatusOK, map[string]any{
		"ok": true, "runtime": "go", "store": "json",
		"stulpVersion": s.options.StulpVersion,
	})
}

func (s *Server) events(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("Connection", "keep-alive")
	// Done VÓÓR de eerste write claimen: de wachthond is de eigenaar van de
	// leeskant én de kop moet "Connection: close" kunnen zeggen — leanhttp
	// dwingt dat sinds review 13-08 (tweeëndertigste ronde) fail-fast af.
	// De flush-toets die hier eerst BOVEN de headers stond flushte bovendien
	// echt, in beide gedaanten: de kop ging zonder Content-Type de deur uit
	// en de browser zag geen event-stream. De preamble-flush hieronder ís de
	// kan-deze-writer-streamen-toets.
	done := stulphttp.Done(request)
	events, cancel := s.store.Subscribe(64)
	defer cancel()
	_, _ = fmt.Fprint(response, ": stulp events\n\n")
	if !stulphttp.Flush(response) {
		return // deze writer kan niet streamen; er valt niets meer te zeggen
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			if !deliverToStream(event, stulphttp.Query(request).Get("manager")) {
				continue
			}
			event = s.realtimeEvent(event)
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(response, "event: %s\ndata: %s\n\n", event.Type, data)
			stulphttp.Flush(response)
		case <-ticker.C:
			_, _ = fmt.Fprint(response, ": keepalive\n\n")
			stulphttp.Flush(response)
		case <-done:
			return
		}
	}
}

// deliverToStream reports whether an event belongs on a stream that asked for
// one manager. The reload marker is the exception: it belongs to no manager
// because it does not describe a change, it says this stream lost events. A
// filtered client needs to hear that as much as an unfiltered one.
func deliverToStream(event store.Event, manager string) bool {
	return manager == "" || manager == event.Manager || event.Manager == "store"
}

// realtimeEvent keeps SSE a transport rather than a second read protocol.
// A device update already carries the store-owned snapshot; enrich that same
// value into the Manage/API shape so the browser never has to GET it again.
//
// Only updates. A newly created device is not on the page yet, so the browser
// has to reload to place it in its group anyway, and enriching it here would be
// work thrown away.
func (s *Server) realtimeEvent(event store.Event) store.Event {
	if event.Manager != "devices" || event.Type != "device.update" {
		return event
	}
	if device, ok := event.Data.(store.Device); ok {
		event.Data = s.deviceObject(device)
	}
	return event
}

func (s *Server) appObject(app store.App) map[string]any {
	state := s.supervisor.State(app.ID)
	name := localized(app.Manifest["name"], s.options.Language)
	author := app.Manifest["author"]
	if author == nil {
		author = map[string]any{"name": "Unknown"}
	}
	return map[string]any{
		"id": app.ID, "name": name, "version": app.Version,
		"compatibility": valueutil.String(app.Manifest["compatibility"]),
		"permissions":   app.Manifest["permissions"], "author": author,
		"state": state.State, "enabled": app.Enabled, "crashed": state.State == "crashed",
		"crashedMessage": state.Error, "retryAt": state.RetryAt,
		"settings": appHasUIAsset(app, "settings/index.html") || app.Manifest["settings"] != nil,
		"channel":  "live", "origin": "devkit_install", "autoupdate": false,
		// offered: deze app heeft zich gemeld en wacht tot iemand hem
		// installeert. Hij draait niet, dus de interface hoort hem ook niet als
		// "uitgeschakeld" te tonen -- dat is iets anders.
		"offered":      app.Offered,
		"source":       app.Source,
		"crashedCount": state.RestartCount,
		"usage":        map[string]any{"cpu": 0, "mem": 0},
	}
}

func (s *Server) driverObject(app store.App, driver manifest.DriverManifest) map[string]any {
	customPairViews := make([]string, 0)
	for _, raw := range driver.Pair {
		view, _ := raw.(map[string]any)
		id, _ := view["id"].(string)
		if id == "" {
			continue
		}
		if appHasUIAsset(app, path.Join("drivers", driver.ID, "pair", id+".html")) {
			customPairViews = append(customPairViews, id)
		}
	}
	return map[string]any{
		"id": driverID(app.ID, driver.ID), "ownerUri": "stulp:app:" + app.ID,
		"ownerName": localized(app.Manifest["name"], s.options.Language),
		"name":      driver.Name.Resolve(s.options.Language), "class": driver.Class,
		"ready": s.supervisor.State(app.ID).State == "running", "pair": len(driver.Pair) > 0,
		"repair": false, "unpair": true, "deprecated": false, "connectivity": "local",
		"pairViews": driver.Pair, "settings": driver.Settings, "capabilities": driver.Capabilities,
		"customPairViews": customPairViews,
	}
}

func (s *Server) deviceObject(device store.Device) map[string]any {
	capabilities := make(map[string]any, len(device.Capabilities))
	for _, id := range device.Capabilities {
		capabilities[id] = s.capabilityObject(device, id, device.State[id])
	}
	result := map[string]any{
		"id": device.ID, "appId": device.AppID, "driverId": driverID(device.AppID, device.DriverID),
		"groupId": device.GroupID, "sortOrder": device.SortOrder,
		"ownerUri": "stulp:app:" + device.AppID, "name": device.Name, "hardwareName": device.HardwareName(),
		"note": metadata(device, "note", ""),
		"zone": metadata(device, "zone", "stulp-home"), "data": device.Data, "class": device.Class,
		"manufacturer": s.deviceManufacturer(device),
		"iconOverride": metadata(device, "iconOverride", nil), "virtualClass": metadata(device, "virtualClass", nil),
		"uiIndicator": metadata(device, "uiIndicator", nil), "hidden": metadata(device, "hidden", false),
		"capabilities": device.Capabilities, "capabilitiesObj": capabilities,
		"settings": device.Settings, "settingsObj": map[string]any{},
		"available": device.Available, "unavailableMessage": device.Message,
		"warningMessage": device.Store["__stulp.warning"], "energy": device.Store["__stulp.energy"],
		"ready": true, "repair": false, "unpair": true, "flags": []string{}, "ui": map[string]any{},
	}
	return result
}

func (s *Server) deviceManufacturer(device store.Device) string {
	if device.AppID == store.NativeSceneAppID {
		return "Stulp"
	}
	for _, values := range []map[string]any{device.Store, device.Data} {
		if value, _ := values["manufacturer"].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	app, err := s.store.App(context.Background(), device.AppID)
	if err != nil {
		return device.AppID
	}
	if value := localized(app.Manifest["manufacturer"], s.options.Language); value != "" {
		return value
	}
	if value := localized(app.Manifest["name"], s.options.Language); value != "" {
		return value
	}
	return device.AppID
}

func (s *Server) capabilityObject(device store.Device, id string, value any) map[string]any {
	result := map[string]any{
		"id": id, "value": value, "type": capabilityType(id, value), "lastUpdated": nil,
		"getable": true, "setable": defaultCapabilitySetable(id), "title": capabilityDisplayTitle(id, nil, s.options.Language),
	}
	applyDefaultCapabilityMetadata(result, id)
	app, err := s.store.App(context.Background(), device.AppID)
	if err != nil {
		// De eenheid van de kern is er ook zonder app -- een tegel hoort niet in
		// Celsius te blijven staan omdat het manifest even niet te lezen was.
		s.showUnits(result)
		return result
	}
	definitions, _ := app.Manifest["capabilities"].(map[string]any)
	definition, _ := definitions[id].(map[string]any)
	for _, key := range []string{"type", "getable", "setable", "title", "desc", "values", "min", "max", "step", "units"} {
		if definition[key] != nil {
			result[key] = definition[key]
		}
	}
	// Als laatste, want pas nu staat de eenheid vast die de app bedoelde.
	s.showUnits(result)
	return result
}

func applyDefaultCapabilityMetadata(result map[string]any, id string) {
	id, _, _ = strings.Cut(id, ".")
	switch id {
	case "dim", "light_hue", "light_saturation", "windowcoverings_set", "volume_set":
		result["type"], result["min"], result["max"], result["step"] = "number", 0.0, 1.0, 0.01
	case "windowcoverings_state":
		// De drie standen die een zonwering kent. Zonder deze lijst weet de
		// interface niet wat er te kiezen valt en biedt ze een tekstveld aan
		// waar je "up" in moet typen.
		result["type"] = "enum"
		result["values"] = []any{
			map[string]any{"id": "up", "title": map[string]any{"nl": "Omhoog", "en": "Up"}},
			map[string]any{"id": "idle", "title": map[string]any{"nl": "Stop", "en": "Stop"}},
			map[string]any{"id": "down", "title": map[string]any{"nl": "Omlaag", "en": "Down"}},
		}
	case "measure_temperature", "target_temperature":
		result["type"], result["step"], result["units"] = "number", 0.01, "°C"
	case "measure_humidity", "measure_battery":
		result["type"], result["min"], result["max"], result["step"], result["units"] = "number", 0.0, 100.0, 0.01, "%"
	case "measure_pressure":
		result["type"], result["step"], result["units"] = "number", 0.1, "hPa"
	case "measure_luminance":
		result["type"], result["min"], result["step"], result["units"] = "number", 0.0, 1.0, "lx"
	case "measure_power":
		result["type"], result["step"], result["units"] = "number", 0.001, "W"
	case "measure_voltage":
		result["type"], result["min"], result["step"], result["units"] = "number", 0.0, 0.001, "V"
	case "measure_current":
		result["type"], result["min"], result["step"], result["units"] = "number", 0.0, 0.001, "A"
	case "meter_power":
		result["type"], result["min"], result["step"], result["units"] = "number", 0.0, 0.001, "kWh"
	}
}

func defaultCapabilitySetable(id string) bool {
	id, _, _ = strings.Cut(id, ".")
	// windowcoverings_state eindigt op _state en niet op _set, maar het is wel
	// degelijk een bediening: het is de richtingknop van een zonwering. Zonder
	// deze regel toont de interface hem als een waarde die je alleen kunt lezen.
	if id == "onoff" || id == "dim" || id == "locked" || id == "light_hue" || id == "light_saturation" ||
		id == "volume_set" || id == "speaker_playing" || id == "windowcoverings_state" {
		return true
	}
	return strings.HasSuffix(id, "_set") || strings.HasPrefix(id, "target_")
}

func (s *Server) capabilities(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	devices, err := s.store.Devices(stulphttp.Context(request), "")
	if err != nil {
		writeError(response, stulphttp.StatusInternalServerError, err)
		return
	}
	result := make(map[string]any)
	for _, device := range devices {
		for _, id := range device.Capabilities {
			if _, exists := result[id]; !exists {
				result[id] = map[string]any{"id": id, "uri": "stulp:manager:devices", "type": capabilityType(id, device.State[id]), "getable": true, "setable": true}
			}
		}
	}
	writeJSON(response, stulphttp.StatusOK, result)
}

func (s *Server) findDriver(id string, ctx context.Context) (store.App, manifest.DriverManifest, error) {
	trimmed := strings.TrimPrefix(id, "stulp:app:")
	separator := strings.LastIndex(trimmed, ":")
	if separator < 1 || separator == len(trimmed)-1 {
		return store.App{}, manifest.DriverManifest{}, fmt.Errorf("driver %q does not exist", id)
	}
	appID, manifestDriverID := trimmed[:separator], trimmed[separator+1:]
	app, err := s.store.App(ctx, appID)
	if err != nil {
		return store.App{}, manifest.DriverManifest{}, err
	}
	appManifest, err := manifest.FromRaw(app.Manifest)
	if err != nil {
		return store.App{}, manifest.DriverManifest{}, err
	}
	driver, ok := appManifest.Driver(manifestDriverID)
	if !ok {
		return store.App{}, manifest.DriverManifest{}, fmt.Errorf("driver %q does not exist", id)
	}
	return app, driver, nil
}

func decodeJSON(request *stulphttp.Request, target any) error {
	defer stulphttp.CloseBody(request)
	decoder := json.NewDecoder(stulphttp.LimitBody(nil, request, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON body: %w", err)
	}
	return nil
}

func writeJSON(response stulphttp.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(emptyNotNull(value))
}

// emptyNotNull maakt van een lege lijst of map een [] of {} in plaats van null.
//
// Een nil-slice in Go wordt `null` in JSON, en dat is geen leeg antwoord maar een
// ánder antwoord: elke aanroeper moet er dan apart op letten. GEMETEN 12-08 op
// een verse node, waar het hele Manage-scherm leeg bleef met "Cannot convert
// undefined or null to object" -- Object.values(null) op een huis dat nog geen
// apparaatgroepen had.
//
// Het hoort hier en niet bij elke handler: dit is de plek waar élk antwoord
// langskomt, en een regel per handler is een regel die iemand vergeet.
func emptyNotNull(value any) any {
	if value == nil {
		return struct{}{}
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Slice, reflect.Map:
		if v.IsNil() {
			if v.Kind() == reflect.Slice {
				return []any{}
			}
			return map[string]any{}
		}
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return struct{}{}
		}
	}
	return value
}

func writeError(response stulphttp.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]any{"error": err.Error(), "error_description": err.Error()})
}

func localized(value any, language string) string {
	switch value := value.(type) {
	case string:
		return value
	case map[string]any:
		if result, ok := value[language].(string); ok && result != "" {
			return result
		}
		if result, ok := value["en"].(string); ok && result != "" {
			return result
		}
		for _, candidate := range value {
			if result, ok := candidate.(string); ok {
				return result
			}
		}
	}
	return ""
}

func capabilityType(id string, value any) string {
	switch value.(type) {
	case bool:
		return "boolean"
	case float64, float32, int, int64, json.Number:
		return "number"
	}
	if id == "onoff" || id == "locked" || strings.HasPrefix(id, "alarm_") {
		return "boolean"
	}
	if id == "dim" || strings.HasPrefix(id, "measure_") || strings.HasPrefix(id, "meter_") || strings.HasPrefix(id, "target_") {
		return "number"
	}
	return "string"
}

func driverID(appID, manifestDriverID string) string {
	return "stulp:app:" + appID + ":" + manifestDriverID
}

func metadata(device store.Device, name string, fallback any) any {
	if value, exists := device.Store["__stulp.api."+name]; exists {
		return value
	}
	return fallback
}
