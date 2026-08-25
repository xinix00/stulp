package webapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

// mcpVirtualPairRuntime is the process boundary seen by the MCP test. It does
// not reimplement mcpCreateDevice: the deliberately odd return value from
// create proves that the handler completes the normal create -> list_devices ->
// add conversation instead of trusting an event response as a candidate.
type mcpVirtualPairRuntime struct {
	plugin.Runtime
	database *store.Store

	mu       sync.Mutex
	sessions map[string]map[string]any
	calls    []string
	nextID   int
	startErr error
	listErr  error
	closed   bool
}

func (r *mcpVirtualPairRuntime) Start(context.Context) error { return nil }

func (r *mcpVirtualPairRuntime) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

func (r *mcpVirtualPairRuntime) StartPairSession(_ context.Context, driverID, sessionID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "start")
	if driverID != virtualSwitchDriver {
		return nil, errors.New("wrong driver")
	}
	if r.sessions == nil {
		r.sessions = make(map[string]map[string]any)
	}
	r.sessions[sessionID] = nil
	if r.startErr != nil {
		return nil, r.startErr
	}
	return []string{"create", "list_devices"}, nil
}

func (r *mcpVirtualPairRuntime) PairEmit(_ context.Context, sessionID, event string, data any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, event)
	if _, exists := r.sessions[sessionID]; !exists {
		return nil, errors.New("unknown pair session")
	}
	switch event {
	case "create":
		request, _ := data.(map[string]any)
		name, _ := request["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, errors.New("name is required")
		}
		r.nextID++
		r.sessions[sessionID] = map[string]any{
			"name":  name,
			"data":  map[string]any{"id": "virtual-mcp-test-" + string(rune('0'+r.nextID))},
			"store": map[string]any{"onoff": false},
		}
		return "prepared, not a candidate", nil
	case "list_devices":
		if r.listErr != nil {
			return nil, r.listErr
		}
		candidate := r.sessions[sessionID]
		if candidate == nil {
			return nil, errors.New("not prepared")
		}
		return []any{candidate}, nil
	default:
		return nil, errors.New("unknown pair event")
	}
}

func (r *mcpVirtualPairRuntime) ClosePairSession(_ context.Context, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "close")
	delete(r.sessions, sessionID)
	return nil
}

func (r *mcpVirtualPairRuntime) AddPairedDevice(ctx context.Context, driverID string, candidate map[string]any) (store.Device, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "add")
	r.mu.Unlock()
	if driverID != virtualSwitchDriver {
		return store.Device{}, errors.New("wrong driver")
	}
	name, _ := candidate["name"].(string)
	data, _ := candidate["data"].(map[string]any)
	deviceStore, _ := candidate["store"].(map[string]any)
	return r.database.AddDevice(ctx, store.Device{
		AppID: virtualDevicesAppID, DriverID: virtualSwitchDriver, Name: name, Class: "other",
		Data: data, Store: deviceStore, Capabilities: []string{"onoff"},
		State: map[string]any{"onoff": false}, Available: true,
	})
}

func (r *mcpVirtualPairRuntime) Registrations(context.Context) (plugin.RegistrationSnapshot, error) {
	return plugin.RegistrationSnapshot{}, nil
}

func (r *mcpVirtualPairRuntime) InvokeCapability(ctx context.Context, deviceID, capabilityID string, value any, _ map[string]any) error {
	if capabilityID != "onoff" {
		return errors.New("unsupported capability")
	}
	on, ok := value.(bool)
	if !ok {
		return errors.New("onoff must be boolean")
	}
	device, err := r.database.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	device.Store["onoff"] = on
	device.State["onoff"] = on
	return r.database.UpdateDevice(ctx, device)
}

