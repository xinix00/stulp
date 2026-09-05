// Package scene activates and restores persisted desired device states.
package scene

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/xinix00/stulp/internal/store"
)

// StateResult describes the outcome of logically processing one scene state.
// A state whose current value is unavailable is a failed attempt without an
// invocation: changing it would create a state that cannot safely be restored.
// A restore whose device already reports the value is a successful attempt
// without an invocation, marked Unchanged.
type StateResult struct {
	DeviceID     string `json:"deviceId"`
	CapabilityID string `json:"capabilityId"`
	Value        any    `json:"value"`
	Success      bool   `json:"success"`
	Unchanged    bool   `json:"unchanged,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ActivationResult is the complete, ordered account of changing a scene's
// on/off state. Active is the state that was durably retained by the store, not
// an optimistic reflection of what was requested. A Momentary scene is pressed
// rather than switched, so its Active is always false.
type ActivationResult struct {
	SceneID     string        `json:"sceneId"`
	SceneName   string        `json:"sceneName"`
	RequestedOn bool          `json:"requestedOn"`
	Momentary   bool          `json:"momentary,omitempty"`
	Active      bool          `json:"active"`
	Success     bool          `json:"success"`
	Attempted   int           `json:"attempted"`
	Succeeded   int           `json:"succeeded"`
	Failed      int           `json:"failed"`
	States      []StateResult `json:"states"`
}

// Command is one capability value for a device.
type Command struct {
	CapabilityID string
	Value        any
}

// Invoker delivers every command for one device in a single call, so an app
// can combine what its device combines: a lamp on and at that level and in
// that colour is one message to a Matter or Hue light, not four. The result
// has one entry per command, in order: nil for success, the error otherwise,
// or ErrNotAttempted for a command the route never sent. An error for the
// call as a whole fails every command.
type Invoker func(ctx context.Context, deviceID string, commands []Command) ([]error, error)

// ErrNotAttempted marks a command that was never handed to the device, so a
// retry can still pick it up. A route that stops at a cancelled context
// reports the rest this way.
var ErrNotAttempted = errors.New("command was not attempted")

// PerCapability adapts a route that takes one capability at a time. Commands
// go out in the given order; once the context is cancelled the rest is
// reported as not attempted.
func PerCapability(single func(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) error) Invoker {
	return func(ctx context.Context, deviceID string, commands []Command) ([]error, error) {
		results := make([]error, len(commands))
		for index, command := range commands {
			if ctx.Err() != nil {
				results[index] = ErrNotAttempted
				continue
			}
			results[index] = single(ctx, deviceID, command.CapabilityID, command.Value, map[string]any{})
		}
		return results, nil
	}
}

// Activator applies scenes through the same capability invocation boundary as
// direct controls and Flow actions.
type Activator struct {
	database *store.Store
	invoke   Invoker
}

// New creates a scene activator. The invoker is deliberately supplied by the
// caller: the web layer owns the route to plugin and native Matter devices.
func New(database *store.Store, invoker Invoker) *Activator {
	return &Activator{database: database, invoke: invoker}
}

type statePlan struct {
	state        store.SceneState
	preflightErr error
	// unchanged marks a restore the device already satisfies. It counts as
	// done without a command: a device that just went to sleep after its
	// power-off would otherwise receive a burst of no-op writes it ignores.
	unchanged bool
}

type stateOutcome struct {
	result    StateResult
	attempted bool
	err       error
}

type sceneStateKey struct {
	deviceID     string
	capabilityID string
}

// Activate retains the original one-way API as shorthand for turning a scene
// on. Call Set when the requested direction is known.
func (a *Activator) Activate(ctx context.Context, id string) (ActivationResult, error) {
	return a.Set(ctx, id, true)
}

// Set turns a scene on or off, or presses a button scene.
//
// The first ON snapshots every known current value before issuing a command.
// Only successfully changed states remain in that persisted restore set. A
// repeated ON uses the existing restore set and can therefore never overwrite
// the original baseline. OFF applies that baseline best-effort and retains
// failed or unattempted states, making another OFF a safe retry. In both
// directions a value the device already reports is not sent again, within the
// tolerance a radio's steps and float rounding leave. A button scene takes no
// snapshot: ON applies its states and OFF is refused, because there is nothing
// to put back.
func (a *Activator) Set(ctx context.Context, id string, on bool) (ActivationResult, error) {
	id = strings.TrimSpace(id)
	result := ActivationResult{SceneID: id, RequestedOn: on, States: []StateResult{}}
	if a == nil || a.database == nil {
		return result, errors.New("scene activator needs a store")
	}
	if err := ctx.Err(); err != nil {
		return a.persistedResult(result), err
	}

	release, err := acquire(ctx, a.database, id)
	if err != nil {
		return a.persistedResult(result), err
	}
	defer release()

	definition, err := a.database.Scene(ctx, id)
	if err != nil {
		// Cancellation can race with receiving the lock token: acquire may win
		// the select, after which Scene correctly rejects the canceled context.
		// Still report the durable runtime state promised by Active.
		return a.persistedResult(result), err
	}
	result.SceneID, result.SceneName, result.Active = definition.ID, definition.Name, definition.Active
	if definition.Momentary() {
		result.Momentary = true
		if !on {
			return result, fmt.Errorf("scene %q is a button: write true to press it, it cannot be turned off", definition.Name)
		}
		return a.press(ctx, definition, result)
	}
	if on {
		return a.setOn(ctx, definition, result)
	}
	return a.setOff(ctx, definition, result)
}

// press applies a button scene. Nothing is remembered, so no current value has
// to be readable first and the store is not involved beyond the definition. A
// value the device already reports is still skipped; one it cannot read is
// simply sent.
func (a *Activator) press(ctx context.Context, definition store.Scene, result ActivationResult) (ActivationResult, error) {
	current, _, err := a.currentValues(ctx, definition.States)
	if err != nil {
		return result, err
	}
	plans := make([]statePlan, len(definition.States))
	for index, desired := range definition.States {
		plans[index].state = desired
		if value, known := current[stateKey(desired)]; known && sameValue(value, desired.Value) {
			plans[index].unchanged = true
		}
	}
	outcomes := a.apply(ctx, plans)
	result, failures := collectResults(result, outcomes)
	operationErrors := append([]error(nil), failures...)
	if err := ctx.Err(); err != nil {
		operationErrors = append(operationErrors, err)
	}
	result.Active = false
	result.Success = result.Attempted == len(plans) && result.Failed == 0 && len(operationErrors) == 0
	return finish(result, operationErrors)
}

func (a *Activator) setOn(ctx context.Context, definition store.Scene, result ActivationResult) (ActivationResult, error) {
	// Always prepare a candidate baseline. BeginScene ignores it when another
	// caller already made the scene active and returns that caller's persisted
	// baseline instead. This closes the race between the initial read and begin.
	current, snapshotErrors, err := a.currentValues(ctx, definition.States)
	if err != nil {
		return result, err
	}
	previous := make([]store.SceneState, 0, len(current))
	for _, desired := range definition.States {
		if value, known := current[stateKey(desired)]; known {
			previous = append(previous, store.SceneState{
				DeviceID: desired.DeviceID, CapabilityID: desired.CapabilityID, Value: value,
			})
		}
	}
	active, first, err := a.database.BeginScene(ctx, definition.ID, previous)
	if err != nil {
		result.Active = definition.Active
		return result, fmt.Errorf("begin scene %q: %w", definition.Name, err)
	}
	result.SceneID, result.SceneName, result.Active = active.ID, active.Name, active.Active

	baseline := indexStates(active.Previous)
	plans := make([]statePlan, len(active.States))
	for index, desired := range active.States {
		plans[index].state = desired
		key := stateKey(desired)
		// Already there is done, without a command -- on the way in as on the
		// way back. The state stays in the restore set: from here on the scene
		// owns that value, and a baseline equal to its target restores as
		// unchanged too. The comparison is against what the device reports
		// now, not against the baseline: on a repeated ON those differ.
		if value, known := current[key]; known && sameValue(value, desired.Value) {
			plans[index].unchanged = true
			continue
		}
		if _, known := baseline[key]; known {
			continue
		}
		if first && snapshotErrors[key] != nil {
			plans[index].preflightErr = snapshotErrors[key]
		} else {
			plans[index].preflightErr = fmt.Errorf(
				"no restore value is stored for %s/%s", desired.DeviceID, desired.CapabilityID)
		}
	}

	outcomes := a.apply(ctx, plans)
	result, failures := collectResults(result, outcomes)
	operationErrors := append([]error(nil), failures...)

	if first {
		successfulPrevious := make([]store.SceneState, 0, result.Succeeded)
		for index, outcome := range outcomes {
			if !outcome.attempted || outcome.err != nil {
				continue
			}
			if before, exists := baseline[stateKey(active.States[index])]; exists {
				successfulPrevious = append(successfulPrevious, before)
			}
		}
		stored, persistErr := a.database.SetScenePrevious(context.WithoutCancel(ctx), active.ID, successfulPrevious)
		if persistErr != nil {
			operationErrors = append(operationErrors, fmt.Errorf("retain scene restore state: %w", persistErr))
			result.Active = a.persistedActive(active.ID, active.Active)
		} else {
			result.Active = stored.Active
		}
	}

	if err := ctx.Err(); err != nil {
		operationErrors = append(operationErrors, err)
	}
	result.Success = result.Active && result.Attempted == len(plans) && result.Failed == 0 && len(operationErrors) == 0
	return finish(result, operationErrors)
}

func (a *Activator) setOff(ctx context.Context, definition store.Scene, result ActivationResult) (ActivationResult, error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !definition.Active {
		result.Active, result.Success = false, true
		return result, nil
	}

	// A value the device already reports is not sent again. An unreadable
	// current value is no reason to skip: then the restore is simply sent.
	current, _, err := a.currentValues(ctx, definition.Previous)
	if err != nil {
		return result, err
	}
	plans := make([]statePlan, len(definition.Previous))
	for index, previous := range definition.Previous {
		plans[index].state = previous
		if value, known := current[stateKey(previous)]; known && sameValue(value, previous.Value) {
			plans[index].unchanged = true
		}
	}
	outcomes := a.apply(ctx, plans)
	result, failures := collectResults(result, outcomes)
	operationErrors := append([]error(nil), failures...)

	remaining := make([]store.SceneState, 0, len(definition.Previous)-result.Succeeded)
	for index, outcome := range outcomes {
		if !outcome.attempted || outcome.err != nil {
			remaining = append(remaining, definition.Previous[index])
		}
	}
	stored, persistErr := a.database.SetScenePrevious(context.WithoutCancel(ctx), definition.ID, remaining)
	if persistErr != nil {
		operationErrors = append(operationErrors, fmt.Errorf("retain unrestored scene state: %w", persistErr))
		result.Active = a.persistedActive(definition.ID, definition.Active)
	} else {
		result.Active = stored.Active
	}
	if err := ctx.Err(); err != nil {
		operationErrors = append(operationErrors, err)
	}
	result.Success = !result.Active && result.Attempted == len(plans) && result.Failed == 0 && len(operationErrors) == 0
	return finish(result, operationErrors)
}

// currentValues reads each device once and returns the value it reports for
// every requested state, plus a reason for every state it could not read.
// Presence plus a non-nil value is the boundary between a known
// false/zero/empty state and an unavailable reading.
func (a *Activator) currentValues(ctx context.Context, states []store.SceneState) (map[sceneStateKey]any, map[sceneStateKey]error, error) {
	values := make(map[sceneStateKey]any, len(states))
	stateErrors := make(map[sceneStateKey]error)
	devices := make(map[string]store.Device)
	deviceErrors := make(map[string]error)
	for _, wanted := range states {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		device, loaded := devices[wanted.DeviceID]
		deviceErr, failed := deviceErrors[wanted.DeviceID]
		if !loaded && !failed {
			device, deviceErr = a.database.Device(ctx, wanted.DeviceID)
			if deviceErr != nil {
				deviceErrors[wanted.DeviceID] = deviceErr
			} else {
				devices[wanted.DeviceID] = device
			}
		}
		key := stateKey(wanted)
		if deviceErr != nil {
			stateErrors[key] = fmt.Errorf("read current value for %s/%s: %w",
				wanted.DeviceID, wanted.CapabilityID, deviceErr)
			continue
		}
		value, exists := device.State[wanted.CapabilityID]
		if !exists || value == nil {
			stateErrors[key] = fmt.Errorf("current value for %s/%s is unavailable",
				wanted.DeviceID, wanted.CapabilityID)
			continue
		}
		values[key] = value
	}
	return values, stateErrors, nil
}

// apply gives every device its commands in one call and runs the devices in
// parallel. Each worker owns disjoint outcome slots, so the final scene-order
// projection needs no synchronization.
func (a *Activator) apply(ctx context.Context, plans []statePlan) []stateOutcome {
	outcomes := make([]stateOutcome, len(plans))
	byDevice := make(map[string][]int)
	for index, plan := range plans {
		outcomes[index].result = StateResult{
			DeviceID: plan.state.DeviceID, CapabilityID: plan.state.CapabilityID, Value: plan.state.Value,
		}
		if plan.preflightErr != nil {
			outcomes[index].attempted = true
			outcomes[index].err = plan.preflightErr
			outcomes[index].result.Error = plan.preflightErr.Error()
			continue
		}
		if plan.unchanged {
			outcomes[index].attempted = true
			outcomes[index].result.Success = true
			outcomes[index].result.Unchanged = true
			continue
		}
		byDevice[plan.state.DeviceID] = append(byDevice[plan.state.DeviceID], index)
	}
	for _, indices := range byDevice {
		// A Scene's persisted order reflects how its states were selected, not
		// necessarily the safe command order for a powered device. Turn a device
		// on before changing values such as dim, and turn it off only after those
		// values have been restored. Stable sorting preserves the user's order for
		// every other capability and the outcomes remain indexed in Scene order.
		sort.SliceStable(indices, func(left, right int) bool {
			return sceneStateApplyPriority(plans[indices[left]].state) <
				sceneStateApplyPriority(plans[indices[right]].state)
		})
	}

	var workers sync.WaitGroup
	for deviceID, indices := range byDevice {
		deviceID, indices := deviceID, indices
		workers.Add(1)
		go func() {
			defer workers.Done()
			if ctx.Err() != nil {
				return
			}
			commands := make([]Command, len(indices))
			for position, index := range indices {
				commands[position] = Command{CapabilityID: plans[index].state.CapabilityID, Value: plans[index].state.Value}
			}
			var results []error
			var err error
			switch {
			case a.invoke == nil:
				err = errors.New("scene activator needs a capability invoker")
			default:
				results, err = a.invoke(ctx, deviceID, commands)
				if err == nil && len(results) != len(commands) {
					err = fmt.Errorf("device %s answered %d of %d commands", deviceID, len(results), len(commands))
				}
			}
			for position, index := range indices {
				commandErr := err
				if commandErr == nil {
					commandErr = results[position]
				}
				if errors.Is(commandErr, ErrNotAttempted) {
					continue
				}
				outcome := &outcomes[index]
				outcome.attempted = true
				outcome.err = commandErr
				outcome.result.Success = commandErr == nil
				if commandErr != nil {
					outcome.result.Error = commandErr.Error()
				}
			}
		}()
	}
	workers.Wait()
	return outcomes
}

func sceneStateApplyPriority(state store.SceneState) int {
	base, _, _ := strings.Cut(state.CapabilityID, ".")
	if base != "onoff" {
		return 1
	}
	on, boolean := state.Value.(bool)
	if !boolean {
		return 1
	}
	if on {
		return 0
	}
	return 2
}

func collectResults(result ActivationResult, outcomes []stateOutcome) (ActivationResult, []error) {
	var failures []error
	for _, outcome := range outcomes {
		if !outcome.attempted {
			continue
		}
		result.Attempted++
		result.States = append(result.States, outcome.result)
		if outcome.err == nil {
			result.Succeeded++
			continue
		}
		result.Failed++
		failures = append(failures, fmt.Errorf("%s/%s: %w",
			outcome.result.DeviceID, outcome.result.CapabilityID, outcome.err))
	}
	if result.Failed > 0 {
		failures = append([]error{fmt.Errorf("set scene %q to %t: %d of %d processed states failed",
			result.SceneName, result.RequestedOn, result.Failed, result.Attempted)}, failures...)
	}
	return result, failures
}

func finish(result ActivationResult, operationErrors []error) (ActivationResult, error) {
	if result.Success {
		return result, nil
	}
	if len(operationErrors) == 0 {
		operationErrors = append(operationErrors, fmt.Errorf(
			"scene %q did not reach requested state %t", result.SceneName, result.RequestedOn))
	}
	return result, errors.Join(operationErrors...)
}

func indexStates(states []store.SceneState) map[sceneStateKey]store.SceneState {
	indexed := make(map[sceneStateKey]store.SceneState, len(states))
	for _, state := range states {
		indexed[stateKey(state)] = state
	}
	return indexed
}

func stateKey(state store.SceneState) sceneStateKey {
	return sceneStateKey{deviceID: state.DeviceID, capabilityID: state.CapabilityID}
}

func (a *Activator) persistedActive(id string, fallback bool) bool {
	definition, err := a.database.Scene(context.Background(), id)
	if err != nil {
		return fallback
	}
	return definition.Active
}

func (a *Activator) persistedResult(result ActivationResult) ActivationResult {
	definition, err := a.database.Scene(context.Background(), result.SceneID)
	if err == nil {
		result.SceneID, result.SceneName, result.Active = definition.ID, definition.Name, definition.Active
	}
	return result
}

type activationLockKey struct {
	database *store.Store
	sceneID  string
}

type activationLock struct {
	token chan struct{}
	refs  int
}

var activationLockRegistry = struct {
	sync.Mutex
	locks map[activationLockKey]*activationLock
}{locks: make(map[activationLockKey]*activationLock)}

// acquire serializes opposite requests for one Scene across every Activator
// that shares a Store. The web API and Flow engine may each hold an Activator;
// a local mutex would let their restore checkpoints race. Waiting remains
// context-aware, and the reference count keeps this registry bounded.
func acquire(ctx context.Context, database *store.Store, id string) (func(), error) {
	key := activationLockKey{database: database, sceneID: id}
	activationLockRegistry.Lock()
	lock := activationLockRegistry.locks[key]
	if lock == nil {
		lock = &activationLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		activationLockRegistry.locks[key] = lock
	}
	lock.refs++
	activationLockRegistry.Unlock()

	dropReference := func() {
		activationLockRegistry.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(activationLockRegistry.locks, key)
		}
		activationLockRegistry.Unlock()
	}
	select {
	case <-lock.token:
		return func() {
			lock.token <- struct{}{}
			dropReference()
		}, nil
	case <-ctx.Done():
		dropReference()
		return nil, ctx.Err()
	}
}
