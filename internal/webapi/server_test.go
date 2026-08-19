package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/internal/backup"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/plugin/plugintest"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

func TestManageAssetKeepsItsDeviceFeatures(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	asset := request(t, server.Handler(), http.MethodGet, "/assets/app.js", nil, "")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "connectRealtime") ||
		!strings.Contains(asset.Body.String(), "percentage-control") || !strings.Contains(asset.Body.String(), "applyRealtimeDevice") ||
		!strings.Contains(asset.Body.String(), "loadDeviceMedia(device.id)") || !strings.Contains(asset.Body.String(), "renameDevice") ||
		!strings.Contains(asset.Body.String(), "hardwareName") || !strings.Contains(asset.Body.String(), "renderDeviceConfiguration") ||
		!strings.Contains(asset.Body.String(), "quickCapabilityPriority") || !strings.Contains(asset.Body.String(), "device-config-group") ||
		!strings.Contains(asset.Body.String(), "$('device-popover').showModal()") {
		t.Fatalf("Manage asset lost a device-management feature: status=%d", asset.Code)
	}
	if strings.Contains(asset.Body.String(), "reorderDeviceGroup") || strings.Contains(asset.Body.String(), "dragGroupId") {
		t.Fatal("Manage still contains group drag-and-drop code")
	}
	page := request(t, server.Handler(), http.MethodGet, "/", nil, "")
	for _, needle := range []string{`<dialog id="device-popover"`, `data-close="device-popover"`, `data-device-tab="overview"`, `data-device-tab="configuration"`,
		`class="brand"`, `/assets/stulp.svg`, `name="color-scheme" content="dark"`} {
		if !strings.Contains(page.Body.String(), needle) {
			t.Errorf("Manage page does not contain modal device dialog element %q", needle)
		}
	}
	if strings.Contains(page.Body.String(), `popover="auto"`) || strings.Contains(page.Body.String(), `popovertarget="device-popover"`) {
		t.Error("Manage page still exposes clickable content behind the device dialog")
	}
	logo := request(t, server.Handler(), http.MethodGet, "/assets/stulp.svg", nil, "")
	if logo.Code != http.StatusOK || !strings.Contains(logo.Header().Get("Content-Type"), "image/svg+xml") ||
		!strings.Contains(logo.Body.String(), "Stulp-tulp") {
		t.Fatalf("Stulp SVG logo is not served as an image: status=%d type=%q", logo.Code, logo.Header().Get("Content-Type"))
	}
	darkFrame := request(t, server.Handler(), http.MethodGet, "/assets/app-frame.css", nil, "")
	if darkFrame.Code != http.StatusOK || !strings.Contains(darkFrame.Body.String(), "color-scheme: dark") {
		t.Fatalf("app-owned UI dark frame is unavailable: status=%d", darkFrame.Code)
	}
	if err := database.InstallMatterApp(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	matterDevice, err := database.AddDevice(context.Background(), store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Aqara Dimmer Switch H2 EU", Class: "light",
		Data: map[string]any{"id": "matter-dimmer"}, Capabilities: []string{"onoff", "dim"},
		State: map[string]any{"onoff": false, "dim": 0.4},
	})
	if err != nil {
		t.Fatal(err)
	}
	liveEvent := server.realtimeEvent(store.Event{
		Manager: "devices", Type: "device.update", ID: matterDevice.ID, Data: matterDevice,
	})
	liveDevice, ok := liveEvent.Data.(map[string]any)
	if !ok || liveDevice["id"] != matterDevice.ID || liveDevice["hardwareName"] != "Aqara Dimmer Switch H2 EU" {
		t.Fatalf("realtime event did not carry the Manage device object: %#v", liveEvent.Data)
	}
	liveCapabilities, _ := liveDevice["capabilitiesObj"].(map[string]any)
	dim, _ := liveCapabilities["dim"].(map[string]any)
	if dim["value"] != 0.4 || dim["step"] != 0.01 {
		t.Fatalf("realtime capability did not carry its current value and metadata: %#v", dim)
	}
	// Hernoemen loopt sinds de overstap via de app die het apparaat bezit, dus
	// dat vraagt een draaiende app. Dat is een verschil met vroeger, toen Matter
	// binnen Stulp zat: de weergavenaam is data van Stulp en zou niet van een
	// draaiend proces af moeten hangen. Zie docs/todo.md.
	renameResponse := request(t, server.Handler(), http.MethodPut,
		"/api/manager/devices/device/"+matterDevice.ID, map[string]any{"name": "Ganglicht"}, "")
	if renameResponse.Code == http.StatusOK {
		t.Fatal("renaming succeeded without the owning app running; update this test if that was fixed")
	}

	// Stulp kent geen Matter-routes: koppelen gaat via de koppelpagina van de
	// app, en het netwerkoverzicht via zijn configuratiepagina. Matter is een
	// app als alle andere, dus de kern hoort er geen woord over te bevatten.
	gone := request(t, server.Handler(), http.MethodPost, "/api/stulp/matter/commission", map[string]any{"code": "nope"}, "")
	if gone.Code != http.StatusNotFound {
		t.Fatalf("Matter commissioning route is still wired: %d", gone.Code)
	}
	created, err := database.CreateNotification(context.Background(), store.NativeMatterAppID, "Live melding")
	if err != nil {
		t.Fatal(err)
	}
	notificationResponse := request(t, server.Handler(), http.MethodGet, "/api/manager/notifications/notification", nil, "")
	var notifications map[string]store.Notification
	decodeResponse(t, notificationResponse, &notifications)
	if notifications[created.ID].Excerpt != "Live melding" {
		t.Fatalf("notification API did not expose history: %#v", notifications)
	}
}