func (r *mcpVirtualPairRuntime) callLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func mcpVirtualCreateServer(t *testing.T) (*Server, *store.Store, *mcpVirtualPairRuntime) {
	t.Helper()
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	appManifest, err := manifest.Parse([]byte(`{
  "id":"com.stulp.virtualdevices","version":"1.0.0","sdk":3,
  "name":{"en":"Virtual devices","nl":"Virtuele apparaten"},
  "drivers":[{"id":"switch","name":{"en":"Virtual switch"},"class":"other","capabilities":["onoff"]}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallApp(context.Background(), appManifest, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	runtime := &mcpVirtualPairRuntime{database: database, sessions: make(map[string]map[string]any)}
	apps := supervisor.New(database, plugin.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRuntime: func(context.Context, *store.Store, string, plugin.Options) (plugin.Runtime, error) {
			return runtime, nil
		},
	})
	t.Cleanup(apps.Close)
	if err := apps.Start(context.Background(), virtualDevicesAppID); err != nil {
		t.Fatal(err)
	}
	server := New(database, apps, Options{Token: "secret", Language: "nl", StulpVersion: "test"})
	t.Cleanup(server.Close)
	return server, database, runtime
}

func TestMCPDevicesCreateUsesVirtualPluginPairingAndReturnsFlowReadyDevice(t *testing.T) {
	server, database, runtime := mcpVirtualCreateServer(t)
	handler := server.Handler()
	created := mcpStructured(t, mcpToolCall(t, handler, "devices_create", map[string]any{
		"type": "virtual_switch", "name": "  Alarm ingeschakeld  ",
	}))
	if created["created"] != true || created["type"] != "virtual_switch" {
		t.Fatalf("create result = %#v", created)
	}
	device, _ := created["device"].(map[string]any)
	deviceID, _ := device["id"].(string)
	if deviceID == "" || device["name"] != "Alarm ingeschakeld" || device["appId"] != virtualDevicesAppID || device["available"] != true {
		t.Fatalf("created device projection = %#v", device)
	}
	if device["data"] != nil || device["store"] != nil || device["settings"] != nil {
		t.Fatalf("private paired-device fields leaked through MCP: %#v", device)
	}
	capabilities, _ := device["capabilities"].(map[string]any)
	onoff, _ := capabilities["onoff"].(map[string]any)
	if onoff["type"] != "boolean" || onoff["setable"] != true || onoff["hasValue"] != true || onoff["value"] != false {
		t.Fatalf("created onoff capability = %#v", onoff)
	}
	stored, err := database.Device(context.Background(), deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Store["onoff"] != false || stored.State["onoff"] != false || stored.Data["id"] == nil {
		t.Fatalf("persisted virtual switch = %#v", stored)
	}
	if got := runtime.callLog(); !reflect.DeepEqual(got, []string{"start", "create", "list_devices", "add", "close"}) {
		t.Fatalf("pairing calls = %v", got)
	}

	// The freshly returned id is immediately accepted by the same generic
	// on/off action that MCP stores in a Flow graph.
	action := mcpStructured(t, mcpToolCall(t, handler, "flow_action_run", map[string]any{
		"appId": "stulp", "cardId": "capability.onoff.set", "args": map[string]any{
			"device": map[string]any{"$device": deviceID}, "value": true,
		},
	}))
	if action["action"] == nil {
		t.Fatalf("generic onoff Flow action result = %#v", action)
	}
	stored, err = database.Device(context.Background(), deviceID)
	if err != nil || stored.State["onoff"] != true || stored.Store["onoff"] != true {
		t.Fatalf("Flow action did not change durable virtual state: state=%#v err=%v", stored.State, err)
	}
}

func TestMCPDevicesCreateClosesPairingOnFailureAndRequiresRunningPlugin(t *testing.T) {
	server, database, runtime := mcpVirtualCreateServer(t)
	runtime.listErr = errors.New("list broke")
	failed := mcpToolCall(t, server.Handler(), "devices_create", map[string]any{
		"type": "virtual_switch", "name": "Mislukt",
	})
	if failed["isError"] != true || !strings.Contains(mcpToolText(failed), "list broke") {
		t.Fatalf("pair failure = %#v", failed)
	}
	if got := runtime.callLog(); !reflect.DeepEqual(got, []string{"start", "create", "list_devices", "close"}) {
		t.Fatalf("failed pairing calls = %v", got)
	}
	if devices, err := database.Devices(context.Background(), virtualDevicesAppID); err != nil || len(devices) != 0 {
		t.Fatalf("failed pairing persisted devices=%#v err=%v", devices, err)
	}

	missingServer, _, _ := mcpTestServer(t)
	missing := mcpToolCall(t, missingServer.Handler(), "devices_create", map[string]any{
		"type": "virtual_switch", "name": "Geen plugin",
	})
	if missing["isError"] != true || !strings.Contains(mcpToolText(missing), virtualDevicesAppID) {
		t.Fatalf("missing plugin result = %#v", missing)
	}
}

func TestMCPDevicesCreateCleansUpWhenPairStartResponseIsLost(t *testing.T) {
	server, database, runtime := mcpVirtualCreateServer(t)
	runtime.startErr = errors.New("pair.start response lost")
	failed := mcpToolCall(t, server.Handler(), "devices_create", map[string]any{
		"type": "virtual_switch", "name": "Geen spooksessie",
	})
	if failed["isError"] != true || !strings.Contains(mcpToolText(failed), "response lost") {
		t.Fatalf("pair.start failure = %#v", failed)
	}
	if got := runtime.callLog(); !reflect.DeepEqual(got, []string{"start", "close"}) {
		t.Fatalf("pair.start cleanup calls = %v", got)
	}
	if devices, err := database.Devices(context.Background(), virtualDevicesAppID); err != nil || len(devices) != 0 {
		t.Fatalf("failed pair.start persisted devices=%#v err=%v", devices, err)
	}
}

func TestMCPDevicesCreateKeepsParallelPairingSessionsIndependent(t *testing.T) {
	server, database, _ := mcpVirtualCreateServer(t)
	names := []string{"Variabele A", "Variabele B", "Variabele C", "Variabele D"}
	var wait sync.WaitGroup
	failures := make(chan string, len(names))
	for _, name := range names {
		name := name
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, known := server.callMCPTool(context.Background(), "devices_create", map[string]any{
				"type": "virtual_switch", "name": name,
			})
			if !known || result["isError"] == true {
				failures <- fmt.Sprintf("%s: %#v", name, result)
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	devices, err := database.Devices(context.Background(), virtualDevicesAppID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != len(names) {
		t.Fatalf("parallel create stored %d devices, want %d: %#v", len(devices), len(names), devices)
	}
	identities := make(map[any]bool, len(devices))
	for _, device := range devices {
		identity := device.Data["id"]
		if identity == nil || identities[identity] {
			t.Fatalf("parallel create reused identity %#v in %#v", identity, devices)
		}
		identities[identity] = true
	}
}
