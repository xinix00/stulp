package scene

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/store"
)

func waitForSceneActive(t *testing.T, database *store.Store, id string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if storedScene(t, database, id).Active == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("scene %q did not become active=%t", id, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// canaryScene is a second scene whose end proves the watcher has handled
// every report published before it: events reach the watcher in order.
func canaryScene(t *testing.T, database *store.Store) store.Scene {
	t.Helper()
	addSceneDevice(t, database, "fan", []string{"onoff"}, map[string]any{"onoff": false})
	definition, err := database.CreateScene(context.Background(), store.Scene{
		Name: "Ventilator", States: []store.SceneState{{DeviceID: "fan", CapabilityID: "onoff", Value: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func tripCanary(t *testing.T, database *store.Store, canary store.Scene) {
	t.Helper()
	if _, err := New(database, func(context.Context, string, string, any, map[string]any) error { return nil }).Set(
		context.Background(), canary.ID, true); err != nil {
		t.Fatal(err)
	}
	reportState(t, database, "fan", map[string]any{"onoff": true})
	reportState(t, database, "fan", map[string]any{"onoff": false})
	waitForSceneActive(t, database, canary.ID, false)
}

func TestWatcherEndsASceneWhenAValueItSetIsChangedElsewhere(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	film := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	canary := canaryScene(t, database)
	watcher := NewWatcher(database, nil)
	t.Cleanup(watcher.Close)

	var invoked atomic.Int32
	if _, err := New(database, func(context.Context, string, string, any, map[string]any) error {
		invoked.Add(1)
		return nil
	}).Set(context.Background(), film.ID, true); err != nil {
		t.Fatal(err)
	}
	sent := invoked.Load()

	// The lamp works its way to the scene: power first, then a fade through
	// levels that never were the target, and a reading that goes missing for a
	// moment. None of that is a change made elsewhere.
	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.8})
	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.55})
	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": nil})
	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.2992})
	tripCanary(t, database, canary)
	if stored := storedScene(t, database, film.ID); !stored.Active || len(stored.Previous) != 2 {
		t.Fatalf("scene ended while the lamp was still settling: %#v", stored)
	}

	// Someone dims the lamp by hand. The scene reads as off and nothing is put
	// back: the other values stay where the scene left them.
	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.6})
	waitForSceneActive(t, database, film.ID, false)
	if stored := storedScene(t, database, film.ID); len(stored.Previous) != 0 {
		t.Fatalf("ended scene kept restore state: %#v", stored)
	}
	if invoked.Load() != sent {
		t.Fatalf("watcher sent %d commands", invoked.Load()-sent)
	}
	lamp, err := database.Device(context.Background(), "lamp")
	if err != nil || lamp.State["dim"] != 0.6 || lamp.State["onoff"] != true {
		t.Fatalf("lamp after the scene ended = %#v, %v", lamp.State, err)
	}
	sceneDevice, err := database.Device(context.Background(), store.SceneDeviceID(film.ID))
	if err != nil || sceneDevice.State["onoff"] != false {
		t.Fatalf("scene device after the scene ended = %#v, %v", sceneDevice.State, err)
	}
}

func TestWatcherIgnoresStatesTheSceneDoesNotOwn(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	film := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	canary := canaryScene(t, database)
	watcher := NewWatcher(database, nil)
	t.Cleanup(watcher.Close)

	// The lamp refuses power on the way in: only dim ends up in the restore set.
	failing := New(database, func(_ context.Context, _ string, capability string, _ any, _ map[string]any) error {
		if capability == "onoff" {
			return errors.New("no power")
		}
		return nil
	})
	if _, err := failing.Set(context.Background(), film.ID, true); err == nil {
		t.Fatal("partial ON did not report its failure")
	}
	reportState(t, database, "lamp", map[string]any{"dim": 0.3})
	// Power toggled by hand: the scene never owned it.
	reportState(t, database, "lamp", map[string]any{"onoff": true})
	reportState(t, database, "lamp", map[string]any{"onoff": false})
	tripCanary(t, database, canary)
	wantPrevious := []store.SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: 0.8}}
	if stored := storedScene(t, database, film.ID); !stored.Active || !reflect.DeepEqual(stored.Previous, wantPrevious) {
		t.Fatalf("scene reacted to a value it never owned: %#v", stored)
	}

	// The dimmer refuses the restore: the scene stays on for a retry. The
	// lamp reporting that restore is not a change made elsewhere either.
	refusing := New(database, func(_ context.Context, _ string, capability string, _ any, _ map[string]any) error {
		if capability == "dim" {
			return errors.New("dimmer is offline")
		}
		return nil
	})
	if _, err := refusing.Set(context.Background(), film.ID, false); err == nil {
		t.Fatal("refused restore did not report its failure")
	}
	if stored := storedScene(t, database, film.ID); !stored.Active || !reflect.DeepEqual(stored.Previous, wantPrevious) {
		t.Fatalf("refused restore lost its retry state: %#v", stored)
	}

	// Now the dim is changed by hand: that value the scene does own.
	reportState(t, database, "lamp", map[string]any{"dim": 0.6})
	waitForSceneActive(t, database, film.ID, false)
}

func TestWatcherIgnoresRestoredStatesAndButtonScenes(t *testing.T) {
	database := sceneStore(t, store.InMemoryPath)
	addSceneDevice(t, database, "lamp", []string{"onoff", "dim"}, map[string]any{"onoff": false, "dim": 0.8})
	film := createScene(t, database,
		store.SceneState{DeviceID: "lamp", CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: "lamp", CapabilityID: "dim", Value: 0.3},
	)
	garden, err := database.CreateScene(context.Background(), store.Scene{
		Name: "Tuin uit", Kind: store.SceneKindButton,
		States: []store.SceneState{{DeviceID: "lamp", CapabilityID: "onoff", Value: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	canary := canaryScene(t, database)
	watcher := NewWatcher(database, nil)
	t.Cleanup(watcher.Close)

	noop := New(database, func(context.Context, string, string, any, map[string]any) error { return nil })
	if _, err := noop.Set(context.Background(), film.ID, true); err != nil {
		t.Fatal(err)
	}
	reportState(t, database, "lamp", map[string]any{"onoff": true, "dim": 0.3})

	// A restore that only half succeeds: power went back, dim did not. The
	// lamp reporting its power-off is the restore, not a change elsewhere.
	refusing := New(database, func(_ context.Context, _ string, capability string, _ any, _ map[string]any) error {
		if capability == "dim" {
			return errors.New("dimmer is offline")
		}
		return nil
	})
	if _, err := refusing.Set(context.Background(), film.ID, false); err == nil {
		t.Fatal("refused restore did not report its failure")
	}
	reportState(t, database, "lamp", map[string]any{"onoff": false})
	// Pressing the button scene is never a session the watcher could end.
	if _, err := noop.Set(context.Background(), garden.ID, true); err != nil {
		t.Fatal(err)
	}
	tripCanary(t, database, canary)
	wantPrevious := []store.SceneState{{DeviceID: "lamp", CapabilityID: "dim", Value: 0.8}}
	if stored := storedScene(t, database, film.ID); !stored.Active || !reflect.DeepEqual(stored.Previous, wantPrevious) {
		t.Fatalf("retry state after a partial restore = %#v", stored)
	}
	if stored := storedScene(t, database, garden.ID); stored.Active || stored.Previous != nil {
		t.Fatalf("button scene = %#v", stored)
	}
}
