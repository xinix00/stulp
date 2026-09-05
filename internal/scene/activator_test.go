package scene

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/store"
)

const sceneTestAppID = "com.example.scenes"

// cancelOnSecondCheckContext deterministically models cancellation in the
// narrow window after Set's initial context check and lock acquisition. The
// second Err check closes Done before returning context.Canceled, preserving
// the context.Context contract while making that race reproducible.
type cancelOnSecondCheckContext struct {
	context.Context
	done   chan struct{}
	checks atomic.Int32
	once   sync.Once
}

func newCancelOnSecondCheckContext() *cancelOnSecondCheckContext {
	return &cancelOnSecondCheckContext{Context: context.Background(), done: make(chan struct{})}
}

func (ctx *cancelOnSecondCheckContext) Done() <-chan struct{} { return ctx.done }

func (ctx *cancelOnSecondCheckContext) Err() error {
	if ctx.checks.Add(1) == 1 {
		return nil
	}
	ctx.once.Do(func() { close(ctx.done) })
	return context.Canceled
}

func sceneStore(t *testing.T, path string) *store.Store {
	t.Helper()
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	app := &manifest.Manifest{
		ID: sceneTestAppID, Version: "1.0.0", SDK: 3,
		Raw: map[string]any{"id": sceneTestAppID, "version": "1.0.0", "sdk": float64(3)},
	}
	if err := database.InstallApp(context.Background(), app, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	return database
}

func addSceneDevice(t *testing.T, database *store.Store, id string, capabilities []string, state map[string]any) store.Device {
	t.Helper()
	device, err := database.AddDevice(context.Background(), store.Device{
		ID: id, AppID: sceneTestAppID, DriverID: "test", Name: id, Class: "other",
		Data: map[string]any{"id": id}, Capabilities: capabilities, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return device
}

func createScene(t *testing.T, database *store.Store, states ...store.SceneState) store.Scene {
	t.Helper()
	definition, err := database.CreateScene(context.Background(), store.Scene{Name: "Film kijken", States: states})
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func storedScene(t *testing.T, database *store.Store, id string) store.Scene {
	t.Helper()
	definition, err := database.Scene(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

// reportState plays the integration: the device says what it did after a
// command. Without it a restore has nothing to put back, because every value
// still reads as it did before the scene.
func reportState(t *testing.T, database *store.Store, deviceID string, values map[string]any) {
	t.Helper()
	device, err := database.Device(context.Background(), deviceID)
	if err != nil {
		t.Fatal(err)
	}
	for capability, value := range values {
		device.State[capability] = value
	}
	if err := database.UpdateDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
}

func TestSetOnSnapshotsCurrentStateAndActivatesDesiredState(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "curtains", []string{"windowcoverings_state"}, map[string]any{"windowcoverings_state": "up"})
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "curtains", CapabilityID: "windowcoverings_state", Value: "down"},
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	type invocation struct {
		device, capability string
		value              any
	}
	var mu sync.Mutex
	var invoked []invocation
	activator := New(database, PerCapability(func(_ context.Context, device, capability string, value any, options map[string]any) error {
		if options == nil || len(options) != 0 {
			return fmt.Errorf("options = %#v, want an empty object", options)
		}
		mu.Lock()
		invoked = append(invoked, invocation{device: device, capability: capability, value: value})
		mu.Unlock()
		return nil
	}))

	result, err := activator.Activate(context.Background(), definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.RequestedOn || !result.Active || result.SceneID != definition.ID ||
		result.SceneName != definition.Name || result.Attempted != 3 || result.Succeeded != 3 || result.Failed != 0 {
		t.Fatalf("activation result = %#v", result)
	}
	wantStates := []StateResult{
		{DeviceID: "curtains", CapabilityID: "windowcoverings_state", Value: "down", Success: true},
		{DeviceID: "lamp", CapabilityID: "onoff", Value: true, Success: true},
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3, Success: true},
	}
	if !reflect.DeepEqual(result.States, wantStates) {
		t.Fatalf("ordered states = %#v, want %#v", result.States, wantStates)
	}
	if len(invoked) != len(wantStates) {
		t.Fatalf("invoked %d states, want %d: %#v", len(invoked), len(wantStates), invoked)
	}

	stored := storedScene(t, database, definition.ID)
	wantPrevious := []store.SceneState{
		{DeviceID: "curtains", CapabilityID: "windowcoverings_state", Value: "up"},
		{DeviceID: "lamp", CapabilityID: "onoff", Value: false},
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.8},
	}
	if !stored.Active || !reflect.DeepEqual(stored.Previous, wantPrevious) {
		t.Fatalf("persisted baseline = active %t, previous %#v", stored.Active, stored.Previous)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if object["requestedOn"] != true || object["active"] != true || object["success"] != true ||
		object["attempted"] != float64(3) || object["succeeded"] != float64(3) || object["failed"] != float64(0) {
		t.Fatalf("JSON contract = %s", encoded)
	}
}

func TestSetOrdersPowerAroundOtherCapabilitiesPerDevice(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	// Intentionally persist dim first, as can happen when the user selects the
	// values in this order in the Scene editor.
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.6},
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	type invocation struct {
		capability string
		value      any
	}
	var invoked []invocation
	activator := New(database, PerCapability(func(_ context.Context, _ string, capability string, value any, _ map[string]any) error {
		invoked = append(invoked, invocation{capability: capability, value: value})
		return nil
	}))

	on, err := activator.Set(context.Background(), definition.ID, true)
	if err != nil || !on.Success {
		t.Fatalf("ON = %#v, error %v", on, err)
	}
	wantOn := []invocation{{capability: "onoff", value: true}, {capability: "dim", value: 0.6}}
	if !reflect.DeepEqual(invoked, wantOn) {
		t.Fatalf("ON invocation order = %#v, want %#v", invoked, wantOn)
	}
	// Execution ordering must not rewrite the persisted/user-facing order.
	wantResults := []StateResult{
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.6, Success: true},
		{DeviceID: "lamp", CapabilityID: "onoff", Value: true, Success: true},
	}
	if !reflect.DeepEqual(on.States, wantResults) {
		t.Fatalf("ON result order = %#v, want %#v", on.States, wantResults)
	}

	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.6})
	invoked = nil
	off, err := activator.Set(context.Background(), definition.ID, false)
	if err != nil || !off.Success {
		t.Fatalf("OFF = %#v, error %v", off, err)
	}
	wantOff := []invocation{{capability: "dim", value: 0.8}, {capability: "onoff", value: false}}
	if !reflect.DeepEqual(invoked, wantOff) {
		t.Fatalf("OFF invocation order = %#v, want %#v", invoked, wantOff)
	}
}

