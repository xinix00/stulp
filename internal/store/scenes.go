package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

const (
	maxSceneNameLength = 160
	maxSceneStates     = 256
	sceneDevicePrefix  = "scene:"
)

// NativeSceneAppID owns the synthetic devices through which Scenes behave like
// persistent on/off devices without a plugin process.
const NativeSceneAppID = "com.stulp.scene"

// SceneDeviceID returns the stable synthetic device id for a Scene.
func SceneDeviceID(sceneID string) string { return sceneDevicePrefix + strings.TrimSpace(sceneID) }

// SceneIDFromDeviceID reverses SceneDeviceID for synthetic Scene device ids.
func SceneIDFromDeviceID(deviceID string) (string, bool) {
	sceneID, found := strings.CutPrefix(strings.TrimSpace(deviceID), sceneDevicePrefix)
	return sceneID, found && sceneID != ""
}

// Scene kinds. A switch remembers what it changed and puts that back when it
// is turned off. A button only applies its states: "garden lights off" has no
// meaningful off, so it must never restore anything.
const (
	SceneKindSwitch = "switch"
	SceneKindButton = "button"
)

// Scene is a user-owned desired state spanning one or more devices. Values are
// configuration rather than live device state, so they are persisted with the
// rest of the document.
type Scene struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is SceneKindSwitch or SceneKindButton. Documents written before
	// kinds existed carry no value; they are switches.
	Kind      string       `json:"kind,omitempty"`
	States    []SceneState `json:"states"`
	Active    bool         `json:"active,omitempty"`
	Previous  []SceneState `json:"previous,omitempty"`
	CreatedAt string       `json:"createdAt"`
	UpdatedAt string       `json:"updatedAt"`
	Revision  uint64       `json:"revision"`
}

// Momentary reports whether the Scene is a button: applied on press, never
// active and never restored.
func (scene Scene) Momentary() bool { return scene.Kind == SceneKindButton }

// CapabilityID is the single capability of the Scene's synthetic device: onoff
// for a switch, button for a momentary scene.
func (scene Scene) CapabilityID() string {
	if scene.Momentary() {
		return "button"
	}
	return "onoff"
}

// SceneState is one capability value a Scene requests when it is activated.
type SceneState struct {
	DeviceID     string `json:"deviceId"`
	CapabilityID string `json:"capabilityId"`
	Value        any    `json:"value"`
}

var (
	// ErrSceneChanged means an update was based on an older Scene revision.
	ErrSceneChanged = errors.New("scene changed since it was read")
	// ErrSceneActive protects the restore snapshot while a Scene is on.
	ErrSceneActive = errors.New("active scene cannot be edited or deleted")
	// ErrSceneInUse protects a Scene device referenced by a persisted Flow.
	ErrSceneInUse = errors.New("scene is used by a flow")
	// ErrSceneMomentary rejects a restore session on a button scene.
	ErrSceneMomentary = errors.New("button scene has no restore state")
)

func (scene *Scene) normalize() error {
	scene.ID = strings.TrimSpace(scene.ID)
	scene.Name = strings.TrimSpace(scene.Name)
	if scene.Name == "" {
		return errors.New("scene name is required")
	}
	if len(scene.Name) > maxSceneNameLength {
		return errors.New("scene name is too long")
	}
	switch scene.Kind = strings.TrimSpace(scene.Kind); scene.Kind {
	case "":
		scene.Kind = SceneKindSwitch
	case SceneKindSwitch, SceneKindButton:
	default:
		return fmt.Errorf("scene kind %q must be %q or %q", scene.Kind, SceneKindSwitch, SceneKindButton)
	}
	if len(scene.States) == 0 {
		return errors.New("scene needs at least one state")
	}
	return normalizeSceneStates(scene.States)
}

