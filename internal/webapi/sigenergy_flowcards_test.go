package webapi

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

func TestSigenergyGetsSemanticFlowCardsFromItsCapabilities(t *testing.T) {
	app, root, err := manifest.Load(filepath.Join("..", "..", "plugins", "sigenergy"))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallApp(context.Background(), app, root, ""); err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Language: "nl"})
	defer server.Close()

	devices := []store.Device{
		{ID: "plant", AppID: app.ID, DriverID: "plant", Name: "SigenStor", Class: "other",
			Capabilities: []string{"measure_power.solar", "grid_status"}, State: map[string]any{"measure_power.solar": 2400.0, "grid_status": "on_grid"}},
		{ID: "inverter", AppID: app.ID, DriverID: "inverter", Name: "Omvormer", Class: "solarpanel",
			Capabilities: []string{"measure_power"}, State: map[string]any{"measure_power": 2300.0}},
		{ID: "gateway", AppID: app.ID, DriverID: "gateway", Name: "Gateway", Class: "other",
			Capabilities: []string{"off_grid", "grid_status"}, State: map[string]any{"off_grid": false, "grid_status": "on_grid"}},
	}
	cards := server.capabilityCards(devices)
	triggers := cardTitles(cards["triggers"])
	conditions := cardTitles(cards["conditions"])
	actions := cardTitles(cards["actions"])

	for id, title := range map[string]string{
		"capability.off_grid.on":               "Noodstroomstand werd aan",
		"capability.off_grid.off":              "Noodstroomstand werd uit",
		"capability.grid_status.became":        "Netstand werd",
		"capability.measure_power.rose_above":       "Vermogen kwam boven",
		"capability.measure_power.fell_below":       "Vermogen kwam onder",
		"capability.measure_power.solar.rose_above": "Zon kwam boven",
	} {
		if triggers[id] != title {
			t.Errorf("trigger %s = %q, want %q", id, triggers[id], title)
		}
	}
	for _, id := range []string{"capability.off_grid.is", "capability.grid_status.is", "capability.measure_power.above", "capability.measure_power.below"} {
		if conditions[id] == "" {
			t.Errorf("missing condition %s", id)
		}
	}
	if actions["capability.off_grid.set"] != "Zet Noodstroomstand" {
		t.Errorf("off-grid action = %q", actions["capability.off_grid.set"])
	}

	var gridTransition map[string]any
	for _, card := range cards["triggers"] {
		if card["id"] == "capability.grid_status.became" {
			gridTransition = card
			break
		}
	}
	arguments, _ := gridTransition["args"].([]any)
	var values []any
	for _, raw := range arguments {
		argument, _ := raw.(map[string]any)
		if argument["name"] == "value" {
			values, _ = argument["values"].([]any)
		}
	}
	if len(values) != 5 {
		t.Fatalf("grid transition has no complete status choices: %#v", values)
	}
}