func TestRepeatedOnPreservesTheOriginalBaseline(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	device := addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database,
		store.SceneState{DeviceID: device.ID, CapabilityID: "onoff", Value: true},
	)
	var invoked atomic.Int32
	activator := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		return nil
	}))
	if result, err := activator.Set(context.Background(), definition.ID, true); err != nil || !result.Active {
		t.Fatalf("first ON = %#v, error %v", result, err)
	}
	first := storedScene(t, database, definition.ID)

	device.State["onoff"] = true
	if err := database.UpdateDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	secondResult, err := activator.Set(context.Background(), definition.ID, true)
	if err != nil || !secondResult.Success || !secondResult.Active {
		t.Fatalf("second ON = %#v, error %v", secondResult, err)
	}
	second := storedScene(t, database, definition.ID)
	// The lamp already reports on, so the repeated ON sends nothing and still
	// keeps the baseline from before the first ON.
	if invoked.Load() != 1 || !secondResult.States[0].Unchanged || !reflect.DeepEqual(second.Previous, first.Previous) || second.Previous[0].Value != false {
		t.Fatalf("repeated ON changed baseline: first=%#v second=%#v invocations=%d states=%#v", first.Previous, second.Previous, invoked.Load(), secondResult.States)
	}
	if second.Revision != first.Revision {
		t.Fatalf("runtime state changed scene revision from %d to %d", first.Revision, second.Revision)
	}
}