// normalizeSceneStates validates both desired states and runtime restore
// snapshots. Callers own the slice because identifiers are canonicalized.
func normalizeSceneStates(states []SceneState) error {
	if len(states) > maxSceneStates {
		return fmt.Errorf("a scene supports at most %d states", maxSceneStates)
	}

	type stateKey struct{ deviceID, capabilityID string }
	seen := make(map[stateKey]struct{}, len(states))
	for index := range states {
		state := &states[index]
		state.DeviceID = strings.TrimSpace(state.DeviceID)
		state.CapabilityID = strings.TrimSpace(state.CapabilityID)
		if state.DeviceID == "" {
			return fmt.Errorf("scene state %d deviceId is required", index+1)
		}
		if state.CapabilityID == "" {
			return fmt.Errorf("scene state %d capabilityId is required", index+1)
		}
		key := stateKey{deviceID: state.DeviceID, capabilityID: state.CapabilityID}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("scene has duplicate state for device %q capability %q", state.DeviceID, state.CapabilityID)
		}
		seen[key] = struct{}{}
		// In-memory stores do not pass through saveDocument, so reject a value
		// that cannot ever be represented in the document here as well.
		if _, err := json.Marshal(state.Value); err != nil {
			return fmt.Errorf("scene state %d value: %w", index+1, err)
		}
	}
	return nil
}

func cloneScene(scene Scene) Scene {
	scene.States = cloneSceneStates(scene.States)
	scene.Previous = cloneSceneStates(scene.Previous)
	return scene
}

func cloneSceneStates(states []SceneState) []SceneState {
	if states == nil {
		return nil
	}
	cloned := make([]SceneState, len(states))
	copy(cloned, states)
	for index := range cloned {
		cloned[index].Value = cloneSceneValue(cloned[index].Value)
	}
	return cloned
}

// cloneSceneValue preserves the concrete type of JSON-compatible values while
// separating all mutable containers. Scene values mostly arrive as decoded
// JSON, but callers inside Stulp may also provide typed slices, maps or structs.
func cloneSceneValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneSceneReflect(reflect.ValueOf(value)).Interface()
}

func cloneSceneReflect(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneSceneReflect(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloneSceneReflect(value.Elem()))
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneSceneReflect(iterator.Key()), cloneSceneReflect(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneSceneReflect(value.Index(index)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneSceneReflect(value.Index(index)))
		}
		return result
	case reflect.Struct:
		// Start with a value copy so fields a type deliberately keeps private
		// remain intact; recursively separate the exported, JSON-visible fields.
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).IsExported() {
				result.Field(index).Set(cloneSceneReflect(value.Field(index)))
			}
		}
		return result
	default:
		return value
	}
}

func newSceneDeviceRecord(scene Scene, previous *deviceRecord, sortOrder int) deviceRecord {
	record := deviceRecord{
		ID: SceneDeviceID(scene.ID), AppID: NativeSceneAppID, DriverID: "scene",
		Name: scene.Name, Class: "scene", Data: map[string]any{"sceneId": scene.ID},
		Capabilities: []string{scene.CapabilityID()}, Available: true,
		SortOrder: sortOrder, CreatedAt: scene.CreatedAt, UpdatedAt: scene.UpdatedAt,
	}
	if previous != nil {
		record.GroupID, record.SortOrder = previous.GroupID, previous.SortOrder
		record.Settings, record.Store = cloneMap(previous.Settings), cloneMap(previous.Store)
		record.CreatedAt = previous.CreatedAt
		if record.CreatedAt == "" {
			record.CreatedAt = scene.CreatedAt
		}
	}
	return record
}

// sceneDeviceState is the live state of the synthetic device. A button has no
// value to report: it is pressed, not read.
func sceneDeviceState(scene Scene) map[string]any {
	if scene.Momentary() {
		return map[string]any{}
	}
	return map[string]any{"onoff": scene.Active}
}

func indexOfDeviceRecord(devices []deviceRecord, id string) int {
	for index, device := range devices {
		if device.ID == id {
			return index
		}
	}
	return -1
}

