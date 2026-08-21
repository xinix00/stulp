package webapi

import (
	"strings"
	"testing"
)

// Een complete Manage-reload bestaat uit meerdere verzoeken. De device-lijst
// kan daardoor al oud zijn terwijl een trage Flow-registratie nog onderweg is:
// een SSE-update die intussen een lamp bereikbaar maakt of een meterwaarde vult
// mag bij het publiceren van die snapshot niet weer verdwijnen.
//
// De browsercode zelf is een ingebed asset zonder JavaScript-runtime in Stulp.
// Deze toets bewaakt daarom de drie ordeningsgrenzen van het protocol in dat
// asset: één load tegelijk, live updates onthouden, en die pas ná de opgehaalde
// device-lijst maar vóór state.devices erbovenop leggen.
func TestManageSnapshotCannotOverwriteARealtimeDeviceUpdate(t *testing.T) {
	raw, err := uiFiles.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)

	assertJavaScriptOrder(t, source,
		"function load() {",
		"if (loadPromise) return loadPromise;",
		"while (loadRequested)",
		"await loadSnapshot();",
	)
	assertJavaScriptOrder(t, source,
		"async function loadSnapshot() {",
		"const liveDeviceUpdates = new Map();",
		"realtimeDeviceUpdatesDuringLoad = liveDeviceUpdates;",
		"await Promise.all([",
		"for (const updated of liveDeviceUpdates.values()) {",
		"state.devices = loadedDevices;",
	)
	assertJavaScriptOrder(t, source,
		"if (event.manager === 'devices' && event.type === 'device.update') {",
		"realtimeDeviceUpdatesDuringLoad.set(event.data.id, event.data);",
		"applyRealtimeDevice(event.data);",
	)
}

func assertJavaScriptOrder(t *testing.T, source string, fragments ...string) {
	t.Helper()
	position := 0
	for _, fragment := range fragments {
		next := strings.Index(source[position:], fragment)
		if next < 0 {
			t.Fatalf("Manage JavaScript mist %q na byte %d", fragment, position)
		}
		position += next + len(fragment)
	}
}
