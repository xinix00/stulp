//go:build tamago

package appsdk

// RunNodeBundle is RunNode voor een bundel: één slot-app die meerdere plugins
// draagt. Eén applib.Init, één netstack, één heap — en per plugin een eigen
// attach-lus met een eigen identiteit, zodat Stulp exact hetzelfde ziet als
// bij losse apps (eigen announce, eigen install, eigen sessie).
//
// De identiteit komt uit het manifest van de plugin zelf (app.json draagt de
// id al); env is alleen wat werkelijk gedeeld wordt: STULP_ATTACH (waar Stulp
// woont) en het bewijs. Dat bewijs kan op twee manieren binnenkomen:
//
//	STULP_ATTACH_SECRET  hetzelfde zaad dat Stulp's document seedt — de bundel
//	                     leidt elk token zelf af (token = HMAC(geheim, id),
//	                     appproto.Token). Dit is de statische-startup-file-weg:
//	                     één gedeeld veld, geen minting vooraf.
//	STULP_TOKENS         JSON, id → token, per stuk gemint via de API. Wint van
//	                     het zaad als een id in de map staat.
//
// Waarom een bundel: op een node betaalt élke losse app ~12,5MB runtime-tax
// plus een kopie van dezelfde basis (runtime/netstack/sdk). Negen pollers
// naast elkaar = ~7× overhead op de inhoud (gemeten 18-08, LicheeRV). In de
// bundel delen ze die basis en de heap; de prijs is gedeeld lot (één fatal
// raakt ze allemaal). Stulp zelf blijft altijd een eigen proces.

import (
	"encoding/json"
	"errors"
	"runtime/debug"
	"strings"
	"time"

	"github.com/xinix00/HopOS/metal/v2/app/applib"
	"github.com/xinix00/HopOS/metal/v2/app/applib/appnet"

	"github.com/xinix00/stulp/internal/appproto"
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

	// De bundel is de pointer-rijke wereld waarvoor memlimit's kleine-venster-
	// GOGC (25 onder een 128MB-limiet) niet klopt: negen werklasten alloceren
	// samen MB's per seconde, en 25 werd 157 GC/s en een thrash-panic bij 18MB
	// live in een 47MB-limiet (gemeten 19-08). Verdubbelen past hier wél, en
	// het geheugenplafond — sinds de anker-fix van 15-08 écht werkend — dempt
	// het tempo vanzelf richting de muur; de thrash-wachter blijft het vangnet.
	// Groter kan het venster op dit board niet: een ≥130MB-partitie past
	// meetkundig niet in de 222MB-pool, dus de knop hoort hier, bij de app die
	// zijn eigen wereld kent.
	debug.SetGCPercent(100)
	app.Logf("bundle: init done (%d plugins, GOGC 100), bringing the netstack up", len(apps))

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
	secret := app.Env("STULP_ATTACH_SECRET")

	for _, b := range apps {
		id := manifestID(b.Manifest)
		if id == "" {
			app.Logf("bundle: %s: manifest carries no id, skipping", b.Name)
			continue
		}
		token := tokens[id]
		if token == "" && secret != "" {
			// Zelfde formule als Stulp's kant (appproto.Token): wie het zaad
			// deelt, hoeft geen tokens vooraf te minten — de startup-file-weg.
			token = appproto.Token(secret, id)
		}
		config := AttachConfig{
			Target: target,
			AppID:  id,
			Token:  token,
			// Geen TLS op het node-netwerk: het token bewijst wie er
			// aanklopt (nonce heen, HMAC terug) — zie examples/virtual.
			Plaintext: true,
			Manifest:  announceManifest(b.Manifest),
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
	lastLogged := ""
	for backoff := time.Second; ; {
		err := Attach(config, p)
		if err == nil {
			err = errors.New("attach session ended")
		}
		if strings.Contains(err.Error(), "is already running") {
			// De wees-sessie van onze voorganger (rolling replace): stulps
			// ping-wachter ruimt hem binnen ~15s. Vlak en snel blijven proberen
			// — exponentieel wachten maakte de wissel tientallen seconden
			// langer dan het lijk zelf leefde.
			backoff = 2 * time.Second
		}
		// "waiting to be installed" is een TOESTAND, geen storing: een
		// aangeboden plugin die nooit geïnstalleerd is klopt netjes elke 32s
		// aan (zodat een install-klik binnen een halve minuut landt), maar
		// dat hoeft niet elke 32s in het log — op een node is de ringbuffer
		// het hele geheugen, en één zwijgzame plugin verzoop er de matter-
		// diagnose van álle andere (gemeten 19-08). Eén regel per toestand;
		// zodra de fout verándert (echte storing, install) is hij weer luid.
		if message := err.Error(); message != lastLogged {
			lastLogged = message
			app.Logf("%s: attach: %v — will keep retrying quietly", name, err)
		}
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