func TestSetOffRestoresBaselineAndMakesSceneInactive(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.9})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	activator := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil }))
	if _, err := activator.Set(context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}

	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.3})

	var restored []any
	restorer := New(database, PerCapability(func(_ context.Context, _ string, _ string, value any, _ map[string]any) error {
		restored = append(restored, value)
		return nil
	}))
	result, err := restorer.Set(context.Background(), definition.ID, false)
	if err != nil || !result.Success || result.RequestedOn || result.Active || result.Attempted != 2 || result.Succeeded != 2 {
		t.Fatalf("OFF = %#v, error %v", result, err)
	}
	if want := []any{0.9, false}; !reflect.DeepEqual(restored, want) {
		t.Fatalf("restored values = %#v, want %#v", restored, want)
	}
	stored := storedScene(t, database, definition.ID)
	if stored.Active || len(stored.Previous) != 0 {
		t.Fatalf("restored scene remains active: %#v", stored)
	}
}

func TestPartialOnShrinksBaselineAndOffCanBeRetried(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	onFailure := errors.New("switch refused ON")
	activator := New(database, PerCapability(func(_ context.Context, _ string, capability string, _ any, _ map[string]any) error {
		if capability == "onoff" {
			return onFailure
		}
		return nil
	}))
	result, err := activator.Set(context.Background(), definition.ID, true)
	if !errors.Is(err, onFailure) || result.Success || !result.Active || result.Attempted != 2 || result.Succeeded != 1 || result.Failed != 1 {
		t.Fatalf("partial ON = %#v, error %v", result, err)
	}
	stored := storedScene(t, database, definition.ID)
	wantRemaining := []store.SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: 0.8}}
	if !stored.Active || !reflect.DeepEqual(stored.Previous, wantRemaining) {
		t.Fatalf("partial ON baseline = %#v", stored.Previous)
	}

	reportState(t, database, "lamp", map[string]any{"dim": 0.3})

	offFailure := errors.New("dimmer is offline")
	failOff := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return offFailure }))
	firstOff, err := failOff.Set(context.Background(), definition.ID, false)
	if !errors.Is(err, offFailure) || firstOff.Success || !firstOff.Active || firstOff.Attempted != 1 || firstOff.Failed != 1 {
		t.Fatalf("first OFF = %#v, error %v", firstOff, err)
	}
	if after := storedScene(t, database, definition.ID); !after.Active || !reflect.DeepEqual(after.Previous, wantRemaining) {
		t.Fatalf("failed OFF was not retained for retry: %#v", after)
	}

	retry := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil }))
	secondOff, err := retry.Set(context.Background(), definition.ID, false)
	if err != nil || !secondOff.Success || secondOff.Active || secondOff.Attempted != 1 || secondOff.Succeeded != 1 {
		t.Fatalf("retried OFF = %#v, error %v", secondOff, err)
	}
	if after := storedScene(t, database, definition.ID); after.Active || len(after.Previous) != 0 {
		t.Fatalf("successful retry did not clear scene: %#v", after)
	}
}

func TestUnknownCurrentValueIsAVisibleFailureAndIsNeverChanged(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": nil, "dim": 0.8})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	var invoked []string
	activator := New(database, PerCapability(func(_ context.Context, _ string, capability string, _ any, _ map[string]any) error {
		invoked = append(invoked, capability)
		return nil
	}))
	result, err := activator.Set(context.Background(), definition.ID, true)
	if err == nil || result.Success || !result.Active || result.Attempted != 2 || result.Succeeded != 1 || result.Failed != 1 {
		t.Fatalf("ON with unknown baseline = %#v, error %v", result, err)
	}
	if want := []string{"dim"}; !reflect.DeepEqual(invoked, want) {
		t.Fatalf("invoked capabilities = %v, want %v", invoked, want)
	}
	if len(result.States) != 2 || result.States[0].Error == "" || result.States[1].Error != "" {
		t.Fatalf("unknown baseline result = %#v", result.States)
	}
	stored := storedScene(t, database, definition.ID)
	if len(stored.Previous) != 1 || stored.Previous[0].CapabilityID != "dim" {
		t.Fatalf("unknown baseline was persisted: %#v", stored.Previous)
	}

	unknownOnly := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: false},
	)
	allUnknown, err := activator.Set(context.Background(), unknownOnly.ID, true)
	if err == nil || allUnknown.Active || allUnknown.Succeeded != 0 || allUnknown.Failed != 1 {
		t.Fatalf("all-unknown ON = %#v, error %v", allUnknown, err)
	}
	if storedScene(t, database, unknownOnly.ID).Active {
		t.Fatal("a scene with zero successful states stayed active")
	}
}

