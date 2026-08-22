//go:build tamago

// Stulp op HopOS: de controller als slot-app op een bare-metal node.
//
// Er is geen besturingssysteem onder deze binary. Geen shell die argumenten
// meegeeft, geen signalen, geen bestandssysteem in het proces, en vooral: geen
// fork. Dat laatste bepaalt de hele vorm, want Stulp start zijn apps normaal
// zelf met fork+exec en een socketpair. Op een node kan dat niet, en het hoeft
// ook niet: HOP plaatst elke app als eigen slot met een eigen IP, en die app
// meldt zich over de attach-poort met zijn token. Dat pad bestond al voor apps
// in een pod; hier is het de énige weg naar binnen, en dat maakt de isolatie
// strenger dan op een host — een app kan niet eens bij het bestandssysteem van
// een ander, want er is er geen.
//
// Wat HOP hier zet (jobspec):
//
//	ER_PORT_HTTP    de gepubliceerde poort voor Manage en de API; bind DIE
//	ER_PORT_ATTACH  de poort waarop apps zich mogen melden
//	HOPOS_HOST      het node-IP waarop die poorten van buiten open staan
//	STULP_TOKEN     toegangssleutel voor /<key> en /mcp/<key>
//	STULP_DOCUMENT  naam van het document in het app-volume (default stulp.json)
//
// Jobspec, met de LicheeRV erbij (riscv64):
//
//	{"name":"stulp","driver":"hop","memory_limit":100663296,
//	 "ports":{"http":80,"attach":7000},
//	 "artifacts":[{"url":".../stulp-riscv64-tamago.elf","match":{"node.arch":"riscv64"}},
//	              {"url":".../stulp-arm64-tamago.elf","match":{"node.arch":"arm64"}}]}
//
// Alle zichtbare tekst is Engels: het is een console die een operator leest.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/lean/leanhttp"

	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"

	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/stats"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
	"github.com/xinix00/stulp/internal/webapi"
)

// defaults for what the jobspec did not set. The HTTP fallback is 8080 and not
// 80 so an image that runs without a published port (someone testing by hand)
// still comes up instead of failing on a privileged bind it cannot explain.
const (
	defaultHTTPPort   = "8080"
	defaultAttachPort = "7000"
	defaultDocument   = "stulp.json"
)

