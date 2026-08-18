//go:build tamago

package main

// start_tamago.go — deze app als HopOS-slot-app.
//
// Op een host start Stulp een app zelf: fork, exec, en één kant van een
// socketpair als fd 3. Op een node bestaat dat niet — er is geen fork en geen
// bestandssysteem — en dat is precies waarom de aanmeld-over-een-poort er is.
// HOP plaatst deze app als eigen slot met een eigen IP, en het eerste wat hij
// doet is aankloppen bij Stulp met zijn token.
//
// Dat is strengere isolatie dan op een host, niet zwakkere: twee slots kunnen
// elkaar alleen bereiken omdat de switch dat is verteld, en er is geen
// gedeelde schijf, geen gedeelde heap en geen proceslijst om in te kijken.
//
// Wat HOP hier zet (jobspec):
//
//	STULP_ATTACH        waar Stulp luistert: zijn SLOT-adres met de attach-poort
//	                    (10.100.0.2:7000 als Stulp in slot 1 zit). Dat is de
//	                    korte weg — twee slots praten over HOP's switch, zonder
//	                    NAT en zonder de node-poort. Het node-adres met de
//	                    gepubliceerde poort (HOPOS_HOST:7000) werkt ook, via de
//	                    hairpin; dat is de weg voor een app die niet op deze node
//	                    draait.
//
//	                    GEMETEN 12-08 op een LicheeRV, en de moeite waard om te
//	                    onthouden: dit leek eerst niet te werken (eindeloos "i/o
//	                    timeout" op het slot-IP terwijl poort 80 van diezelfde
//	                    Stulp meteen antwoordde). Dat was geen HOP-probleem maar
//	                    een gat in onze TCP: een app die HOP herstart belt met
//	                    exact hetzelfde vier-tupel als zijn voorganger — elk vers
//	                    leannet begint zijn efemere reeks op 49152 en het slot-IP
//	                    is per slot vast — en Stulp hield de oude verbinding voor
//	                    levend. Zie leannet's recvSynSent (RFC 9293 §3.10.7.3).
//	STULP_ATTACH_TOKEN  het token van DEZE app (stulp attach-token <id>)
//	STULP_APP_ID        de app-id; verplicht, want er is geen app.json naast een
//	                    slot-image om hem uit te lezen
//
// Jobspec:
//
//	{"name":"stulp-virtual","driver":"hop","memory_limit":33554432,
//	 "env":{"STULP_ATTACH":"192.168.99.2:7000","STULP_APP_ID":"com.stulp.virtual",
//	        "STULP_ATTACH_TOKEN":"..."},
//	 "artifacts":[{"url":".../virtual-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}]}
//
// Bouwen met -tags stulp_notls: op het node-netwerk voegt TLS niets toe dat de
// token-uitwisseling niet al doet, en het scheelt de app een TLS-stapel.

import (
	"embed"

	"github.com/xinix00/stulp/internal/appsdk"
)

// appJSON is het manifest van deze app, meegebakken.
//
// Op een host leest Stulp app.json uit de map naast de binary. Een slot-image
// heeft die map niet, dus draagt hij hem zelf en stuurt hem bij de begroeting.
// Dat is ook wat Stulp nodig heeft om een app die hij nog niet kent te kunnen
// aanbieden in plaats van weg te sturen.
//
//go:embed app.json
var appJSON []byte

//go:embed settings drivers locales
var appUI embed.FS

func start(p appsdk.Plugin) {
	p.UI = appUI
	appsdk.RunNode("virtual", appJSON, p)
}
