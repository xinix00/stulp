package webapi

import (
	"strings"
	"testing"
)

// Manage opent de eventstream vóór de compacte snapshot. Updates die tijdens
// die GET arriveren worden op de snapshot gelegd, zodat een oude availability
// of capabilitywaarde nooit over een al getoonde livewaarde heen kan schrijven.
func TestManageSnapshotCannotOverwriteARealtimeDeviceUpdate(t *testing.T) {
	source := manageJavaScript(t)

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
		"api('/api/stulp/manage/bootstrap')",
		"for (const updated of liveDeviceUpdates.values()) {",
		"state.devices = loadedDevices;",
	)
	assertJavaScriptOrder(t, source,
		"if (event.manager === 'devices' && event.type === 'device.update') {",
		"realtimeDeviceUpdatesDuringLoad.set(event.data.id, event.data);",
		"applyRealtimeDevice(event.data);",
	)
	assertJavaScriptOrder(t, source,
		"fetch('/api/stulp/events?view=overview'",
		"await load();",
		"const reader = response.body.getReader();",
	)
}

// De eerste render mag nooit opnieuw afhankelijk worden van apps, drivers of
// Flow-registraties. Vooral /flow/cards kan op een app wachten en hoort alleen
// bij de editor die die kaarten daadwerkelijk toont.
func TestManageStartupOnlyLoadsCompactBootstrap(t *testing.T) {
	source := manageJavaScript(t)
	start := strings.Index(source, "async function loadSnapshot() {")
	if start < 0 {
		t.Fatal("Manage JavaScript mist de bootstrapfunctie")
	}
	end := strings.Index(source[start:], "\nfunction renderDevices() {")
	if end < 0 {
		t.Fatal("Manage JavaScript mist de afgebakende bootstrapfunctie")
	}
	bootstrap := source[start : start+end]
	if !strings.Contains(bootstrap, "/api/stulp/manage/bootstrap") {
		t.Fatal("Manage bootstrap gebruikt de compacte route niet")
	}
	for _, forbidden := range []string{
		"/api/manager/apps/app", "/api/manager/devices/device'", "/api/stulp/device-groups",
		"/api/manager/drivers/driver", "/api/stulp/scenes", "/api/manager/flow/flow",
		"/api/stulp/flow/cards", "/api/stulp/system",
	} {
		if strings.Contains(bootstrap, forbidden) {
			t.Errorf("bootstrap haalt verborgen resource %q toch op", forbidden)
		}
	}
	assertJavaScriptOrder(t, source,
		"async function refreshActivePage()",
		"if (page === 'apps')",
		"Promise.all([ensureResource('apps'), ensureResource('system')])",
		"else if (page === 'flows')",
		"ensureResource('flows')",
	)
	refreshStart := strings.Index(source, "async function refreshActivePage()")
	if refreshStart < 0 {
		t.Fatal("Manage JavaScript mist de zichtbare-paginabundle")
	}
	refreshEnd := strings.Index(source[refreshStart:], "\nasync function activatePage(page)")
	if refreshEnd < 0 {
		t.Fatal("Manage JavaScript mist de zichtbare-paginabundle")
	}
	visibleBundle := source[refreshStart : refreshStart+refreshEnd]
	for _, hidden := range []string{"flowCards", "drivers", "deviceCatalog"} {
		if strings.Contains(visibleBundle, hidden) {
			t.Errorf("gewone tabwissel haalt verborgen editorresource %q op", hidden)
		}
	}
	assertJavaScriptOrder(t, source,
		"async function activatePage(page)",
		"await refreshActivePage();",
	)
	assertJavaScriptOrder(t, source,
		"async function openFlow(existing = null)",
		"ensureResource('flowCards')",
		"ensureResource('deviceCatalog')",
		"renderFlowEditor();",
	)
	assertJavaScriptOrder(t, source,
		"async function prepareDevicePopover(device)",
		"ensureDeviceDetail(device.id)",
		"renderDeviceOverview(detail);",
	)
	assertJavaScriptOrder(t, source,
		"async function showDeviceTab(tab)",
		"ensureDeviceDetail(deviceID)",
		"ensureDriver(device.driverId)",
		"renderDeviceConfiguration",
	)
}

// Een invalidatie tijdens een fetch verhoogt de generatie. Het oude antwoord
// wordt dan niet gecommit; dezelfde gedeelde Promise leest nog één snapshot.
func TestManageLazyResourcesAreSingleFlightAndGenerationSafe(t *testing.T) {
	source := manageJavaScript(t)
	assertJavaScriptOrder(t, source,
		"async function ensureResource(name)",
		"if (item.promise) return item.promise;",
		"const generation = item.generation;",
		"const raw = await api(item.path);",
		"if (generation !== item.generation) continue;",
		"item.commit(value);",
		"item.dirty = false;",
	)
	assertJavaScriptOrder(t, source,
		"function invalidateResources(...names)",
		"item.generation++;",
		"item.dirty = true;",
	)
}

func manageJavaScript(t *testing.T) string {
	t.Helper()
	raw, err := uiFiles.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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
