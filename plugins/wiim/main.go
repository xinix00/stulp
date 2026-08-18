// De WiiM-app.
//
// Eén soort apparaat: een audiospeler in huis, gevonden met SSDP of met de hand
// toegevoegd op adres. Er is geen account, geen cloud en geen sleutel — alles
// gaat rechtstreeks naar de speler op je eigen netwerk. Wat er wel en niet
// geport is staat in PORTED.md, met verwijzingen naar waar het in de
// oorspronkelijke app stond.
package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/wiim/internal/wiim"
)

// app is de plugin als geheel. Anders dan bij een app met één hub heeft elke
// speler hier zijn eigen adres en zijn eigen ronde; wat hier staat is dus
// alleen wie er zijn.
type app struct {
	mu      sync.RWMutex
	players map[string]*player // Stulp-device-id -> speler
	// found is wat het koppelscherm het laatst vond. Het staat hier en niet in
	// de sessie omdat de lijst tussen twee koppelpagina's door bewaard moet
	// blijven: zoeken gebeurt op de ene pagina, kiezen op de volgende.
	found []appsdk.PairedDevice
}

var instance = &app{players: map[string]*player{}}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit:  instance.start,
		OnStop:  instance.stop,
		Drivers: map[string]appsdk.Driver{"player": playerDriver{}},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.registerAPI(stulp)
	a.registerFlow(stulp)
	return nil
}

// stop laat elke speler zijn ronde afbreken. Het proces stopt daarna toch, maar
// een ronde die nog loopt zou anders in een gesloten verbinding schrijven.
func (a *app) stop() {
	a.mu.RLock()
	players := make([]*player, 0, len(a.players))
	for _, target := range a.players {
		players = append(players, target)
	}
	a.mu.RUnlock()
	for _, target := range players {
		target.halt()
	}
}

func (a *app) watch(deviceID string, target *player) {
	a.mu.Lock()
	a.players[deviceID] = target
	a.mu.Unlock()
}

func (a *app) forget(deviceID string) {
	a.mu.Lock()
	delete(a.players, deviceID)
	a.mu.Unlock()
}

// player zoekt de speler op die bij een Flow-kaart hoort.
func (a *app) player(deviceID string) (*player, error) {
	a.mu.RLock()
	target := a.players[deviceID]
	a.mu.RUnlock()
	if target == nil {
		return nil, fmt.Errorf("deze kaart wijst naar een speler die niet draait")
	}
	return target, nil
}

// registerFlow hangt de twee kaarten op die de bron ook heeft
// (`drivers/player/driver.flow.compose.json`).
func (a *app) registerFlow(stulp *appsdk.Stulp) {
	stulp.OnFlowAction("switch_off", func(args, _ map[string]any) (any, error) {
		target, err := a.player(appsdk.DeviceArg(args, "device"))
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		return nil, target.command(ctx, func(client *wiim.Client) error { return client.Stop(ctx) })
	})

	stulp.OnFlowAction("call_preset", func(args, _ map[string]any) (any, error) {
		target, err := a.player(appsdk.DeviceArg(args, "device"))
		if err != nil {
			return nil, err
		}
		// Het argument komt als tekst binnen: de kaart is een keuzelijst met
		// "1" tot "12", net als in de bron.
		number, err := presetNumber(args["preset_number"])
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		return nil, target.command(ctx, func(client *wiim.Client) error { return client.Preset(ctx, number) })
	})
}

func presetNumber(value any) (int, error) {
	switch number := value.(type) {
	case string:
		parsed, err := strconv.Atoi(number)
		if err != nil {
			return 0, fmt.Errorf("%q is geen voorkeurnummer", number)
		}
		return parsed, nil
	case float64:
		return int(number), nil
	}
	return 0, fmt.Errorf("deze kaart draagt geen voorkeurnummer")
}

// commandTimeout begrenst één opdracht aan een speler. Ruim genoeg voor een
// apparaat in huis, kort genoeg om een Flow niet vast te laten lopen op een
// speler die uit staat.
const commandTimeout = 10 * time.Second

// De koppelschermen vragen de driver wat er te vinden is. Dat gaat via een
// type-assertie, dus zonder deze regels merk je een verkeerde handtekening pas
// als iemand op koppelen drukt.
var (
	_ appsdk.Pairer            = playerDriver{}
	_ appsdk.PairPages         = playerDriver{}
	_ appsdk.DeviceHandler     = (*player)(nil)
	_ appsdk.CapabilityHandler = (*player)(nil)
	_ appsdk.SettingsChanger   = (*player)(nil)
	_ appsdk.Deleter           = (*player)(nil)
)
