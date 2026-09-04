package store

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSceneCRUDPersistsRevisionsAndOwnsItsSnapshots(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := database.Subscribe(4)
	defer cancel()

	input := Scene{
		ID: "movie", Name: "  Filmavond  ",
		States: []SceneState{{
			DeviceID: "  curtains  ", CapabilityID: " windowcoverings_set ",
			Value: map[string]any{"nested": []any{map[string]any{"value": "kept"}}},
		}},
		Active: true, Previous: []SceneState{{DeviceID: "forged", CapabilityID: "onoff", Value: true}},
		CreatedAt: "forged", UpdatedAt: "forged", Revision: 99,
	}
	created, err := database.CreateScene(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "movie" || created.Name != "Filmavond" || created.Revision != 1 ||
		created.CreatedAt == "" || created.UpdatedAt != created.CreatedAt || created.Active || len(created.Previous) != 0 {
		t.Fatalf("created scene metadata = %#v", created)
	}
	if created.States[0].DeviceID != "curtains" || created.States[0].CapabilityID != "windowcoverings_set" {
		t.Fatalf("scene state ids were not normalized: %#v", created.States[0])
	}
	createdEvent := <-events
	if createdEvent.Manager != "scene" || createdEvent.Type != "scene.create" || createdEvent.ID != created.ID {
		t.Fatalf("create event = %#v", createdEvent)
	}
	published, ok := createdEvent.Data.(Scene)
	if !ok {
		t.Fatalf("create event data = %T, want Scene", createdEvent.Data)
	}
	createdDeviceEvent := <-events
	assertSceneDeviceEvent(t, createdDeviceEvent, "device.create", created.ID, false)

	setSceneNestedValue(&input, "input")
	setSceneNestedValue(&created, "created")
	setSceneNestedValue(&published, "published")
	assertStoredSceneNestedValue(t, database, created.ID, "kept")

	before, err := database.Scene(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	previousCreatedAt := before.CreatedAt
	before.Name = "Film"
	before.CreatedAt, before.UpdatedAt = "forged", "forged"
	before.Active = true
	before.Previous = []SceneState{{DeviceID: "forged", CapabilityID: "onoff", Value: true}}
	before.States = append(before.States, SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3})
	updated, err := database.UpdateScene(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Film" || updated.CreatedAt != previousCreatedAt || updated.UpdatedAt == "forged" || updated.Revision != 2 ||
		updated.Active || len(updated.Previous) != 0 {
		t.Fatalf("updated scene metadata = %#v", updated)
	}
	updatedEvent := <-events
	if updatedEvent.Manager != "scene" || updatedEvent.Type != "scene.update" || updatedEvent.ID != updated.ID {
		t.Fatalf("update event = %#v", updatedEvent)
	}
	assertSceneDeviceEvent(t, <-events, "device.update", updated.ID, false)

	stale := before
	stale.Name = "Stale"
	if _, err := database.UpdateScene(ctx, stale); !errors.Is(err, ErrSceneChanged) {
		t.Fatalf("stale update error = %v, want ErrSceneChanged", err)
	}
	stored, err := database.Scene(ctx, updated.ID)
	if err != nil || stored.Name != "Film" || stored.Revision != updated.Revision {
		t.Fatalf("stale update changed scene: %#v err=%v", stored, err)
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Scene(ctx, updated.ID)
	if err != nil || !reflect.DeepEqual(persisted, stored) {
		t.Fatalf("reopened scene = %#v, want %#v, err=%v", persisted, stored, err)
	}
	persistedDevice, err := reopened.Device(ctx, SceneDeviceID(updated.ID))
	if err != nil || persistedDevice.Name != updated.Name || persistedDevice.State["onoff"] != false {
		t.Fatalf("reopened scene device = %#v, err=%v", persistedDevice, err)
	}
	listed, err := reopened.Scenes(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != updated.ID {
		t.Fatalf("listed scenes = %#v err=%v", listed, err)
	}

	deleteEvents, stopDeleteEvents := reopened.Subscribe(2)
	defer stopDeleteEvents()
	if err := reopened.DeleteScene(ctx, updated.ID); err != nil {
		t.Fatal(err)
	}
	deletedEvent := <-deleteEvents
	if deletedEvent.Manager != "scene" || deletedEvent.Type != "scene.delete" || deletedEvent.ID != updated.ID {
		t.Fatalf("delete event = %#v", deletedEvent)
	}
	assertSceneDeviceEvent(t, <-deleteEvents, "device.delete", updated.ID, false)
	if _, err := reopened.Scene(ctx, updated.ID); err == nil {
		t.Fatal("deleted scene is still readable")
	}
	if _, err := reopened.Device(ctx, SceneDeviceID(updated.ID)); err == nil {
		t.Fatal("deleted scene device is still readable")
	}
}

func TestSceneStructuralValidation(t *testing.T) {
	ctx := context.Background()
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	valid := func() Scene {
		return Scene{Name: "Avond", States: []SceneState{{DeviceID: "lamp", CapabilityID: "onoff", Value: true}}}
	}
	tooMany := make([]SceneState, maxSceneStates+1)
	for index := range tooMany {
		tooMany[index] = SceneState{DeviceID: "lamp", CapabilityID: "capability-" + strings.Repeat("x", index%3) + string(rune(index+1)), Value: true}
	}
	tests := []struct {
		name  string
		scene Scene
	}{
		{"empty name", Scene{Name: " ", States: valid().States}},
		{"long name", Scene{Name: strings.Repeat("x", maxSceneNameLength+1), States: valid().States}},
		{"no states", Scene{Name: "Empty"}},
		{"too many states", Scene{Name: "Huge", States: tooMany}},
		{"missing device", Scene{Name: "Bad", States: []SceneState{{CapabilityID: "onoff", Value: true}}}},
		{"missing capability", Scene{Name: "Bad", States: []SceneState{{DeviceID: "lamp", Value: true}}}},
		{"duplicate state", Scene{Name: "Bad", States: []SceneState{
			{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
			{DeviceID: " lamp ", CapabilityID: " onoff ", Value: false},
		}}},
		{"non-JSON value", Scene{Name: "Bad", States: []SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: math.NaN()}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := database.CreateScene(ctx, test.scene); err == nil {
				t.Fatal("invalid scene was accepted")
			}
		})
	}
	if scenes, err := database.Scenes(ctx); err != nil || len(scenes) != 0 {
		t.Fatalf("invalid creates changed scenes: %#v err=%v", scenes, err)
	}

	first := valid()
	first.ID = "fixed"
	if _, err := database.CreateScene(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateScene(ctx, first); err == nil {
		t.Fatal("duplicate scene id was accepted")
	}
	invalidPrevious := []SceneState{
		{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		{DeviceID: " lamp ", CapabilityID: " onoff ", Value: false},
	}
	if _, started, err := database.BeginScene(ctx, first.ID, invalidPrevious); err == nil || started {
		t.Fatalf("invalid BeginScene = started %t, err=%v", started, err)
	}
	if _, err := database.SetScenePrevious(ctx, first.ID, []SceneState{{
		DeviceID: "lamp", CapabilityID: "dim", Value: math.NaN(),
	}}); err == nil {
		t.Fatal("SetScenePrevious accepted a non-JSON restore value")
	}
	if stored := databaseScene(t, database, first.ID); stored.Active || len(stored.Previous) != 0 {
		t.Fatalf("invalid runtime states changed scene: %#v", stored)
	}
}

func TestSceneDeepClonesTypedJSONContainers(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	value := map[string][]int{"levels": {10, 30}}
	created, err := database.CreateScene(context.Background(), Scene{
		Name:   "Film",
		States: []SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: value}},
	})
	if err != nil {
		t.Fatal(err)
	}
	value["levels"][0] = 99
	created.States[0].Value.(map[string][]int)["levels"][1] = 99

	stored, err := database.Scene(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.States[0].Value.(map[string][]int)["levels"]; !reflect.DeepEqual(got, []int{10, 30}) {
		t.Fatalf("stored typed scene value = %#v, want [10 30]", got)
	}
}

func TestSceneDeviceMirrorsDefinitionAndKeepsItsPlacement(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	created, err := database.CreateScene(ctx, Scene{
		ID: "movie", Name: "Film", States: []SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deviceID := SceneDeviceID(created.ID)
	if sceneID, ok := SceneIDFromDeviceID(deviceID); !ok || sceneID != created.ID {
		t.Fatalf("SceneIDFromDeviceID(%q) = %q, %t", deviceID, sceneID, ok)
	}
	device, err := database.Device(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if device.AppID != NativeSceneAppID || device.DriverID != "scene" || device.Class != "scene" ||
		device.Name != created.Name || !device.Available || !reflect.DeepEqual(device.Capabilities, []string{"onoff"}) ||
		device.State["onoff"] != false || device.Data["sceneId"] != created.ID {
		t.Fatalf("scene device = %#v", device)
	}
	devices, err := database.Devices(ctx, NativeSceneAppID)
	if err != nil || len(devices) != 1 || devices[0].ID != deviceID {
		t.Fatalf("native scene devices = %#v, err=%v", devices, err)
	}

	group, err := database.CreateDeviceGroup(ctx, DeviceGroup{Name: "Woonkamer"})
	if err != nil {
		t.Fatal(err)
	}
	placed, err := database.SetDeviceGroup(ctx, deviceID, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	placed.Settings = map[string]any{"iconOverride": "cinema"}
	placed.Store = map[string]any{"note": "niet verbergen"}
	if err := database.UpdateDevice(ctx, placed); err != nil {
		t.Fatal(err)
	}
	created.Name = "Bioscoop"
	updated, err := database.UpdateScene(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	mirrored, err := database.Device(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if mirrored.Name != updated.Name || mirrored.GroupID != placed.GroupID || mirrored.SortOrder != placed.SortOrder ||
		mirrored.Settings["iconOverride"] != "cinema" || mirrored.Store["note"] != "niet verbergen" {
		t.Fatalf("updated scene device lost placement: before=%#v after=%#v", placed, mirrored)
	}
	if err := database.DeleteDevice(ctx, deviceID); err == nil {
		t.Fatal("DeleteDevice orphaned a Scene through its synthetic device")
	}
	if _, err := database.Scene(ctx, created.ID); err != nil {
		t.Fatalf("refused device delete still removed scene: %v", err)
	}
}

func TestSceneRuntimeSessionPersistsAndProtectsItsRestoreSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := database.CreateScene(ctx, Scene{
		ID: "movie", Name: "Film", States: []SceneState{
			{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
			{DeviceID: "curtain", CapabilityID: "position", Value: 0.0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := database.Subscribe(8)
	defer cancel()
	previous := []SceneState{
		{DeviceID: "lamp", CapabilityID: "dim", Value: map[string]any{"nested": []any{map[string]any{"value": "kept"}}}},
		{DeviceID: "curtain", CapabilityID: "position", Value: 0.8},
	}
	begun, started, err := database.BeginScene(ctx, created.ID, previous)
	if err != nil || !started {
		t.Fatalf("BeginScene = started %t, err=%v", started, err)
	}
	if !begun.Active || begun.Revision != created.Revision || begun.UpdatedAt != created.UpdatedAt || !reflect.DeepEqual(begun.Previous, previous) {
		t.Fatalf("begun scene = %#v", begun)
	}
	stateEvent := <-events
	if stateEvent.Manager != "scene" || stateEvent.Type != "scene.state" || stateEvent.ID != created.ID {
		t.Fatalf("begin scene event = %#v", stateEvent)
	}
	deviceEvent := <-events
	assertSceneDeviceEvent(t, deviceEvent, "device.update", created.ID, true)
	published := stateEvent.Data.(Scene)
	setSceneStateNestedValue(previous, "input")
	setSceneStateNestedValue(begun.Previous, "returned")
	setSceneStateNestedValue(published.Previous, "event")
	deviceEvent.Data.(Device).State["onoff"] = false
	assertStoredPreviousNestedValue(t, database, created.ID, "kept")
	assertSceneDeviceActive(t, database, created.ID, true)

	// Once active, even a caller with a now-stale/invalid candidate snapshot may
	// only observe the first persisted one; it cannot replace or invalidate it.
	repeated, started, err := database.BeginScene(ctx, created.ID, []SceneState{{DeviceID: "other", Value: math.NaN()}})
	if err != nil || started || !reflect.DeepEqual(repeated.Previous, databaseScene(t, database, created.ID).Previous) {
		t.Fatalf("repeated BeginScene = %#v, started=%t, err=%v", repeated, started, err)
	}
	assertNoSceneEvent(t, events)
	if _, err := database.UpdateScene(ctx, begun); !errors.Is(err, ErrSceneActive) {
		t.Fatalf("active UpdateScene error = %v, want ErrSceneActive", err)
	}
	if err := database.DeleteScene(ctx, created.ID); !errors.Is(err, ErrSceneActive) {
		t.Fatalf("active DeleteScene error = %v, want ErrSceneActive", err)
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored := databaseScene(t, reopened, created.ID)
	if !restored.Active || len(restored.Previous) != 2 {
		t.Fatalf("reopened active scene = %#v", restored)
	}
	assertSceneDeviceActive(t, reopened, created.ID, true)

	restoreEvents, stopRestoreEvents := reopened.Subscribe(4)
	defer stopRestoreEvents()
	remaining := cloneSceneStates(restored.Previous[:1])
	checkpoint, err := reopened.SetScenePrevious(ctx, created.ID, remaining)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Active || len(checkpoint.Previous) != 1 || checkpoint.Revision != created.Revision || checkpoint.UpdatedAt != created.UpdatedAt {
		t.Fatalf("checkpointed scene = %#v", checkpoint)
	}
	if event := <-restoreEvents; event.Manager != "scene" || event.Type != "scene.state" {
		t.Fatalf("checkpoint scene event = %#v", event)
	}
	assertSceneDeviceEvent(t, <-restoreEvents, "device.update", created.ID, true)
	setSceneStateNestedValue(remaining, "remaining")
	setSceneStateNestedValue(checkpoint.Previous, "checkpoint")
	assertStoredPreviousNestedValue(t, reopened, created.ID, "kept")

	stopped, err := reopened.SetScenePrevious(ctx, created.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Active || len(stopped.Previous) != 0 || stopped.Revision != created.Revision {
		t.Fatalf("stopped scene = %#v", stopped)
	}
	if event := <-restoreEvents; event.Manager != "scene" || event.Type != "scene.state" {
		t.Fatalf("stop scene event = %#v", event)
	}
	assertSceneDeviceEvent(t, <-restoreEvents, "device.update", created.ID, false)
	assertSceneDeviceActive(t, reopened, created.ID, false)
}

func TestBeginSceneIsAtomicAcrossConcurrentCallers(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	created, err := database.CreateScene(ctx, Scene{
		Name: "Film", States: []SceneState{{DeviceID: "lamp", CapabilityID: "onoff", Value: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := database.Subscribe(4)
	defer cancel()
	const callers = 32
	start := make(chan struct{})
	results := make(chan bool, callers)
	errorsSeen := make(chan error, callers)
	var workers sync.WaitGroup
	for index := 0; index < callers; index++ {
		workers.Add(1)
		go func(value int) {
			defer workers.Done()
			<-start
			_, started, err := database.BeginScene(ctx, created.ID, []SceneState{{
				DeviceID: "lamp", CapabilityID: "onoff", Value: value%2 == 0,
			}})
			results <- started
			errorsSeen <- err
		}(index)
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	startedCount := 0
	for started := range results {
		if started {
			startedCount++
		}
	}
	if startedCount != 1 {
		t.Fatalf("BeginScene started %d times, want exactly one", startedCount)
	}
	if event := <-events; event.Manager != "scene" || event.Type != "scene.state" {
		t.Fatalf("concurrent begin scene event = %#v", event)
	}
	assertSceneDeviceEvent(t, <-events, "device.update", created.ID, true)
	assertNoSceneEvent(t, events)
}

func TestDeleteSceneAndFlowWritesShareReferentialIntegrity(t *testing.T) {
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	newReferenceFlow := func(sceneID string) Flow {
		return Flow{Name: "Scene gebruiken", Nodes: []FlowNode{{ID: "set", Step: FlowStep{
			AppID: "stulp", CardID: "set_device_capability", CardType: "action",
			Args: map[string]any{
				"device": map[string]any{"$device": SceneDeviceID(sceneID)}, "capability": "onoff", "value": true,
			},
		}}}}
	}
	createScene := func() Scene {
		definition, err := database.CreateScene(ctx, Scene{
			Name: "Film", States: []SceneState{{DeviceID: "lamp", CapabilityID: "onoff", Value: false}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return definition
	}

	definition := createScene()
	flow, err := database.CreateFlow(ctx, newReferenceFlow(definition.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteScene(ctx, definition.ID); !errors.Is(err, ErrSceneInUse) {
		t.Fatalf("DeleteScene error = %v, want ErrSceneInUse", err)
	}
	if _, err := database.Scene(ctx, definition.ID); err != nil {
		t.Fatalf("in-use Scene was removed: %v", err)
	}
	if err := database.DeleteFlow(ctx, flow.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteScene(ctx, definition.ID); err != nil {
		t.Fatal(err)
	}

	// Exercise both possible lock orders. If the Flow write wins, delete sees
	// it; if delete wins, the waiting Flow write sees the Scene tombstone.
	for iteration := 0; iteration < 64; iteration++ {
		definition = createScene()
		start := make(chan struct{})
		var createdFlow Flow
		var flowErr, deleteErr error
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			createdFlow, flowErr = database.CreateFlow(ctx, newReferenceFlow(definition.ID))
		}()
		go func() {
			defer workers.Done()
			<-start
			deleteErr = database.DeleteScene(ctx, definition.ID)
		}()
		close(start)
		workers.Wait()

		if flowErr == nil && deleteErr == nil {
			t.Fatal("concurrent Flow create and Scene delete both succeeded")
		}
		if flowErr != nil && deleteErr != nil {
			t.Fatalf("concurrent Flow create and Scene delete both failed: flow=%v delete=%v", flowErr, deleteErr)
		}
		if flowErr == nil {
			if !errors.Is(deleteErr, ErrSceneInUse) {
				t.Fatalf("delete lost race with Flow but returned %v", deleteErr)
			}
			if err := database.DeleteFlow(ctx, createdFlow.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.DeleteScene(ctx, definition.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if deleteErr != nil {
			t.Fatal(deleteErr)
		}
	}
}

func TestVersionTwoSceneWithoutDeviceIsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stulp.json")
	raw := `{"version":2,"apps":[],"devices":[],"deviceGroups":[],"scenes":[{"id":"movie","name":"Film","states":[{"deviceId":"lamp","capabilityId":"onoff","value":false}],"active":true,"previous":[{"deviceId":"lamp","capabilityId":"onoff","value":true}],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","revision":1}],"flows":[]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	device, err := database.Device(context.Background(), SceneDeviceID("movie"))
	if err != nil {
		t.Fatal(err)
	}
	if device.AppID != NativeSceneAppID || device.Data["sceneId"] != "movie" || device.State["onoff"] != true {
		t.Fatalf("migrated scene device = %#v", device)
	}
}

func TestSnapshotRestoreReinstatesActiveSceneAndVirtualState(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created, err := database.CreateScene(ctx, Scene{
		ID: "movie", Name: "Film", States: []SceneState{{DeviceID: "lamp", CapabilityID: "onoff", Value: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, started, err := database.BeginScene(ctx, created.ID, []SceneState{{
		DeviceID: "lamp", CapabilityID: "onoff", Value: true,
	}})
	if err != nil || !started {
		t.Fatalf("BeginScene = %#v, started=%t, err=%v", active, started, err)
	}
	data, err := database.SnapshotBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ParseSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetScenePrevious(ctx, created.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteScene(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RestoreSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	restored := databaseScene(t, database, created.ID)
	if !reflect.DeepEqual(restored, active) {
		t.Fatalf("restored Scene = %#v, want %#v", restored, active)
	}
	device, err := database.Device(ctx, SceneDeviceID(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if device.AppID != NativeSceneAppID || device.Data["sceneId"] != created.ID || device.State["onoff"] != true {
		t.Fatalf("restored Scene device = %#v", device)
	}
}

func TestSceneMutationsAreCopyOnWrite(t *testing.T) {
	backend := &flowFailingFiles{documents: make(map[string][]byte)}
	previousFiles := files
	files = backend
	t.Cleanup(func() { files = previousFiles })

	database, err := Open("/virtual/scene-transaction-test.json")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	events, cancel := database.Subscribe(4)
	defer cancel()
	failure := errors.New("injected scene persistence failure")

	backend.writeErr = failure
	definition := Scene{ID: "movie", Name: "Film", States: []SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3}}}
	if _, err := database.CreateScene(ctx, definition); !errors.Is(err, failure) {
		t.Fatalf("CreateScene error = %v, want persistence failure", err)
	}
	if scenes, err := database.Scenes(ctx); err != nil || len(scenes) != 0 {
		t.Fatalf("failed create leaked scenes: %#v err=%v", scenes, err)
	}
	if _, err := database.Device(ctx, SceneDeviceID(definition.ID)); err == nil {
		t.Fatal("failed create leaked a synthetic scene device")
	}
	assertNoSceneEvent(t, events)

	backend.writeErr = nil
	created, err := database.CreateScene(ctx, definition)
	if err != nil {
		t.Fatal(err)
	}
	<-events
	<-events
	persistedBefore := append([]byte(nil), backend.documents[database.Path()]...)
	backend.writeErr = failure
	edit := cloneScene(created)
	edit.Name = "Changed"
	if _, err := database.UpdateScene(ctx, edit); !errors.Is(err, failure) {
		t.Fatalf("UpdateScene error = %v, want persistence failure", err)
	}
	assertRejectedSceneMutation(t, database, events, created, persistedBefore, backend)
	if err := database.DeleteScene(ctx, created.ID); !errors.Is(err, failure) {
		t.Fatalf("DeleteScene error = %v, want persistence failure", err)
	}
	assertRejectedSceneMutation(t, database, events, created, persistedBefore, backend)
	previous := []SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: 0.8}}
	if _, started, err := database.BeginScene(ctx, created.ID, previous); !errors.Is(err, failure) || started {
		t.Fatalf("BeginScene = started %t, error %v; want persistence failure", started, err)
	}
	assertRejectedSceneMutation(t, database, events, created, persistedBefore, backend)

	backend.writeErr = nil
	active, started, err := database.BeginScene(ctx, created.ID, previous)
	if err != nil || !started {
		t.Fatalf("successful BeginScene = started %t, err=%v", started, err)
	}
	<-events
	<-events
	activeDocument := append([]byte(nil), backend.documents[database.Path()]...)
	backend.writeErr = failure
	if _, err := database.SetScenePrevious(ctx, created.ID, nil); !errors.Is(err, failure) {
		t.Fatalf("SetScenePrevious error = %v, want persistence failure", err)
	}
	assertRejectedSceneMutation(t, database, events, active, activeDocument, backend)
}

func TestVersionOneDocumentOpensWithEmptyScenes(t *testing.T) {
	loaded, err := decodeDocument("v1.json", []byte(`{"version":1,"apps":[],"devices":[],"deviceGroups":[],"flows":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != documentVersion || len(loaded.Scenes) != 0 {
		t.Fatalf("migrated document = version %d scenes %#v", loaded.Version, loaded.Scenes)
	}
}

func setSceneNestedValue(scene *Scene, value string) {
	nested := scene.States[0].Value.(map[string]any)["nested"].([]any)[0].(map[string]any)
	nested["value"] = value
}

func setSceneStateNestedValue(states []SceneState, value string) {
	nested := states[0].Value.(map[string]any)["nested"].([]any)[0].(map[string]any)
	nested["value"] = value
}

func databaseScene(t *testing.T, database *Store, id string) Scene {
	t.Helper()
	definition, err := database.Scene(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func assertStoredPreviousNestedValue(t *testing.T, database *Store, id, want string) {
	t.Helper()
	stored := databaseScene(t, database, id)
	nested := stored.Previous[0].Value.(map[string]any)["nested"].([]any)[0].(map[string]any)
	if nested["value"] != want {
		t.Fatalf("stored previous nested scene value = %v, want %s", nested["value"], want)
	}
}

func assertSceneDeviceActive(t *testing.T, database *Store, sceneID string, active bool) {
	t.Helper()
	device, err := database.Device(context.Background(), SceneDeviceID(sceneID))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := device.State["onoff"].(bool); !ok || got != active {
		t.Fatalf("scene device onoff = %#v, want %t", device.State["onoff"], active)
	}
}

func assertSceneDeviceEvent(t *testing.T, event Event, eventType, sceneID string, active bool) {
	t.Helper()
	deviceID := SceneDeviceID(sceneID)
	if event.Manager != "devices" || event.Type != eventType || event.ID != deviceID {
		t.Fatalf("scene device event = %#v", event)
	}
	if eventType == "device.delete" {
		return
	}
	device, ok := event.Data.(Device)
	if !ok || device.ID != deviceID || device.AppID != NativeSceneAppID || device.Data["sceneId"] != sceneID ||
		device.State["onoff"] != active {
		t.Fatalf("scene device event data = %#v", event.Data)
	}
}

func assertStoredSceneNestedValue(t *testing.T, database *Store, id, want string) {
	t.Helper()
	stored, err := database.Scene(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	nested := stored.States[0].Value.(map[string]any)["nested"].([]any)[0].(map[string]any)
	if nested["value"] != want {
		t.Fatalf("stored nested scene value = %v, want %s", nested["value"], want)
	}
}

func assertNoSceneEvent(t *testing.T, events <-chan Event) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("failed scene mutation published event %#v", event)
	default:
	}
}

func assertRejectedSceneMutation(t *testing.T, database *Store, events <-chan Event, want Scene, document []byte, backend *flowFailingFiles) {
	t.Helper()
	got, err := database.Scene(context.Background(), want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("failed mutation changed scene: got %#v want %#v err=%v", got, want, err)
	}
	if !bytes.Equal(backend.documents[database.Path()], document) {
		t.Fatal("failed scene mutation changed persisted document")
	}
	device, err := database.Device(context.Background(), SceneDeviceID(want.ID))
	if err != nil || device.Name != want.Name || device.State["onoff"] != want.Active {
		t.Fatalf("failed mutation changed scene device: got %#v err=%v", device, err)
	}
	assertNoSceneEvent(t, events)
}

func TestButtonSceneIsMomentaryAndHasNoRestoreSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stulp.json")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	states := []SceneState{{DeviceID: "lamp", CapabilityID: "onoff", Value: false}}
	if _, err := database.CreateScene(ctx, Scene{Name: "Fout", Kind: "toggle", States: states}); err == nil ||
		!strings.Contains(err.Error(), "kind") {
		t.Fatalf("unknown scene kind accepted: %v", err)
	}
	plain, err := database.CreateScene(ctx, Scene{ID: "movie", Name: "Film", States: states})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Kind != SceneKindSwitch || plain.Momentary() || plain.CapabilityID() != "onoff" {
		t.Fatalf("default scene kind = %#v", plain)
	}
	button, err := database.CreateScene(ctx, Scene{ID: "garden-off", Name: "Tuin uit", Kind: " button ", States: states})
	if err != nil {
		t.Fatal(err)
	}
	if button.Kind != SceneKindButton || !button.Momentary() || button.CapabilityID() != "button" {
		t.Fatalf("button scene = %#v", button)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// The kind is what the document says, and the device follows it after a restart.
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	device, err := database.Device(ctx, SceneDeviceID(button.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(device.Capabilities, []string{"button"}) || len(device.State) != 0 {
		t.Fatalf("button scene device = %#v", device)
	}
	if _, _, err := database.BeginScene(ctx, button.ID, states); !errors.Is(err, ErrSceneMomentary) {
		t.Fatalf("BeginScene on a button scene: %v", err)
	}
	if _, err := database.SetScenePrevious(ctx, button.ID, states); !errors.Is(err, ErrSceneMomentary) {
		t.Fatalf("SetScenePrevious with states on a button scene: %v", err)
	}
	if cleared, err := database.SetScenePrevious(ctx, button.ID, nil); err != nil || cleared.Active {
		t.Fatalf("SetScenePrevious(nil) on a button scene = %#v, %v", cleared, err)
	}

	// Changing the kind swaps the device capability along with it.
	stored, err := database.Scene(ctx, button.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Kind = SceneKindSwitch
	if _, err := database.UpdateScene(ctx, stored); err != nil {
		t.Fatal(err)
	}
	device, err = database.Device(ctx, SceneDeviceID(button.ID))
	if err != nil || !reflect.DeepEqual(device.Capabilities, []string{"onoff"}) || device.State["onoff"] != false {
		t.Fatalf("scene device after kind change = %#v, %v", device, err)
	}
}
