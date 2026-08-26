package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/sigenergy/internal/mysigen"
)

type fakeGatewayCloud struct {
	stations  mysigen.StationList
	status    map[int64]mysigen.GatewayStatus
	statusErr map[int64]error

	mu              sync.Mutex
	preparedTargets []mysigen.GridTarget
	prepare         mysigen.PreparedSwitch
	prepareErr      error
	execute         mysigen.SwitchResult
	executeErr      error
	executed        chan string
}

func (f *fakeGatewayCloud) Stations(context.Context) (mysigen.StationList, error) {
	return f.stations, nil
}

func (f *fakeGatewayCloud) GatewayStatus(_ context.Context, stationID int64) (mysigen.GatewayStatus, error) {
	if err := f.statusErr[stationID]; err != nil {
		return mysigen.GatewayStatus{}, err
	}
	return f.status[stationID], nil
}

func (f *fakeGatewayCloud) PrepareSwitch(_ context.Context, stationID int64, target mysigen.GridTarget) (mysigen.PreparedSwitch, error) {
	f.mu.Lock()
	f.preparedTargets = append(f.preparedTargets, target)
	f.mu.Unlock()
	prepared := f.prepare
	prepared.StationID = stationID
	prepared.Target = target
	return prepared, f.prepareErr
}

func (f *fakeGatewayCloud) ExecuteSwitch(_ context.Context, confirmation string, _ mysigen.PollOptions) (mysigen.SwitchResult, error) {
	if f.executed != nil {
		f.executed <- confirmation
	}
	return f.execute, f.executeErr
}

type fakeGatewayHandle struct {
	mu          sync.Mutex
	values      map[string]any
	available   bool
	unavailable string
	errors      []string
	updates     chan struct{}
}

func (d *fakeGatewayHandle) ID() string { return "gateway-test" }

func (d *fakeGatewayHandle) SetCapabilityValues(values map[string]any) error {
	d.mu.Lock()
	if d.values == nil {
		d.values = map[string]any{}
	}
	for name, value := range values {
		d.values[name] = value
	}
	d.mu.Unlock()
	return nil
}

func (d *fakeGatewayHandle) SetAvailable() error {
	d.mu.Lock()
	d.available = true
	d.mu.Unlock()
	if d.updates != nil {
		select {
		case d.updates <- struct{}{}:
		default:
		}
	}
	return nil
}

func (d *fakeGatewayHandle) SetUnavailable(reason string) error {
	d.mu.Lock()
	d.available, d.unavailable = false, reason
	d.mu.Unlock()
	return nil
}

func (d *fakeGatewayHandle) Error(message string) {
	d.mu.Lock()
	d.errors = append(d.errors, message)
	d.mu.Unlock()
}

