package webapi

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/flow"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

// capabilityCardServer installs a small app so the generated cards can be
// checked against a real manifest, including a custom capability that
// carries its own title.
func capabilityCardServer(t *testing.T) (*Server, store.Device) {
	t.Helper()
	root := t.TempDir()
	appJSON := `{
      "id":"com.acme.sensors","version":"1.0.0","sdk":3,"name":{"en":"Sensors"},
      "capabilities":{
        "alarm_vape":{"type":"boolean","title":{"en":"Vape detected","nl":"Vape gedetecteerd"},"getable":true,"setable":false},
        "chime_tune":{"type":"enum","title":{"en":"Chime tune"},"getable":true,"setable":true,
          "values":[{"id":"ding","title":{"en":"Ding"}},{"id":"dong","title":{"en":"Dong"}}]}
      },
      "drivers":[{"id":"sensor","name":{"en":"Sensor"},"class":"sensor",
        "capabilities":["alarm_smoke","alarm_vape","measure_temperature","onoff","chime_tune"]}]
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
		AppID: "com.acme.sensors", DriverID: "sensor", Name: "Hal", Class: "sensor",
		Capabilities: []string{"alarm_smoke", "alarm_vape", "measure_temperature", "onoff", "chime_tune"},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(apps.Close)
	server := New(database, apps, Options{})
	t.Cleanup(server.Close)
	return server, device
}

func cardTitles(cards []map[string]any) map[string]string {
	titles := make(map[string]string, len(cards))
	for _, card := range cards {
		id, _ := card["id"].(string)
		title, _ := card["title"].(string)
		titles[id] = title
	}
	return titles
}

// The cards a device exposes have to come from its capabilities too, not
// only from what the app happened to declare in its manifest.
func TestCapabilityCardsCoverWhatTheDeviceReports(t *testing.T) {
	server, device := capabilityCardServer(t)
	cards := server.capabilityCards([]store.Device{device})

	triggers := cardTitles(cards["triggers"])
	// A standard capability gets the SDK's own name, and an alarm reads
	// as an event rather than as a switch.
	if got := triggers["capability.alarm_smoke.on"]; got != "Rookalarm ging af" {
		t.Fatalf("alarm_smoke trigger = %q", got)
	}
	if got := triggers["capability.alarm_smoke.off"]; got != "Rookalarm is voorbij" {
		t.Fatalf("alarm_smoke off trigger = %q", got)
	}
	// A custom capability keeps the app's own title.
	if got := triggers["capability.alarm_vape.on"]; got != "Vape detected ging af" {
		t.Fatalf("alarm_vape trigger = %q", got)
	}
	// A non-boolean reports that it changed, and a plain switch is not an alarm.
	if got := triggers["capability.measure_temperature.changed"]; got != "Temperatuur is veranderd" {
		t.Fatalf("measure_temperature trigger = %q", got)
	}
	if got := triggers["capability.onoff.on"]; got != "Aan/uit werd aan" {
		t.Fatalf("onoff trigger = %q", got)
	}

	conditions := cardTitles(cards["conditions"])
	if _, ok := conditions["capability.alarm_smoke.is"]; !ok {
		t.Fatalf("no condition for alarm_smoke: %v", conditions)
	}

	// Only a setable capability gets an action; a smoke alarm cannot be set.
	actions := cardTitles(cards["actions"])
	if got := actions["capability.onoff.set"]; got != "Zet Aan/uit" {
		t.Fatalf("onoff action = %q", got)
	}
	if _, present := actions["capability.alarm_smoke.set"]; present {
		t.Fatal("a read-only alarm got a set action")
	}
}

func TestCapabilityStaysCardUsesPlainSeconds(t *testing.T) {
	card := builtinCapabilityStaysTrigger()
	if card["id"] != flow.DeviceCapabilityStaysCardID {
		t.Fatalf("stability card id = %#v", card["id"])
	}
	arguments, _ := card["args"].([]any)
	var seconds map[string]any
	for _, raw := range arguments {
		argument, _ := raw.(map[string]any)
		if argument["name"] == "seconds" {
			seconds = argument
			break
		}
	}
	if seconds == nil || seconds["type"] != "number" || seconds["title"] != "Seconden" ||
		seconds["min"] != 1 || seconds["max"] != 86400 || seconds["step"] != 1 {
		t.Fatalf("seconds argument = %#v", seconds)
	}
}

func TestCapabilityCardsAreScopedToTheDevicesThatHaveThem(t *testing.T) {
	server, device := capabilityCardServer(t)
	other := store.Device{ID: "other", AppID: "com.acme.sensors", DriverID: "sensor",
		Name: "Kaal", Capabilities: []string{"onoff"}}
	cards := server.capabilityCards([]store.Device{device, other})

	for _, card := range cards["triggers"] {
		id, _ := card["id"].(string)
		ids, _ := card["deviceIds"].([]string)
		if card["scope"] != "device" {
			t.Fatalf("%s is not device-scoped", id)
		}
		switch {
		case strings.HasPrefix(id, "capability.onoff."):
			if len(ids) != 2 {
				t.Fatalf("%s applies to %v, want both devices", id, ids)
			}
		case strings.HasPrefix(id, "capability.alarm_smoke."):
			if len(ids) != 1 || ids[0] != device.ID {
				t.Fatalf("%s applies to %v, want only the sensor", id, ids)
			}
		}
		if card["capability"] == nil {
			t.Fatalf("%s does not name its capability, so the value widget cannot follow it", id)
		}
	}
}

func TestSceneDeviceGetsGenericOnOffFlowCards(t *testing.T) {
	server, _ := capabilityCardServer(t)
	sceneDevice := store.Device{
		ID: store.SceneDeviceID("movie"), AppID: store.NativeSceneAppID, DriverID: "scene", Name: "Film kijken", Class: "scene",
		Capabilities: []string{"onoff"}, State: map[string]any{"onoff": false},
	}
	cards := server.capabilityCards([]store.Device{sceneDevice})

	for _, expected := range []struct {
		kind, id string
	}{
		{"triggers", "capability.onoff.on"},
		{"triggers", "capability.onoff.off"},
		{"conditions", "capability.onoff.is"},
		{"actions", "capability.onoff.set"},
	} {
		var found map[string]any
		for _, card := range cards[expected.kind] {
			if card["id"] == expected.id {
				found = card
				break
			}
		}
		if found == nil {
			t.Errorf("scene device has no %s card %q", expected.kind, expected.id)
			continue
		}
		deviceIDs, ok := found["deviceIds"].([]string)
		if !ok || len(deviceIDs) != 1 || deviceIDs[0] != sceneDevice.ID {
			t.Errorf("%s deviceIds = %#v, want [%q]", expected.id, found["deviceIds"], sceneDevice.ID)
		}
		if found["scope"] != "device" || found["capability"] != "onoff" {
			t.Errorf("%s is not the generic device-scoped onoff card: %#v", expected.id, found)
		}
	}
}

func TestSceneDeviceKeepsOnOffActionBesideReadOnlyDevice(t *testing.T) {
	server, _ := capabilityCardServer(t)
	readOnlyManifest, err := manifest.Parse([]byte(`{
		"id":"com.acme.readonly","version":"1.0.0","sdk":3,"name":{"en":"Read only"},
		"capabilities":{"onoff":{"type":"boolean","getable":true,"setable":false}},
		"drivers":[{"id":"sensor","name":{"en":"Sensor"},"class":"sensor","capabilities":["onoff"]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.InstallApp(context.Background(), readOnlyManifest, "", ""); err != nil {
		t.Fatal(err)
	}

	readOnlyDevice := store.Device{
		ID: "read-only", AppID: readOnlyManifest.ID, DriverID: "sensor", Name: "Sensor", Class: "sensor",
		Capabilities: []string{"onoff"}, State: map[string]any{"onoff": false},
	}
	sceneDevice := store.Device{
		ID: store.SceneDeviceID("movie"), AppID: store.NativeSceneAppID, DriverID: "scene", Name: "Film kijken", Class: "scene",
		Capabilities: []string{"onoff"}, State: map[string]any{"onoff": false},
	}
	cards := server.capabilityCards([]store.Device{readOnlyDevice, sceneDevice})

	var action map[string]any
	for _, card := range cards["actions"] {
		if card["id"] == "capability.onoff.set" {
			action = card
			break
		}
	}
	if action == nil {
		t.Fatal("scene device lost its generic onoff action because a read-only onoff device was listed first")
	}
	if got := action["deviceIds"]; !reflect.DeepEqual(got, []string{sceneDevice.ID}) {
		t.Fatalf("onoff action deviceIds = %#v, want only the writable scene device", got)
	}

	var trigger map[string]any
	for _, card := range cards["triggers"] {
		if card["id"] == "capability.onoff.on" {
			trigger = card
			break
		}
	}
	if trigger == nil {
		t.Fatal("missing generic onoff trigger")
	}
	if got := trigger["deviceIds"]; !reflect.DeepEqual(got, []string{readOnlyDevice.ID, sceneDevice.ID}) {
		t.Fatalf("onoff trigger deviceIds = %#v, want both readable devices", got)
	}
}

func TestCapabilityTitlesDisambiguate(t *testing.T) {
	titles := map[string]string{
		"last_motion_at": "Laatste beweging", "last_motion_date": "Laatste beweging",
		"measure_temperature": "Temperatuur",
	}
	disambiguate(titles)
	if titles["last_motion_at"] != "Laatste beweging (last_motion_at)" ||
		titles["last_motion_date"] != "Laatste beweging (last_motion_date)" {
		t.Fatalf("colliding titles were not disambiguated: %v", titles)
	}
	if titles["measure_temperature"] != "Temperatuur" {
		t.Fatalf("a unique title was needlessly changed: %q", titles["measure_temperature"])
	}
}

func TestCapabilityDisplayTitlePreference(t *testing.T) {
	manifestTitle := map[string]any{"en": "Vape detected", "nl": "Vape gedetecteerd"}
	if got := capabilityDisplayTitle("alarm_vape", manifestTitle, "nl"); got != "Vape gedetecteerd" {
		t.Fatalf("manifest title ignored: %q", got)
	}
	if got := capabilityDisplayTitle("alarm_smoke", nil, "nl"); got != "Rookalarm" {
		t.Fatalf("standard title missing: %q", got)
	}
	// Some manifests set the title to the identifier; that is no better than
	// the standard name.
	if got := capabilityDisplayTitle("alarm_motion", "alarm_motion", "nl"); got != "Bewegingsalarm" {
		t.Fatalf("identifier-as-title was preferred over the standard name: %q", got)
	}
	if got := capabilityDisplayTitle("vendor_thing", nil, "nl"); got != "vendor_thing" {
		t.Fatalf("unknown capability = %q, want its identifier", got)
	}
}

func TestDimCapabilityUsesStandardScaleMetadata(t *testing.T) {
	server, device := capabilityCardServer(t)
	definition := server.capabilityObject(device, "dim", 0.25)
	if definition["title"] != "Dimniveau" || definition["type"] != "number" ||
		definition["min"] != 0.0 || definition["max"] != 1.0 || definition["step"] != 0.01 {
		t.Fatalf("dim metadata = %#v", definition)
	}
}

func TestLuminanceCapabilityUsesLuxMetadata(t *testing.T) {
	server, device := capabilityCardServer(t)
	definition := server.capabilityObject(device, "measure_luminance", 100.0)
	if definition["title"] != "Lichtsterkte" || definition["type"] != "number" ||
		definition["min"] != 0.0 || definition["step"] != 1.0 || definition["units"] != "lx" {
		t.Fatalf("luminance metadata = %#v", definition)
	}
}

func TestElectricalCapabilitiesUseStandardUnits(t *testing.T) {
	server, device := capabilityCardServer(t)
	power := server.capabilityObject(device, "measure_power", 7.25)
	if power["title"] != "Vermogen" || power["type"] != "number" || power["step"] != 0.001 || power["units"] != "W" {
		t.Fatalf("power metadata = %#v", power)
	}
	energy := server.capabilityObject(device, "meter_power", 1.25)
	if energy["title"] != "Energieverbruik" || energy["type"] != "number" ||
		energy["min"] != 0.0 || energy["step"] != 0.001 || energy["units"] != "kWh" {
		t.Fatalf("energy metadata = %#v", energy)
	}
}
