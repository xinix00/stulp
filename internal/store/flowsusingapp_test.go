package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
)

// flowFixture installs two apps with a device each, so a flow can be checked
// for being affected by one uninstall and untouched by the other.
func flowFixture(t *testing.T) (*Store, Device, Device) {
	t.Helper()
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	devices := make([]Device, 0, 2)
	for _, id := range []string{"com.acme.lights", "com.acme.sensors"} {
		raw := map[string]any{"id": id, "version": "1.0.0", "sdk": 3, "name": map[string]any{"nl": "Acme"}}
		appManifest := &manifest.Manifest{ID: id, Version: "1.0.0", SDK: 3, Raw: raw}
		if err := database.InstallApp(ctx, appManifest, bundle(t, raw), ""); err != nil {
			t.Fatal(err)
		}
		device, err := database.AddDevice(ctx, Device{AppID: id, DriverID: "d", Name: id, Class: "light",
			Capabilities: []string{"onoff"}})
		if err != nil {
			t.Fatal(err)
		}
		devices = append(devices, device)
	}
	return database, devices[0], devices[1]
}

func createFlow(t *testing.T, database *Store, name string, steps ...FlowStep) Flow {
	t.Helper()
	nodes := make([]FlowNode, 0, len(steps))
	edges := make([]FlowEdge, 0, len(steps))
	for index, step := range steps {
		id := name + "-" + step.CardID
		nodes = append(nodes, FlowNode{ID: id, X: float64(index * 200), Step: step})
		if index > 0 {
			edges = append(edges, FlowEdge{ID: id + "-edge", From: nodes[index-1].ID, To: id})
		}
	}
	flow, err := database.CreateFlow(context.Background(), Flow{Name: name, Enabled: true, Nodes: nodes, Edges: edges})
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

func TestFlowsUsingAppFindsCardsAndDeviceArguments(t *testing.T) {
	ctx := context.Background()
	database, light, sensor := flowFixture(t)

	// The app owns the card itself.
	ownCard := createFlow(t, database, "eigen-kaart",
		FlowStep{AppID: "com.acme.lights", CardID: "turned_on", CardType: "trigger"},
		FlowStep{AppID: "stulp", CardID: "notification", CardType: "action",
			Args: map[string]any{"text": "hallo"}})
	// A built-in card that merely points at one of the app's devices breaks
	// just as thoroughly once that device is gone.
	viaDevice := createFlow(t, database, "via-apparaat",
		FlowStep{AppID: "stulp", CardID: "capability.alarm_motion.on", CardType: "trigger",
			Args: map[string]any{"device": map[string]any{"$device": sensor.ID}}},
		FlowStep{AppID: "stulp", CardID: "set_device_capability", CardType: "action",
			Args: map[string]any{"device": map[string]any{"$device": light.ID}, "capability": "onoff", "value": true}})
	// Nothing here belongs to the app being removed.
	untouched := createFlow(t, database, "ongemoeid",
		FlowStep{AppID: "stulp", CardID: "capability.alarm_motion.on", CardType: "trigger",
			Args: map[string]any{"device": map[string]any{"$device": sensor.ID}}},
		FlowStep{AppID: "stulp", CardID: "notification", CardType: "action",
			Args: map[string]any{"text": "hallo"}})

	affected, err := database.FlowsUsingApp(ctx, "com.acme.lights", []string{light.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(affected))
	for _, flow := range affected {
		found[flow.ID] = true
	}
	if !found[ownCard.ID] {
		t.Error("a flow holding the app's own card was not reported")
	}
	if !found[viaDevice.ID] {
		t.Error("a built-in card pointing at the app's device was not reported")
	}
	if found[untouched.ID] {
		t.Error("a flow that never touches the app was reported")
	}
}

// A flow the user built is worth more than the app that happened to supply one
// of its cards, so an uninstall disables it and says why instead of deleting it.
func TestDisableFlowsForExplainsWhyAndKeepsTheFlow(t *testing.T) {
	ctx := context.Background()
	database, light, _ := flowFixture(t)
	created := createFlow(t, database, "keuken",
		FlowStep{AppID: "com.acme.lights", CardID: "turned_on", CardType: "trigger"},
		FlowStep{AppID: "stulp", CardID: "notification", CardType: "action",
			Args: map[string]any{"text": "hallo"}})

	app, devices, err := database.UninstallApp(ctx, "com.acme.lights")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := database.DisableFlowsFor(ctx, app, []string{devices[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0].ID != created.ID {
		t.Fatalf("disabled %#v, want the one affected flow", disabled)
	}
	stored, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatalf("the flow was deleted instead of disabled: %v", err)
	}
	if stored.Enabled {
		t.Error("the flow is still enabled and will fail at its next trigger")
	}
	if stored.LastError == "" {
		t.Error("the flow gives no reason for being off")
	}
	// The app's display name, not its identifier, is what the user recognizes.
	if !strings.Contains(stored.LastError, "Acme") {
		t.Errorf("last error %q does not name the app the way the user knows it", stored.LastError)
	}
	_ = light
}
