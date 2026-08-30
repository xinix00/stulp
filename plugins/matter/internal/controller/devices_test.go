package controller

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestNativeMatterEndpointsBecomeOneDevice(t *testing.T) {
	devices := []Device{
		{
			ID: "light", DriverID: "matter", Name: "Light · 1", Class: "light",
			Capabilities: []string{"onoff"}, State: map[string]any{"onoff": true}, Available: true,
			Data:  map[string]any{"id": "light", "endpoint": uint16(1)},
			Store: testMatterStore(1, map[string]uint16{"onoff": 1}, onOffCluster),
		},
		{
			ID: "button-4", DriverID: "matter", Name: "Light · 4", Class: "sensor",
			Capabilities: []string{"button"}, State: map[string]any{"button": false},
			Store: testMatterStore(4, map[string]uint16{"button": 4}, switchCluster),
		},
		{
			ID: "button-5", DriverID: "matter", Name: "Light · 5", Class: "sensor",
			Capabilities: []string{"button"}, State: map[string]any{"button": false},
			Store: testMatterStore(5, map[string]uint16{"button": 5}, switchCluster),
		},
	}
	combined, replacements := combineNativeEndpoints(devices)
	if len(combined) != 1 {
		t.Fatalf("combined devices = %d, want one physical light", len(combined))
	}
	light := combined[0]
	if light.Name != "Light" || light.ID != "light" || light.Class != "light" {
		t.Fatalf("combined light identity = %q/%q/%q", light.ID, light.Name, light.Class)
	}
	for _, capability := range []string{"onoff", "button.1", "button.2"} {
		if !slices.Contains(light.Capabilities, capability) {
			t.Fatalf("combined capabilities %v omit %q", light.Capabilities, capability)
		}
	}
	if got := capabilityEndpoint(light, "onoff", 0); got != 1 {
		t.Fatalf("onoff endpoint = %d, want 1", got)
	}
	if got := capabilityEndpoint(light, "button.1", 0); got != 4 {
		t.Fatalf("first button endpoint = %d, want 4", got)
	}
	if got := capabilityEndpoint(light, "button.2", 0); got != 5 {
		t.Fatalf("second button endpoint = %d, want 5", got)
	}
	if replacements["button-5"].capabilities["button"] != "button.2" {
		t.Fatalf("button replacement = %#v", replacements["button-5"])
	}
}

func TestBridgedEnvironmentalEndpointsStaySeparate(t *testing.T) {
	temperature := testMatterSensor("temperature", 1, "measure_temperature", 20.0, temperatureCluster)
	humidity := testMatterSensor("humidity", 2, "measure_humidity", 50.0, humidityCluster)
	temperature.Store["matter.bridged"] = true
	humidity.Store["matter.bridged"] = true
	if combined, _ := combineNativeEndpoints([]Device{temperature, humidity}); len(combined) != 2 {
		t.Fatalf("two bridged accessories were collapsed into %d device(s)", len(combined))
	}
}

func TestCombinedEndpointSubscriptionPathsUseCapabilityEndpoint(t *testing.T) {
	combined, _ := combineNativeEndpoints([]Device{
		testMatterSensor("temperature", 1, "measure_temperature", 21.0, temperatureCluster),
		testMatterSensor("humidity", 2, "measure_humidity", 55.0, humidityCluster),
	})
	attributes, events := subscriptionPaths(combined)
	if len(attributes) != 2 || len(events) != 2 {
		t.Fatalf("subscription paths = %d attributes/%d events, want 2/2", len(attributes), len(events))
	}
	want := map[string]bool{
		"1/1026/0": false,
		"2/1029/0": false,
	}
	for _, path := range attributes {
		if path.Endpoint == nil || path.Cluster == nil || path.Attribute == nil {
			t.Fatalf("subscription contains a wildcard path: %#v", path)
		}
		key := endpointPathKey(*path.Endpoint, *path.Cluster, *path.Attribute)
		if _, exists := want[key]; !exists {
			t.Fatalf("unexpected subscription path %s", key)
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("subscription omitted %s", key)
		}
	}
	eventEndpoints := map[uint16]bool{1: false, 2: false}
	for _, path := range events {
		if path.Endpoint == nil || path.Urgent == nil || !*path.Urgent {
			t.Fatalf("automation event path is not urgent: %#v", path)
		}
		if _, expected := eventEndpoints[*path.Endpoint]; !expected {
			t.Fatalf("unexpected event endpoint %d", *path.Endpoint)
		}
		eventEndpoints[*path.Endpoint] = true
	}
	for endpoint, seen := range eventEndpoints {
		if !seen {
			t.Fatalf("subscription omitted event endpoint %d", endpoint)
		}
	}
}

