package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// De configuratiepagina van deze app.
//
// Eén vraag die er echt toe doet: waar staat het systeem. Poort, ritme en
// wachttijd hebben een waarde die klopt en zijn er voor het geval iemands
// opstelling anders is. Wat er achter dat adres zit -- welke units er zijn en
// wat ze aanbieden -- wordt gevraagd en niet ingevuld: daar is Proberen voor,
// en daarna vullen de koppelschermen zich vanzelf.
//
// Anders dan in de bron staat het adres hier bij de app en niet bij elk
// apparaat. Een Sigenergy-systeem is één Modbus/TCP-adres met units erachter, en
// dat adres zes keer overtypen is zes plekken om het fout te doen. Zie PORTED.md.

func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("status", func(map[string]any, map[string]any) (any, error) {
		a.mu.RLock()
		connected := a.client != nil
		lastErr := a.lastErr
		devices := len(a.devices)
		a.mu.RUnlock()
		return map[string]any{
			"host":      stulp.SettingText("host"),
			"port":      stulp.SettingNumber("port", defaultPort),
			"interval":  stulp.SettingNumber("interval", defaultInterval),
			"timeout":   stulp.SettingNumber("timeout", defaultTimeout),
			"units":     unitsSetting(stulp),
			"connected": connected && lastErr == "",
			"error":     lastErr,
			"devices":   devices,
		}, nil
	})

	// test probeert de opgegeven gegevens meteen uit zonder ze te bewaren, en
	// zegt wat er antwoordde. Zo weet iemand vóór het opslaan of het klopt, en
	// waaraan het lag als het niet klopt.
	stulp.OnRequest("test", func(_, body map[string]any) (any, error) {
		host, _ := body["host"].(string)
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, fmt.Errorf("vul het adres van het Sigenergy-systeem in")
		}
		port := defaultPort
		if value, ok := body["port"].(float64); ok && value > 0 {
			port = int(value)
		}
		unitText, _ := body["units"].(string)
		units, err := parseUnits(unitText)
		if err != nil {
			return nil, err
		}

		// Kort van draad: dit is iemand die op een knop wacht. Een systeem dat
		// binnen twee seconden niets zegt gaat dat bij unit 200 ook niet doen.
		client := modbus.New(host, port, 2*time.Second)
		defer client.Close()

		return probe(client, units)
	})
}

// probe kijkt per unit wat er te vinden is.
//
// Er is geen lijst op te vragen: Modbus/TCP kent geen ontdekking. Wat er wel is
// zijn de registers waaraan elke driver zijn apparaat herkent, en die worden
// hier één voor één geprobeerd -- precies zoals het koppelen het straks doet.
// Wat de pagina laat zien is dus wat er in de koppelschermen zal staan.
func probe(client *modbus.Client, units []uint8) (any, error) {
	kinds := []struct {
		label string
		card  sigen.Card
	}{
		{"systeem", sigen.Plant},
		{"netmeter", sigen.Energy},
		{"omvormer", sigen.Inverter},
		{"batterij", sigen.Battery},
		{"AC-laadpaal", sigen.EvACCharger},
	}

	type answer struct {
		Unit   int      `json:"unit"`
		Offers []string `json:"offers"`
	}
	var found []answer
	var firstFailure error
	for _, unit := range units {
		var offers []string
		for _, kind := range kinds {
			reg := kind.card.Probe()
			if _, err := client.ReadHolding(unit, reg.Addr, reg.Count); err == nil {
				offers = append(offers, kind.label)
				continue
			} else if !isRefusal(err) && firstFailure == nil {
				firstFailure = err
			}
		}
		if len(offers) > 0 {
			found = append(found, answer{Unit: int(unit), Offers: offers})
		}
	}
	if len(found) == 0 {
		if firstFailure != nil {
			return nil, firstFailure
		}
		return nil, fmt.Errorf("het adres antwoordt, maar op geen van de %d afgetaste units staat een Sigenergy-apparaat", len(units))
	}
	return map[string]any{"found": found, "units": len(units)}, nil
}

// unitsSetting is wat er af te tasten valt, met de standaard als er niets staat.
func unitsSetting(stulp *appsdk.Stulp) string {
	if text := stulp.SettingText("units"); text != "" {
		return text
	}
	return defaultUnits
}
