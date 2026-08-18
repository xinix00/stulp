package webapi

import (
	"encoding/json"
	"testing"

	"github.com/xinix00/stulp/internal/store"
)

func testDevice(driver string, capabilities ...string) store.Device {
	return store.Device{
		ID: "device-" + driver, DriverID: "stulp:app:com.ubnt.unifiprotect:" + driver,
		Class: "sensor", Capabilities: capabilities,
	}
}

// These are the filter forms that occur verbatim in the public UniFi Protect
// manifest. Getting any of them wrong means a card silently stops being
// offered for the device it belongs to.
func TestDeviceFilterFormsFromRealManifests(t *testing.T) {
	doorbell := testDevice("protectdoorbell", "alarm_motion")
	camera := testDevice("protectcamera", "alarm_motion")
	chime := testDevice("protectchime", "onoff", "volume_set")
	speaker := testDevice("protectchime", "measure_battery")

	cases := []struct {
		name   string
		filter any
		device store.Device
		want   bool
	}{
		{"single driver matches", "driver_id=protectdoorbell", doorbell, true},
		{"single driver rejects another", "driver_id=protectdoorbell", camera, false},
		{"alternatives accept the first", "driver_id=protectdoorbell|protectcamera", doorbell, true},
		{"alternatives accept the second", "driver_id=protectdoorbell|protectcamera", camera, true},
		{"alternatives reject a third", "driver_id=protectdoorbell|protectcamera", chime, false},
		{"driver and capability both hold", "driver_id=protectchime&capabilities=onoff|volume_set", chime, true},
		{"driver holds but capability does not", "driver_id=protectchime&capabilities=onoff|volume_set", speaker, false},
		{"capability alone", "capabilities=alarm_motion", camera, true},
		{"capability alone rejects", "capabilities=alarm_motion", chime, false},
		{"object form", map[string]any{"driver_id": "protectcamera"}, camera, true},
		{"object form rejects", map[string]any{"driver_id": "protectcamera"}, doorbell, false},
		{"object form with a list", map[string]any{"driver_id": []any{"protectchime", "protectcamera"}}, chime, true},
		{"empty filter matches everything", "", doorbell, true},
		{"absent filter matches everything", nil, chime, true},
		{"class condition", "class=sensor", doorbell, true},
		{"class condition rejects", "class=light", doorbell, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseDeviceFilter(testCase.filter).matches(testCase.device); got != testCase.want {
				t.Fatalf("filter %v against %s = %v, want %v",
					testCase.filter, testCase.device.DriverID, got, testCase.want)
			}
		})
	}
}

// Sub-capabilities are written as "onoff.socket1"; a filter naming the base
// capability still applies to them.
func TestDeviceFilterMatchesSubCapabilities(t *testing.T) {
	socket := testDevice("protect-relay", "onoff.left", "onoff.right")
	if !parseDeviceFilter("capabilities=onoff").matches(socket) {
		t.Fatal("a base capability filter did not match its sub-capabilities")
	}
	if parseDeviceFilter("capabilities=dim").matches(socket) {
		t.Fatal("an unrelated capability matched")
	}
}

// An unknown condition key must not silently hide a card: erring towards
// offering it is recoverable, hiding it looks like the card does not exist.
func TestUnknownFilterKeysDoNotHideCards(t *testing.T) {
	if !parseDeviceFilter("driver_id=protectdoorbell&somethingNew=x").matches(testDevice("protectdoorbell")) {
		t.Fatal("an unknown condition key hid a matching card")
	}
}

func TestDeviceArgumentFilterFindsTheDeviceArgument(t *testing.T) {
	args := []any{
		map[string]any{"name": "device", "type": "device", "filter": "driver_id=protectdoorbell"},
		map[string]any{"name": "audio_type", "type": "dropdown"},
	}
	filter, name, ok := deviceArgumentFilter(args)
	if !ok || name != "device" {
		t.Fatalf("device argument = %q, ok = %v", name, ok)
	}
	if !filter.matches(testDevice("protectdoorbell")) {
		t.Fatal("the filter on the device argument was lost")
	}
	if _, _, ok := deviceArgumentFilter([]any{map[string]any{"name": "fob", "type": "autocomplete"}}); ok {
		t.Fatal("a card without a device argument reported one")
	}
}

// A card is device-scoped only when it has a device argument, and then it
// must list exactly the devices it applies to.
func TestAnnotateDeviceScope(t *testing.T) {
	devices := []store.Device{
		testDevice("protectdoorbell"), testDevice("protectcamera"), testDevice("protect-fob"),
	}

	appWide := map[string]any{}
	annotateDeviceScope(appWide, []any{map[string]any{"name": "fob", "type": "autocomplete"}}, devices)
	if appWide["scope"] != "app" {
		t.Fatalf("a card without a device argument got scope %v", appWide["scope"])
	}
	if _, present := appWide["deviceIds"]; present {
		t.Fatal("an app-wide card listed device IDs")
	}

	scoped := map[string]any{}
	annotateDeviceScope(scoped, []any{
		map[string]any{"name": "device", "type": "device", "filter": "driver_id=protectdoorbell|protectcamera"},
	}, devices)
	if scoped["scope"] != "device" || scoped["deviceArgument"] != "device" {
		t.Fatalf("scoped card annotated as %+v", scoped)
	}
	ids, _ := scoped["deviceIds"].([]string)
	if len(ids) != 2 || ids[0] != "device-protectdoorbell" || ids[1] != "device-protectcamera" {
		t.Fatalf("deviceIds = %v", ids)
	}

	// A card whose filter matches nothing must say so with an empty list
	// rather than by omitting the field, so the editor can hide it.
	unmatched := map[string]any{}
	annotateDeviceScope(unmatched, []any{
		map[string]any{"name": "device", "type": "device", "filter": "driver_id=access-garagedoor"},
	}, devices)
	ids, ok := unmatched["deviceIds"].([]string)
	if !ok || len(ids) != 0 {
		t.Fatalf("unmatched card deviceIds = %v (present: %v)", ids, ok)
	}
	encoded, err := json.Marshal(unmatched["deviceIds"])
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("deviceIds serialized as %s", encoded)
	}
}

func TestDriverName(t *testing.T) {
	cases := map[string]string{
		"stulp:app:com.ubnt.unifiprotect:protectdoorbell": "protectdoorbell",
		"protectdoorbell": "protectdoorbell",
		"":                "",
	}
	for input, want := range cases {
		if got := driverName(input); got != want {
			t.Fatalf("driverName(%q) = %q, want %q", input, got, want)
		}
	}
}