func TestSetRunsDevicesInParallelButPreservesTheirStateOrder(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "a", []string{"first", "second"}, map[string]any{"first": 0, "second": 0})
	addSceneDevice(t, database, "b", []string{"first", "second"}, map[string]any{"first": 0, "second": 0})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "a", CapabilityID: "first", Value: 1},
		store.SceneState{DeviceID: "b", CapabilityID: "first", Value: 1},
		store.SceneState{DeviceID: "a", CapabilityID: "second", Value: 2},
		store.SceneState{DeviceID: "b", CapabilityID: "second", Value: 2},
	)
	firstCalls := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var mu sync.Mutex
	sequence := map[string][]string{}
	activator := New(database, PerCapability(func(ctx context.Context, device, capability string, _ any, _ map[string]any) error {
		mu.Lock()
		sequence[device] = append(sequence[device], capability)
		mu.Unlock()
		if capability == "first" {
			firstCalls <- device
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type activation struct {
		result ActivationResult
		err    error
	}
	done := make(chan activation, 1)
	go func() {
		result, err := activator.Set(ctx, definition.ID, true)
		done <- activation{result: result, err: err}
	}()
	started := map[string]bool{}
	for len(started) < 2 {
		select {
		case device := <-firstCalls:
			started[device] = true
		case <-ctx.Done():
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("different devices did not start in parallel: %v", started)
		}
	}
	releaseOnce.Do(func() { close(release) })
	finished := <-done
	if finished.err != nil || !finished.result.Success {
		t.Fatalf("activation = %#v, error %v", finished.result, finished.err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, device := range []string{"a", "b"} {
		if want := []string{"first", "second"}; !reflect.DeepEqual(sequence[device], want) {
			t.Errorf("device %s order = %v, want %v", device, sequence[device], want)
		}
	}
}

func TestCancelledOffRetainsUnattemptedStatesForRetry(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	if _, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}

	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.3})

	ctx, cancel := context.WithCancel(context.Background())
	var invoked atomic.Int32
	cancelling := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		cancel()
		return nil
	}))
	result, err := cancelling.Set(ctx, definition.ID, false)
	if !errors.Is(err, context.Canceled) || result.Success || !result.Active ||
		result.Attempted != 1 || result.Succeeded != 1 || result.Failed != 0 || invoked.Load() != 1 {
		t.Fatalf("cancelled OFF = %#v, error %v, invocations %d", result, err, invoked.Load())
	}
	stored := storedScene(t, database, definition.ID)
	if !stored.Active || len(stored.Previous) != 1 || stored.Previous[0].CapabilityID != "onoff" {
		t.Fatalf("unattempted restore state was not retained: %#v", stored.Previous)
	}
	if retry, retryErr := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, false); retryErr != nil || !retry.Success || retry.Active {
		t.Fatalf("OFF retry = %#v, error %v", retry, retryErr)
	}
}

func TestSetSerializesAcrossActivatorsAndNormalizedIDs(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	if _, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}

	reportState(t, database, "lamp", map[string]any{"onoff": true})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})).Set(context.Background(), definition.ID, false)
		firstDone <- err
	}()
	<-firstStarted

	var secondInvoked atomic.Int32
	type setOutcome struct {
		result ActivationResult
		err    error
	}
	secondDone := make(chan setOutcome, 1)
	go func() {
		result, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
			secondInvoked.Add(1)
			return nil
		})).Set(context.Background(), "  "+definition.ID+"  ", false)
		secondDone <- setOutcome{result: result, err: err}
	}()

	// Observe that both calls registered for the same normalized, Store-scoped
	// lock before releasing the first one. This avoids a timing-based assertion
	// that merely hopes the second goroutine had time to start.
	key := activationLockKey{database: database, sceneID: definition.ID}
	deadline := time.Now().Add(2 * time.Second)
	for {
		activationLockRegistry.Lock()
		lock := activationLockRegistry.locks[key]
		waiting := lock != nil && lock.refs == 2
		activationLockRegistry.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			close(releaseFirst)
			t.Fatal("second Activator never waited on the shared scene lock")
		}
		time.Sleep(time.Millisecond)
	}
	if secondInvoked.Load() != 0 {
		close(releaseFirst)
		t.Fatal("second Activator invoked a restore while the first still owned the scene")
	}
	select {
	case outcome := <-secondDone:
		close(releaseFirst)
		t.Fatalf("second OFF completed before the first: %#v, error %v", outcome.result, outcome.err)
	default:
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first OFF: %v", err)
	}
	second := <-secondDone
	if second.err != nil || !second.result.Success || second.result.Active || second.result.Attempted != 0 {
		t.Fatalf("serialized second OFF = %#v, error %v", second.result, second.err)
	}
	if secondInvoked.Load() != 0 {
		t.Fatalf("second OFF invoked %d already-restored states", secondInvoked.Load())
	}
}

