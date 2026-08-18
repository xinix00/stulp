//go:build tamago

package appsdk

// RunNodeBundle is RunNode voor een bundel: één slot-app die meerdere plugins
// draagt. Eén applib.Init, één netstack, één heap — en per plugin een eigen
// attach-lus met een eigen identiteit, zodat Stulp exact hetzelfde ziet als
// bij losse apps (eigen announce, eigen install, eigen sessie).
//
// De identiteit komt uit het manifest van de plugin zelf (app.json draagt de
// id al); env is alleen wat werkelijk gedeeld wordt: STULP_ATTACH (waar Stulp
// woont) en STULP_TOKENS (JSON, id → token — tokens zijn per app-id, dus de
// bundel krijgt ze als één map mee in de jobspec).
//
// Waarom een bundel: op een node betaalt élke losse app ~12,5MB runtime-tax
// plus een kopie van dezelfde basis (runtime/netstack/sdk). Negen pollers
// naast elkaar = ~7× overhead op de inhoud (gemeten 18-08, LicheeRV). In de
// bundel delen ze die basis en de heap; de prijs is gedeeld lot (één fatal
// raakt ze allemaal). Stulp zelf blijft altijd een eigen proces.

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"
)

// BundleApp is één plugin in een bundel: naam (voor de log), het meegebakken
// manifest en de plugin-beschrijving zelf.
type BundleApp struct {
	Name     string
	Manifest []byte
	Plugin   Plugin
}

func RunNodeBundle(apps []BundleApp) {
	app := applib.Init() // eerste regel: READY + heartbeat + kill-flag
	app.Logf("bundle: init done (%d plugins), bringing the netstack up", len(apps))

	if _, err := appnet.Up(app); err != nil {
		app.Logf("bundle: net: %v", err)
		app.Exit(1)
	}
	target := app.Env("STULP_ATTACH")
	if target == "" {
		app.Logf("bundle: STULP_ATTACH is empty -- put stulp's address in the jobspec env")
		app.Exit(1)
	}
	tokens := map[string]string{}
	if raw := app.Env("STULP_TOKENS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
			app.Logf("bundle: STULP_TOKENS is not valid JSON: %v", err)
			app.Exit(1)
		}
	}

	for _, b := range apps {
		id := manifestID(b.Manifest)
		if id == "" {
			app.Logf("bundle: %s: manifest carries no id, skipping", b.Name)
			continue
		}
		config := AttachConfig{
			Target: target,
			AppID:  id,
			Token:  tokens[id],
			// Geen TLS op het node-netwerk: het token bewijst wie er
			// aanklopt (nonce heen, HMAC terug) — zie examples/virtual.
			Plaintext: true,
			Manifest:  b.Manifest,
		}
		app.Logf("bundle: %s: attaching to stulp at %s as %s", b.Name, target, id)
		go attachForever(app, b.Name, config, b.Plugin)
	}

	// WACHTEN, niet terugkeren: de lussen hierboven zijn de app. De kill-vlag
	// van HOP blijft de enige echte exit (applib regelt die).
	select {}
}

// attachForever is de wacht-niet-sterf-lus van RunNode, per plugin: refused
// of weggevallen betekent opnieuw aanmelden, nooit exiten — een exit zou de
// hele bundel herstarten om één plugin.
func attachForever(app *applib.App, name string, config AttachConfig, p Plugin) {
	for backoff := time.Second; ; {
		err := Attach(config, p)
		if err == nil {
			err = errors.New("attach session ended")
		}
		app.Logf("%s: attach: %v — retrying in %s", name, err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// manifestID leest de app-id uit het manifest — de app weet zelf wie hij is.
func manifestID(manifest []byte) string {
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return ""
	}
	return doc.ID
}
