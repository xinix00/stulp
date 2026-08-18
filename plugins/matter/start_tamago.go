//go:build tamago

package main

// Deze app als HopOS-slot-app. De hele node-gedaante staat in appsdk.RunNode;
// het volledige verhaal (STULP_ATTACH/hairpin, tokens, waarom plaintext) staat
// éénmalig in examples/virtual/start_tamago.go.
//
// Matter praat met apparaten op het THUIS-LAN: mDNS-discovery loopt via
// leannets link-local multicast (internal/discovery/transports_tamago.go) en
// de UDP-transportpoort hoort in de jobspec gepubliceerd te worden
// (ports {"matter": 5540}, proto udp) zodat apparaten de controller op het
// node-IP terugvinden. Geen meegebakken CA-roots: matter gebruikt eigen
// certificaten (fabric-CA), geen web-PKI.

import (
	"embed"

	"github.com/xinix00/stulp/internal/appsdk"
)

//go:embed app.json
var appJSON []byte

//go:embed settings drivers
var appUI embed.FS

func start(p appsdk.Plugin) {
	p.UI = appUI
	appsdk.RunNode("matter", appJSON, p)
}