func TestPreCancelledSetReportsTheDurableActiveState(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	if _, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var invoked atomic.Int32
	result, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		return nil
	})).Set(ctx, definition.ID, false)
	if !errors.Is(err, context.Canceled) || result.Success || !result.Active || result.RequestedOn ||
		result.SceneID != definition.ID || result.SceneName != definition.Name || result.Attempted != 0 || invoked.Load() != 0 {
		t.Fatalf("pre-cancelled OFF = %#v, error %v, invocations %d", result, err, invoked.Load())
	}
}

func TestCancellationAfterLockAcquisitionReportsTheDurableActiveState(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	if _, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}

	var invoked atomic.Int32
	result, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		return nil
	})).Set(newCancelOnSecondCheckContext(), definition.ID, false)
	if !errors.Is(err, context.Canceled) || result.Success || !result.Active || result.RequestedOn ||
		result.SceneID != definition.ID || result.SceneName != definition.Name || result.Attempted != 0 || invoked.Load() != 0 {
		t.Fatalf("OFF canceled after acquire = %#v, error %v, invocations %d", result, err, invoked.Load())
	}
}

func TestBeginPersistenceFailurePreventsDeviceChanges(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "live")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	database := sceneStore(t, filepath.Join(directory, "stulp.json"))
	addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	if err := os.Rename(directory, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	var invoked atomic.Int32
	result, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		return nil
	})).Set(context.Background(), definition.ID, true)
	if err == nil || result.Active || result.Attempted != 0 || invoked.Load() != 0 {
		t.Fatalf("failed begin = %#v, error %v, invocations %d", result, err, invoked.Load())
	}
	if storedScene(t, database, definition.ID).Active {
		t.Fatal("failed BeginScene made the scene active in memory")
	}
}

func TestRestoreSetPersistenceFailureIsReportedAndLeavesSafeBaseline(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "live")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	database := sceneStore(t, filepath.Join(directory, "stulp.json"))
	addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	var moved atomic.Bool
	result, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		if moved.CompareAndSwap(false, true) {
			return os.Rename(directory, filepath.Join(root, "moved"))
		}
		return nil
	})).Set(context.Background(), definition.ID, true)
	if err == nil || result.Success || !result.Active || result.Attempted != 1 || result.Succeeded != 1 {
		t.Fatalf("failed baseline shrink = %#v, error %v", result, err)
	}
	stored := storedScene(t, database, definition.ID)
	if !stored.Active || len(stored.Previous) != 1 || stored.Previous[0].Value != false {
		t.Fatalf("persistence failure lost safe baseline: %#v", stored)
	}
}

func TestSetMissingSceneDoesNotInvokeCapabilities(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	var invoked atomic.Int32
	activator := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		return nil
	}))
	result, err := activator.Set(context.Background(), "missing", true)
	if err == nil || result.SceneID != "missing" || result.Attempted != 0 || invoked.Load() != 0 {
		t.Fatalf("missing activation = %#v, error %v, invocations %d", result, err, invoked.Load())
	}
}

