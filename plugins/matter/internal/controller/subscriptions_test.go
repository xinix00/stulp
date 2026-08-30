package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func TestSubscriptionDoesNotBatchAutomationReports(t *testing.T) {
	if subscriptionMinInterval != 0 {
		t.Fatalf("subscription minimum interval = %d, want 0 for immediate motion/contact/button reports", subscriptionMinInterval)
	}
}

func TestApplyReportsPersistsStateAndEmitsDeduplicatedMatterEvent(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	device, err := database.AddDevice(ctx, Device{
		DriverID: "matter", Name: "Front door", Class: "sensor",
		Data: map[string]any{"id": "matter-test"}, Capabilities: []string{"onoff", "button"},
		State: map[string]any{"onoff": false, "button": false}, Available: true,
		Store: map[string]any{
			"matter.nodeId": "0000000000010000", "matter.endpoint": 1,
			"matter.address": "127.0.0.1:5540", "matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	endpoint, attribute, cluster, eventID := uint16(1), uint32(0), switchCluster, uint32(1)
	timestamp := uint64(1000)
	attributeReport := im.AttributeReport{
		Path:  im.AttributePath{Endpoint: &endpoint, Cluster: uint32Pointer(onOffCluster), Attribute: &attribute},
		Value: im.Value{Type: tlv.TypeBool, Bool: true},
	}
	eventReport := im.EventReport{
		Path:   im.EventPath{Endpoint: &endpoint, Cluster: &cluster, Event: &eventID},
		Number: 10, Priority: 1, SystemTimestamp: &timestamp,
		Value: im.Value{Type: tlv.TypeStructure},
	}
	controller.applyReports(ctx, 0x10000, []im.AttributeReport{attributeReport}, []im.EventReport{eventReport})

	updated, err := database.Device(ctx, device.ID)
	if err != nil || updated.State["onoff"] != true || updated.State["button"] != true ||
		updated.Store["matter.lastEventNumber"] != "10" {
		t.Fatalf("updated Matter device = %#v, %v", updated, err)
	}
	// Een Matter-event wordt als Flow-kaart gemeld. Wat Stulp daarmee doet is
	// zijn zaak; hier telt dat het één keer gemeld wordt.
	fired := database.flowEvents()
	if len(fired) != 1 || fired[0].cardType != "trigger" || fired[0].cardID != "matter_event" {
		t.Fatalf("gemelde Flow-kaarten = %#v", fired)
	}
	tokens, _ := fired[0].tokens.(map[string]any)
	state, _ := fired[0].state.(map[string]any)
	if tokens["endpoint"] != endpoint || tokens["capability"] != "button" || tokens["pressed"] != true ||
		state["endpoint"] != endpoint || state["capability"] != "button" || state["pressed"] != true {
		t.Fatalf("Matter button event mist routeinformatie: tokens=%#v state=%#v", tokens, state)
	}

	// Hetzelfde rapport nog eens: een node herhaalt zijn events na een
	// herverbinding, en dat mag geen tweede keer een Flow starten.
	controller.applyReports(ctx, 0x10000, []im.AttributeReport{attributeReport}, []im.EventReport{eventReport})
	if again := database.flowEvents(); len(again) != 1 {
		t.Fatalf("een herhaald event startte de Flow opnieuw: %#v", again)
	}

	// Aqara H2 switches can emit another InitialPress without ever reporting a
	// release. The state is already true, but a new Matter event number is a new
	// physical press and must fire the semantic button card again.
	eventReport.Number = 11
	controller.applyReports(ctx, 0x10000, nil, []im.EventReport{eventReport})
	repeated := database.flowEvents()
	if len(repeated) != 3 || repeated[1].cardID != "capability.button.on" || repeated[2].cardID != "matter_event" {
		t.Fatalf("een tweede InitialPress vuurde niet opnieuw: %#v", repeated)
	}
	repeatedTokens, _ := repeated[1].tokens.(map[string]any)
	if repeatedTokens["deviceId"] != device.ID || repeatedTokens["capability"] != "button" ||
		repeatedTokens["value"] != true || repeatedTokens["oldValue"] != true {
		t.Fatalf("herhaalde knopkaart mist capability-state: %#v", repeatedTokens)
	}
}

func uint32Pointer(value uint32) *uint32 { return &value }

func TestMatterEventNames(t *testing.T) {
	if got := matterEventName(switchCluster, 1); got != "initial_press" {
		t.Fatalf("switch event name = %q", got)
	}
	if got := matterEventName(0x9999, 7); got != "matter_0x9999_0x0007" {
		t.Fatalf("fallback event name = %q", got)
	}
}

func TestSubscriptionRetryGivesBusyCASEPeerRoom(t *testing.T) {
	err := fmt.Errorf("CASE: %w", pase.Failure(pase.StatusBusy))
	if got := subscriptionRetryDelay(err, time.Second); got != 15*time.Second {
		t.Fatalf("busy retry delay = %v", got)
	}
}

func TestApplyReportsRoutesRepeatedButtonToItsEndpointCapability(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	device, err := database.AddDevice(ctx, Device{
		DriverID: "matter", Name: "Wall switch", Class: "light",
		Data: map[string]any{"id": "matter-switch"}, Capabilities: []string{"onoff", "button.1", "button.2"},
		State: map[string]any{"onoff": false, "button.1": false, "button.2": false}, Available: true,
		Store: map[string]any{
			"matter.nodeId": "0000000000010000", "matter.endpoint": 1,
			"matter.endpoints":           []uint16{1, 4, 5},
			"matter.capabilityEndpoints": map[string]uint16{"onoff": 1, "button.1": 4, "button.2": 5},
			"matter.address":             "127.0.0.1:5540", "matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	endpoint, cluster, eventID := uint16(5), switchCluster, uint32(1)
	controller.applyReports(ctx, 0x10000, nil, []im.EventReport{{
		Path: im.EventPath{Endpoint: &endpoint, Cluster: &cluster, Event: &eventID}, Number: 1,
		Value: im.Value{Type: tlv.TypeStructure},
	}})
	updated, err := database.Device(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State["button.2"] != true || updated.State["button.1"] != false {
		t.Fatalf("button endpoint was routed to wrong state: %#v", updated.State)
	}
}

func TestApplyReportsConvertsIlluminanceAndPersistsLiveState(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	device, err := database.AddDevice(ctx, Device{
		DriverID: "matter", Name: "Motion sensor", Class: "sensor",
		Data: map[string]any{"id": "matter-lux"}, Capabilities: []string{"alarm_motion", "measure_luminance"},
		State: map[string]any{"alarm_motion": false}, Available: true,
		Store: map[string]any{
			"matter.nodeId": "0000000000010000", "matter.endpoint": 3,
			"matter.capabilityEndpoints": map[string]uint16{"alarm_motion": 3, "measure_luminance": 3},
			"matter.address":             "127.0.0.1:5540", "matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	endpoint, cluster, attribute := uint16(3), illuminanceCluster, uint32(0)
	controller.applyReports(ctx, 0x10000, []im.AttributeReport{{
		Path:  im.AttributePath{Endpoint: &endpoint, Cluster: &cluster, Attribute: &attribute},
		Value: im.Value{Type: tlv.TypeUint, Uint: 20001},
	}}, nil)
	updated, err := database.Device(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	lux, ok := updated.State["measure_luminance"].(float64)
	if !ok || math.Abs(lux-100) > 1e-9 {
		t.Fatalf("live illuminance state = %#v, want 100 lx", updated.State["measure_luminance"])
	}
}

func TestElectricalMeasurementSubscriptionPathsAndLiveConversion(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	device, err := database.AddDevice(ctx, Device{
		DriverID: "matter", Name: "Metered light", Class: "light",
		Data:         map[string]any{"id": "matter-metered-light"},
		Capabilities: []string{"onoff", "measure_power", "meter_power"},
		State:        map[string]any{"onoff": true}, Available: true,
		Store: map[string]any{
			"matter.nodeId": "0000000000010000", "matter.endpoint": 1,
			"matter.capabilityEndpoints": map[string]uint16{"onoff": 1, "measure_power": 1, "meter_power": 1},
			"matter.address":             "127.0.0.1:5540", "matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := subscriptionPaths([]Device{device})
	wantedPaths := map[string]bool{
		fmt.Sprintf("%d/%d", electricalPowerCluster, activePowerAttribute):               false,
		fmt.Sprintf("%d/%d", electricalEnergyCluster, cumulativeEnergyImportedAttribute): false,
	}
	for _, path := range paths {
		if path.Cluster != nil && path.Attribute != nil {
			key := fmt.Sprintf("%d/%d", *path.Cluster, *path.Attribute)
			if _, wanted := wantedPaths[key]; wanted {
				wantedPaths[key] = true
			}
		}
	}
	for path, found := range wantedPaths {
		if !found {
			t.Fatalf("subscription omitted electrical attribute %s: %#v", path, paths)
		}
	}

	endpoint := uint16(1)
	powerCluster, powerAttribute := electricalPowerCluster, uint32(activePowerAttribute)
	energyCluster, energyAttribute := electricalEnergyCluster, uint32(cumulativeEnergyImportedAttribute)
	controller := &Controller{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	controller.applyReports(ctx, 0x10000, []im.AttributeReport{
		{
			Path:  im.AttributePath{Endpoint: &endpoint, Cluster: &powerCluster, Attribute: &powerAttribute},
			Value: im.Value{Type: tlv.TypeInt, Int: 7_250},
		},
		{
			Path: im.AttributePath{Endpoint: &endpoint, Cluster: &energyCluster, Attribute: &energyAttribute},
			Value: im.Value{Type: tlv.TypeStructure, Children: []im.Value{{
				Tag: tlv.Context(0), Type: tlv.TypeInt, Int: 1_250_000,
			}}},
		},
	}, nil)
	updated, err := database.Device(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(updated.State["measure_power"].(float64)-7.25) > 1e-9 ||
		math.Abs(updated.State["meter_power"].(float64)-1.25) > 1e-9 {
		t.Fatalf("live electrical state = %#v", updated.State)
	}
}

func TestSensitivitySettingUsesTheGenericSubscriptionPath(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	setting := MatterSetting{
		ID: "matter.sensitivity.4", Kind: "sensitivity", Endpoint: 4,
		Cluster: matterHexID(booleanStateConfigurationCluster), Attribute: matterHexID(0), Levels: 3,
	}
	device, err := database.AddDevice(ctx, Device{
		DriverID: "matter", Name: "Presence sensor", Class: "sensor",
		Data: map[string]any{"id": "matter-fp300"}, Settings: map[string]any{setting.ID: uint64(1)}, Available: true,
		Store: map[string]any{
			"matter.nodeId": "0000000000010000", "matter.endpoint": 4, "matter.endpoints": []uint16{4},
			"matter.settings": []MatterSetting{setting}, "matter.address": "127.0.0.1:5540",
			"matter.noc": base64.StdEncoding.EncodeToString([]byte{1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := subscriptionPaths([]Device{device})
	found := false
	for _, path := range paths {
		found = found || path.Endpoint != nil && *path.Endpoint == 4 && path.Cluster != nil &&
			*path.Cluster == booleanStateConfigurationCluster && path.Attribute != nil && *path.Attribute == 0
	}
	if !found {
		t.Fatalf("sensitivity subscription missing: %#v", paths)
	}

	endpoint, cluster, attribute := uint16(4), booleanStateConfigurationCluster, uint32(0)
	controller := &Controller{store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	controller.applyReports(ctx, 0x10000, []im.AttributeReport{{
		Path:  im.AttributePath{Endpoint: &endpoint, Cluster: &cluster, Attribute: &attribute},
		Value: im.Value{Type: tlv.TypeUint, Uint: 2},
	}}, nil)
	updated, err := database.Device(ctx, device.ID)
	if err != nil || updated.Settings[setting.ID] != uint64(2) {
		t.Fatalf("live sensitivity = %#v, %v", updated.Settings, err)
	}
}
