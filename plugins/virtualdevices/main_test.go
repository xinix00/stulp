package main

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/internal/manifest"
)

type fakeSwitchDevice struct {
	store         map[string]any
	capability    map[string]any
	available     bool
	operations    []string
	storeErr      error
	capabilityErr error
}

func newFakeSwitchDevice(store map[string]any) *fakeSwitchDevice {
	if store == nil {
		store = map[string]any{}
	}
	return &fakeSwitchDevice{store: store, capability: map[string]any{}}
}

func (d *fakeSwitchDevice) StoreValue(key string) (any, bool) {
	value, found := d.store[key]
	return value, found
}

func (d *fakeSwitchDevice) SetStore(patch map[string]any) error {
	d.operations = append(d.operations, "store")
	if d.storeErr != nil {
		return d.storeErr
	}
	for key, value := range patch {
		d.store[key] = value
	}
	return nil
}

func (d *fakeSwitchDevice) SetCapabilityValue(name string, value any) error {
	d.operations = append(d.operations, "capability")
	if d.capabilityErr != nil {
		return d.capabilityErr
	}
	d.capability[name] = value
	return nil
}

func (d *fakeSwitchDevice) SetAvailable() error {
	d.operations = append(d.operations, "available")
	d.available = true
	return nil
}

func TestVirtualSwitchRestoresPersistedStateWithoutOverwritingIt(t *testing.T) {
	device := newFakeSwitchDevice(map[string]any{virtualStateKey: true})
	handler := &virtualSwitch{device: device}

	if err := handler.OnInit(); err != nil {
		t.Fatal(err)
	}
	if device.capability["onoff"] != true || !device.available {
		t.Fatalf("restored device = capability %#v, available %t", device.capability, device.available)
	}
	if _, wroteStore := indexOf(device.operations, "store"); wroteStore {
		t.Fatalf("OnInit overwrote an existing durable state: %v", device.operations)
	}
	if !reflect.DeepEqual(device.operations, []string{"capability", "available"}) {
		t.Fatalf("OnInit operations = %v", device.operations)
	}
}

func TestVirtualSwitchInitialisesMissingOrInvalidStateToOff(t *testing.T) {
	for _, initial := range []map[string]any{nil, {virtualStateKey: "aan"}} {
		device := newFakeSwitchDevice(initial)
		handler := &virtualSwitch{device: device}
		if err := handler.OnInit(); err != nil {
			t.Fatal(err)
		}
		if device.store[virtualStateKey] != false || device.capability["onoff"] != false {
			t.Fatalf("initialised state = store %#v, capability %#v", device.store, device.capability)
		}
		if !reflect.DeepEqual(device.operations, []string{"store", "capability", "available"}) {
			t.Fatalf("OnInit operations = %v", device.operations)
		}
	}
}

func TestVirtualSwitchPersistsBeforePublishingAChange(t *testing.T) {
	device := newFakeSwitchDevice(map[string]any{virtualStateKey: false})
	handler := &virtualSwitch{device: device}

	if err := handler.OnCapability("onoff", true); err != nil {
		t.Fatal(err)
	}
	if device.store[virtualStateKey] != true || device.capability["onoff"] != true {
		t.Fatalf("changed state = store %#v, capability %#v", device.store, device.capability)
	}
	if !reflect.DeepEqual(device.operations, []string{"store", "capability"}) {
		t.Fatalf("write order = %v, want durable store before capability", device.operations)
	}

	device.storeErr = errors.New("disk vol")
	device.operations = nil
	if err := handler.OnCapability("onoff", false); err == nil || !strings.Contains(err.Error(), "stand bewaren") {
		t.Fatalf("store failure = %v", err)
	}
	if len(device.operations) != 1 || device.operations[0] != "store" {
		t.Fatalf("published after store failure: %v", device.operations)
	}
}

func TestVirtualSwitchRejectsAnythingButBooleanOnOff(t *testing.T) {
	device := newFakeSwitchDevice(nil)
	handler := &virtualSwitch{device: device}
	for _, test := range []struct {
		name  string
		value any
	}{
		{"dim", true},
		{"onoff", "aan"},
	} {
		if err := handler.OnCapability(test.name, test.value); err == nil {
			t.Fatalf("OnCapability(%q, %#v) succeeded", test.name, test.value)
		}
	}
	if len(device.operations) != 0 {
		t.Fatalf("invalid writes reached the device: %v", device.operations)
	}
}

