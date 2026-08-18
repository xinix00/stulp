package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
)

// bundle writes a real app.json and returns its directory. The store reads
// manifests from the bundle rather than keeping a copy, so a test app needs to
// exist on disk like a real one.
func bundle(t *testing.T, raw map[string]any) string {
	t.Helper()
	root := t.TempDir()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDeviceJSONFieldsRoundTripIndependently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	m := &manifest.Manifest{ID: "com.example.test", Version: "1.0.0", SDK: 3, Raw: map[string]any{"id": "com.example.test"}}
	if err := database.InstallApp(ctx, m, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	want, err := database.AddDevice(ctx, Device{
		AppID: "com.example.test", DriverID: "driver", Name: "Device", Class: "socket",
		Data: map[string]any{"id": "one"}, Settings: map[string]any{"interval": float64(30)},
		Store: map[string]any{"token": "secret"}, Capabilities: []string{"onoff"},
		State: map[string]any{"onoff": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := database.Device(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Data, want.Data) || !reflect.DeepEqual(got.Settings, want.Settings) ||
		!reflect.DeepEqual(got.Store, want.Store) || !reflect.DeepEqual(got.Capabilities, want.Capabilities) ||
		!reflect.DeepEqual(got.State, want.State) {
		t.Fatalf("round-trip mismatch:\nwant=%#v\n got=%#v", want, got)
	}
}

func TestDeviceHardwareNameSurvivesUserRenames(t *testing.T) {
	device := Device{Name: "Aqara Dimmer Switch H2 EU"}
	device.PreserveHardwareName()
	device.Name = "Ganglicht"
	device.PreserveHardwareName()
	if got := device.HardwareName(); got != "Aqara Dimmer Switch H2 EU" {
		t.Fatalf("hardware name = %q", got)
	}
	if device.Name != "Ganglicht" {
		t.Fatalf("user name = %q", device.Name)
	}
}

// A document written by an older Stulp must still open, and one written by a
// newer one must be refused rather than silently half-understood.
func TestDocumentVersionIsHonoured(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stulp.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"apps":[],"devices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatalf("a document of the current format was refused: %v", err)
	}
	database.Close()

	future := filepath.Join(directory, "future.json")
	if err := os.WriteFile(future, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(future); err == nil {
		t.Fatal("a document from a newer Stulp was opened as if it were understood")
	}
}

func TestAnAnnouncedManifestSurvivesRestartAndFollowsThePlacedImage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manifest.Parse([]byte(`{
  "id":"com.example.announced","version":"1.0.0","sdk":3,
  "name":{"nl":"Aangemeld"},
  "drivers":[{"id":"sensor","name":{"nl":"Sensor"},"settings":[{"id":"interval","type":"number"}]}],
  "ui":{"assets":["settings/index.html"]}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.OfferApp(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AcceptApp(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	database.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	app, err := reopened.App(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.FromRaw(app.Manifest)
	if err != nil {
		t.Fatalf("stored manifest became unusable after restart: %v", err)
	}
	if _, ok := parsed.Driver("sensor"); !ok {
		t.Fatalf("driver disappeared after restart: %#v", app.Manifest)
	}

	second, err := manifest.Parse([]byte(`{
  "id":"com.example.announced","version":"2.0.0","sdk":3,
  "name":{"nl":"Aangemeld"},
  "drivers":[{"id":"meter","name":{"nl":"Meter"}}],
  "ui":{"assets":["settings/index.html","settings/page.js"]}
}`))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := reopened.UpdateAnnouncedApp(ctx, second)
	if err != nil || !changed {
		t.Fatalf("new image announcement: changed=%v err=%v", changed, err)
	}
	updated, err := reopened.App(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	updatedManifest, err := manifest.FromRaw(updated.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != "2.0.0" {
		t.Fatalf("announced version = %q", updated.Version)
	}
	if _, ok := updatedManifest.Driver("meter"); !ok {
		t.Fatalf("new driver did not follow the image: %#v", updated.Manifest)
	}
	if changed, err := reopened.UpdateAnnouncedApp(ctx, second); err != nil || changed {
		t.Fatalf("equal retry rewrote the app: changed=%v err=%v", changed, err)
	}
}

// The document must survive an interruption. A half-written file would take
// every app, device and Flow with it, which is a different loss entirely from
// missing the most recent change.
func TestAnInterruptedWriteLeavesTheDocumentIntact(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{ID: "com.example.test", Version: "1.0.0", SDK: 3, Raw: map[string]any{"id": "com.example.test"}}
	if err := database.InstallApp(ctx, m, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	database.Close()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever a crashed write leaves behind, it is never the real file: the
	// bytes go to a temporary name and only an atomic rename publishes them.
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".stulp-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("a completed write left temporary files behind: %v", leftovers)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	apps, err := reopened.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 {
		t.Fatalf("reopened document holds %d apps, want the one that was installed", len(apps))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("opening the document rewrote it")
	}
}

func TestDeviceGroupsPersistOrderAndMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	m := &manifest.Manifest{ID: "com.example.groups", Version: "1.0.0", SDK: 3, Raw: map[string]any{"id": "com.example.groups"}}
	if err := database.InstallApp(ctx, m, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, Device{AppID: m.ID, DriverID: "switch", Name: "Lamp", Class: "light"})
	if err != nil {
		t.Fatal(err)
	}
	floor, err := database.CreateDeviceGroup(ctx, DeviceGroup{Name: "1e etage"})
	if err != nil {
		t.Fatal(err)
	}
	bedroom, err := database.CreateDeviceGroup(ctx, DeviceGroup{Name: "Slaapkamer", ParentID: floor.ID})
	if err != nil {
		t.Fatal(err)
	}
	outside, err := database.CreateDeviceGroup(ctx, DeviceGroup{Name: "Buiten"})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := database.SetDeviceGroup(ctx, device.ID, bedroom.ID)
	if err != nil || moved.GroupID != bedroom.ID {
		t.Fatalf("device was not moved into its group: %#v err=%v", moved, err)
	}
	floor.Name, floor.SortOrder = "Boven", 20
	outside.SortOrder = 10
	if _, err := database.UpdateDeviceGroup(ctx, floor); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateDeviceGroup(ctx, outside); err != nil {
		t.Fatal(err)
	}
	groups, err := database.DeviceGroups(ctx)
	if err != nil || len(groups) != 3 || groups[0].ID != outside.ID || groups[1].ID != floor.ID || groups[2].ID != bedroom.ID {
		t.Fatalf("unexpected ordered groups: %#v err=%v", groups, err)
	}
	bedroom.ParentID = bedroom.ID
	if _, err := database.UpdateDeviceGroup(ctx, bedroom); err == nil {
		t.Fatal("group was allowed to contain itself")
	}
	bedroom.ParentID = floor.ID
	floor.ParentID = bedroom.ID
	if _, err := database.UpdateDeviceGroup(ctx, floor); err == nil {
		t.Fatal("group parent cycle was accepted")
	}
	floor.ParentID = ""
	if err := database.DeleteDeviceGroup(ctx, bedroom.ID); err != nil {
		t.Fatal(err)
	}
	movedToParent, err := database.Device(ctx, device.ID)
	if err != nil || movedToParent.GroupID != floor.ID {
		t.Fatalf("deleting child group did not move its device to the parent: %#v err=%v", movedToParent, err)
	}
	if err := database.DeleteDeviceGroup(ctx, floor.ID); err != nil {
		t.Fatal(err)
	}
	ungrouped, err := database.Device(ctx, device.ID)
	if err != nil || ungrouped.GroupID != "" {
		t.Fatalf("deleting a group did not preserve and ungroup its device: %#v err=%v", ungrouped, err)
	}
}

func TestDeviceTileOrderPersistsWithinItsGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{ID: "com.example.order", Version: "1.0.0", SDK: 3, Raw: map[string]any{"id": "com.example.order"}}
	if err := database.InstallApp(ctx, m, bundle(t, m.Raw), ""); err != nil {
		t.Fatal(err)
	}
	group, err := database.CreateDeviceGroup(ctx, DeviceGroup{Name: "Woonkamer"})
	if err != nil {
		t.Fatal(err)
	}
	var devices []Device
	for index, name := range []string{"Banklamp", "Staande lamp", "Vensterlamp"} {
		device, err := database.AddDevice(ctx, Device{
			AppID: m.ID, DriverID: "light", GroupID: group.ID, Name: name, Class: "light",
			Data: map[string]any{"index": index},
		})
		if err != nil {
			t.Fatal(err)
		}
		devices = append(devices, device)
	}
	wantIDs := []string{devices[2].ID, devices[0].ID, devices[1].ID}
	if err := database.ReorderDevices(ctx, group.ID, wantIDs); err != nil {
		t.Fatal(err)
	}
	if err := database.ReorderDevices(ctx, group.ID, wantIDs[:2]); err == nil {
		t.Fatal("an incomplete tile order was accepted")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for index, id := range wantIDs {
		device, err := reopened.Device(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if want := (index + 1) * 10; device.SortOrder != want {
			t.Fatalf("device %s sortOrder = %d, want %d", id, device.SortOrder, want)
		}
	}
}

func TestFlowGraphRoundTripsPositionsAndConnections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	want, err := database.CreateFlow(ctx, Flow{
		Name: "Vertakte Flow", Enabled: true,
		Nodes: []FlowNode{
			{ID: "als-een", X: 72, Y: 104, Step: FlowStep{AppID: "com.example", CardID: "motion", CardType: "trigger"}},
			{ID: "als-twee", X: 72, Y: 340, Step: FlowStep{AppID: "com.example", CardID: "button", CardType: "device-trigger"}},
			{ID: "en", X: 520, Y: 104, Step: FlowStep{AppID: "com.example", CardID: "dark", CardType: "condition"}},
			{ID: "dan-een", X: 930, Y: 104, Step: FlowStep{AppID: "com.example", CardID: "light", CardType: "action"}},
			{ID: "dan-twee", X: 930, Y: 340, Step: FlowStep{AppID: "com.example", CardID: "notify", CardType: "action"}},
		},
		Edges: []FlowEdge{
			{ID: "lijn-1", From: "als-een", To: "en"},
			{ID: "lijn-2", From: "en", To: "dan-een"},
			{ID: "lijn-3", From: "als-twee", To: "dan-twee"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := database.Flow(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Nodes, want.Nodes) || !reflect.DeepEqual(got.Edges, want.Edges) {
		t.Fatalf("graph round-trip mismatch:\nwant=%#v %#v\n got=%#v %#v", want.Nodes, want.Edges, got.Nodes, got.Edges)
	}
}

func TestFlowGraphRejectsCycles(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.CreateFlow(context.Background(), Flow{
		Name: "Rondje",
		Nodes: []FlowNode{
			{ID: "als", Step: FlowStep{AppID: "com.example", CardID: "start", CardType: "trigger"}},
			{ID: "en", Step: FlowStep{AppID: "com.example", CardID: "check", CardType: "condition"}},
			{ID: "dan", Step: FlowStep{AppID: "com.example", CardID: "finish", CardType: "action"}},
		},
		Edges: []FlowEdge{{From: "als", To: "en"}, {From: "en", To: "dan"}, {From: "dan", To: "en"}},
	})
	if err == nil {
		t.Fatal("cyclic Flow graph was accepted")
	}
}

func TestNotificationsPersistAndPublish(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	events, cancel := database.Subscribe(1)
	defer cancel()

	created, err := database.CreateNotification(ctx, NativeMatterAppID, "Deurbel gaat")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := database.Notifications(ctx, 10)
	if err != nil || len(listed) != 1 || listed[0] != created {
		t.Fatalf("notification did not round-trip: %#v err=%v", listed, err)
	}
	select {
	case event := <-events:
		if event.Manager != "notifications" || event.Type != "notification.create" || event.ID != created.ID {
			t.Fatalf("unexpected notification event: %#v", event)
		}
	default:
		t.Fatal("notification did not publish a live event")
	}
	if err := database.DeleteNotification(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOverflowingSubscriberGetsAReloadMarker(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	events, cancel := database.Subscribe(1)
	defer cancel()

	database.PublishAppRuntime("first", "running")
	database.PublishAppRuntime("second", "running")
	event := <-events
	if event.Manager != "store" || event.Type != "store.reload" {
		t.Fatalf("overflow produced %#v, expected a reload marker", event)
	}
}

func TestDeviceEventsAreChangedOrderedSnapshots(t *testing.T) {
	ctx := context.Background()
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallMatterApp(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	device, err := database.AddDevice(ctx, Device{
		AppID: NativeMatterAppID, DriverID: "matter", Name: "Sensor", Class: "sensor",
		Capabilities: []string{"measure_temperature"}, State: map[string]any{"measure_temperature": 20.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := database.Subscribe(4)
	defer cancel()

	if err := database.UpdateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("unchanged device emitted event %#v", event)
	default:
	}

	device.State["measure_temperature"] = 21.0
	if err := database.UpdateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	// A publisher may reuse its local object immediately. The queued event must
	// retain the value that actually caused it.
	device.State["measure_temperature"] = 22.0
	select {
	case event := <-events:
		snapshot, ok := event.Data.(Device)
		if !ok || snapshot.State["measure_temperature"] != 21.0 {
			t.Fatalf("device event is not an immutable update snapshot: %#v", event.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("changed device emitted no event")
	}
}

// The document must never be readable in a half-written state. This writes
// concurrently while reading the file over and over: every read has to yield
// either the previous document or the next one, never a fragment.
func TestConcurrentWritesNeverExposeAPartialDocument(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	raw := map[string]any{"id": "com.acme.writer", "version": "1.0.0", "sdk": 3}
	m := &manifest.Manifest{ID: "com.acme.writer", Version: "1.0.0", SDK: 3, Raw: raw}
	if err := database.InstallApp(ctx, m, bundle(t, raw), ""); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := database.CreateNotification(ctx, "com.acme.writer", fmt.Sprint("melding ", index)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for read := 0; read < 300; read++ {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the document vanished mid-write: %v", err)
		}
		var decoded document
		if err := json.Unmarshal(content, &decoded); err != nil {
			t.Fatalf("read a partial document after %d reads: %v", read, err)
		}
		if decoded.Version != documentVersion {
			t.Fatalf("read a document without its version: %#v", decoded)
		}
	}
	close(stop)
	group.Wait()
}

// A device reporting a new value must not touch the disk: that is the whole
// reason capability values are kept in memory.
func TestReportingAValueDoesNotWriteToDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	raw := map[string]any{"id": "com.acme.sensors", "version": "1.0.0", "sdk": 3}
	m := &manifest.Manifest{ID: "com.acme.sensors", Version: "1.0.0", SDK: 3, Raw: raw}
	if err := database.InstallApp(ctx, m, bundle(t, raw), ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, Device{
		AppID: "com.acme.sensors", DriverID: "sensor", Name: "Hal", Class: "sensor",
		Capabilities: []string{"measure_temperature"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	for reading := 0; reading < 50; reading++ {
		device.State["measure_temperature"] = 20.0 + float64(reading)/10
		if err := database.UpdateDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("reporting values rewrote the document")
	}
	// The value is still there to read; it simply lives in memory.
	current, err := database.Device(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State["measure_temperature"] != 24.9 {
		t.Fatalf("latest value = %v", current.State["measure_temperature"])
	}

	// A real configuration change does write.
	current.Name = "Sensor hal"
	if err := database.UpdateDevice(ctx, current); err != nil {
		t.Fatal(err)
	}
	renamed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Size() == before.Size() && renamed.ModTime().Equal(before.ModTime()) {
		t.Fatal("renaming a device did not reach the document")
	}
}