func TestSetOffSkipsValuesTheDeviceAlreadyReports(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim", "light_saturation"},
		map[string]any{"onoff": true, "dim": 0.9, "light_saturation": 0.5})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: false},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
		store.SceneState{DeviceID: "lamp", CapabilityID: "light_saturation", Value: 0.5},
	)
	if _, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}
	// The lamp reports in the steps its radio allows: 0.3 comes back as 76/254,
	// and the saturation the scene left alone drifts by one step as well.
	reportState(t, database, "lamp", map[string]any{"onoff": false, "dim": 0.2992, "light_saturation": 0.5039})

	type invocation struct {
		capability string
		value      any
	}
	var invoked []invocation
	result, err := New(database, PerCapability(func(_ context.Context, _ string, capability string, value any, _ map[string]any) error {
		invoked = append(invoked, invocation{capability: capability, value: value})
		return nil
	})).Set(context.Background(), definition.ID, false)
	if err != nil || !result.Success || result.Active || result.Attempted != 3 || result.Succeeded != 3 || result.Failed != 0 {
		t.Fatalf("OFF = %#v, error %v", result, err)
	}
	// Power first, then the level. The saturation is not sent at all: it is
	// already there, and a lamp that just powered off ignores it anyway.
	wantInvoked := []invocation{{capability: "onoff", value: true}, {capability: "dim", value: 0.9}}
	if !reflect.DeepEqual(invoked, wantInvoked) {
		t.Fatalf("restore invocations = %#v, want %#v", invoked, wantInvoked)
	}
	wantStates := []StateResult{
		{DeviceID: "lamp", CapabilityID: "onoff", Value: true, Success: true},
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.9, Success: true},
		{DeviceID: "lamp", CapabilityID: "light_saturation", Value: 0.5, Success: true, Unchanged: true},
	}
	if !reflect.DeepEqual(result.States, wantStates) {
		t.Fatalf("restore states = %#v, want %#v", result.States, wantStates)
	}
	if stored := storedScene(t, database, definition.ID); stored.Active || len(stored.Previous) != 0 {
		t.Fatalf("scene after OFF = %#v", stored)
	}
}

func TestSetOnSkipsValuesTheDeviceAlreadyReports(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	// The lamp is on and dimmed to what its radio calls 0.3; only the color
	// differs from the scene. Floats and 1/254 steps must not turn "already
	// there" into a command.
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim", "light_saturation"},
		map[string]any{"onoff": true, "dim": 0.2992, "light_saturation": 0.5})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
		store.SceneState{DeviceID: "lamp", CapabilityID: "light_saturation", Value: 0.8},
	)
	type invocation struct {
		capability string
		value      any
	}
	var invoked []invocation
	activator := New(database, PerCapability(func(_ context.Context, _ string, capability string, value any, _ map[string]any) error {
		invoked = append(invoked, invocation{capability: capability, value: value})
		return nil
	}))
	result, err := activator.Set(context.Background(), definition.ID, true)
	if err != nil || !result.Success || !result.Active || result.Attempted != 3 || result.Succeeded != 3 || result.Failed != 0 {
		t.Fatalf("ON = %#v, error %v", result, err)
	}
	if want := []invocation{{capability: "light_saturation", value: 0.8}}; !reflect.DeepEqual(invoked, want) {
		t.Fatalf("ON invocations = %#v, want %#v", invoked, want)
	}
	wantStates := []StateResult{
		{DeviceID: "lamp", CapabilityID: "onoff", Value: true, Success: true, Unchanged: true},
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3, Success: true, Unchanged: true},
		{DeviceID: "lamp", CapabilityID: "light_saturation", Value: 0.8, Success: true},
	}
	if !reflect.DeepEqual(result.States, wantStates) {
		t.Fatalf("ON states = %#v, want %#v", result.States, wantStates)
	}
	// The scene owns all three values now, so all three are in the restore set.
	stored := storedScene(t, database, definition.ID)
	if !stored.Active || len(stored.Previous) != 3 {
		t.Fatalf("scene after ON = %#v", stored)
	}

	// Off puts back only what actually moved.
	reportState(t, database, "lamp", map[string]any{"light_saturation": 0.8})
	invoked = nil
	result, err = activator.Set(context.Background(), definition.ID, false)
	if err != nil || !result.Success || result.Active || result.Attempted != 3 || result.Failed != 0 {
		t.Fatalf("OFF = %#v, error %v", result, err)
	}
	if want := []invocation{{capability: "light_saturation", value: 0.5}}; !reflect.DeepEqual(invoked, want) {
		t.Fatalf("OFF invocations = %#v, want %#v", invoked, want)
	}
}