// reconcileSceneDevices migrates documents written before Scenes had their
// synthetic Device records. It also keeps existing grouping and ordering while
// repairing the fields owned by the Scene implementation.
func reconcileSceneDevices(document *document) error {
	if document == nil {
		return nil
	}
	scenes := make(map[string]Scene, len(document.Scenes))
	for _, scene := range document.Scenes {
		scenes[scene.ID] = scene
	}
	existing := make(map[string]deviceRecord, len(document.Scenes))
	devices := make([]deviceRecord, 0, len(document.Devices)+len(document.Scenes))
	occupied := make(map[string]string, len(document.Devices))
	rootOrderFound, rootOrderUnordered, highestRootOrder := false, false, 0
	for _, record := range document.Devices {
		if record.GroupID == "" {
			rootOrderFound = true
			if record.SortOrder == 0 {
				rootOrderUnordered = true
			} else if record.SortOrder > highestRootOrder {
				highestRootOrder = record.SortOrder
			}
		}
		if record.AppID != NativeSceneAppID {
			devices = append(devices, record)
			occupied[record.ID] = record.AppID
			continue
		}
		sceneID, _ := record.Data["sceneId"].(string)
		if sceneID == "" {
			sceneID, _ = SceneIDFromDeviceID(record.ID)
		}
		if _, found := scenes[sceneID]; found {
			if _, duplicate := existing[sceneID]; !duplicate {
				existing[sceneID] = record
			}
		}
	}
	nextRootOrder := 0
	if rootOrderFound && !rootOrderUnordered {
		nextRootOrder = highestRootOrder + 10
	}
	for _, scene := range document.Scenes {
		deviceID := SceneDeviceID(scene.ID)
		if appID, collision := occupied[deviceID]; collision {
			return fmt.Errorf("scene %q device id collides with app %q", scene.ID, appID)
		}
		if record, found := existing[scene.ID]; found {
			devices = append(devices, newSceneDeviceRecord(scene, &record, nextRootOrder))
			continue
		}
		devices = append(devices, newSceneDeviceRecord(scene, nil, nextRootOrder))
		if nextRootOrder > 0 {
			nextRootOrder += 10
		}
	}
	document.Devices = devices
	return nil
}

// seedSceneDeviceStates restores the only live state that is intentionally
// durable: whether a Scene currently owns a restore snapshot.
func (s *Store) seedSceneDeviceStates() {
	for _, scene := range s.doc.Scenes {
		s.state[SceneDeviceID(scene.ID)] = sceneDeviceState(scene)
	}
}

// validateSceneDeviceReferencesLocked makes Flow writes participate in the
// same lock ordering as DeleteScene. A physical device whose id happens to use
// the scene prefix stays a normal device; only native records and scene-delete
// tombstones carry Scene semantics.
func (s *Store) validateSceneDeviceReferencesLocked(flow Flow) error {
	references := make(map[string]struct{})
	for _, step := range flow.Steps() {
		collectDeviceReferences(step.Args, references)
		collectDeviceReferences(step.State, references)
	}
	for deviceID := range references {
		deviceIndex := indexOfDeviceRecord(s.doc.Devices, deviceID)
		if deviceIndex >= 0 {
			record := s.doc.Devices[deviceIndex]
			if record.AppID != NativeSceneAppID {
				continue
			}
			sceneID, _ := record.Data["sceneId"].(string)
			if sceneID == "" {
				sceneID, _ = SceneIDFromDeviceID(record.ID)
			}
			if indexOfScene(s.doc.Scenes, sceneID) >= 0 {
				continue
			}
			return fmt.Errorf("flow %q references missing scene %q", flow.ID, sceneID)
		}
		if _, wasScene := s.deletedSceneDevices[deviceID]; wasScene {
			return fmt.Errorf("flow %q references deleted scene device %q", flow.ID, deviceID)
		}
	}
	return nil
}

func collectDeviceReferences(value any, references map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		if id, _ := typed["$device"].(string); id != "" {
			references[id] = struct{}{}
		}
		for _, child := range typed {
			collectDeviceReferences(child, references)
		}
	case []any:
		for _, child := range typed {
			collectDeviceReferences(child, references)
		}
	}
}

