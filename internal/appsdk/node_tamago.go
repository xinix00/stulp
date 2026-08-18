//go:build tamago

package appsdk

// RunNode is Run voor een HopOS-slot-app: netstack op, aanmelden bij Stulp
// met het token uit de jobspec, manifest meegebakken (een slot heeft geen
// app.json op schijf). Dit is de ENE implementatie van de node-gedaante —
// het volledige verhaal (STULP_ATTACH en de hairpin, het vier-tupel-litteken,
// waarom plaintext op het node-net) staat éénmalig in
// examples/virtual/start_tamago.go; de plugins zijn dunne aanroepers.

import (
	"errors"
	"time"

	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"
)

func RunNode(name string, manifest []byte, p Plugin) {
	app := applib.Init() // eerste regel: READY + heartbeat + kill-flag
	// Eén regel vóór het netwerk: een app die hierna zwijgt hangt in de
	// netstack, een app die dít al niet haalt in Init — dat onderscheid
	// kostte op 15-08 een ochtend (stille Init-dood zonder deze regel).
	app.Logf("%s: init done, bringing the netstack up", name)

	if _, err := appnet.Up(app); err != nil {
		app.Logf("%s: net: %v", name, err)
		app.Exit(1)
	}
	config := AttachConfig{
		Target:   app.Env("STULP_ATTACH"),
		AppID:    app.Env("STULP_APP_ID"),
		Token:    app.Env("STULP_ATTACH_TOKEN"),
		Manifest: manifest,
		// Geen TLS op het node-netwerk: het token bewijst wie er aanklopt
		// (nonce heen, HMAC terug, beide richtingen) — zie examples/virtual.
		Plaintext: true,
	}
	if config.Target == "" {
		app.Logf("%s: STULP_ATTACH is empty -- put stulp's address in the jobspec env", name)
		app.Exit(1)
	}
	if config.AppID == "" {
		app.Logf("%s: STULP_APP_ID is empty -- a slot image has no app.json to read it from", name)
		app.Exit(1)
	}
	app.Logf("%s: attaching to stulp at %s as %s", name, config.Target, config.AppID)
	// WACHTEN, niet sterven. "attach refused: waiting to be installed" is de
	// normale eerste ronde (de install komt zo), en een dichte poort betekent
	// dat stulp (her)start — beide zijn wacht-redenen, geen stervensredenen.
	// Exiten liet HOP de app herstarten, en elke herstart streamt het hele
	// image opnieuw: iedere plugin-start kostte zo twee streams (gemeten
	// 18-08 op de LicheeRV: ~36s per 6MB — de halve vloot-opstarttijd).
	// Attach keert ook ná een geslaagde sessie terug zodra de verbinding
	// wegvalt. De SDK-heartbeat maakt daarbij ook een half-open verbinding na
	// een stulp-herstart lokaal stuk; ook dan is opnieuw aanmelden het antwoord.
	// De kill-vlag van HOP blijft de enige echte exit (applib regelt die).
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
