// Package scene activates and restores persisted desired device states.
package scene

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/xinix00/stulp/internal/store"
)

// StateResult describes the outcome of logically processing one scene state.
// A state whose current value is unavailable is a failed attempt without an
// invocation: changing it would create a state that cannot safely be restored.
type StateResult struct {
	DeviceID     string `json:"deviceId"`
	CapabilityID string `json:"capabilityId"`
	Value        any    `json:"value"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
}

// ActivationResult is the complete, ordered account of changing a scene's
// on/off state. Active is the state that was durably retained by the store, not
// an optimistic reflection of what was requested.
type ActivationResult struct {
	SceneID     string        `json:"sceneId"`
	SceneName   string        `json:"sceneName"`
	RequestedOn bool          `json:"requestedOn"`
	Active      bool          `json:"active"`
	Success     bool          `json:"success"`
	Attempted   int           `json:"attempted"`
	Succeeded   int           `json:"succeeded"`
	Failed      int           `json:"failed"`
	States      []StateResult `json:"states"`
}

// Activator applies scenes through the same capability invocation boundary as
// direct controls and Flow actions.
type Activator struct {
	database *store.Store
	invoke   func(context.Context, string, string, any, map[string]any) error
}

// New creates a scene activator. The invoker is deliberately supplied by the
// caller: the web layer owns the route to plugin and native Matter devices.
func New(database *store.Store, invoker func(context.Context, string, string, any, map[string]any) error) *Activator {
	return &Activator{database: database, invoke: invoker}
}

type statePlan struct {
	state        store.SceneState
	preflightErr error
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

// Set turns a scene on or off.
//
// The first ON snapshots every known current value before issuing a command.
// Only successfully changed states remain in that persisted restore set. A
// repeated ON uses the existing restore set and can therefore never overwrite
// the original baseline. OFF applies that baseline best-effort and retains
// failed or unattempted states, making another OFF a safe retry.
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
	if on {
		return a.setOn(ctx, definition, result)
	}
	return a.setOff(ctx, definition, result)
}

func (a *Activator) setOn(ctx context.Context, definition store.Scene, result ActivationResult) (ActivationResult, error) {
	// Always prepare a candidate baseline. BeginScene ignores it when another
	// caller already made the scene active and returns that caller's persisted
	// baseline instead. This closes the race between the initial read and begin.
	previous, snapshotErrors, err := a.snapshot(ctx, definition)
	if err != nil {
		return result, err
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

	plans := make([]statePlan, len(definition.Previous))
	for index, previous := range definition.Previous {
		plans[index].state = previous
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

// snapshot returns the candidate restore set and a reason for every desired
// state it had to omit. Presence plus a non-nil value is the boundary between a
// known false/zero/empty state and an unavailable reading.
func (a *Activator) snapshot(ctx context.Context, definition store.Scene) ([]store.SceneState, map[sceneStateKey]error, error) {
	previous := make([]store.SceneState, 0, len(definition.States))
	stateErrors := make(map[sceneStateKey]error)
	devices := make(map[string]store.Device)
	deviceErrors := make(map[string]error)
	for _, desired := range definition.States {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		device, loaded := devices[desired.DeviceID]
		deviceErr, failed := deviceErrors[desired.DeviceID]
		if !loaded && !failed {
			device, deviceErr = a.database.Device(ctx, desired.DeviceID)
			if deviceErr != nil {
				deviceErrors[desired.DeviceID] = deviceErr
			} else {
				devices[desired.DeviceID] = device
			}
		}
		key := stateKey(desired)
		if deviceErr != nil {
			stateErrors[key] = fmt.Errorf("read current value for %s/%s: %w",
				desired.DeviceID, desired.CapabilityID, deviceErr)
			continue
		}
		value, exists := device.State[desired.CapabilityID]
		if !exists || value == nil {
			stateErrors[key] = fmt.Errorf("current value for %s/%s is unavailable",
				desired.DeviceID, desired.CapabilityID)
			continue
		}
		previous = append(previous, store.SceneState{
			DeviceID: desired.DeviceID, CapabilityID: desired.CapabilityID, Value: value,
		})
	}
	return previous, stateErrors, nil
}

// apply runs one sequential worker per device. Each worker owns disjoint
// outcome slots, so the final scene-order projection needs no synchronization.
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
		byDevice[plan.state.DeviceID] = append(byDevice[plan.state.DeviceID], index)
	}

	var workers sync.WaitGroup
	for _, indices := range byDevice {
		indices := indices
		workers.Add(1)
		go func() {
			defer workers.Done()
			for _, index := range indices {
				if ctx.Err() != nil {
					return
				}
				outcome := &outcomes[index]
				outcome.attempted = true
				if a.invoke == nil {
					outcome.err = errors.New("scene activator needs a capability invoker")
				} else {
					state := plans[index].state
					outcome.err = a.invoke(ctx, state.DeviceID, state.CapabilityID, state.Value, map[string]any{})
				}
				outcome.result.Success = outcome.err == nil
				if outcome.err != nil {
					outcome.result.Error = outcome.err.Error()
				}
			}
		}()
	}
	workers.Wait()
	return outcomes
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