func TestButtonSceneSkipsValuesTheDeviceAlreadyReports(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.2039})
	definition, err := database.CreateScene(context.Background(), store.Scene{
		Name: "Tuin uit", Kind: store.SceneKindButton, States: []store.SceneState{
			{DeviceID: "lamp", CapabilityID: "onoff", Value: false},
			{DeviceID: "lamp", CapabilityID: "dim", Value: 0.2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	invoked := 0
	result, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error {
		invoked++
		return nil
	})).Set(context.Background(), definition.ID, true)
	if err != nil || !result.Success || result.Attempted != 2 || result.Succeeded != 2 || invoked != 0 {
		t.Fatalf("pressing a button whose states already hold = %#v, error %v, %d invocations", result, err, invoked)
	}
	for _, state := range result.States {
		if !state.Unchanged {
			t.Fatalf("state %s was not reported as unchanged: %#v", state.CapabilityID, state)
		}
	}
}

func TestSetOffStillRestoresWhenTheCurrentValueIsUnknown(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff"}, map[string]any{"onoff": false})
	definition := createScene(t, database, store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true})
	if _, err := New(database, PerCapability(func(context.Context, string, string, any, map[string]any) error { return nil })).Set(
		context.Background(), definition.ID, true); err != nil {
		t.Fatal(err)
	}
	// The lamp stopped reporting. Unknown is not "already there".
	reportState(t, database, "lamp", map[string]any{"onoff": nil})

	var invoked []any
	result, err := New(database, PerCapability(func(_ context.Context, _ string, _ string, value any, _ map[string]any) error {
		invoked = append(invoked, value)
		return nil
	})).Set(context.Background(), definition.ID, false)
	if err != nil || !result.Success || result.Active || result.Attempted != 1 || result.States[0].Unchanged {
		t.Fatalf("OFF with unknown current value = %#v, error %v", result, err)
	}
	if want := []any{false}; !reflect.DeepEqual(invoked, want) {
		t.Fatalf("restore invocations = %#v, want %#v", invoked, want)
	}
}

