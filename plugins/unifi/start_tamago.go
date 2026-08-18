//go:build tamago

package main

// Deze app als HopOS-slot-app. De hele node-gedaante staat in appsdk.RunNode;
// het volledige verhaal (STULP_ATTACH/hairpin, tokens, waarom plaintext) staat
// éénmalig in examples/virtual/start_tamago.go.

import (
	"embed"

	"github.com/xinix00/stulp/internal/appsdk"
)

//go:embed app.json
var appJSON []byte

//go:embed settings
var appUI embed.FS

func start(p appsdk.Plugin) {
	p.UI = appUI
	appsdk.RunNode("unifi", appJSON, p)
}
