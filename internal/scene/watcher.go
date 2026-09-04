package scene

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"sync"

	"github.com/xinix00/stulp/internal/store"
)

// Watcher ends a switch scene as soon as a value it set is changed by
// something else: a wall switch, an app, a Flow or a person in Manage. The
// scene then reads as off, and the restore snapshot is dropped rather than
// applied. Putting every other device back because someone brightened one
// lamp would be the surprising choice.
//
// A value counts as changed elsewhere only when it moves away from the
// scene's target after having reached it. Devices report their way towards a
// new level (a dimmer fading, a light that reports on before its color) and
// those intermediate readings never matched the target in the first place.
type Watcher struct {
	database    *store.Store
	logger      *slog.Logger
	events      <-chan store.Event
	unsubscribe func()
	ctx         context.Context
	stop        context.CancelFunc
	done        chan struct{}
	closeOnce   sync.Once

	// Only the event loop touches these.
	deviceState map[string]map[string]any
	targets     map[string][]sceneTarget
}

// sceneTarget is one value an active switch scene owns on a device: it set
// the value and still holds the restore for it.
type sceneTarget struct {
	sceneID      string
	capabilityID string
	value        any
}

// NewWatcher starts following the store. Close stops it.
func NewWatcher(database *store.Store, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	events, unsubscribe := database.Subscribe(256)
	ctx, stop := context.WithCancel(context.Background())
	w := &Watcher{
		database: database, logger: logger, events: events, unsubscribe: unsubscribe,
		ctx: ctx, stop: stop, done: make(chan struct{}),
		deviceState: make(map[string]map[string]any), targets: make(map[string][]sceneTarget),
	}
	go w.run()
	return w
}

// Close stops the watcher and waits for its loop to end.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		w.stop()
		w.unsubscribe()
		<-w.done
	})
}

func (w *Watcher) run() {
	defer close(w.done)
	w.reload()
	for {
		select {
		case <-w.ctx.Done():
			return
		case event, open := <-w.events:
			if !open {
				return
			}
			w.handle(event)
		}
	}
}

func (w *Watcher) handle(event store.Event) {
	switch event.Manager {
	case "store":
		// The store emptied this subscriber's queue: whatever was mirrored is
		// stale, and the changes in between are unknowable.
		w.reload()
	case "scene":
		w.reloadTargets()
	case "devices":
		switch event.Type {
		case "device.delete":
			delete(w.deviceState, event.ID)
		case "device.create", "device.update":
			if device, ok := event.Data.(store.Device); ok {
				w.observe(device)
			}
		}
	}
}

func (w *Watcher) reload() {
	w.deviceState = make(map[string]map[string]any)
	devices, err := w.database.Devices(w.ctx, "")
	if err != nil {
		w.logger.Warn("scene watcher cannot read devices", "error", err)
	}
	for _, device := range devices {
		w.deviceState[device.ID] = maps.Clone(device.State)
	}
	w.reloadTargets()
}

// reloadTargets indexes the values active switch scenes currently own. A
// state that failed to apply, or was already restored, is not in Previous and
// is nobody's business here.
func (w *Watcher) reloadTargets() {
	scenes, err := w.database.Scenes(w.ctx)
	if err != nil {
		w.logger.Warn("scene watcher cannot read scenes", "error", err)
		return
	}
	targets := make(map[string][]sceneTarget)
	for _, definition := range scenes {
		if !definition.Active || definition.Momentary() {
			continue
		}
		owned := indexStates(definition.Previous)
		for _, desired := range definition.States {
			if _, held := owned[stateKey(desired)]; !held {
				continue
			}
			targets[desired.DeviceID] = append(targets[desired.DeviceID], sceneTarget{
				sceneID: definition.ID, capabilityID: desired.CapabilityID, value: desired.Value,
			})
		}
	}
	w.targets = targets
}

// observe compares a device report with the previous one and ends every
// scene whose value just left its target.
func (w *Watcher) observe(device store.Device) {
	previous := w.deviceState[device.ID]
	w.deviceState[device.ID] = maps.Clone(device.State)
	if previous == nil {
		return
	}
	for _, target := range w.targets[device.ID] {
		before, known := previous[target.capabilityID]
		current := device.State[target.capabilityID]
		if !known || current == nil || reflect.DeepEqual(before, current) {
			continue
		}
		if !sameValue(target.value, before) || sameValue(target.value, current) {
			continue
		}
		w.end(target.sceneID, device.ID, target.capabilityID)
	}
}

// end drops the restore snapshot of one scene. It takes the scene's
// activation lock so a Set in flight finishes first, then re-reads everything
// it decided on: the scene may have been turned off meanwhile, the state may
// have been restored already, or the device may have returned to the target.
func (w *Watcher) end(sceneID, deviceID, capabilityID string) {
	release, err := acquire(w.ctx, w.database, sceneID)
	if err != nil {
		return
	}
	defer release()
	definition, err := w.database.Scene(w.ctx, sceneID)
	if err != nil || !definition.Active || definition.Momentary() {
		return
	}
	key := sceneStateKey{deviceID: deviceID, capabilityID: capabilityID}
	if _, held := indexStates(definition.Previous)[key]; !held {
		return
	}
	target, wanted := indexStates(definition.States)[key]
	if !wanted {
		return
	}
	device, err := w.database.Device(w.ctx, deviceID)
	if err != nil {
		return
	}
	current := device.State[capabilityID]
	if current == nil || sameValue(target.Value, current) {
		return
	}
	if _, err := w.database.SetScenePrevious(w.ctx, sceneID, nil); err != nil {
		w.logger.Warn("scene could not be ended after a value changed elsewhere",
			"scene", definition.Name, "device", device.Name, "capability", capabilityID, "error", err)
		return
	}
	w.logger.Info("scene ended: a value it set was changed elsewhere",
		"scene", definition.Name, "device", device.Name, "capability", capabilityID,
		"value", current, "wanted", target.Value)
}
