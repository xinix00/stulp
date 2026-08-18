package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/somfy/internal/tahoma"
)

// Scenario's zijn wat de gebruiker in de Somfy-app zelf heeft gemaakt: "alles
// dicht", "ochtend". De app hoeft er niets van te weten behalve hoe ze heten en
// hoe je ze start -- de doos doet de rest.
//
// De bron heeft hier één Flow-kaart voor (app.js, addScenarioActionListeners) en
// hier is het dezelfde.

func (a *app) registerFlow(stulp *appsdk.Stulp) {
	stulp.OnFlowAction("activate_scenario", func(args, _ map[string]any) (any, error) {
		client, err := a.api()
		if err != nil {
			return nil, err
		}
		oid := scenarioID(args["scenario"])
		if oid == "" {
			return nil, fmt.Errorf("deze kaart heeft geen scenario gekozen")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RunScenario(ctx, oid); err != nil {
			return nil, err
		}
		// Een scenario is net zo asynchroon als een los commando: TaHoma neemt
		// hem aan en voert hem daarna uit. Wat de apparaten ervan worden komt
		// via de poll binnen, dus die krijgt een zetje.
		a.nudge()
		return nil, nil
	})

	stulp.OnFlowAutocomplete("action", "activate_scenario", "scenario",
		func(query string, _ map[string]any) ([]appsdk.AutocompleteItem, error) {
			client, err := a.api()
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			scenarios, err := client.Scenarios(ctx)
			if err != nil {
				return nil, err
			}
			return matching(scenarios, query), nil
		})
}

// matching filtert op wat iemand intypt, hoofdletterongevoelig -- zo doet de
// bron het ook, en een scenario dat "Alles dicht" heet hoort ook op "alles" te
// verschijnen.
func matching(scenarios []tahoma.Scenario, query string) []appsdk.AutocompleteItem {
	needle := strings.ToLower(strings.TrimSpace(query))
	items := make([]appsdk.AutocompleteItem, 0, len(scenarios))
	for _, scenario := range scenarios {
		if needle != "" && !strings.Contains(strings.ToLower(scenario.Label), needle) {
			continue
		}
		items = append(items, appsdk.AutocompleteItem{ID: scenario.OID, Name: scenario.Label})
	}
	return items
}

// scenarioID haalt het oid uit wat de kaart bewaard heeft. Een keuzelijst geeft
// het gekozen item terug als object; een Flow die met de hand gemaakt is kan er
// een kale tekst neerzetten.
func scenarioID(value any) string {
	switch chosen := value.(type) {
	case string:
		return chosen
	case map[string]any:
		id, _ := chosen["id"].(string)
		return id
	}
	return ""
}
