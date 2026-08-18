package webapi

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	flowengine "github.com/xinix00/stulp/internal/flow"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
	"github.com/xinix00/stulp/internal/units"
)

// Een app die meet zoals de weer-app dat doet: canoniek, met eenheden op de
// capabilities en op de argumenten van zijn kaarten.
func unitsServer(t *testing.T) (*Server, store.Device) {
	t.Helper()
	root := t.TempDir()
	appJSON := `{
      "id":"com.acme.weather","version":"1.0.0","sdk":3,"name":{"en":"Weather"},
      "capabilities":{
        "measure_wind_strength":{"type":"number","title":{"en":"Wind"},"units":{"en":"m/s","nl":"m/s"},
          "min":0,"max":40,"getable":true,"setable":false},
        "visibility":{"type":"number","title":{"en":"Visibility"},"units":"km","min":0,"getable":true,"setable":false}
      },
      "drivers":[{"id":"location","name":{"en":"Location"},"class":"sensor",
        "capabilities":["measure_temperature","measure_wind_strength","visibility","measure_humidity"]}],
      "flow":{
        "triggers":[
          {"id":"wind_changed","title":{"nl":"Windkracht komt boven"},
           "args":[{"name":"device","type":"device"},
                   {"name":"speed","type":"number","units":"m/s","min":0,"max":40,"step":0.5}],
           "tokens":[{"name":"temperature","type":"number","title":{"nl":"Temperatuur"},"units":"°C"},
                     {"name":"wind_speed","type":"number","title":{"nl":"Windkracht"},"units":"m/s"},
                     {"name":"code","type":"number","title":{"nl":"WMO-code"}},
                     {"name":"description","type":"string","title":{"nl":"Weer"}}]}
        ],
        "conditions":[
          {"id":"temperature_above","title":{"nl":"Temperatuur is boven"},
           "args":[{"name":"device","type":"device"},
                   {"name":"celsius","type":"number","units":"°C","min":-40,"max":50,"step":0.5}]},
          {"id":"wind_above","title":{"nl":"Windkracht is boven"},
           "args":[{"name":"device","type":"device"},
                   {"name":"speed","type":"number","units":"m/s","min":0,"max":40,"step":0.5}]},
          {"id":"rain_today","title":{"nl":"Kans op regen"},
           "args":[{"name":"device","type":"device"},
                   {"name":"chance","type":"number","units":"%","min":0,"max":100}]}
        ]
      }
    }`
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(appJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, resolved, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()
	if err := database.InstallApp(ctx, appManifest, resolved, ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: "com.acme.weather", DriverID: "location", Name: "Thuis", Class: "sensor",
		Capabilities: []string{"measure_temperature", "measure_wind_strength", "visibility", "measure_humidity"},
		State: map[string]any{
			"measure_temperature":   22.1,
			"measure_wind_strength": 4.2,
			"visibility":            24.1,
			"measure_humidity":      78.0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(apps.Close)
	server := New(database, apps, Options{Language: "nl"})
	t.Cleanup(server.Close)
	return server, device
}

func capabilityValue(t *testing.T, server *Server, device store.Device, capability string) (any, any) {
	t.Helper()
	object := server.capabilityObject(device, capability, device.State[capability])
	return object["value"], object["units"]
}

// Zonder keuze verandert er niets. Dit is de test die elke bestaande installatie
// beschermt: één afronding te veel en elke tegel in huis leest anders.
func TestWithoutAChoiceEveryValueStaysAsMeasured(t *testing.T) {
	server, device := unitsServer(t)
	for capability, want := range map[string]float64{
		"measure_temperature": 22.1, "visibility": 24.1, "measure_humidity": 78.0,
	} {
		if value, _ := capabilityValue(t, server, device, capability); value != want {
			t.Errorf("%s las %v, wil %v", capability, value, want)
		}
	}
	// Wind is de uitzondering: gemeten in meters per seconde, gelezen in Beaufort,
	// zoals de tegel altijd al deed.
	value, unit := capabilityValue(t, server, device, "measure_wind_strength")
	if value != 3.0 || unit != "Bft" {
		t.Errorf("4,2 m/s las als %v %v, wil 3 Bft", value, unit)
	}
}

func TestATileReadsInTheChosenUnit(t *testing.T) {
	server, device := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F", Wind: "Bft", Distance: "mi"})

	for _, test := range []struct {
		capability string
		value      any
		unit       string
	}{
		{"measure_temperature", 71.8, "°F"},
		{"measure_wind_strength", 3.0, "Bft"},
		{"visibility", 15.0, "mi"},
		// Luchtvochtigheid is overal procenten: daar valt niets te kiezen en er
		// hoort dus ook niets te veranderen.
		{"measure_humidity", 78.0, "%"},
	} {
		value, unit := capabilityValue(t, server, device, test.capability)
		if value != test.value || unit != test.unit {
			t.Errorf("%s las %v %v, wil %v %s", test.capability, value, unit, test.value, test.unit)
		}
	}
	// De grenzen gaan mee, anders staat er een schuif van 0 tot 40 met Bft erop.
	object := server.capabilityObject(device, "measure_wind_strength", 4.2)
	if object["min"] != 0.0 || object["max"] != 12.0 {
		t.Errorf("de grenzen zijn %v tot %v, wil 0 tot 12", object["min"], object["max"])
	}
}

// Wat iemand intypt is zijn eigen eenheid; het apparaat krijgt graden Celsius.
func TestATypedValueGoesBackToCanonical(t *testing.T) {
	server, device := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F"})
	ctx := context.Background()

	got := server.canonicalCapabilityValue(ctx, device, "target_temperature", 70.0)
	number, ok := numberOf(got)
	if !ok || number < 21.1 || number > 21.2 {
		t.Errorf("70 °F werd %v, wil rond 21,1 °C", got)
	}
	// Een percentage blijft een percentage, en een woord blijft een woord.
	if got := server.canonicalCapabilityValue(ctx, device, "measure_humidity", 78.0); got != 78.0 {
		t.Errorf("een percentage werd %v", got)
	}
	if got := server.canonicalCapabilityValue(ctx, device, "weather_state", "rain"); got != "rain" {
		t.Errorf("een woord werd %v", got)
	}
}

func TestCardArgumentsReadInTheChosenUnit(t *testing.T) {
	server, _ := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F", Wind: "Bft"})
	cards, err := server.flowCards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]map[string]any{}
	for _, card := range cards["conditions"] {
		if id, _ := card["id"].(string); id == "temperature_above" || id == "wind_above" || id == "rain_today" {
			list, _ := card["args"].([]any)
			for _, raw := range list {
				argument, _ := raw.(map[string]any)
				name, _ := argument["name"].(string)
				arguments[id+"."+name] = argument
			}
		}
	}
	celsius := arguments["temperature_above.celsius"]
	if celsius["units"] != "°F" || celsius["min"] != -40.0 || celsius["max"] != 122.0 || celsius["step"] != 1.0 {
		t.Errorf("het temperatuurveld werd %v", celsius)
	}
	wind := arguments["wind_above.speed"]
	if wind["units"] != "Bft" || wind["min"] != 0.0 || wind["max"] != 12.0 || wind["step"] != 1.0 {
		t.Errorf("het windveld werd %v", wind)
	}
	if chance := arguments["rain_today.chance"]; chance["units"] != "%" || chance["max"] != 100.0 {
		t.Errorf("het procentveld werd %v", chance)
	}
}

// Het manifest is een levende map in het geheugen. Wordt daar tijdens het
// omrekenen in geschreven, dan staat er na één GET Fahrenheit in de installatie
// zelf -- en dan rekent de tweede GET van Fahrenheit naar Fahrenheit.
func TestReadingTheCardsDoesNotChangeTheManifest(t *testing.T) {
	server, _ := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F"})
	ctx := context.Background()

	first, err := server.flowCards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.flowCards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	maximumOf := func(cards map[string][]map[string]any) any {
		for _, card := range cards["conditions"] {
			if id, _ := card["id"].(string); id != "temperature_above" {
				continue
			}
			list, _ := card["args"].([]any)
			for _, raw := range list {
				argument, _ := raw.(map[string]any)
				if name, _ := argument["name"].(string); name == "celsius" {
					return argument["max"]
				}
			}
		}
		return nil
	}
	if maximumOf(first) != 122.0 || maximumOf(second) != 122.0 {
		t.Errorf("twee keer lezen gaf %v en %v", maximumOf(first), maximumOf(second))
	}
	// En in het manifest staat nog steeds wat de app declareerde.
	app, err := server.store.App(ctx, "com.acme.weather")
	if err != nil {
		t.Fatal(err)
	}
	flowManifest, _ := app.Manifest["flow"].(map[string]any)
	conditions, _ := flowManifest["conditions"].([]any)
	for _, raw := range conditions {
		card, _ := raw.(map[string]any)
		if id, _ := card["id"].(string); id != "temperature_above" {
			continue
		}
		list, _ := card["args"].([]any)
		for _, rawArgument := range list {
			argument, _ := rawArgument.(map[string]any)
			if name, _ := argument["name"].(string); name != "celsius" {
				continue
			}
			if argument["max"] != 50.0 || canonicalUnitOf(argument["units"]) != "°C" {
				t.Errorf("het manifest werd veranderd: %v", argument)
			}
		}
	}
}

func storeFlow(t *testing.T, server *Server, args map[string]any) store.Flow {
	t.Helper()
	created, err := server.store.CreateFlow(context.Background(), store.Flow{
		Name: "Zonwering", Enabled: true,
		Nodes: []store.FlowNode{
			{ID: "als", Step: store.FlowStep{AppID: "com.acme.weather", CardID: "wind_changed", CardType: "trigger"}},
			{ID: "en", Step: store.FlowStep{AppID: "com.acme.weather", CardID: "temperature_above", CardType: "condition", Args: args}},
			{ID: "dan", Step: store.FlowStep{AppID: "stulp", CardID: "set_device_capability", CardType: "action",
				Args: map[string]any{"capability": "target_temperature", "value": 20.0}}},
		},
		Edges: []store.FlowEdge{{ID: "een", From: "als", To: "en"}, {ID: "twee", From: "en", To: "dan"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestAFlowThresholdReadsAndWritesInTheChosenUnit(t *testing.T) {
	server, _ := unitsServer(t)
	stored := storeFlow(t, server, map[string]any{"celsius": 20.0})
	server.rememberUnits(units.Set{Temperature: "°F"})
	ctx := context.Background()
	declared := server.declaredArgumentUnits(ctx)

	shown := server.showFlow(ctx, stored, declared)
	if got := shown.Nodes[1].Step.Args["celsius"]; got != 68.0 {
		t.Errorf("20 °C leest als %v, wil 68", got)
	}
	// De kaart die een waarde zet leest ook om: de capability staat in de stap.
	if got := shown.Nodes[2].Step.Args["value"]; got != 68.0 {
		t.Errorf("de gezette waarde leest als %v, wil 68", got)
	}
	// En het document is niet aangeraakt door dat lezen.
	again, err := server.store.Flow(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Nodes[1].Step.Args["celsius"]; got != 20.0 {
		t.Errorf("het lezen veranderde het document: %v", got)
	}

	// De weg terug: iemand typt 77 en het document krijgt 25.
	incoming := server.showFlow(ctx, again, declared)
	incoming.Nodes[1].Step.Args = map[string]any{"celsius": 77.0}
	canonical := server.canonicalFlow(ctx, incoming, &again)
	got, _ := numberOf(canonical.Nodes[1].Step.Args["celsius"])
	if got < 24.99 || got > 25.01 {
		t.Errorf("77 °F werd %v, wil 25 °C", got)
	}
}

// Bewaren zonder iets aan te raken hoort een drempel niet te verschuiven. 12 m/s
// leest als 6 Bft, en 6 Bft terug is 10,8 -- dus zonder deze regel kruipt elke
// drempel omlaag bij elke keer bewaren.
func TestSavingWithoutTouchingKeepsTheThreshold(t *testing.T) {
	server, _ := unitsServer(t)
	stored := storeFlow(t, server, map[string]any{"celsius": 20.0})
	stored.Nodes[0].Step.Args = map[string]any{"speed": 12.0}
	updated, err := server.store.UpdateFlow(context.Background(), stored)
	if err != nil {
		t.Fatal(err)
	}
	server.rememberUnits(units.Set{Wind: "Bft"})
	ctx := context.Background()
	declared := server.declaredArgumentUnits(ctx)

	shown := server.showFlow(ctx, updated, declared)
	if got := shown.Nodes[0].Step.Args["speed"]; got != 6.0 {
		t.Fatalf("12 m/s leest als %v, wil 6 Bft", got)
	}
	kept := server.canonicalFlow(ctx, shown, &updated)
	if got := kept.Nodes[0].Step.Args["speed"]; got != 12.0 {
		t.Errorf("bewaren zonder wijziging maakte er %v van, wil 12", got)
	}
	// Wie de wind wél verandert krijgt de ondergrens van die kracht.
	changed := server.showFlow(ctx, updated, declared)
	changed.Nodes[0].Step.Args = map[string]any{"speed": 7.0}
	after := server.canonicalFlow(ctx, changed, &updated)
	if got := after.Nodes[0].Step.Args["speed"]; got != 13.9 {
		t.Errorf("7 Bft werd %v, wil 13,9 m/s", got)
	}
}

// De hele weg zonder keuze: lezen en bewaren laat elk getal staan zoals het was.
func TestWithoutAChoiceAFlowSurvivesARoundTrip(t *testing.T) {
	server, _ := unitsServer(t)
	stored := storeFlow(t, server, map[string]any{"celsius": 20.5})
	ctx := context.Background()
	declared := server.declaredArgumentUnits(ctx)

	shown := server.showFlow(ctx, stored, declared)
	if got := shown.Nodes[1].Step.Args["celsius"]; got != 20.5 {
		t.Errorf("lezen maakte er %v van", got)
	}
	back := server.canonicalFlow(ctx, shown, &stored)
	if got := back.Nodes[1].Step.Args["celsius"]; got != 20.5 {
		t.Errorf("bewaren maakte er %v van", got)
	}
}

// Een token in een zin leest in de eenheid van dit huis. Dat is wat er in een
// pushbericht terechtkomt, en daar hoort geen graad Celsius te staan in een huis
// dat Fahrenheit leest.
func TestATokenInASentenceReadsInTheChosenUnit(t *testing.T) {
	server, device := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F", Wind: "Bft"})
	source := flowengine.Trigger{
		AppID: "com.acme.weather", CardID: "wind_changed", CardType: "trigger",
		DeviceID: device.ID,
		Tokens:   map[string]any{"temperature": 22.1, "wind_speed": 4.2, "description": "Zwaar bewolkt"},
	}
	for token, want := range map[string]string{"temperature": "71.8 °F", "wind_speed": "3 Bft"} {
		text, ok := server.readFlowToken(source, token, source.Tokens[token])
		if !ok || text != want {
			t.Errorf("%s werd %q (%v), wil %q", token, text, ok, want)
		}
	}
	// Een token zonder eenheid, en een token dat geen getal is, blijven met rust.
	if _, ok := server.readFlowToken(source, "description", "Zwaar bewolkt"); ok {
		t.Error("een tekst werd omgerekend")
	}
	if _, ok := server.readFlowToken(source, "code", 3.0); ok {
		t.Error("een WMO-code werd omgerekend")
	}
}

// De waardetokens van een afgeleide kaart zijn de capability zelf, dus die kent
// zijn eenheid zonder dat iemand hem declareert.
func TestADerivedCardsValueTokenKnowsItsCapability(t *testing.T) {
	server, device := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F"})
	source := flowengine.Trigger{
		AppID: "stulp", CardID: "capability.measure_temperature.changed", CardType: "trigger",
		DeviceID: device.ID, Tokens: map[string]any{"value": 22.1, "oldValue": 20.0},
	}
	if text, ok := server.readFlowToken(source, "value", 22.1); !ok || text != "71.8 °F" {
		t.Errorf("de nieuwe waarde werd %q (%v)", text, ok)
	}
	if text, ok := server.readFlowToken(source, "oldValue", 20.0); !ok || text != "68 °F" {
		t.Errorf("de vorige waarde werd %q (%v)", text, ok)
	}
}

// Een veld dat een getal verwacht houdt de meting: daar rekent een plugin mee.
// Een veld dat een zin is leest als zin. Zonder dit onderscheid zou een token in
// een drempel een tekst worden, of een pushbericht een kaal getal.
func TestOnlyTextFieldsReadTokensAsText(t *testing.T) {
	server, _ := unitsServer(t)
	for _, test := range []struct {
		step     store.FlowStep
		argument string
		number   bool
	}{
		{store.FlowStep{AppID: "com.acme.weather", CardID: "temperature_above", CardType: "condition"}, "celsius", true},
		{store.FlowStep{AppID: "com.acme.weather", CardID: "wind_changed", CardType: "trigger"}, "speed", true},
		{store.FlowStep{AppID: "stulp", CardID: "notification", CardType: "action"}, "excerpt", false},
		{store.FlowStep{AppID: "stulp", CardID: "push", CardType: "action"}, "message", false},
		{store.FlowStep{AppID: "stulp", CardID: "set_device_capability", CardType: "action"}, "value", true},
		{store.FlowStep{AppID: "stulp", CardID: "delay", CardType: "action"}, "seconds", true},
	} {
		if got := server.flowArgumentWantsNumber(test.step, test.argument); got != test.number {
			t.Errorf("%s.%s verwacht getal=%v, wil %v", test.step.CardID, test.argument, got, test.number)
		}
	}
}

// Een pagina van een app leest in dezelfde eenheid als de tegel ernaast. De
// plugin zegt alleen wát hij meet.
func TestAPageAnswerIsConverted(t *testing.T) {
	server, _ := unitsServer(t)
	server.rememberUnits(units.Set{Temperature: "°F", Wind: "mph"})
	answer := server.showMeasures(map[string]any{
		"locations": []any{map[string]any{
			"name":        "Nijmegen",
			"temperature": map[string]any{"$measure": 22.1, "units": "°C"},
			"wind":        map[string]any{"$measure": 4.2, "units": "m/s"},
			// Een gewoon veld blijft een gewoon veld, ook als het "value" heet.
			"answered": true, "value": 12.0, "units": "stuks",
		}},
	})
	first := answer.(map[string]any)["locations"].([]any)[0].(map[string]any)
	temperature := first["temperature"].(map[string]any)
	if temperature["text"] != "71.8 °F" || temperature["value"] != 71.8 || temperature["measured"] != 22.1 {
		t.Errorf("de temperatuur werd %v", temperature)
	}
	if wind := first["wind"].(map[string]any); wind["text"] != "9 mph" {
		t.Errorf("de wind werd %v", wind)
	}
	if first["value"] != 12.0 || first["units"] != "stuks" {
		t.Errorf("een gewoon veld werd aangeraakt: %v", first)
	}
}

func TestAnUnknownUnitIsRefusedByTheAPI(t *testing.T) {
	if _, ok := (units.Set{}).Choose("temperature", "graden"); ok {
		t.Error("een onbekende eenheid werd aangenomen")
	}
}