func (s *Store) CreateScene(ctx context.Context, scene Scene) (Scene, error) {
	if err := ctx.Err(); err != nil {
		return Scene{}, err
	}
	// normalize edits state identifiers, so own the state slice before it runs.
	scene.States = append([]SceneState(nil), scene.States...)
	if err := scene.normalize(); err != nil {
		return Scene{}, err
	}
	// Active and Previous are runtime-owned. A create request cannot forge an
	// already-running restore session.
	scene.Active, scene.Previous = false, nil
	scene = cloneScene(scene)
	if scene.ID == "" {
		scene.ID = newID()
	}
	now := nowRFC3339()
	scene.CreatedAt, scene.UpdatedAt, scene.Revision = now, now, 1

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Scene{}, err
	}
	if indexOfScene(s.doc.Scenes, scene.ID) >= 0 {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q already exists", scene.ID)
	}
	deviceID := SceneDeviceID(scene.ID)
	if indexOfDeviceRecord(s.doc.Devices, deviceID) >= 0 {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q device id %q already exists", scene.ID, deviceID)
	}
	scenes := make([]Scene, len(s.doc.Scenes)+1)
	copy(scenes, s.doc.Scenes)
	scenes[len(s.doc.Scenes)] = cloneScene(scene)
	record := newSceneDeviceRecord(scene, nil, s.nextDeviceOrderLocked("", ""))
	devices := append([]deviceRecord(nil), s.doc.Devices...)
	devices = append(devices, record)
	err := s.commitScenesLocked(scenes, devices)
	var device Device
	if err == nil {
		s.state[deviceID] = sceneDeviceState(scene)
		delete(s.deletedSceneDevices, deviceID)
		device = record.device(cloneMap(s.state[deviceID]))
	}
	s.mu.Unlock()
	if err != nil {
		return Scene{}, fmt.Errorf("create scene: %w", err)
	}
	s.publish(Event{Manager: "scene", Type: "scene.create", ID: scene.ID, Data: cloneScene(scene)})
	s.publish(Event{Manager: "devices", Type: "device.create", ID: deviceID, Data: device})
	return scene, nil
}

// UpdateScene replaces a Scene only if its supplied Revision still matches the
// stored one. The comparison and persistence share the same write lock.
func (s *Store) UpdateScene(ctx context.Context, scene Scene) (Scene, error) {
	if err := ctx.Err(); err != nil {
		return Scene{}, err
	}
	scene.ID = strings.TrimSpace(scene.ID)
	if scene.ID == "" {
		return Scene{}, errors.New("scene id is required")
	}
	scene.States = append([]SceneState(nil), scene.States...)
	// Only BeginScene and SetScenePrevious may change runtime state.
	scene.Active, scene.Previous = false, nil

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Scene{}, err
	}
	index := indexOfScene(s.doc.Scenes, scene.ID)
	if index < 0 {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q does not exist", scene.ID)
	}
	existing := s.doc.Scenes[index]
	if existing.Active {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q: %w", scene.ID, ErrSceneActive)
	}
	if err := scene.normalize(); err != nil {
		s.mu.Unlock()
		return Scene{}, err
	}
	scene = cloneScene(scene)
	if scene.Revision != existing.Revision {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q: %w", scene.ID, ErrSceneChanged)
	}
	scene.CreatedAt = existing.CreatedAt
	scene.UpdatedAt, scene.Revision = nowRFC3339(), existing.Revision+1
	scenes := append([]Scene(nil), s.doc.Scenes...)
	scenes[index] = cloneScene(scene)
	devices := append([]deviceRecord(nil), s.doc.Devices...)
	deviceID := SceneDeviceID(scene.ID)
	deviceIndex := indexOfDeviceRecord(devices, deviceID)
	var previous *deviceRecord
	if deviceIndex >= 0 {
		previous = &devices[deviceIndex]
	}
	record := newSceneDeviceRecord(scene, previous, s.nextDeviceOrderLocked("", ""))
	if deviceIndex >= 0 {
		devices[deviceIndex] = record
	} else {
		devices = append(devices, record)
	}
	err := s.commitScenesLocked(scenes, devices)
	var device Device
	if err == nil {
		s.state[deviceID] = sceneDeviceState(scene)
		device = record.device(cloneMap(s.state[deviceID]))
	}
	s.mu.Unlock()
	if err != nil {
		return Scene{}, fmt.Errorf("update scene %q: %w", scene.ID, err)
	}
	s.publish(Event{Manager: "scene", Type: "scene.update", ID: scene.ID, Data: cloneScene(scene)})
	s.publish(Event{Manager: "devices", Type: "device.update", ID: deviceID, Data: device})
	return scene, nil
}

