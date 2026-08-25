package webapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

func TestManageBootstrapKeepsFiftyDeviceStartupCompact(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	capabilityDefinitions := make(map[string]any, 30)
	capabilityIDs := make([]string, 0, 30)
	capabilityIDs = append(capabilityIDs, "onoff")
	capabilityDefinitions["onoff"] = map[string]any{
		"type": "boolean", "setable": true, "title": map[string]any{"nl": "Aan of uit", "en": "On or off"},
	}
	for index := 1; index < 30; index++ {
		id := fmt.Sprintf("measure_metric_%02d", index)
		capabilityIDs = append(capabilityIDs, id)
		capabilityDefinitions[id] = map[string]any{
			"type": "number", "setable": false, "units": "unit",
			"title": map[string]any{"nl": "Uitgebreide meetwaarde", "en": "Detailed measurement"},
			"desc":  "Metadata that belongs in an automation editor, not in the initial tile payload.",
		}
	}
	appManifest := &manifest.Manifest{ID: "com.example.compact", Version: "1.0.0", SDK: 3, Raw: map[string]any{
		"id": "com.example.compact", "name": map[string]any{"en": "Compact fixture"},
		"manufacturer": "Example Industries", "capabilities": capabilityDefinitions,
	}}
	if err := database.InstallApp(ctx, appManifest, "", ""); err != nil {
		t.Fatal(err)
	}
	group, err := database.CreateDeviceGroup(ctx, store.DeviceGroup{Name: "Living room"})
	if err != nil {
		t.Fatal(err)
	}

	var first store.Device
	for deviceIndex := 0; deviceIndex < 50; deviceIndex++ {
		data := make(map[string]any, 30)
		settings := make(map[string]any, 30)
		state := make(map[string]any, 30)
		for attributeIndex, capabilityID := range capabilityIDs {
			data[fmt.Sprintf("attribute_%02d", attributeIndex)] = fmt.Sprintf("device-%02d-private-value-%02d", deviceIndex, attributeIndex)
			settings[fmt.Sprintf("setting_%02d", attributeIndex)] = strings.Repeat(fmt.Sprintf("%d", attributeIndex%10), 32)
			state[capabilityID] = float64(deviceIndex*100 + attributeIndex)
		}
		state["onoff"] = deviceIndex%2 == 0
		device, addErr := database.AddDevice(ctx, store.Device{
			ID: fmt.Sprintf("compact-%02d", deviceIndex), AppID: appManifest.ID, DriverID: "sensor",
			GroupID: group.ID, Name: fmt.Sprintf("Sensor %02d", deviceIndex), Class: "sensor",
			Data: data, Settings: settings, Capabilities: capabilityIDs, State: state,
		})
		if addErr != nil {
			t.Fatal(addErr)
		}
		if deviceIndex == 0 {
			first = device
		}
	}

	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{StulpVersion: "0.8.5", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	fullResponse := request(t, server.Handler(), http.MethodGet, "/api/manager/devices/device", nil, "")
	if fullResponse.Code != http.StatusOK {
		t.Fatalf("full device list returned %d: %s", fullResponse.Code, fullResponse.Body.String())
	}
	bootstrapResponse := request(t, server.Handler(), http.MethodGet, "/api/stulp/manage/bootstrap", nil, "")
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("Manage bootstrap returned %d: %s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	if bootstrapResponse.Body.Len()*4 >= fullResponse.Body.Len() {
		t.Fatalf("bootstrap is not at least 75%% smaller than the full device list: compact=%d full=%d", bootstrapResponse.Body.Len(), fullResponse.Body.Len())
	}
	t.Logf("50 devices x 30 attributes: bootstrap=%d bytes, full=%d bytes (%.1f%% smaller)",
		bootstrapResponse.Body.Len(), fullResponse.Body.Len(),
		100*(1-float64(bootstrapResponse.Body.Len())/float64(fullResponse.Body.Len())))

	var bootstrap map[string]any
	decodeResponse(t, bootstrapResponse, &bootstrap)
	if bootstrap["ok"] != true || bootstrap["stulpVersion"] != "0.8.5" {
		t.Fatalf("bootstrap health/version are missing: %#v", bootstrap)
	}
	groups, _ := bootstrap["deviceGroups"].([]any)
	devices, _ := bootstrap["devices"].(map[string]any)
	if len(groups) != 1 || len(devices) != 50 {
		t.Fatalf("bootstrap does not contain the initial layout: groups=%d devices=%d", len(groups), len(devices))
	}
	overview, _ := devices[first.ID].(map[string]any)
	assertCompactDeviceHasNoPrivateDetail(t, overview)
	if overview["capabilitiesComplete"] != false || overview["detailComplete"] != false || overview["quickCapability"] != "onoff" {
		t.Fatalf("overview completeness/quick capability is wrong: %#v", overview)
	}
	if _, duplicated := overview["quickCapabilityId"]; duplicated {
		t.Fatalf("overview duplicates its quick capability id: %#v", overview)
	}
	compactCapabilities, _ := overview["capabilities"].([]any)
	compactCapabilityObjects, _ := overview["capabilitiesObj"].(map[string]any)
	if len(compactCapabilities) != 1 || len(compactCapabilityObjects) != 1 || compactCapabilityObjects["onoff"] == nil {
		t.Fatalf("overview sent more than its primary capability: %#v", overview)
	}
	overviewResponse := request(t, server.Handler(), http.MethodGet, "/api/manager/devices/device?view=overview", nil, "")
	if overviewResponse.Code != http.StatusOK {
		t.Fatalf("overview device list returned %d: %s", overviewResponse.Code, overviewResponse.Body.String())
	}
	var overviewDevices map[string]map[string]any
	decodeResponse(t, overviewResponse, &overviewDevices)
	if overviewDevices[first.ID]["quickCapability"] != "onoff" {
		t.Fatalf("overview query does not use the bootstrap projection: %#v", overviewDevices[first.ID])
	}

	automationResponse := request(t, server.Handler(), http.MethodGet, "/api/manager/devices/device?view=automation", nil, "")
	if automationResponse.Code != http.StatusOK {
		t.Fatalf("automation device list returned %d: %s", automationResponse.Code, automationResponse.Body.String())
	}
	var automationDevices map[string]map[string]any
	decodeResponse(t, automationResponse, &automationDevices)
	automation := automationDevices[first.ID]
	assertCompactDeviceHasNoPrivateDetail(t, automation)
	allCapabilities, _ := automation["capabilitiesObj"].(map[string]any)
	if automation["capabilitiesComplete"] != true || automation["detailComplete"] != false || len(allCapabilities) != 30 {
		t.Fatalf("automation catalog is not complete: %#v", automation)
	}
	if automation["manufacturer"] != "Example Industries" {
		t.Fatalf("automation identity lost its manufacturer: %#v", automation)
	}

	var fullDevices map[string]map[string]any
	decodeResponse(t, fullResponse, &fullDevices)
	if fullDevices[first.ID]["settings"] == nil || fullDevices[first.ID]["data"] == nil {
		t.Fatalf("the backwards-compatible full device response was made compact: %#v", fullDevices[first.ID])
	}

	bad := request(t, server.Handler(), http.MethodGet, "/api/manager/devices/device?view=everything", nil, "")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown device view returned %d: %s", bad.Code, bad.Body.String())
	}
	badEvents := request(t, server.Handler(), http.MethodGet, "/api/stulp/events?view=automation", nil, "")
	if badEvents.Code != http.StatusBadRequest {
		t.Fatalf("unknown event view returned %d: %s", badEvents.Code, badEvents.Body.String())
	}
}