func TestPairCreatesNamedIndependentPersistentSwitches(t *testing.T) {
	next := 0
	driver := switchDriver{newID: func() (string, error) {
		next++
		return "virtual-test-" + string(rune('0'+next)), nil
	}}

	firstSession := driver.Pair()
	secondSession := driver.Pair()
	if _, err := firstSession["list_devices"](nil); err == nil {
		t.Fatal("list_devices succeeded before a name was submitted")
	}
	if _, err := firstSession["create"](map[string]any{"name": "  Alarm aan  "}); err != nil {
		t.Fatal(err)
	}
	if _, err := secondSession["create"](map[string]any{"name": "Alarm aan"}); err != nil {
		t.Fatal(err)
	}

	first := pairedCandidates(t, firstSession)[0]
	second := pairedCandidates(t, secondSession)[0]
	if first.Name != "Alarm aan" || first.Store[virtualStateKey] != false {
		t.Fatalf("first candidate = %#v", first)
	}
	if first.Data["id"] == second.Data["id"] {
		t.Fatalf("separate virtual switches share identity: %#v and %#v", first, second)
	}
	// list_devices must keep the same identity so a lost add response can be
	// retried without creating another logical switch.
	again := pairedCandidates(t, firstSession)[0]
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("candidate changed within its pairing session: %#v then %#v", first, again)
	}
}

// PairEmit crosses the app process as JSON and MCP deliberately consumes the
// candidate from list_devices, not from create. Keep that wire contract tied to
// the real driver so a future pairing-UI refactor cannot silently break remote
// creation while the in-process tests still pass.
func TestPairCandidateSurvivesTheRemotePairingWireShape(t *testing.T) {
	handlers := (switchDriver{newID: func() (string, error) { return "virtual-wire-test", nil }}).Pair()
	if _, err := handlers["create"](map[string]any{"name": "Flow-geheugen"}); err != nil {
		t.Fatal(err)
	}
	found, err := handlers["list_devices"](nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(found)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []map[string]any
	if err := json.Unmarshal(wire, &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0]["name"] != "Flow-geheugen" {
		t.Fatalf("wire candidates = %#v", candidates)
	}
	data, _ := candidates[0]["data"].(map[string]any)
	deviceStore, _ := candidates[0]["store"].(map[string]any)
	if data["id"] != "virtual-wire-test" || deviceStore[virtualStateKey] != false {
		t.Fatalf("wire candidate lost identity or initial state: %#v", candidates[0])
	}
}

func TestPairValidatesNameAndIDCreation(t *testing.T) {
	driver := switchDriver{newID: func() (string, error) { return "", errors.New("geen entropy") }}
	for _, name := range []string{"", "   ", strings.Repeat("x", maxVirtualNameLength+1)} {
		handlers := driver.Pair()
		if _, err := handlers["create"](map[string]any{"name": name}); err == nil {
			t.Fatalf("invalid name %q was accepted", name)
		}
	}
	handlers := driver.Pair()
	if _, err := handlers["create"](map[string]any{"name": "Alarm"}); err == nil || !strings.Contains(err.Error(), "identiteit") {
		t.Fatalf("id creation failure = %v", err)
	}
}

func TestManifestExposesOneWritableOnOffDriverAndItsPairPage(t *testing.T) {
	appManifest, _, err := manifest.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	if appManifest.ID != "com.stulp.virtualdevices" {
		t.Fatalf("app id = %q", appManifest.ID)
	}
	driver, found := appManifest.Driver("switch")
	if !found || driver.Class != "other" || !reflect.DeepEqual(driver.Capabilities, []string{"onoff"}) {
		t.Fatalf("switch driver = %#v, found %t", driver, found)
	}
	if len(driver.Pair) != 3 {
		t.Fatalf("pair views = %#v", driver.Pair)
	}
	if _, err := os.Stat("drivers/switch/pair/name.html"); err != nil {
		t.Fatalf("custom name page: %v", err)
	}
}

func pairedCandidates(t *testing.T, handlers map[string]appsdk.PairHandler) []appsdk.PairedDevice {
	t.Helper()
	answer, err := handlers["list_devices"](nil)
	if err != nil {
		t.Fatal(err)
	}
	devices, ok := answer.([]appsdk.PairedDevice)
	if !ok {
		t.Fatalf("list_devices answer = %T", answer)
	}
	return devices
}

func indexOf(values []string, want string) (int, bool) {
	for index, value := range values {
		if value == want {
			return index, true
		}
	}
	return -1, false
}
