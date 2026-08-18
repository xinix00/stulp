//go:build tamago

package main

// Deze app als HopOS-slot-app. De hele node-gedaante staat in appsdk.RunNode;
// het volledige verhaal (STULP_ATTACH/hairpin, tokens, waarom plaintext) staat
// éénmalig in examples/virtual/start_tamago.go.
//
// GEEN certificaat-verificatie (Derek, 15-08): deze app haalt alleen publieke,
// read-only weerdata op en draagt geen enkel geheim. Het ergste dat een
// man-in-the-middle hier kan is nepweer tonen — en wie MITM kan, kan wel
// erger. Daar staat tegenover dat de meegebakken CA-roots de dure lading
// waren: ~150 certificaten parsen op een klein venster was mede de
// GC-doodspiraal van 14/15-08. Plugins die een token dragen (spotify, nibe,
// somfy, notify) houden WEL volledige verificatie — daar is de identiteit van
// de overkant het hele punt.

import (
	"crypto/tls"
	"embed"
	"net/http"

	"github.com/xinix00/stulp/internal/appsdk"
)

//go:embed app.json
var appJSON []byte

//go:embed settings drivers
var appUI embed.FS

func start(p appsdk.Plugin) {
	p.UI = appUI
	// openmeteo's client laat Transport nil = http.DefaultTransport; die hier
	// bijstellen raakt dus élke fetch van deze plugin.
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	appsdk.RunNode("weather", appJSON, p)
}