func TestOverviewQuickCapabilitySelectionAndRealtimeShape(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{})
	defer server.Close()

	battery := store.Device{
		ID: "battery", AppID: "com.example.energy", DriverID: "battery", Name: "Battery", Class: "battery", Available: true,
		Capabilities: []string{"measure_temperature", "measure_battery", "measure_power"},
		State:        map[string]any{"measure_temperature": 30.0, "measure_battery": 98.0, "measure_power": -840.0},
	}
	batteryOverview := server.deviceOverviewObject(battery)
	if batteryOverview["quickCapability"] != "measure_power" {
		t.Fatalf("battery tile selected %#v instead of power", batteryOverview["quickCapability"])
	}

	cover := store.Device{
		ID: "cover", AppID: "com.example.cover", DriverID: "cover", Name: "Awning", Class: "blinds", Available: true,
		Capabilities: []string{"measure_temperature", "windowcoverings_set", "windowcoverings_state"},
		State:        map[string]any{"measure_temperature": 22.0, "windowcoverings_set": 0.4, "windowcoverings_state": "idle"},
	}
	coverOverview := server.deviceOverviewObject(cover)
	coverCapabilities, _ := coverOverview["capabilitiesObj"].(map[string]any)
	if coverOverview["quickCapability"] != "windowcoverings_state" || len(coverCapabilities) != 2 || coverCapabilities["windowcoverings_set"] == nil {
		t.Fatalf("window covering overview lost its position companion: %#v", coverOverview)
	}

	suffixed := store.Device{
		ID: "switch", AppID: "com.example.switch", DriverID: "switch", Name: "Switch", Class: "socket", Available: true,
		Capabilities: []string{"measure_temperature", "onoff.2"}, State: map[string]any{"measure_temperature": 20.0, "onoff.2": true},
	}
	if overview := server.deviceOverviewObject(suffixed); overview["quickCapability"] != "onoff.2" {
		t.Fatalf("suffixed capability did not share base priority: %#v", overview)
	}
	sceneDevice := store.Device{
		ID: store.SceneDeviceID("evening"), AppID: store.NativeSceneAppID, DriverID: "scene",
		Name: "Evening", Class: "scene", Available: true, Data: map[string]any{"sceneId": "evening", "private": "omit me"},
		Capabilities: []string{"onoff"}, State: map[string]any{"onoff": false},
	}
	sceneAutomation := server.deviceAutomationObject(sceneDevice)
	if sceneAutomation["sceneId"] != "evening" {
		t.Fatalf("scene automation identity lost its scene id: %#v", sceneAutomation)
	}
	assertCompactDeviceHasNoPrivateDetail(t, sceneAutomation)

	compactEvent := server.realtimeOverviewEvent(store.Event{
		Manager: "devices", Type: "device.update", ID: cover.ID, Data: cover,
	})
	compact, ok := compactEvent.Data.(map[string]any)
	if !ok {
		t.Fatalf("compact realtime event has unexpected data: %#v", compactEvent.Data)
	}
	values, _ := compact["capabilityValues"].(map[string]any)
	if len(values) != 3 || values["measure_temperature"] != 22.0 {
		t.Fatalf("compact realtime event cannot refresh a detailed cache: %#v", compact)
	}
	assertCompactDeviceHasNoPrivateDetail(t, compact)

	fullEvent := server.realtimeEvent(store.Event{
		Manager: "devices", Type: "device.update", ID: cover.ID, Data: cover,
	})
	full, ok := fullEvent.Data.(map[string]any)
	if !ok || full["data"] == nil || full["settings"] == nil {
		t.Fatalf("default realtime shape is no longer backwards compatible: %#v", fullEvent.Data)
	}
}

func assertCompactDeviceHasNoPrivateDetail(t *testing.T, device map[string]any) {
	t.Helper()
	for _, forbidden := range []string{"data", "settings", "settingsObj", "store", "hardwareName", "note", "warningMessage", "energy"} {
		if _, exists := device[forbidden]; exists {
			t.Errorf("compact device unexpectedly contains %q: %#v", forbidden, device[forbidden])
		}
	}
}