func (s *Store) Scene(ctx context.Context, id string) (Scene, error) {
	if err := ctx.Err(); err != nil {
		return Scene{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if index := indexOfScene(s.doc.Scenes, id); index >= 0 {
		return cloneScene(s.doc.Scenes[index]), nil
	}
	return Scene{}, fmt.Errorf("scene %q does not exist", id)
}

func (s *Store) Scenes(ctx context.Context) ([]Scene, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	var scenes []Scene
	if s.doc.Scenes != nil {
		scenes = make([]Scene, len(s.doc.Scenes))
		for index := range s.doc.Scenes {
			scenes[index] = cloneScene(s.doc.Scenes[index])
		}
	}
	s.mu.RUnlock()
	sort.SliceStable(scenes, func(left, right int) bool {
		if scenes[left].CreatedAt != scenes[right].CreatedAt {
			return scenes[left].CreatedAt < scenes[right].CreatedAt
		}
		return scenes[left].ID < scenes[right].ID
	})
	return scenes, nil
}

func (s *Store) DeleteScene(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	index := indexOfScene(s.doc.Scenes, id)
	if index < 0 {
		s.mu.Unlock()
		return fmt.Errorf("scene %q does not exist", id)
	}
	if s.doc.Scenes[index].Active {
		s.mu.Unlock()
		return fmt.Errorf("scene %q: %w", id, ErrSceneActive)
	}
	deviceID := SceneDeviceID(id)
	removed := map[string]struct{}{deviceID: {}}
	for _, flow := range s.doc.Flows {
		for _, step := range flow.Steps() {
			if referencesRemovedDevice(step.Args, removed) || referencesRemovedDevice(step.State, removed) {
				s.mu.Unlock()
				return fmt.Errorf("scene %q is used by flow %q: %w", id, flow.ID, ErrSceneInUse)
			}
		}
	}
	scenes := removeWhere(s.doc.Scenes, func(scene Scene) bool { return scene.ID == id })
	devices := removeWhere(s.doc.Devices, func(record deviceRecord) bool { return record.ID == deviceID })
	err := s.commitScenesLocked(scenes, devices)
	if err == nil {
		delete(s.state, deviceID)
		s.deletedSceneDevices[deviceID] = struct{}{}
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("delete scene %q: %w", id, err)
	}
	s.publish(Event{Manager: "scene", Type: "scene.delete", ID: id})
	s.publish(Event{Manager: "devices", Type: "device.delete", ID: deviceID})
	return nil
}

// BeginScene atomically claims an inactive Scene and records the values needed
// to turn it off again. Repeated or concurrent begins return the first restore
// snapshot without overwriting it.
func (s *Store) BeginScene(ctx context.Context, id string, previous []SceneState) (Scene, bool, error) {
	if err := ctx.Err(); err != nil {
		return Scene{}, false, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Scene{}, false, errors.New("scene id is required")
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Scene{}, false, err
	}
	index := indexOfScene(s.doc.Scenes, id)
	if index < 0 {
		s.mu.Unlock()
		return Scene{}, false, fmt.Errorf("scene %q does not exist", id)
	}
	if s.doc.Scenes[index].Momentary() {
		s.mu.Unlock()
		return Scene{}, false, fmt.Errorf("scene %q: %w", id, ErrSceneMomentary)
	}
	if s.doc.Scenes[index].Active {
		definition := cloneScene(s.doc.Scenes[index])
		s.mu.Unlock()
		return definition, false, nil
	}
	previous = append([]SceneState(nil), previous...)
	if err := normalizeSceneStates(previous); err != nil {
		s.mu.Unlock()
		return Scene{}, false, fmt.Errorf("scene previous states: %w", err)
	}
	previous = cloneSceneStates(previous)
	scenes := append([]Scene(nil), s.doc.Scenes...)
	scenes[index] = cloneScene(scenes[index])
	scenes[index].Active = true
	scenes[index].Previous = cloneSceneStates(previous)
	definition := cloneScene(scenes[index])
	deviceID := SceneDeviceID(id)
	devices := append([]deviceRecord(nil), s.doc.Devices...)
	deviceIndex := indexOfDeviceRecord(devices, deviceID)
	var record deviceRecord
	if deviceIndex >= 0 {
		record = devices[deviceIndex]
	} else {
		record = newSceneDeviceRecord(definition, nil, s.nextDeviceOrderLocked("", ""))
		devices = append(devices, record)
	}
	err := s.commitScenesLocked(scenes, devices)
	var device Device
	if err == nil {
		s.state[deviceID] = sceneDeviceState(definition)
		device = record.device(cloneMap(s.state[deviceID]))
	}
	s.mu.Unlock()
	if err != nil {
		return Scene{}, false, fmt.Errorf("begin scene %q: %w", id, err)
	}
	s.publish(Event{Manager: "scene", Type: "scene.state", ID: id, Data: cloneScene(definition)})
	s.publish(Event{Manager: "devices", Type: "device.update", ID: deviceID, Data: device})
	return definition, true, nil
}

// SetScenePrevious checkpoints the restore work still outstanding. Once no
// states remain, the Scene is inactive and may be edited or deleted again.
func (s *Store) SetScenePrevious(ctx context.Context, id string, remaining []SceneState) (Scene, error) {
	if err := ctx.Err(); err != nil {
		return Scene{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Scene{}, errors.New("scene id is required")
	}
	remaining = append([]SceneState(nil), remaining...)
	if err := normalizeSceneStates(remaining); err != nil {
		return Scene{}, fmt.Errorf("scene previous states: %w", err)
	}
	remaining = cloneSceneStates(remaining)

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Scene{}, err
	}
	index := indexOfScene(s.doc.Scenes, id)
	if index < 0 {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q does not exist", id)
	}
	if s.doc.Scenes[index].Momentary() && len(remaining) > 0 {
		s.mu.Unlock()
		return Scene{}, fmt.Errorf("scene %q: %w", id, ErrSceneMomentary)
	}
	scenes := append([]Scene(nil), s.doc.Scenes...)
	scenes[index] = cloneScene(scenes[index])
	scenes[index].Previous = cloneSceneStates(remaining)
	scenes[index].Active = len(remaining) > 0
	definition := cloneScene(scenes[index])
	deviceID := SceneDeviceID(id)
	devices := append([]deviceRecord(nil), s.doc.Devices...)
	deviceIndex := indexOfDeviceRecord(devices, deviceID)
	var record deviceRecord
	if deviceIndex >= 0 {
		record = devices[deviceIndex]
	} else {
		record = newSceneDeviceRecord(definition, nil, s.nextDeviceOrderLocked("", ""))
		devices = append(devices, record)
	}
	err := s.commitScenesLocked(scenes, devices)
	var device Device
	if err == nil {
		s.state[deviceID] = sceneDeviceState(definition)
		device = record.device(cloneMap(s.state[deviceID]))
	}
	s.mu.Unlock()
	if err != nil {
		return Scene{}, fmt.Errorf("set scene %q previous states: %w", id, err)
	}
	s.publish(Event{Manager: "scene", Type: "scene.state", ID: id, Data: cloneScene(definition)})
	s.publish(Event{Manager: "devices", Type: "device.update", ID: deviceID, Data: device})
	return definition, nil
}

// commitScenesLocked persists a copy-on-write document and only publishes the
// candidate in memory after the write succeeds. Callers hold s.mu for writing.
func (s *Store) commitScenesLocked(scenes []Scene, devices []deviceRecord) error {
	candidate := *s.doc
	candidate.Scenes = scenes
	candidate.Devices = devices
	// saveDocument sorts and bounds notifications, so keep that mutation inside
	// the candidate as well.
	if s.doc.Notifications != nil {
		candidate.Notifications = append([]Notification(nil), s.doc.Notifications...)
	}
	if s.path != InMemoryPath {
		if err := saveDocument(s.path, &candidate); err != nil {
			return err
		}
	}
	s.doc = &candidate
	return nil
}

func indexOfScene(scenes []Scene, id string) int {
	for index, scene := range scenes {
		if scene.ID == id {
			return index
		}
	}
	return -1
}
