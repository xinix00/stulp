package store

import (
	"context"
	"fmt"
	"testing"
)

// linearFlow is de eigen versie van storetest.LinearFlow: dat pakket importeert
// de store, dus een test binnen de store kan hem niet gebruiken zonder een
// importcyclus te maken.
func linearFlow(name string, trigger FlowStep, actions []FlowStep) Flow {
	steps := append([]FlowStep{trigger}, actions...)
	flow := Flow{Name: name, Enabled: true}
	for index, step := range steps {
		id := fmt.Sprintf("node-%d", index)
		flow.Nodes = append(flow.Nodes, FlowNode{ID: id, X: 80 + float64(index*400), Y: 120, Step: step})
		if index > 0 {
			flow.Edges = append(flow.Edges, FlowEdge{
				ID: fmt.Sprintf("edge-%d", index-1), From: flow.Nodes[index-1].ID, To: id,
			})
		}
	}
	return flow
}

// Een app kan zijn apparaatmodel wijzigen: twee endpoints die één apparaat
// blijken te zijn, of een capability die van naam verandert omdat er een tweede
// bijkwam. De Flows van de gebruiker mogen daar niet aan kapotgaan.
//
// De app leest of schrijft die Flows niet -- hij meldt alleen "dit apparaat is
// dat geworden, en deze capability heet nu zo". Wat dat voor een Flow betekent
// wordt hier beslist, en daarom hoort deze toets hier en niet bij de app.
func TestReplaceDeviceReferencesRewritesFlows(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	flow, err := database.CreateFlow(ctx, linearFlow("Tweede knop",
		FlowStep{AppID: "stulp", CardID: "capability.button.on", CardType: "trigger", Args: map[string]any{
			"device": map[string]any{"$device": "button-5"}, "capability": "button",
		}},
		[]FlowStep{{AppID: "stulp", CardID: "notification", CardType: "action", Args: map[string]any{"excerpt": "Gedrukt"}}}))
	if err != nil {
		t.Fatal(err)
	}

	if err := database.ReplaceDeviceReferences(ctx, map[string]DeviceReplacement{
		"button-5": {DeviceID: "light", Capabilities: map[string]string{"button": "button.2"}},
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := database.Flow(ctx, flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := updated.Nodes[0].Step
	if got := step.Args["device"].(map[string]any)["$device"]; got != "light" {
		t.Fatalf("Flow wijst nog naar %v, wil light", got)
	}
	if got := step.Args["capability"]; got != "button.2" {
		t.Fatalf("capability = %v, wil button.2", got)
	}
	// De kaart heet naar de capability, dus die moet mee. Blijft hij staan, dan
	// wijst de Flow naar een kaart die het apparaat niet meer aanbiedt.
	if step.CardID != "capability.button.2.on" {
		t.Fatalf("kaart = %q, wil capability.button.2.on", step.CardID)
	}
	// De rest van de Flow hoort onaangeroerd te blijven: er is één apparaat
	// vervangen, niet een Flow herschreven.
	if got := updated.Nodes[len(updated.Nodes)-1].Step.Args["excerpt"]; got != "Gedrukt" {
		t.Fatalf("de actie is meeveranderd: %v", got)
	}
	if !updated.Enabled {
		t.Fatal("de Flow is uitgezet door een vervanging")
	}
}

// Een vervanging zonder capability-hernoeming laat de capability staan. Anders
// zou elke samenvoeging de Flow leeghalen omdat er geen nieuwe naam gegeven is.
func TestReplaceDeviceReferencesKeepsUnnamedCapabilities(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	flow, err := database.CreateFlow(ctx, linearFlow("Vocht",
		FlowStep{AppID: "stulp", CardID: "device_capability_changed", CardType: "trigger", Args: map[string]any{
			"device": map[string]any{"$device": "humidity"}, "capability": "measure_humidity",
		}},
		[]FlowStep{{AppID: "stulp", CardID: "notification", CardType: "action"}}))
	if err != nil {
		t.Fatal(err)
	}

	if err := database.ReplaceDeviceReferences(ctx, map[string]DeviceReplacement{
		"humidity": {DeviceID: "temperature"},
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := database.Flow(ctx, flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := updated.Nodes[0].Step
	if got := step.Args["device"].(map[string]any)["$device"]; got != "temperature" {
		t.Fatalf("Flow wijst nog naar %v", got)
	}
	if got := step.Args["capability"]; got != "measure_humidity" {
		t.Fatalf("capability = %v, wil measure_humidity", got)
	}
}

// Een apparaat dat niet in de vervangingslijst staat blijft waar het is.
func TestReplaceDeviceReferencesLeavesOthersAlone(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	flow, err := database.CreateFlow(ctx, linearFlow("Gang",
		FlowStep{AppID: "stulp", CardID: "device_capability_changed", CardType: "trigger", Args: map[string]any{
			"device": map[string]any{"$device": "hall"},
		}},
		[]FlowStep{{AppID: "stulp", CardID: "notification", CardType: "action"}}))
	if err != nil {
		t.Fatal(err)
	}

	if err := database.ReplaceDeviceReferences(ctx, map[string]DeviceReplacement{
		"button-5": {DeviceID: "light"},
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := database.Flow(ctx, flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Nodes[0].Step.Args["device"].(map[string]any)["$device"]; got != "hall" {
		t.Fatalf("een Flow die er niets mee te maken had is veranderd: %v", got)
	}
}