func TestButtonSceneIsPressedWithoutSnapshotAndCannotBeTurnedOff(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": true, "dim": 0.8})
	// A value nobody has read yet is no obstacle: a button needs no restore point.
	addSceneDevice(t, database, "blind", []string{"windowcoverings_set"}, map[string]any{"windowcoverings_set": nil})
	definition, err := database.CreateScene(context.Background(), store.Scene{
		Name: "Tuin uit", Kind: store.SceneKindButton, States: []store.SceneState{
			{DeviceID: "lamp", CapabilityID: "onoff", Value: false},
			{DeviceID: "lamp", CapabilityID: "dim", Value: 0.2},
			{DeviceID: "blind", CapabilityID: "windowcoverings_set", Value: 0.0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var invoked []store.SceneState
	activator := New(database, PerCapability(func(_ context.Context, device, capability string, value any, _ map[string]any) error {
		mu.Lock()
		invoked = append(invoked, store.SceneState{DeviceID: device, CapabilityID: capability, Value: value})
		mu.Unlock()
		return nil
	}))
	for press := 1; press <= 2; press++ {
		result, err := activator.Set(context.Background(), definition.ID, true)
		if err != nil || !result.Success || !result.Momentary || result.Active || !result.RequestedOn ||
			result.Attempted != 3 || result.Succeeded != 3 || result.Failed != 0 {
			t.Fatalf("press %d = %#v, error %v", press, result, err)
		}
		if stored := storedScene(t, database, definition.ID); stored.Active || stored.Previous != nil {
			t.Fatalf("press %d left restore state behind: %#v", press, stored)
		}
	}
	mu.Lock()
	var lamp, blind []store.SceneState
	for _, call := range invoked {
		if call.DeviceID == "lamp" {
			lamp = append(lamp, call)
		} else {
			blind = append(blind, call)
		}
	}
	mu.Unlock()
	// Devices run in parallel; within the lamp, power still goes last.
	wantLamp := []store.SceneState{
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.2}, {DeviceID: "lamp", CapabilityID: "onoff", Value: false},
		{DeviceID: "lamp", CapabilityID: "dim", Value: 0.2}, {DeviceID: "lamp", CapabilityID: "onoff", Value: false},
	}
	if !reflect.DeepEqual(lamp, wantLamp) || len(blind) != 2 || blind[0].Value != 0.0 {
		t.Fatalf("press invocations: lamp=%#v blind=%#v", lamp, blind)
	}

	result, err := activator.Set(context.Background(), definition.ID, false)
	if err == nil || !strings.Contains(err.Error(), "button") || result.Success || !result.Momentary || result.Active || result.Attempted != 0 {
		t.Fatalf("OFF on a button scene = %#v, error %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(invoked) != 6 {
		t.Fatalf("OFF on a button scene sent commands: %#v", invoked)
	}
}

// A device gets everything a scene wants from it in one call, in command order,
// so the app can send one message where the device takes one. Devices still
// run side by side.
func TestSetHandsEachDeviceItsCommandsInOneCall(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim", "light_hue"}, map[string]any{"onoff": false, "dim": 0.1, "light_hue": 0.0})
	addSceneDevice(t, database, "blind", []string{"windowcoverings_set"}, map[string]any{"windowcoverings_set": 1.0})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.6},
		store.SceneState{DeviceID: "blind", CapabilityID: "windowcoverings_set", Value: 0.2},
		store.SceneState{DeviceID: "lamp", CapabilityID: "light_hue", Value: 0.5},
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
	)
	var mu sync.Mutex
	calls := map[string][]Command{}
	activator := New(database, func(_ context.Context, deviceID string, commands []Command) ([]error, error) {
		mu.Lock()
		defer mu.Unlock()
		if _, again := calls[deviceID]; again {
			t.Errorf("device %s was called twice", deviceID)
		}
		calls[deviceID] = commands
		return make([]error, len(commands)), nil
	})
	result, err := activator.Set(context.Background(), definition.ID, true)
	if err != nil || !result.Success || result.Attempted != 4 || result.Succeeded != 4 {
		t.Fatalf("ON = %#v, error %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	// Power first, then the rest in the scene's own order.
	wantLamp := []Command{{CapabilityID: "onoff", Value: true}, {CapabilityID: "dim", Value: 0.6}, {CapabilityID: "light_hue", Value: 0.5}}
	if !reflect.DeepEqual(calls["lamp"], wantLamp) {
		t.Fatalf("lamp commands = %#v, want %#v", calls["lamp"], wantLamp)
	}
	if want := []Command{{CapabilityID: "windowcoverings_set", Value: 0.2}}; !reflect.DeepEqual(calls["blind"], want) {
		t.Fatalf("blind commands = %#v, want %#v", calls["blind"], want)
	}
}

// What the route answers per command is what the scene reports per state: a
// failed command fails its state, an error for the whole call fails them all,
// and a command the route never sent stays unattempted and in the restore set.
func TestSetProjectsTheRouteAnswerOntoEachState(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.1})
	addSceneDevice(t, database, "fan", []string{"onoff"}, map[string]any{"onoff": false})
	addSceneDevice(t, database, "blind", []string{"windowcoverings_set"}, map[string]any{"windowcoverings_set": 1.0})
	definition := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.6},
		store.SceneState{DeviceID: "fan", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "blind", CapabilityID: "windowcoverings_set", Value: 0.2},
	)
	dimFailure := errors.New("level rejected")
	fanFailure := errors.New("fan is offline")
	activator := New(database, func(_ context.Context, deviceID string, commands []Command) ([]error, error) {
		switch deviceID {
		case "lamp":
			return []error{nil, dimFailure}, nil
		case "fan":
			return nil, fanFailure
		default:
			return []error{ErrNotAttempted}, nil
		}
	})
	result, err := activator.Set(context.Background(), definition.ID, true)
	if err == nil || result.Success || result.Attempted != 3 || result.Succeeded != 1 || result.Failed != 2 {
		t.Fatalf("ON = %#v, error %v", result, err)
	}
	if !errors.Is(err, dimFailure) || !errors.Is(err, fanFailure) {
		t.Fatalf("ON error lost a cause: %v", err)
	}
	byCapability := map[string]StateResult{}
	for _, state := range result.States {
		byCapability[state.DeviceID+"/"+state.CapabilityID] = state
	}
	if !byCapability["lamp/onoff"].Success || byCapability["lamp/dim"].Error != "level rejected" || byCapability["fan/onoff"].Error != "fan is offline" {
		t.Fatalf("states = %#v", result.States)
	}
	if _, attempted := byCapability["blind/windowcoverings_set"]; attempted {
		t.Fatalf("an unsent command was reported as attempted: %#v", result.States)
	}
	// Only the state that actually changed is in the restore set.
	stored := storedScene(t, database, definition.ID)
	if len(stored.Previous) != 1 || stored.Previous[0].DeviceID != "lamp" || stored.Previous[0].CapabilityID != "onoff" {
		t.Fatalf("restore set = %#v", stored.Previous)
	}
}