func TestAnnouncedAppManifestFeedsManageDriversAndConfiguration(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	announced, err := manifest.Parse([]byte(`{
  "id":"com.example.slot","version":"1.0.0","sdk":3,
  "name":{"nl":"Slot-app"},
  "drivers":[{
    "id":"switch","name":{"nl":"Schakelaar"},"class":"socket",
    "capabilities":["onoff"],
    "settings":[{"id":"address","type":"text","label":{"nl":"Adres"}}],
    "pair":[{"id":"search"},{"id":"list_devices","template":"list_devices"}]
  }],
  "ui":{"assets":["settings/index.html","drivers/switch/pair/search.html"]}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.OfferApp(ctx, announced); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AcceptApp(ctx, announced.ID); err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Language: "nl"})
	defer server.Close()

	response := request(t, server.Handler(), http.MethodGet, "/api/manager/apps/app", nil, "")
	var appObjects map[string]map[string]any
	decodeResponse(t, response, &appObjects)
	if appObjects[announced.ID]["name"] != "Slot-app" || appObjects[announced.ID]["settings"] != true {
		t.Fatalf("announced app configuration did not reach Manage: %#v", appObjects[announced.ID])
	}

	response = request(t, server.Handler(), http.MethodGet, "/api/manager/drivers/driver", nil, "")
	var drivers map[string]map[string]any
	decodeResponse(t, response, &drivers)
	driver := drivers["stulp:app:"+announced.ID+":switch"]
	if driver["name"] != "Schakelaar" {
		t.Fatalf("announced driver did not reach Manage: %#v", driver)
	}
	settings, _ := driver["settings"].([]any)
	custom, _ := driver["customPairViews"].([]any)
	if len(settings) != 1 || len(custom) != 1 || custom[0] != "search" {
		t.Fatalf("driver configuration/custom pairing was lost: %#v", driver)
	}

	response = request(t, server.Handler(), http.MethodGet,
		"/api/manager/drivers/driver/stulp:app:"+announced.ID+":switch", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("single announced driver returned %d: %s", response.Code, response.Body.String())
	}
}

func TestAnnouncedAppServesItsEmbeddedConfigurationToManage(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	appJSON := []byte(`{
  "id":"com.example.embedded-ui","version":"1.0.0","sdk":3,
  "name":{"nl":"Ingebedde UI"},
  "drivers":[{"id":"switch","name":{"nl":"Schakelaar"},"pair":[{"id":"search"}]}]
}`)
	appManifest, err := manifest.Parse(appJSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.OfferApp(ctx, appManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AcceptApp(ctx, appManifest.ID); err != nil {
		t.Fatal(err)
	}

	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	directory, err := os.MkdirTemp("", "stulp-web-ui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	listener, err := appproto.Listen(filepath.Join(directory, "attach.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = apps.ServeAttach(listener) }()

	done := make(chan error, 1)
	go func() {
		done <- appsdk.Attach(appsdk.AttachConfig{
			Target: listener.Addr().String(), AppID: appManifest.ID, Manifest: appJSON,
		}, appsdk.Plugin{UI: fstest.MapFS{
			"settings/index.html":             {Data: []byte(`<html><head></head><body>Pluginconfiguratie</body></html>`)},
			"settings/page.js":                {Data: []byte(`window.configLoaded = true;`)},
			"drivers/switch/pair/search.html": {Data: []byte(`<p>Eigen zoekpagina</p>`)},
			"locales/nl.json":                 {Data: []byte(`{"title":"Instellingen"}`)},
		}})
	}()
	t.Cleanup(func() {
		apps.Close()
		listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("de embedded-UI-testplugin stopte niet")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for apps.State(appManifest.ID).State != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("aangemelde app werd niet running: %#v", apps.State(appManifest.ID))
		}
		time.Sleep(time.Millisecond)
	}

	server := New(database, apps, Options{Language: "nl"})
	defer server.Close()
	response := request(t, server.Handler(), http.MethodGet,
		"/app-ui/"+appManifest.ID+"/settings/", nil, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Pluginconfiguratie") ||
		!strings.Contains(response.Body.String(), "__STULP_CONTEXT__") ||
		!strings.Contains(response.Body.String(), `"title":"Instellingen"`) {
		t.Fatalf("embedded settings page = %d %s", response.Code, response.Body.String())
	}
	response = request(t, server.Handler(), http.MethodGet,
		"/app-ui/"+appManifest.ID+"/settings/page.js", nil, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "configLoaded") {
		t.Fatalf("embedded settings asset = %d %s", response.Code, response.Body.String())
	}
	response = request(t, server.Handler(), http.MethodGet,
		"/app-ui/"+appManifest.ID+"/pair/switch/search.html?session=s1", nil, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Eigen zoekpagina") {
		t.Fatalf("embedded pair page = %d %s", response.Code, response.Body.String())
	}

	updated, err := database.App(ctx, appManifest.ID)
	if err != nil || !appHasUIAsset(updated, "settings/index.html") ||
		!appHasUIAsset(updated, "drivers/switch/pair/search.html") {
		t.Fatalf("embedded UI catalog did not reach the stored manifest: app=%#v err=%v", updated, err)
	}
}

func TestAPIControlsRunningApp(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	appManifest, root, err := manifest.Load(plugintest.Example(t, filepath.Join("..", "..", "examples", "virtual")))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallApp(ctx, appManifest, root, ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: appManifest.ID, DriverID: "switch", Name: "Kitchen switch", Class: "socket",
		Data: map[string]any{"id": "api-switch"}, Capabilities: []string{"onoff"},
	})
	if err != nil {
		t.Fatal(err)
	}

	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	if err := apps.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	apiServer := New(database, apps, Options{})
	defer apiServer.Close()
	api := apiServer.Handler()

	response := request(t, api, http.MethodGet, "/api/manager/devices/device", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list devices returned %d: %s", response.Code, response.Body.String())
	}
	var devices map[string]map[string]any
	decodeResponse(t, response, &devices)
	if devices[device.ID]["name"] != "Kitchen switch" || devices[device.ID]["hardwareName"] != "Kitchen switch" {
		t.Fatalf("unexpected device response: %#v", devices[device.ID])
	}
	if devices[device.ID]["driverId"] != "stulp:app:com.stulp.virtual:switch" {
		t.Fatalf("unexpected driver URI: %#v", devices[device.ID]["driverId"])
	}
	if devices[device.ID]["appId"] != appManifest.ID || devices[device.ID]["manufacturer"] != "Stulp Virtual" {
		t.Fatalf("device manufacturer grouping metadata is missing: %#v", devices[device.ID])
	}
	response = request(t, api, http.MethodPut, "/api/manager/devices/device/"+device.ID,
		map[string]any{"name": "Ganglicht"}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("rename plugin device returned %d: %s", response.Code, response.Body.String())
	}
	var renamedDevice map[string]any
	decodeResponse(t, response, &renamedDevice)
	if renamedDevice["name"] != "Ganglicht" || renamedDevice["hardwareName"] != "Kitchen switch" {
		t.Fatalf("display/hardware names were not separated: %#v", renamedDevice)
	}
	renamedStored, err := database.Device(ctx, device.ID)
	if err != nil || renamedStored.Name != "Ganglicht" || renamedStored.HardwareName() != "Kitchen switch" {
		t.Fatalf("plugin rename was not persisted: device=%#v err=%v", renamedStored, err)
	}
	response = request(t, api, http.MethodPost, "/api/stulp/device-groups", map[string]any{"name": "Woonkamer"}, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("create device group returned %d: %s", response.Code, response.Body.String())
	}
	var deviceGroup store.DeviceGroup
	if err := json.Unmarshal(response.Body.Bytes(), &deviceGroup); err != nil {
		t.Fatal(err)
	}
	response = request(t, api, http.MethodPost, "/api/stulp/device-groups",
		map[string]any{"name": "Slaapkamer", "parentId": deviceGroup.ID}, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("create child device group returned %d: %s", response.Code, response.Body.String())
	}
	var childGroup store.DeviceGroup
	if err := json.Unmarshal(response.Body.Bytes(), &childGroup); err != nil {
		t.Fatal(err)
	}
	if childGroup.ParentID != deviceGroup.ID {
		t.Fatalf("child group has no parent: %#v", childGroup)
	}
	response = request(t, api, http.MethodPut, "/api/stulp/devices/"+device.ID+"/group", map[string]any{"groupId": childGroup.ID}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("move device to group returned %d: %s", response.Code, response.Body.String())
	}
	var groupedDevice map[string]any
	decodeResponse(t, response, &groupedDevice)
	if groupedDevice["groupId"] != childGroup.ID {
		t.Fatalf("device group membership is missing: %#v", groupedDevice)
	}
	response = request(t, api, http.MethodGet, "/api/stulp/device-groups", nil, "")
	var deviceGroups []store.DeviceGroup
	decodeResponse(t, response, &deviceGroups)
	if len(deviceGroups) != 2 {
		t.Fatalf("unexpected device groups: %#v", deviceGroups)
	}
	response = request(t, api, http.MethodPut, "/api/stulp/device-groups/"+deviceGroup.ID,
		map[string]any{"parentId": childGroup.ID}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("cyclic device group update returned %d: %s", response.Code, response.Body.String())
	}

	events, cancel := database.Subscribe(8)
	defer cancel()
	response = request(t, api, http.MethodPut,
		"/api/manager/devices/device/"+device.ID+"/capability/onoff",
		map[string]any{"value": true}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("set capability returned %d: %s", response.Code, response.Body.String())
	}
	updated, err := database.Device(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := updated.State["onoff"].(bool); !ok || !value {
		t.Fatalf("capability listener did not persist true: %#v", updated.State)
	}
	response = request(t, api, http.MethodGet,
		"/api/manager/devices/device/"+device.ID+"/capability/onoff", nil, "")
	if response.Code != http.StatusOK || response.Body.String() != "true\n" {
		t.Fatalf("unexpected capability read response: %d %q", response.Code, response.Body.String())
	}
	select {
	case event := <-events:
		if event.Manager != "devices" || event.Type != "device.update" || event.ID != device.ID {
			t.Fatalf("unexpected realtime event: %#v", event)
		}
	default:
		t.Fatal("capability update did not emit a realtime event")
	}

	response = request(t, api, http.MethodGet, "/api/manager/apps/app", nil, "")
	var installedApps map[string]map[string]any
	decodeResponse(t, response, &installedApps)
	if installedApps[appManifest.ID]["state"] != "running" {
		t.Fatalf("app is not exposed as running: %#v", installedApps[appManifest.ID])
	}
	response = request(t, api, http.MethodGet, "/api/stulp/apps/"+appManifest.ID+"/registrations", nil, "")
	var registrations plugin.RegistrationSnapshot
	decodeResponse(t, response, &registrations)
	if len(registrations.Drivers) != 1 || len(registrations.Flows) != 4 {
		t.Fatalf("unexpected registrations: %#v", registrations)
	}
	response = request(t, api, http.MethodGet, "/api/stulp/flow/cards", nil, "")
	var flowCards map[string][]map[string]any
	decodeResponse(t, response, &flowCards)
	if len(flowCards["triggers"]) < 2 || len(flowCards["actions"]) < 3 {
		t.Fatalf("manifest and Stulp Flow cards were not exposed: %#v", flowCards)
	}
	response = request(t, api, http.MethodPost, "/api/stulp/flow/autocomplete", map[string]any{
		"appId": appManifest.ID, "cardId": "choose", "cardType": "action", "argument": "choice", "query": "API", "args": map[string]any{},
	}, "")
	var autocomplete []map[string]any
	decodeResponse(t, response, &autocomplete)
	if len(autocomplete) != 1 || autocomplete[0]["name"] != "One API" {
		t.Fatalf("unexpected Flow autocomplete result: %#v", autocomplete)
	}
	response = request(t, api, http.MethodPost, "/api/manager/flow/flow", map[string]any{
		"name": "API Flow", "enabled": true,
		"nodes": []any{
			map[string]any{"id": "node-0", "x": 80, "y": 120, "step": map[string]any{
				"appId": "stulp", "cardId": "device_capability_changed", "cardType": "trigger",
				"args": map[string]any{"device": map[string]any{"$device": device.ID}},
			}},
			map[string]any{"id": "node-1", "x": 480, "y": 120, "step": map[string]any{
				"appId": appManifest.ID, "cardId": "device_name", "cardType": "action",
				"args": map[string]any{"device": map[string]any{"$device": device.ID}},
			}},
		},
		"edges": []any{map[string]any{"id": "edge-0", "from": "node-0", "to": "node-1"}},
	}, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("create Flow returned %d: %s", response.Code, response.Body.String())
	}
	var createdFlow store.Flow
	if err := json.Unmarshal(response.Body.Bytes(), &createdFlow); err != nil {
		t.Fatal(err)
	}
	response = request(t, api, http.MethodPost, "/api/stulp/flows/"+createdFlow.ID+"/run", nil, "")
	var flowRun map[string]any
	decodeResponse(t, response, &flowRun)
	if flowRun["success"] != true {
		t.Fatalf("Flow did not execute through the API: %#v", flowRun)
	}
	response = request(t, api, http.MethodPut, "/api/manager/flow/flow/"+createdFlow.ID+"/enabled", map[string]any{"enabled": false}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("disable Flow returned %d: %s", response.Code, response.Body.String())
	}
	response = request(t, api, http.MethodPost, "/api/stulp/apps/"+appManifest.ID+"/api/echo", map[string]any{"value": "woot"}, "")
	var apiResult map[string]any
	decodeResponse(t, response, &apiResult)
	if apiResult["value"] != "woot" || apiResult["app"] != appManifest.ID {
		t.Fatalf("unexpected custom app API result: %#v", apiResult)
	}
	response = request(t, api, http.MethodPut, "/api/manager/apps/app/"+appManifest.ID+"/setting/displayName", map[string]any{"value": "Keuken"}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("set app setting returned %d: %s", response.Code, response.Body.String())
	}
	response = request(t, api, http.MethodPost, "/api/stulp/apps/"+appManifest.ID+"/api/echo", map[string]any{"value": "event"}, "")
	decodeResponse(t, response, &apiResult)
	if apiResult["lastSetting"] != "displayName" {
		t.Fatalf("settings page update did not notify running app: %#v", apiResult)
	}
	response = request(t, api, http.MethodGet, "/api/stulp/apps/"+appManifest.ID+"/drivers/switch/pair/devices", nil, "")
	var candidates []map[string]any
	decodeResponse(t, response, &candidates)
	if len(candidates) != 1 || candidates[0]["name"] != "Stulp switch" {
		t.Fatalf("unexpected pair candidates: %#v", candidates)
	}
	response = request(t, api, http.MethodPost, "/api/stulp/pair", map[string]any{"appId": appManifest.ID, "driverId": "switch"}, "")
	var pairSession map[string]any
	if response.Code != http.StatusCreated {
		t.Fatalf("create pair session returned %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pairSession); err != nil {
		t.Fatal(err)
	}
	pairID, _ := pairSession["id"].(string)
	response = request(t, api, http.MethodPost, "/api/stulp/pair/"+pairID+"/emit/validate", nil, "")
	if response.Code != http.StatusOK || response.Body.String() != "\"ok\"\n" {
		t.Fatalf("pair view event returned %d: %s", response.Code, response.Body.String())
	}
	response = request(t, api, http.MethodDelete, "/api/stulp/pair/"+pairID, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("close pair session returned %d: %s", response.Code, response.Body.String())
	}
	response = request(t, api, http.MethodPost, "/api/stulp/apps/"+appManifest.ID+"/drivers/switch/pair/devices", candidates[0], "")
	if response.Code != http.StatusCreated {
		t.Fatalf("add paired device returned %d: %s", response.Code, response.Body.String())
	}
	pairedDevices, err := database.Devices(ctx, appManifest.ID)
	if err != nil || len(pairedDevices) != 2 {
		t.Fatalf("paired device was not persisted and started: count=%d err=%v", len(pairedDevices), err)
	}
	response = request(t, api, http.MethodGet, "/app-ui/"+appManifest.ID+"/settings/", nil, "")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("__STULP_CONTEXT__")) || !bytes.Contains(response.Body.Bytes(), []byte("/stulp.js")) {
		t.Fatalf("app settings footprint was not hosted: %d %s", response.Code, response.Body.String())
	}
	response = request(t, api, http.MethodGet, "/app-ui/"+appManifest.ID+"/pair/switch/validate.html?session=test", nil, "")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("Stulp.emit")) || !bytes.Contains(response.Body.Bytes(), []byte("/stulp.js")) {
		t.Fatalf("pair footprint was not hosted: %d %s", response.Code, response.Body.String())
	}
	response = request(t, api, http.MethodGet, "/api/stulp/devices/"+device.ID+"/media", nil, "")
	var deviceMedia []plugin.MediaRegistration
	decodeResponse(t, response, &deviceMedia)
	if len(deviceMedia) != 2 || deviceMedia[0].Slot != "live" || deviceMedia[0].Kind != "image" || deviceMedia[1].Kind != "video" {
		t.Fatalf("unexpected device media: %#v", deviceMedia)
	}
	if bytes.Contains(response.Body.Bytes(), []byte("live.mp4")) {
		t.Fatal("media registration response leaked the stream address")
	}
	// De keten tot aan de plugin: route, supervisor, resolve, en dan de poging
	// om op te halen. De voorbeeldplugin wijst naar een poort waar niets
	// luistert, dus dit hoort te falen -- maar als 502 met uitleg, niet als 404.
	response = request(t, api, http.MethodGet,
		"/api/stulp/devices/"+device.ID+"/media/live/stream", nil, "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("unreachable stream returned %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("does not serve this stream")) {
		t.Fatalf("unreachable stream did not say why: %s", response.Body.String())
	}

	response = request(t, api, http.MethodGet, "/api/manager/system/ping", nil, "")
	if response.Code != http.StatusOK || response.Body.String() != "true\n" {
		t.Fatalf("unexpected ping response: %d %q", response.Code, response.Body.String())
	}
}

func TestAccessKeyOpensManageAndItsSession(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	apiServer := New(database, apps, Options{Token: "secret"})
	defer apiServer.Close()
	handler := apiServer.Handler()

	closed := request(t, handler, http.MethodGet, "/", nil, "")
	if closed.Code != http.StatusNotFound {
		t.Fatalf("root without key returned %d", closed.Code)
	}
	webapp := request(t, handler, http.MethodGet, "/secret", nil, "")
	if webapp.Code != http.StatusOK || !bytes.Contains(webapp.Body.Bytes(), []byte("Apps & settings")) ||
		!bytes.Contains(webapp.Body.Bytes(), []byte("Backup downloaden")) || !bytes.Contains(webapp.Body.Bytes(), []byte("Nieuwe Flow")) ||
		!bytes.Contains(webapp.Body.Bytes(), []byte(`id="flow-canvas"`)) || !bytes.Contains(webapp.Body.Bytes(), []byte(`id="flow-links"`)) ||
		!bytes.Contains(webapp.Body.Bytes(), []byte(`id="flow-card-dialog"`)) || !bytes.Contains(webapp.Body.Bytes(), []byte(`id="flow-type-dialog"`)) ||
		!bytes.Contains(webapp.Body.Bytes(), []byte(`id="group-dialog"`)) || !bytes.Contains(webapp.Body.Bytes(), []byte(`id="add-group"`)) ||
		!bytes.Contains(webapp.Body.Bytes(), []byte(`id="download-backup"`)) || !bytes.Contains(webapp.Body.Bytes(), []byte(`id="restore-backup"`)) ||
		!bytes.Contains(webapp.Body.Bytes(), []byte(`id="restore-dialog"`)) || !bytes.Contains(webapp.Body.Bytes(), []byte(`id="notifications-dialog"`)) {
		t.Fatalf("webapp unavailable through key URL: %d %s", webapp.Code, webapp.Body.String())
	}
	if bytes.Contains(webapp.Body.Bytes(), []byte("API-sleutel")) || bytes.Contains(webapp.Body.Bytes(), []byte("Bearer token")) {
		t.Fatal("Manage still contains the old API key UI")
	}
	asset := request(t, handler, http.MethodGet, "/assets/app.js", nil, "")
	if bytes.Contains(asset.Body.Bytes(), []byte("stulp-token")) || bytes.Contains(asset.Body.Bytes(), []byte("authHeaders")) {
		t.Fatal("Manage still stores or sends the old API credential")
	}
	cookie := strings.Split(webapp.Header().Get("Set-Cookie"), ";")[0]
	if !strings.HasPrefix(cookie, accessCookie+"=") || !strings.Contains(webapp.Header().Get("Set-Cookie"), "HttpOnly") ||
		!strings.Contains(webapp.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("key entry did not set a protected session cookie: %q", webapp.Header().Get("Set-Cookie"))
	}

	unauthorized := request(t, handler, http.MethodGet, "/api/stulp/health", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("without key session: got %d", unauthorized.Code)
	}
	stillUnauthorized := request(t, handler, http.MethodGet, "/api/stulp/health", nil, "Bearer secret")
	if stillUnauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("old bearer token still works: got %d", stillUnauthorized.Code)
	}
	authorized := requestWithCookie(t, handler, http.MethodGet, "/api/stulp/health", nil, cookie)
	if authorized.Code != http.StatusOK {
		t.Fatalf("with key session: got %d", authorized.Code)
	}
	rootAfterEntry := requestWithCookie(t, handler, http.MethodGet, "/", nil, cookie)
	if rootAfterEntry.Code != http.StatusOK {
		t.Fatalf("session could not reopen Manage: got %d", rootAfterEntry.Code)
	}
	backupResponse := requestWithCookie(t, handler, http.MethodGet, "/api/stulp/backup", nil, cookie)
	if backupResponse.Code != http.StatusOK || backupResponse.Header().Get("Content-Type") != "application/zip" ||
		!bytes.HasPrefix(backupResponse.Body.Bytes(), []byte("PK")) {
		t.Fatalf("authenticated backup failed: %d %q", backupResponse.Code, backupResponse.Header().Get("Content-Type"))
	}
}

func TestManageRestoresAnAnnouncedAppBackup(t *testing.T) {
	ctx := context.Background()
	announced, err := manifest.Parse([]byte(`{
  "id":"com.stulp.restore-ui","version":"2.0.0","sdk":3,
  "name":{"nl":"Restore"},"drivers":[{"id":"sensor","name":{"nl":"Sensor"}}],
  "ui":{"settings":"/settings/index.html"}
}`))
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Open(filepath.Join(t.TempDir(), "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.OfferApp(ctx, announced); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AcceptApp(ctx, announced.ID); err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice(ctx, store.Device{
		AppID: announced.ID, DriverID: "sensor", Name: "Herstelde sensor", Class: "sensor",
		Data: map[string]any{"serial": "restore-1"}, Capabilities: []string{"measure_temperature"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := backup.Write(ctx, source, &archive); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(t.TempDir(), "target.json")
	target, err := store.Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	apps := supervisor.New(target, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(target, apps, Options{Token: "secret", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()
	entry := request(t, server.Handler(), http.MethodGet, "/secret", nil, "")
	cookie := strings.Split(entry.Header().Get("Set-Cookie"), ";")[0]

	upload := httptest.NewRequest(http.MethodPost, "/api/stulp/restore", bytes.NewReader(archive.Bytes()))
	upload.Header.Set("Cookie", cookie)
	upload.Header.Set("Content-Type", "application/zip")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, upload)
	if response.Code != http.StatusOK {
		t.Fatalf("restore returned %d: %s", response.Code, response.Body.String())
	}
	var answer map[string]any
	decodeResponse(t, response, &answer)
	if answer["restored"] != true || answer["previousDocument"] == "" {
		t.Fatalf("restore result is incomplete: %#v", answer)
	}
	restored, err := target.Device(ctx, device.ID)
	if err != nil || restored.Name != device.Name {
		t.Fatalf("device did not reach the live store: %#v err=%v", restored, err)
	}
	// Het manifest (en dus de UI-beschrijving) reist niet mee in een backup —
	// de app vertelt het zelf bij zijn eerstvolgende announce; hier hoort na
	// een restore alleen zijn identiteit te staan.
	app, err := target.App(ctx, announced.ID)
	if err != nil || !app.Enabled || app.Version == "" {
		t.Fatalf("announced app identity did not reach the live store: %#v err=%v", app, err)
	}
	if runtime := apps.State(announced.ID); runtime.State != "waiting" {
		t.Fatalf("announced app did not resume in reconnect state: %#v", runtime)
	}

	bad := httptest.NewRequest(http.MethodPost, "/api/stulp/restore", strings.NewReader("not a zip"))
	bad.Header.Set("Cookie", cookie)
	bad.Header.Set("Content-Type", "application/zip")
	badResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid backup returned %d: %s", badResponse.Code, badResponse.Body.String())
	}
	if _, err := target.Device(ctx, device.ID); err != nil {
		t.Fatalf("invalid restore changed the live document: %v", err)
	}
}

func request(t *testing.T, handler http.Handler, method, path string, body any, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, input)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requestWithCookie(t *testing.T, handler http.Handler, method, path string, body any, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, input)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Cookie", cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("response returned %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

type webRoundTripFunc func(*http.Request) (*http.Response, error)

func (function webRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

//
// Een app is een binary plus zijn manifest, dus de binary hoort erin -- met zijn
// uitvoerbit, anders installeert Stulp iets dat hij niet kan starten.

func TestManagerFilterNeverHidesTheReloadMarker(t *testing.T) {
	reload := store.Event{Manager: "store", Type: "store.reload"}
	deviceUpdate := store.Event{Manager: "devices", Type: "device.update"}
	appUpdate := store.Event{Manager: "apps", Type: "app.update"}

	for _, test := range []struct {
		name    string
		event   store.Event
		manager string
		want    bool
	}{
		{name: "unfiltered stream takes everything", event: appUpdate, want: true},
		{name: "matching manager passes", event: deviceUpdate, manager: "devices", want: true},
		{name: "other manager is filtered out", event: appUpdate, manager: "devices", want: false},
		{name: "reload marker survives a filter", event: reload, manager: "devices", want: true},
		{name: "reload marker survives any filter", event: reload, manager: "flow", want: true},
	} {
		if got := deliverToStream(test.event, test.manager); got != test.want {
			t.Errorf("%s: deliverToStream(%q, %q) = %v, want %v",
				test.name, test.event.Manager, test.manager, got, test.want)
		}
	}
}

// Een zonwering hoort te bedienen te zijn.
//
// windowcoverings_state eindigt op _state en niet op _set, en viel daarmee door
// de standaardregel heen: de interface toonde de richting als een waarde die je
// alleen kunt aflezen, terwijl elke zonweringsplugin er juist commando's op
// aanneemt. En zonder de drie standen erbij weet de interface niet wat er te
// kiezen valt en biedt ze een tekstveld aan waar "up" in getypt moet worden.
func TestACoveringCanBeOperatedFromTheInterface(t *testing.T) {
	if !defaultCapabilitySetable("windowcoverings_state") {
		t.Error("windowcoverings_state geldt niet als bedienbaar")
	}

	metadata := map[string]any{}
	applyDefaultCapabilityMetadata(metadata, "windowcoverings_state")
	values, ok := metadata["values"].([]any)
	if !ok || len(values) != 3 {
		t.Fatalf("windowcoverings_state biedt %v aan, wil drie standen", metadata["values"])
	}
	want := map[string]bool{"up": false, "idle": false, "down": false}
	for _, value := range values {
		entry, _ := value.(map[string]any)
		id, _ := entry["id"].(string)
		if _, known := want[id]; !known {
			t.Errorf("onbekende stand %q", id)
			continue
		}
		want[id] = true
		if entry["title"] == nil {
			t.Errorf("stand %q heeft geen naam", id)
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("stand %q ontbreekt", id)
		}
	}

	// De positie blijft een getal van 0 tot 1 met stapjes -- daar maakt de
	// interface een schuif van.
	position := map[string]any{}
	applyDefaultCapabilityMetadata(position, "windowcoverings_set")
	if position["type"] != "number" || position["min"] != 0.0 || position["max"] != 1.0 {
		t.Errorf("windowcoverings_set is %v", position)
	}
	if !defaultCapabilitySetable("windowcoverings_set") {
		t.Error("windowcoverings_set geldt niet als bedienbaar")
	}
}

// Installeren is accepteren wat zich gemeld heeft, en niets meer. Er valt niets
// te downloaden: een app komt binnen doordat iemand hem neerzet, meldt zich met
// zijn manifest, en staat dan als aangeboden in het document.
//
// De knop is de handeling die de app bewust NIET zelf mag doen. Zou aanmelden al
// genoeg zijn, dan was een gelekt token een sleutel tot het huis.
func TestInstallerenIsAccepteren(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	// Zoals de supervisor hem opschrijft als hij zich meldt.
	announced, err := manifest.Parse([]byte(
		`{"id":"com.acme.aangeboden","version":"3.0.0","sdk":3,"name":{"en":"Offered"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.OfferApp(ctx, announced); err != nil {
		t.Fatal(err)
	}

	before, err := database.App(ctx, announced.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Offered || before.Enabled {
		t.Fatalf("voor het installeren: offered=%v enabled=%v", before.Offered, before.Enabled)
	}

	response := request(t, server.Handler(), http.MethodPost,
		"/api/stulp/apps/"+announced.ID+"/install", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("install gaf %d: %s", response.Code, response.Body.String())
	}

	after, err := database.App(ctx, announced.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case after.Offered:
		t.Error("hij staat er na installeren nog als aangeboden in")
	case !after.Enabled:
		t.Error("hij mag na installeren draaien")
	case after.Version != "3.0.0":
		t.Errorf("versie = %q, want die uit zijn eigen manifest", after.Version)
	}

	// Een app die zich nooit gemeld heeft, valt niet te installeren.
	if response := request(t, server.Handler(), http.MethodPost,
		"/api/stulp/apps/com.acme.nooit/install", nil, ""); response.Code != http.StatusNotFound {
		t.Errorf("een onbekende app gaf %d, want 404", response.Code)
	}
}

// De brug moet zonder sessiecookie te laden zijn. Een app-pagina draait in een
// iframe met sandbox="allow-scripts allow-forms allow-modals" — zonder
// allow-same-origin, dus met een opaque origin — en zo'n document stuurt bij een
// subresource geen SameSite=Strict-cookie mee. Stond /stulp.js achter de sleutel,
// dan kreeg élke koppel- en instelpagina een 404 op zijn brug en meldde de eerste
// aanroep "Stulp is not defined".
func TestBridgeScriptLoadsWithoutTheSessionCookie(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	apiServer := New(database, apps, Options{Token: "secret"})
	defer apiServer.Close()
	handler := apiServer.Handler()

	bridge := request(t, handler, http.MethodGet, "/stulp.js", nil, "")
	if bridge.Code != http.StatusOK {
		t.Fatalf("de brug gaf %d zonder cookie: een gesandboxte app-pagina kan hem dan nooit laden", bridge.Code)
	}
	if !bytes.Contains(bridge.Body.Bytes(), []byte("stulp-plugin-response")) {
		t.Fatalf("dit is de brug niet: %.80s", bridge.Body.String())
	}

	// En de sleutel blijft staan waar hij hoort.
	for _, path := range []string{"/", "/api/stulp/health"} {
		if closed := request(t, handler, http.MethodGet, path, nil, ""); closed.Code == http.StatusOK {
			t.Fatalf("%s is zonder sleutel open (%d)", path, closed.Code)
		}
	}
}

// En de brug alleen is niet genoeg: het eigen script van zo'n pagina (page.js)
// is nét zo goed een subresource zonder cookie. Na de brug-fix laadde het
// document wel (de navigatie komt van de ouder, mét cookie) maar bleef élke
// instelpagina leeg omdat page.js 404 gaf — page.js draaide dus helemaal niet,
// en de pagina vult zijn velden nooit. Heel /app-ui/ hoort publiek.
func TestAppPageScriptsLoadWithoutTheSessionCookie(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
	  "id":"com.acme.pages","version":"1.0.0","sdk":3,"name":{"en":"Pages"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings", "index.html"),
		[]byte(`<p>instellingen</p><script src="page.js"></script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings", "page.js"),
		[]byte(`Stulp.ready();`), 0o600); err != nil {
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
	defer database.Close()
	if err := database.InstallApp(context.Background(), appManifest, resolved, ""); err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	apiServer := New(database, apps, Options{Token: "secret"})
	defer apiServer.Close()
	handler := apiServer.Handler()

	page := request(t, handler, http.MethodGet, "/app-ui/com.acme.pages/settings/", nil, "")
	if page.Code != http.StatusOK || !bytes.Contains(page.Body.Bytes(), []byte("instellingen")) {
		t.Fatalf("de instelpagina zelf laadt niet zonder cookie: %d", page.Code)
	}
	script := request(t, handler, http.MethodGet, "/app-ui/com.acme.pages/settings/page.js", nil, "")
	if script.Code != http.StatusOK || !bytes.Contains(script.Body.Bytes(), []byte("Stulp.ready")) {
		t.Fatalf("page.js laadt niet zonder cookie (%d): de pagina draait dan niet en blijft leeg", script.Code)
	}
}
