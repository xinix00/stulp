package webapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	scenerunner "github.com/xinix00/stulp/internal/scene"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
	"github.com/xinix00/stulp/internal/units"
)

func sceneServer(t *testing.T) (*Server, store.Device) {
	t.Helper()
	root := t.TempDir()
	appJSON := `{
      "id":"com.acme.scene-fixture","version":"1.0.0","sdk":3,"name":{"nl":"Scene fixture"},
      "capabilities":{
        "mode":{"type":"enum","title":{"nl":"Stand"},"getable":true,"setable":true,
          "values":[{"id":"day","title":{"nl":"Dag"}},{"id":"movie","title":{"nl":"Film"}}]},
        "target_wind":{"type":"number","title":{"nl":"Doelwind"},"units":"m/s","min":0,"max":40,"step":0.1,
          "getable":true,"setable":true},
        "read_only":{"type":"number","title":{"nl":"Meting"},"getable":true,"setable":false},
        "speaker_next":{"type":"boolean","title":{"nl":"Volgende"},"getable":false,"setable":true}
      },
      "drivers":[{"id":"room","name":{"nl":"Kamer"},"class":"light",
        "capabilities":["onoff","mode","target_wind","read_only","speaker_next"]}]
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
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	if err := database.InstallApp(ctx, appManifest, resolved, ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: appManifest.ID, DriverID: "room", Name: "Woonkamer", Class: "light",
		Capabilities: []string{"onoff", "mode", "target_wind", "read_only", "speaker_next"},
		State: map[string]any{
			"onoff": false, "mode": "day", "target_wind": 12.0, "read_only": 42.0,
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

func createAPIScene(t *testing.T, server *Server, name string, states ...store.SceneState) store.Scene {
	t.Helper()
	response := request(t, server.Handler(), http.MethodPost, "/api/stulp/scenes", map[string]any{
		"name": name, "states": states,
	}, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("create scene returned %d: %s", response.Code, response.Body.String())
	}
	var created store.Scene
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestSceneAPICreatesListsUpdatesAndActsAsAnOnOffDevice(t *testing.T) {
	server, device := sceneServer(t)
	created := createAPIScene(t, server, "  Film kijken  ",
		store.SceneState{DeviceID: device.ID, CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: device.ID, CapabilityID: "mode", Value: "movie"},
	)
	if created.Name != "Film kijken" || created.Revision != 1 || len(created.States) != 2 {
		t.Fatalf("created scene = %#v", created)
	}

	response := request(t, server.Handler(), http.MethodGet, "/api/stulp/scenes", nil, "")
	var listed []store.Scene
	decodeResponse(t, response, &listed)
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].States[0].Value != true {
		t.Fatalf("listed scenes = %#v", listed)
	}

	created.Name = "Bioscoop"
	response = request(t, server.Handler(), http.MethodPut, "/api/stulp/scenes/"+created.ID, created, "")
	if response.Code != http.StatusOK {
		t.Fatalf("update scene returned %d: %s", response.Code, response.Body.String())
	}
	var updated store.Scene
	decodeResponse(t, response, &updated)
	if updated.Name != "Bioscoop" || updated.Revision != 2 || updated.ID != created.ID {
		t.Fatalf("updated scene = %#v", updated)
	}

	// The old editor revision cannot overwrite the newer name and states.
	response = request(t, server.Handler(), http.MethodPut, "/api/stulp/scenes/"+created.ID, created, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("stale update returned %d: %s", response.Code, response.Body.String())
	}

	virtualID := store.SceneDeviceID(created.ID)
	virtual, err := server.store.Device(context.Background(), virtualID)
	if err != nil {
		t.Fatal(err)
	}
	if virtual.AppID != store.NativeSceneAppID || virtual.Class != "scene" ||
		len(virtual.Capabilities) != 1 || virtual.Capabilities[0] != "onoff" || virtual.State["onoff"] != false ||
		virtual.Data["sceneId"] != created.ID {
		t.Fatalf("scene device = %#v", virtual)
	}

	var calls []store.SceneState
	server.scenes = scenerunner.New(server.store, func(_ context.Context, deviceID, capabilityID string, value any, options map[string]any) error {
		if len(options) != 0 {
			t.Fatalf("scene invocation options = %#v", options)
		}
		calls = append(calls, store.SceneState{DeviceID: deviceID, CapabilityID: capabilityID, Value: value})
		return nil
	})
	response = request(t, server.Handler(), http.MethodPut, "/api/manager/devices/device/"+virtualID+"/capability/onoff", map[string]any{"value": true}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("turn scene on returned %d: %s", response.Code, response.Body.String())
	}
	if len(calls) != 2 {
		t.Fatalf("turn-on calls = %#v", calls)
	}
	if calls[0].DeviceID != device.ID || calls[0].CapabilityID != "onoff" || calls[0].Value != true ||
		calls[1].CapabilityID != "mode" || calls[1].Value != "movie" {
		t.Fatalf("scene sent wrong canonical states: %#v", calls)
	}
	// Accepted is not reported: device state remains authoritative until the
	// integration publishes what the hardware actually did.
	reported, err := server.store.Device(context.Background(), device.ID)
	if err != nil || reported.State["onoff"] != false {
		t.Fatalf("scene activation optimistically changed state: device=%#v err=%v", reported, err)
	}
	definition, err := server.store.Scene(context.Background(), created.ID)
	if err != nil || !definition.Active || len(definition.Previous) != 2 {
		t.Fatalf("active scene = %#v err=%v", definition, err)
	}
	virtual, err = server.store.Device(context.Background(), virtualID)
	if err != nil || virtual.State["onoff"] != true {
		t.Fatalf("scene device did not report on: %#v err=%v", virtual, err)
	}

	// Runtime restore state is protected: changing the definition while it is on
	// could otherwise make "uit" restore an unrelated set of properties.
	updated.Name = "Mag nog niet"
	response = request(t, server.Handler(), http.MethodPut, "/api/stulp/scenes/"+created.ID, updated, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("editing an active scene returned %d: %s", response.Code, response.Body.String())
	}
	response = request(t, server.Handler(), http.MethodDelete, "/api/manager/devices/device/"+virtualID, nil, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("deleting an active scene device returned %d: %s", response.Code, response.Body.String())
	}

	response = request(t, server.Handler(), http.MethodPut, "/api/manager/devices/device/"+virtualID+"/capability/onoff", map[string]any{"value": false}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("turn scene off returned %d: %s", response.Code, response.Body.String())
	}
	if len(calls) != 4 || calls[2].Value != false || calls[3].Value != "day" {
		t.Fatalf("restore calls = %#v", calls)
	}
	definition, err = server.store.Scene(context.Background(), created.ID)
	if err != nil || definition.Active || len(definition.Previous) != 0 {
		t.Fatalf("restored scene = %#v err=%v", definition, err)
	}

	// A DAN-step takes exactly the normal onoff path; there is no scene-only
	// Flow card or callback in the engine.
	for _, on := range []bool{true, false} {
		result, runErr := server.flows.RunAction(context.Background(), store.FlowStep{
			AppID: "stulp", CardID: "capability.onoff.set", CardType: "action",
			Args: map[string]any{"device": map[string]any{"$device": virtualID}, "value": on},
		})
		if runErr != nil || result.Result != true {
			t.Fatalf("generic onoff DAN=%t result=%#v err=%v", on, result, runErr)
		}
	}
	if len(calls) != 8 {
		t.Fatalf("generic Flow did not apply and restore every state: %#v", calls)
	}
}

func TestSceneDeviceRenameAndDeleteUseTheSceneLifecycle(t *testing.T) {
	server, device := sceneServer(t)
	created := createAPIScene(t, server, "Avond",
		store.SceneState{DeviceID: device.ID, CapabilityID: "onoff", Value: true})
	deviceID := store.SceneDeviceID(created.ID)
	response := request(t, server.Handler(), http.MethodGet, "/api/manager/devices/device", nil, "")
	var devices map[string]map[string]any
	decodeResponse(t, response, &devices)
	if devices[deviceID]["class"] != "scene" || devices[deviceID]["manufacturer"] != "Stulp" {
		t.Fatalf("scene is not exposed as a normal Manage device: %#v", devices[deviceID])
	}

	response = request(t, server.Handler(), http.MethodPut, "/api/manager/devices/device/"+deviceID,
		map[string]any{"name": "Late avond"}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("rename scene device returned %d: %s", response.Code, response.Body.String())
	}
	renamed, err := server.store.Scene(context.Background(), created.ID)
	if err != nil || renamed.Name != "Late avond" || renamed.Revision != created.Revision+1 {
		t.Fatalf("renamed scene = %#v err=%v", renamed, err)
	}

	response = request(t, server.Handler(), http.MethodDelete, "/api/manager/devices/device/"+deviceID, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete scene device returned %d: %s", response.Code, response.Body.String())
	}
	if _, err := server.store.Scene(context.Background(), created.ID); err == nil {
		t.Fatal("deleting the scene device left its scene behind")
	}
	if _, err := server.store.Device(context.Background(), deviceID); err == nil {
		t.Fatal("deleting the scene device left its synthetic device behind")
	}
}

func TestSceneAPIRejectsAnythingThatIsNotWritableState(t *testing.T) {
	server, device := sceneServer(t)
	other := createAPIScene(t, server, "Andere scene",
		store.SceneState{DeviceID: device.ID, CapabilityID: "onoff", Value: true})
	tests := []struct {
		name         string
		deviceID     string
		capabilityID string
		value        any
	}{
		{"read only", device.ID, "read_only", 1.0},
		{"write-only command", device.ID, "speaker_next", true},
		{"missing capability", device.ID, "does_not_exist", true},
		{"invalid enum", device.ID, "mode", "party"},
		{"invalid type", device.ID, "onoff", "yes"},
		{"outside range", device.ID, "target_wind", 99.0},
		{"missing device", "missing-device", "onoff", true},
		{"nested scene", store.SceneDeviceID(other.ID), "onoff", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := request(t, server.Handler(), http.MethodPost, "/api/stulp/scenes", map[string]any{
				"name": "Ongeldig", "states": []any{map[string]any{
					"deviceId": test.deviceID, "capabilityId": test.capabilityID, "value": test.value,
				}},
			}, "")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid scene returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSceneValuesRoundTripWithoutUnitDrift(t *testing.T) {
	server, device := sceneServer(t)
	server.rememberUnits(units.Set{Wind: "Bft"})
	created := createAPIScene(t, server, "Windstand",
		store.SceneState{DeviceID: device.ID, CapabilityID: "target_wind", Value: 6.0})
	stored, err := server.store.Scene(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.States[0].Value != 12.0 {
		t.Fatalf("capturing displayed 6 Bft stored %#v, want the current 12 m/s", stored.States[0].Value)
	}
	if created.States[0].Value != 6.0 {
		t.Fatalf("scene reads as %#v Bft, want 6", created.States[0].Value)
	}

	response := request(t, server.Handler(), http.MethodPut, "/api/stulp/scenes/"+created.ID, created, "")
	if response.Code != http.StatusOK {
		t.Fatalf("unchanged unit roundtrip returned %d: %s", response.Code, response.Body.String())
	}
	stored, err = server.store.Scene(context.Background(), created.ID)
	if err != nil || stored.States[0].Value != 12.0 {
		t.Fatalf("saving without edits drifted to %#v: %v", stored.States[0].Value, err)
	}

	var shown store.Scene
	decodeResponse(t, response, &shown)
	shown.States[0].Value = 7.0
	response = request(t, server.Handler(), http.MethodPut, "/api/stulp/scenes/"+created.ID, shown, "")
	if response.Code != http.StatusOK {
		t.Fatalf("changed unit update returned %d: %s", response.Code, response.Body.String())
	}
	stored, err = server.store.Scene(context.Background(), created.ID)
	if err != nil || stored.States[0].Value != 13.9 {
		t.Fatalf("7 Bft stored as %#v m/s, want 13.9: %v", stored.States[0].Value, err)
	}
}

func TestSceneDeletionRefusesToBreakAFlow(t *testing.T) {
	server, device := sceneServer(t)
	created := createAPIScene(t, server, "Nacht",
		store.SceneState{DeviceID: device.ID, CapabilityID: "onoff", Value: false})
	flow, err := server.store.CreateFlow(context.Background(), store.Flow{
		Name: "Gebruik scene", Nodes: []store.FlowNode{{ID: "scene", Step: store.FlowStep{
			AppID: "stulp", CardID: "capability.onoff.set", CardType: "action",
			Args: map[string]any{"device": map[string]any{"$device": store.SceneDeviceID(created.ID)}, "value": true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server.Handler(), http.MethodDelete, "/api/stulp/scenes/"+created.ID, nil, "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), flow.Name) {
		t.Fatalf("delete used scene returned %d: %s", response.Code, response.Body.String())
	}
	if err := server.store.DeleteFlow(context.Background(), flow.ID); err != nil {
		t.Fatal(err)
	}
	response = request(t, server.Handler(), http.MethodDelete, "/api/stulp/scenes/"+created.ID, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete unused scene returned %d: %s", response.Code, response.Body.String())
	}
}

func TestSceneDeviceAddConfigurationAndRealtimeWiringAreServed(t *testing.T) {
	server, _ := sceneServer(t)
	page := request(t, server.Handler(), http.MethodGet, "/", nil, "").Body.String()
	for _, needle := range []string{
		`id="add-device"`, `id="scene-dialog"`, `id="scene-capture"`,
		`class="page-heading"`, `id="flow-dialog" class="flow-dialog settings-shell"`,
		`class="flow-editor-toolbar"`, `class="actions flow-editor-actions"`,
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("management UI page misses %q", needle)
		}
	}
	asset := request(t, server.Handler(), http.MethodGet, "/assets/app.js", nil, "").Body.String()
	for _, needle := range []string{
		"scene.addEventListener('click', () => openScene())", "renderSceneConfiguration", "sceneForDevice",
		"Standen configureren", "captureWholeScene", "event.manager === 'scene'",
		"overview-card flow-overview-card", "compactIconButton",
	} {
		if !strings.Contains(asset, needle) {
			t.Errorf("management UI asset misses %q", needle)
		}
	}
	stylesheet := request(t, server.Handler(), http.MethodGet, "/assets/style.css", nil, "").Body.String()
	for _, needle := range []string{
		"--control-height: 36px", ".overview-card {", ".overview-card-status {",
		".scene-config-preview {", ".flow-editor-toolbar {", ".compact-icon-button {",
	} {
		if !strings.Contains(stylesheet, needle) {
			t.Errorf("management stylesheet misses %q", needle)
		}
	}
	for _, stale := range []string{
		`data-page="scenes"`, `id="scenes-page"`, "renderScenes", "overview-card scene-card",
		".scene-list {", ".scene-card {", "#7c83ff22", ".flow-icon-button",
	} {
		if strings.Contains(page, stale) || strings.Contains(stylesheet, stale) || strings.Contains(asset, stale) {
			t.Errorf("management UI still contains obsolete styling %q", stale)
		}
	}
}