func main() {
	app := applib.Init() // first line: READY + heartbeat + kill flag

	ip, err := appnet.Up(app) // own TCP/IP stack, own IP
	if err != nil {
		app.Logf("stulp: net: %v", err)
		app.Exit(1)
	}

	// The document lives on a volume the kernel owns; this is the only way an app
	// reaches it. Injected before Open, because Open reads it right away.
	//
	// Not every board has a volume. Storage on HopOS is NVMe over PCIe, and a
	// board without a PCIe window (a LicheeRV Nano) has none at all — every file
	// call answers "no storage layer on board", and so does the object store,
	// because Pull writes to that same volume. MEASURED 12-08 on a LicheeRV: this
	// is what made stulp fail five times in a row, three seconds in.
	//
	// So it falls back to memory and says so, loudly, every time it saves. A
	// controller that forgets the house on a reboot is nearly useless — but
	// nearly useless and honest beats a restart loop, because this way the page
	// comes up and you can see what the node CAN do.
	documentName := envOr(app, "STULP_DOCUMENT", defaultDocument)
	files := store.NewABFileStore(volumeFiles{app}, documentName)
	if _, err := app.Stat(documentName); err != nil {
		switch {
		case storageMissing(err):
			app.Logf("stulp: WARNING no storage on this board (%v) -- nothing will be remembered "+
				"across a reboot. Give this node a volume, or run stulp on a board that has one.", err)
			files = newMemoryFiles()
		case errors.Is(err, fs.ErrNotExist):
			// A fresh A/B installation deliberately leaves the legacy name absent,
			// so this is not enough to call it a first run.
			app.Logf("stulp: document %s: no legacy file; checking A/B slots", documentName)
		default:
			// Open below performs the authoritative read and reports the useful
			// path-specific error. Keep this probe visible as extra diagnosis.
			app.Logf("stulp: document %s: volume probe: %v", documentName, err)
		}
	}
	store.UseFileStore(files)
	database, err := store.Open(documentName)
	if err != nil {
		app.Logf("stulp: document %s: %v", documentName, err)
		app.Exit(1)
	}
	defer database.Close()

	// Het attach-geheim uit de jobspec, als het document er nog geen heeft.
	// Zonder volume is het document per boot vers en zou elk token bij een
	// reboot breken; met dit zaad blijven tokens uit de startup-file geldig
	// (en kan een bundel ze zelf afleiden). Een document mét geheim wint.
	if secret := app.Env("STULP_ATTACH_SECRET"); secret != "" {
		if err := database.SeedAttachSecret(context.Background(), secret); err != nil {
			app.Logf("stulp: attach secret seed: %v", err)
		}
	}

	// Netstack-tellers in het task-log (spin/stilte-jacht 15-08): als Manage
	// zwijgt terwijl de task leeft, zeggen deze tellers of de pot leeg is.
	appnet.WatchStats(app.Logf, 20*time.Second)

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(consoleWriter{app}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	options := plugin.Options{
		Language: "nl", Timezone: "UTC",
		StulpID: "stulp-hopos", StulpVersion: version, Logger: logger,
	}
	apps := supervisor.New(database, options)
	defer apps.Close()

	images := imageshare.New()
	apps.UseImages(images)

	statistics := stats.New()
	defer statistics.Close()
	if system, err := database.System(ctx); err == nil && system.Statistics {
		statistics.Start(database)
	}

	// The attach door opens before any app is asked to start: an app marked
	// external goes straight to waiting, and then the door should already be
	// open rather than a second later.
	//
	// No TLS here, and that is a choice with a reason rather than a shortcut.
	// The token still proves who is knocking (a nonce out, an HMAC back, both
	// ways), and this port lives on the node's internal network, which HOP
	// isolates per slot — an app can only reach Stulp because the switch was
	// told to let it. What TLS would add is secrecy against something already
	// inside that network, and there is nothing there to hide from that could
	// not equally well read the memory of the slot it lives in. Publishing this
	// port outside the node is what would make it wrong, so do not.
	attachAddress := net.JoinHostPort("", envOr(app, "ER_PORT_ATTACH", defaultAttachPort))
	attachListener, err := net.Listen("tcp", attachAddress)
	if err != nil {
		app.Logf("stulp: attach port %s: %v", attachAddress, err)
		app.Exit(1)
	}
	defer attachListener.Close()
	go func() {
		if err := apps.ServeAttach(attachListener); err != nil {
			app.Logf("stulp: attach listener stopped: %v", err)
		}
	}()
	app.Logf("stulp: apps may attach on %s:%s (token per app, no TLS on the node network)",
		ip, envOr(app, "ER_PORT_ATTACH", defaultAttachPort))

	// StartAll will refuse to start any app that is not marked external, and it
	// says why: there is no fork on this platform. That refusal is the correct
	// outcome, not a failure to work around — on a node an app is a slot image
	// that HOP places, and it attaches over the port above.
	if err := apps.StartAll(ctx); err != nil {
		app.Logf("stulp: not every app could be started here: %v", err)
	}

	api := webapi.New(database, apps, webapi.Options{
		StulpID: options.StulpID, StulpVersion: options.StulpVersion,
		Language: options.Language, Timezone: options.Timezone,
		Token: app.Env("STULP_TOKEN"), Logger: logger,
	})
	api.UseStatistics(statistics)
	api.UseImages(images)
	defer api.Close()

	httpPort := envOr(app, "ER_PORT_HTTP", defaultHTTPPort)
	listener, err := net.Listen("tcp", net.JoinHostPort("", httpPort))
	if err != nil {
		app.Logf("stulp: http port %s: %v", httpPort, err)
		app.Exit(1)
	}
	app.Logf("stulp %s: Manage and MCP on http://%s:%s (node %s)",
		version, ip, httpPort, app.Env("HOPOS_HOST"))
	// leanhttp begrenst de verzoekkop en de body zelf; er is hier geen
	// slowloris-wacht in te stellen omdat er ook geen publiek internet aan deze
	// poort hangt -- wat er binnenkomt, komt via HOP's switch.
	if err := leanhttp.Serve(listener, api.Handler()); err != nil {
		app.Logf("stulp: http server stopped: %v", err)
		app.Exit(1)
	}
}

// envOr reads a jobspec value, falling back when HOP did not publish it.
func envOr(app *applib.App, key, fallback string) string {
	if v := app.Env(key); v != "" {
		return v
	}
	return fallback
}

// volumeFiles is the document's raw home on a node: the kernel owns the volume
// and an app asks it over RPC. applib truncates first and then writes in chunks,
// so WriteFile itself is not crash-atomic. main wraps this backend in the
// store's A/B file store before opening the live document.
type volumeFiles struct{ app *applib.App }

func (v volumeFiles) ReadFile(path string) ([]byte, error) { return v.app.ReadFile(path) }

func (v volumeFiles) WriteFile(path string, data []byte) error {
	return v.app.WriteFile(path, data)
}

// storageMissing herkent het antwoord van een node zonder volume. De hop-ABI
// draagt geen statuscode voor "geen storage", alleen de tekst van de kern, dus
// staat die hier -- en als hij verandert, valt Stulp terug op memory in plaats
// van op een herstartlus, wat de veilige kant van deze gok is.
func storageMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no storage layer")
}

// memoryFiles is het document in RAM: alles vergeten bij een herstart. Alleen
// voor een board zonder volume, en dan met de waarschuwing hierboven.
type memoryFiles struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMemoryFiles() *memoryFiles { return &memoryFiles{files: map[string][]byte{}} }

func (m *memoryFiles) ReadFile(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[path]
	if !ok {
		return nil, fs.ErrNotExist // eerste start: precies wat de store verwacht
	}
	return data, nil
}

func (m *memoryFiles) WriteFile(path string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = append([]byte(nil), data...)
	return nil
}

// consoleWriter sends slog's lines to the node console. Every line costs a
// busy-wait per byte on some boards, so this is for events and not for a data
// path — the logger runs at info level for that reason.
type consoleWriter struct{ app *applib.App }

func (c consoleWriter) Write(p []byte) (int, error) {
	c.app.Logf("%s", trimNewline(p))
	return len(p), nil
}

func trimNewline(p []byte) []byte {
	for len(p) > 0 && (p[len(p)-1] == '\n' || p[len(p)-1] == '\r') {
		p = p[:len(p)-1]
	}
	return p
}
