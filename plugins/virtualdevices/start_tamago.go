//go:build tamago

package main

import (
	"embed"

	"github.com/xinix00/stulp/internal/appsdk"
)

//go:embed app.json
var appJSON []byte

//go:embed drivers
var appUI embed.FS

func start(p appsdk.Plugin) {
	p.UI = appUI
	appsdk.RunNode("virtualdevices", appJSON, p)
}