func gatewayStatus(t *testing.T, onOff, manual, control int, show bool) mysigen.GatewayStatus {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"onOffGridStatus": onOff, "manualOffGridStatus": manual,
		"onOffGridControlMode": control, "showButton": show,
	})
	if err != nil {
		t.Fatal(err)
	}
	var status mysigen.GatewayStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestGatewayCapabilityPreparesOnceAndPublishesReadback(t *testing.T) {
	cloud := &fakeGatewayCloud{
		prepare:  mysigen.PreparedSwitch{Confirmation: "eenmalig"},
		execute:  mysigen.SwitchResult{Status: gatewayStatus(t, mysigen.StatusManualIsland, 0, 1, true), CommandSent: true},
		executed: make(chan string, 1),
	}
	device := &fakeGatewayHandle{updates: make(chan struct{}, 1)}
	gateway := &gatewayDevice{device: device, stationID: 42, cloud: cloud}

	if err := gateway.OnCapability("off_grid", true); err != nil {
		t.Fatal(err)
	}
	select {
	case confirmation := <-cloud.executed:
		if confirmation != "eenmalig" {
			t.Fatalf("confirmation = %q", confirmation)
		}
	case <-time.After(time.Second):
		t.Fatal("Gateway-command werd niet uitgevoerd")
	}
	select {
	case <-device.updates:
	case <-time.After(time.Second):
		t.Fatal("bevestigde Gateway-status werd niet gepubliceerd")
	}

	cloud.mu.Lock()
	targets := append([]mysigen.GridTarget(nil), cloud.preparedTargets...)
	cloud.mu.Unlock()
	if !reflect.DeepEqual(targets, []mysigen.GridTarget{mysigen.TargetOffGrid}) {
		t.Fatalf("targets = %v", targets)
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.values["off_grid"] != true || device.values["grid_status"] != "off_grid_manual" || !device.available {
		t.Fatalf("Gateway-projectie = values %#v, available %t", device.values, device.available)
	}
}

func TestGatewayDoesNotExecuteWithoutAValidPreflight(t *testing.T) {
	cloud := &fakeGatewayCloud{prepareErr: mysigen.ErrAutomaticOffGrid, executed: make(chan string, 1)}
	gateway := &gatewayDevice{device: &fakeGatewayHandle{}, stationID: 42, cloud: cloud}
	if err := gateway.OnCapability("off_grid", false); !errors.Is(err, mysigen.ErrAutomaticOffGrid) {
		t.Fatalf("reconnect tijdens automatische eilandstand = %v", err)
	}
	select {
	case <-cloud.executed:
		t.Fatal("een geweigerde preflight verstuurde toch een command")
	default:
	}
	if err := gateway.OnCapability("off_grid", "ja"); err == nil {
		t.Fatal("niet-booleaanse noodstroomstand werd geaccepteerd")
	}
}

func TestGatewayPairingDoesNotConfuseHiddenManualButtonWithMissingGateway(t *testing.T) {
	cloud := &fakeGatewayCloud{
		stations: mysigen.StationList{Stations: []mysigen.Station{
			{ID: 11, Name: "Thuis"}, {ID: 12, Name: "Schuur"},
		}},
		status: map[int64]mysigen.GatewayStatus{
			11: gatewayStatus(t, 0, 0, 1, true),
			12: gatewayStatus(t, 0, 0, 0, false),
		},
	}
	instance.mu.Lock()
	previous := instance.cloud
	instance.cloud = cloud
	instance.mu.Unlock()
	t.Cleanup(func() {
		instance.mu.Lock()
		instance.cloud = previous
		instance.mu.Unlock()
	})

	found, err := (gatewayDriver{}).ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Name != "Thuis Gateway" || found[0].Data["stationId"] != "11" ||
		found[1].Name != "Schuur Gateway" || found[1].Data["stationId"] != "12" {
		t.Fatalf("pairing = %#v", found)
	}
	if _, dynamicStateWasPersisted := found[1].Store["manualControllable"]; dynamicStateWasPersisted {
		t.Fatalf("tijdelijke knopstatus werd permanent opgeslagen: %#v", found[1].Store)
	}
}

func TestCloudSummarySeparatesGatewayPresenceFromManualControl(t *testing.T) {
	stations := mysigen.StationList{Stations: []mysigen.Station{
		{ID: 11, Name: "Thuis"}, {ID: 12, Name: "Schuur"},
	}}
	cloud := &fakeGatewayCloud{status: map[int64]mysigen.GatewayStatus{
		11: gatewayStatus(t, mysigen.StatusOnGrid, 0, 1, true),
		12: gatewayStatus(t, mysigen.StatusAutomaticIsland, 0, 0, true),
	}}
	summary := describeCloudStations(context.Background(), cloud, stations)
	items, ok := summary["stations"].([]map[string]any)
	if !ok || len(items) != 2 {
		t.Fatalf("cloudsamenvatting = %#v", summary)
	}
	if items[0]["gateway"] != true || items[0]["gatewayControllable"] != true || items[0]["offGrid"] != false {
		t.Fatalf("bedienbare Gateway = %#v", items[0])
	}
	if items[1]["gateway"] != true || items[1]["gatewayControllable"] != true || items[1]["offGrid"] != true {
		t.Fatalf("Gateway met zichtbare knop en controlmodus 0 = %#v", items[1])
	}
}

func TestCloudStateContainsTokensButNeverAPassword(t *testing.T) {
	raw, err := writeStoredCloud(storedCloud{
		Region: mysigen.RegionEU, Username: "owner@example.test",
		Tokens: mysigen.Tokens{AccessToken: "access", RefreshToken: "refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(strings.ToLower(text), "password") || !strings.Contains(text, "refresh") {
		t.Fatalf("private cloudstate = %s", text)
	}
	stored, err := readStoredCloud(raw)
	if err != nil || stored.Username != "owner@example.test" || stored.Tokens.AccessToken != "access" {
		t.Fatalf("cloudstate roundtrip = %#v, %v", stored, err)
	}
	if _, err := readStoredCloud(json.RawMessage(`{"version":99}`)); err == nil {
		t.Fatal("onbekende cloudstateversie werd geaccepteerd")
	}
}

func TestStationIDAcceptsPersistentTextAndRejectsFractions(t *testing.T) {
	for _, value := range []any{"42", float64(42), int64(42), 42} {
		if got, err := stationIDOf(value); err != nil || got != 42 {
			t.Errorf("stationIDOf(%#v) = %d, %v", value, got, err)
		}
	}
	for _, value := range []any{"", "x", float64(1.5), 0, -1} {
		if _, err := stationIDOf(value); err == nil {
			t.Errorf("stationIDOf(%#v) succeeded", value)
		}
	}
}
