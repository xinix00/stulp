package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/wiim/internal/wiim"
)

// Koppelen gaat in twee stappen: eerst zoeken (of een adres intypen), dan
// kiezen. De eigen pagina hoort bij de eerste stap — zie
// drivers/player/pair/search.html.

// searchTimeout is hoe lang een koppelronde mag duren: de zoekvraag zelf, plus
// het opvragen van de beschrijving bij alles wat antwoordde.
const searchTimeout = 12 * time.Second

// Pair is wat de koppelpagina mag sturen.
func (playerDriver) Pair() map[string]appsdk.PairHandler {
	return map[string]appsdk.PairHandler{
		"search": func(any) (any, error) { return instance.search() },
		"manual": func(data any) (any, error) {
			request, _ := data.(map[string]any)
			address, _ := request["address"].(string)
			return instance.manual(strings.TrimSpace(address))
		},
	}
}

// ListDevices levert wat de vorige stap vond.
//
// Hier wordt niet opnieuw gezocht: de pagina heeft net laten zien wat er is, en
// een tweede ronde zou een andere lijst kunnen opleveren dan waar iemand op
// klikte.
func (playerDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	instance.mu.RLock()
	defer instance.mu.RUnlock()
	if len(instance.found) == 0 {
		return nil, fmt.Errorf("er is nog geen speler gevonden; zoek eerst, of voeg er een toe op adres")
	}
	return instance.found, nil
}

// search doet één SSDP-ronde en onthoudt wat eruit kwam.
func (a *app) search() (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	players, err := wiim.Discover(ctx, wiim.SearchOptions{})
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0, len(players))
	for _, player := range players {
		found = append(found, paired(player))
	}

	a.mu.Lock()
	a.found = found
	a.mu.Unlock()

	return answer(players), nil
}

// manual voegt een speler toe op adres.
//
// Dit pad is er met opzet en is geen tweede rangorde: multicast komt niet door
// een VPN heen, niet over een gastnetwerk en niet langs elke switch. Zonder dit
// zou een speler die prima bereikbaar is toch onkoppelbaar zijn. Wie hij is
// vraagt de app aan de speler zelf — een naam of een uuid overtypen hoeft
// niemand.
func (a *app) manual(address string) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	player, err := wiim.Identify(ctx, address)
	if err != nil {
		return nil, err
	}
	// Het adres dat iemand intypte is waar deze app naartoe gaat praten, niet
	// wat er toevallig in de beschrijving stond.
	player.Address = address

	a.mu.Lock()
	a.found = []appsdk.PairedDevice{paired(player)}
	a.mu.Unlock()

	return answer([]wiim.Player{player}), nil
}

// paired maakt van een gevonden speler een apparaat om te koppelen.
func paired(player wiim.Player) appsdk.PairedDevice {
	return appsdk.PairedDevice{
		Name: player.Name,
		// De identiteit ligt hierna vast en mag dus niets veranderlijks
		// bevatten. Het uuid uit de UDN is hetzelfde nummer dat de bron
		// bewaarde, dus een speler die daar gekoppeld was blijft dezelfde.
		Data: map[string]any{"id": player.UUID},
		// Het adres staat in de instellingen en niet in de store: het verandert
		// zodra de router iets anders uitdeelt, en dan hoort iemand het te
		// kunnen bijwerken zonder opnieuw te koppelen.
		Settings: map[string]any{"address": player.Address},
		Store:    map[string]any{"model": player.Model, "port": player.Port},
	}
}

// answer is wat de koppelpagina en de instellingenpagina te zien krijgen.
func answer(players []wiim.Player) map[string]any {
	list := make([]map[string]any, 0, len(players))
	for _, player := range players {
		list = append(list, map[string]any{
			"uuid":    player.UUID,
			"name":    player.Name,
			"model":   player.Model,
			"address": player.Address,
		})
	}
	return map[string]any{"found": len(list), "players": list}
}
