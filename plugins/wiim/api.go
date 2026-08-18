package main

import (
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
)

// De configuratiepagina van deze app.
//
// Er valt niets in te stellen: een speler heeft geen account en geen sleutel,
// en zijn adres hoort bij het apparaat en niet bij de app. Wat deze pagina wél
// doet is de vraag beantwoorden die overblijft als er iets niet werkt — komt
// multicast hier rond, en antwoorden de spelers die gekoppeld zijn? Dat is
// precies wat je niet kunt zien aan een tegel die grijs is.

func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("status", func(map[string]any, map[string]any) (any, error) {
		a.mu.RLock()
		players := make([]*player, 0, len(a.players))
		for _, target := range a.players {
			players = append(players, target)
		}
		a.mu.RUnlock()

		list := make([]map[string]any, 0, len(players))
		for _, target := range players {
			list = append(list, target.report())
		}
		return map[string]any{"players": list}, nil
	})

	// search doet dezelfde ronde als het koppelscherm, maar koppelt niets. Wie
	// hier niets ziet en zijn speler wel kan pingen weet meteen waar het aan
	// ligt: multicast komt niet rond, en dan is met de hand toevoegen de weg.
	stulp.OnRequest("search", func(map[string]any, map[string]any) (any, error) {
		return a.search()
	})
}

// report is wat de pagina over één speler laat zien.
func (p *player) report() map[string]any {
	p.mu.Lock()
	lastErr := p.lastErr
	lastOK := p.lastOK
	failures := p.failures
	p.mu.Unlock()

	entry := map[string]any{
		"name":     p.device.Name(),
		"uuid":     p.uuid,
		"address":  p.address(),
		"answers":  failures < failuresBeforeUnavailable && !lastOK.IsZero(),
		"failures": failures,
	}
	if lastErr != "" {
		entry["error"] = lastErr
	}
	if !lastOK.IsZero() {
		entry["secondsAgo"] = int(time.Since(lastOK).Seconds())
	}
	return entry
}