func TestFourButtonSwitchRequestsUrgentReportsForWirelessButtons(t *testing.T) {
	capabilityEndpoints := map[string]uint16{
		"onoff.1": 1, "onoff.2": 2,
		"button.1": 4, "button.2": 5, "button.3": 6, "button.4": 7,
	}
	device := Device{
		ID: "aqara-h2", DriverID: "matter", Name: "Aqara H2", Class: "light",
		Capabilities: []string{"onoff.1", "onoff.2", "button.1", "button.2", "button.3", "button.4"},
		Store:        testMatterStore(1, capabilityEndpoints, onOffCluster, switchCluster),
	}
	device.Store["matter.endpoints"] = []uint16{1, 2, 4, 5, 6, 7}
	_, events := subscriptionPaths([]Device{device})
	urgentEndpoints := make(map[uint16]bool, len(events))
	for _, path := range events {
		if path.Endpoint != nil && path.Urgent != nil && *path.Urgent {
			urgentEndpoints[*path.Endpoint] = true
		}
	}
	for _, endpoint := range []uint16{4, 5, 6, 7} {
		if !urgentEndpoints[endpoint] {
			t.Fatalf("wireless button endpoint %d is not subscribed as urgent: %#v", endpoint, events)
		}
	}
}

func TestExistingMotionSensorGainsIlluminanceWithoutRecommissioning(t *testing.T) {
	device := testMatterSensor("motion", 3, "alarm_motion", false, occupancyCluster)
	device.Store["matter.serverClusters"] = []string{clusterHex(occupancyCluster), clusterHex(illuminanceCluster)}
	database := newBacking()
	ctx := context.Background()
	added, err := database.AddDevice(ctx, device)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileNativeDevices(ctx, database); err != nil {
		t.Fatal(err)
	}
	upgraded, err := database.Device(ctx, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(upgraded.Capabilities, "measure_luminance") {
		t.Fatalf("upgraded capabilities = %v", upgraded.Capabilities)
	}
	if got := capabilityEndpoint(upgraded, "measure_luminance", 0); got != 3 {
		t.Fatalf("illuminance endpoint = %d, want motion endpoint 3", got)
	}
	if err := reconcileNativeDevices(ctx, database); err != nil {
		t.Fatal(err)
	}
	reconciled, err := database.Device(ctx, added.ID)
	if err != nil || len(reconciled.Capabilities) != 2 {
		t.Fatalf("idempotent reconciliation = %#v, %v", reconciled.Capabilities, err)
	}
}

func TestRefreshedNodeModelRecoversPreviouslyUnknownEndpoint(t *testing.T) {
	oldEndpoints, _ := combineNativeEndpoints([]Device{
		testMatterSensor("motion", 1, "alarm_motion", false, occupancyCluster),
		testMatterSensor("temperature", 3, "measure_temperature", 21.0, temperatureCluster),
		testMatterSensor("humidity", 4, "measure_humidity", 52.0, humidityCluster),
		testMatterSensor("battery", 5, "measure_battery", 86.0, powerSourceCluster),
	})
	old := oldEndpoints[0]
	delete(old.Store, "matter.modelVersion")
	old.Name = "Presence Multi-Sensor FP300"
	old.PreserveHardwareName()
	old.Name = "Gang sensor"
	old.GroupID = "first-floor/bedroom"
	old.Settings = map[string]any{"sensitivity": "high"}
	old.Data["vendorId"] = uint16(4447)
	old.Data["productId"] = uint16(8197)

	database := newBacking()
	ctx := context.Background()
	added, err := database.AddDevice(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	refreshedEndpoints, _ := combineNativeEndpoints([]Device{
		testMatterSensor("motion", 1, "alarm_motion", false, occupancyCluster),
		testMatterSensor("light", 2, "measure_luminance", 14.5, illuminanceCluster),
		testMatterSensor("temperature", 3, "measure_temperature", 21.5, temperatureCluster),
		testMatterSensor("humidity", 4, "measure_humidity", 53.0, humidityCluster),
		testMatterSensor("battery", 5, "measure_battery", 85.0, powerSourceCluster),
	})
	if err := persistRefreshedNodeModel(ctx, database, []Device{added}, refreshedEndpoints); err != nil {
		t.Fatal(err)
	}

	updated, err := database.Device(ctx, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != added.ID || updated.Name != "Gang sensor" || updated.HardwareName() != "Presence Multi-Sensor FP300" {
		t.Fatalf("refreshed identity = id %q, name %q, hardware %q", updated.ID, updated.Name, updated.HardwareName())
	}
	if updated.GroupID != old.GroupID || updated.Settings["sensitivity"] != "high" {
		t.Fatalf("refreshed user configuration = group %q, settings %#v", updated.GroupID, updated.Settings)
	}
	if !slices.Contains(updated.Capabilities, "measure_luminance") {
		t.Fatalf("refreshed capabilities = %v", updated.Capabilities)
	}
	if endpoint := capabilityEndpoint(updated, "measure_luminance", 0); endpoint != 2 {
		t.Fatalf("illuminance endpoint = %d, want 2", endpoint)
	}
	if value := updated.State["measure_luminance"]; value != 14.5 {
		t.Fatalf("initial illuminance = %v, want 14.5", value)
	}
	if endpoints := deviceEndpoints(updated); !slices.Equal(endpoints, []uint16{1, 2, 3, 4, 5}) {
		t.Fatalf("refreshed endpoints = %v", endpoints)
	}
	if modelRefreshRequired([]Device{updated}) {
		t.Fatal("current model was left marked for another refresh")
	}
	// Het hoofd-endpoint houdt zijn id, dus er valt niets te vervangen. Zou dat
	// wel gebeuren, dan wees elke Flow van de gebruiker ineens naar een ander
	// apparaat.
	if replaced := database.replacements(); len(replaced) != 0 {
		t.Fatalf("een verversing meldde een vervanging: %#v", replaced)
	}
}

func TestReconcileNativeDevicesPreservesFlowReferences(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	temperature := testMatterSensor("temperature", 1, "measure_temperature", 21.0, temperatureCluster)
	humidity := testMatterSensor("humidity", 2, "measure_humidity", 55.0, humidityCluster)
	if _, err := database.AddDevice(ctx, temperature); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AddDevice(ctx, humidity); err != nil {
		t.Fatal(err)
	}
	if err := reconcileNativeDevices(ctx, database); err != nil {
		t.Fatal(err)
	}
	devices, err := database.Devices(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("reconciled devices = %v error %v", devices, err)
	}
	if devices[0].ID != temperature.ID || !slices.Contains(devices[0].Capabilities, "measure_humidity") {
		t.Fatalf("unexpected reconciled device: %#v", devices[0])
	}
	// Het vochtapparaat is opgegaan in het temperatuurapparaat. De controller
	// herschrijft geen Flows -- hij meldt de vervanging, en Stulp doet de rest.
	replaced := database.replacements()
	if got, ok := replaced[humidity.ID]; !ok || got.DeviceID != temperature.ID {
		t.Fatalf("vervanging = %#v, wil %s -> %s", replaced, humidity.ID, temperature.ID)
	}
}

func TestReconcileRepeatedCapabilityRewritesFlowCapability(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	light := Device{
		ID: "light", DriverID: "matter", Name: "Light · 1", Class: "light",
		Capabilities: []string{"onoff"}, State: map[string]any{"onoff": true}, Available: true,
		Data:  map[string]any{"id": "light", "endpoint": uint16(1)},
		Store: testMatterStore(1, map[string]uint16{"onoff": 1}, onOffCluster),
	}
	button4 := Device{
		ID: "button-4", DriverID: "matter", Name: "Light · 4", Class: "sensor",
		Capabilities: []string{"button"}, State: map[string]any{"button": false}, Available: true,
		Data:  map[string]any{"id": "button-4", "endpoint": uint16(4)},
		Store: testMatterStore(4, map[string]uint16{"button": 4}, switchCluster),
	}
	button5 := button4
	button5.ID, button5.Name = "button-5", "Light · 5"
	button5.Data = map[string]any{"id": "button-5", "endpoint": uint16(5)}
	button5.Store = testMatterStore(5, map[string]uint16{"button": 5}, switchCluster)
	for _, device := range []Device{light, button4, button5} {
		if _, err := database.AddDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcileNativeDevices(ctx, database); err != nil {
		t.Fatal(err)
	}
	// De tweede knop is de tweede geworden op één apparaat: hij verhuist naar
	// het licht en zijn capability heet nu button.2. Beide moeten gemeld worden,
	// want zonder de tweede wijst een Flow straks naar een naam die niet meer
	// bestaat.
	replaced := database.replacements()
	got, ok := replaced[button5.ID]
	if !ok || got.DeviceID != light.ID {
		t.Fatalf("vervanging = %#v, wil %s -> %s", replaced, button5.ID, light.ID)
	}
	if got.Capabilities["button"] != "button.2" {
		t.Fatalf("capability-hernoeming = %#v, wil button -> button.2", got.Capabilities)
	}
}

func testMatterSensor(id string, endpoint uint16, capability string, value any, cluster uint32) Device {
	return Device{
		ID: id, DriverID: "matter", Name: "Sensor · " + fmtUint(endpoint), Class: "sensor",
		Capabilities: []string{capability}, State: map[string]any{capability: value}, Available: true,
		Data: map[string]any{"id": id, "endpoint": endpoint},
		Store: map[string]any{
			"manufacturer": "Test", "matter.nodeId": "0000000000001234", "matter.endpoint": endpoint,
			"matter.address": "127.0.0.1:5540", "matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
			"matter.endpoints": []uint16{endpoint}, "matter.capabilityEndpoints": map[string]uint16{capability: endpoint},
			"matter.serverClusters": []string{clusterHex(cluster)}, "matter.deviceTypes": []string{"0x302"},
			"matter.modelVersion": matterModelVersion,
		},
	}
}

func testMatterStore(endpoint uint16, capabilityEndpoints map[string]uint16, clusters ...uint32) map[string]any {
	serverClusters := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		serverClusters = append(serverClusters, clusterHex(cluster))
	}
	return map[string]any{
		"manufacturer": "Test", "matter.nodeId": "0000000000001234", "matter.endpoint": endpoint,
		"matter.address": "127.0.0.1:5540", "matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
		"matter.endpoints": []uint16{endpoint}, "matter.capabilityEndpoints": capabilityEndpoints,
		"matter.serverClusters": serverClusters, "matter.deviceTypes": []string{"0x302"},
		"matter.modelVersion": matterModelVersion,
	}
}

func endpointPathKey(endpoint uint16, cluster, attribute uint32) string {
	return fmtUint(endpoint) + "/" + fmtUint32(cluster) + "/" + fmtUint32(attribute)
}

func fmtUint(value uint16) string { return fmtUint32(uint32(value)) }

func fmtUint32(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}

func clusterHex(value uint32) string {
	return "0x" + strings.ToUpper(strconv.FormatUint(uint64(value), 16))
}
