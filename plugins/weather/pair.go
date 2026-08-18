package main

import (
	"fmt"
	"strings"

	"github.com/xinix00/stulp/internal/appsdk"
)

// Koppelen gaat in twee stappen: eerst een plek kiezen, dan toevoegen. De eigen
// pagina hoort bij de eerste stap -- zie drivers/location/pair/search.html.
//
// Waarom een eigen pagina en niet gewoon een lijst: een locatie valt niet te
// ontdekken. Er is geen netwerk om af te tasten en geen account met plaatsen
// erin; welke plek je wilt volgen weet alleen jij.

// Pair is wat de koppelpagina mag sturen.
func (locationDriver) Pair() map[string]appsdk.PairHandler {
	return map[string]appsdk.PairHandler{
		"add": func(data any) (any, error) {
			request, _ := data.(map[string]any)
			name, _ := request["name"].(string)
			where, _ := request["where"].(string)
			latitude, okLat := coordinate(request["latitude"])
			longitude, okLon := coordinate(request["longitude"])
			if !okLat || !okLon {
				return nil, errNoCoordinate
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, fmt.Errorf("geef de plek een naam")
			}
			instance.propose(name, where, latitude, longitude)
			return map[string]any{"name": name}, nil
		},
	}
}

// ListDevices levert wat de vorige stap koos.
//
// Hier wordt niet opnieuw gezocht: de pagina heeft net laten zien wat er op die
// plek gebeurt, en een tweede ronde zou een andere plaats kunnen opleveren dan
// waar iemand op klikte.
func (locationDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return instance.proposed(), nil
}

// propose legt de gekozen plek klaar voor de volgende stap.
func (a *app) propose(name, where string, latitude, longitude float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = []appsdk.PairedDevice{{
		Name: name,
		// Het coördinaat is de identiteit: een naam mag de gebruiker later
		// veranderen, de plek niet.
		Data: map[string]any{"latitude": latitude, "longitude": longitude},
		Store: map[string]any{
			"where": where,
		},
	}}
}

func (a *app) proposed() []appsdk.PairedDevice {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]appsdk.PairedDevice, len(a.pending))
	copy(out, a.pending)
	return out
}
