package webapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/stulphttp"
)

// Scenes are desired state, not miniature Flows. Each Scene owns a normal
// virtual onoff device: turning it on applies these declarations after taking
// a restore snapshot, and turning it off puts that snapshot back.
func (s *Server) handleScenes() {
	s.mux.HandleFunc("GET /api/stulp/scenes", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		scenes, err := s.store.Scenes(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.showScenes(stulphttp.Context(request), scenes))
	})
	s.mux.HandleFunc("POST /api/stulp/scenes", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var definition store.Scene
		if err := decodeJSON(request, &definition); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		canonical, err := s.canonicalScene(stulphttp.Context(request), definition, nil)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		created, err := s.store.CreateScene(stulphttp.Context(request), canonical)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusCreated, s.showScene(stulphttp.Context(request), created))
	})
	s.mux.HandleFunc("GET /api/stulp/scenes/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		definition, err := s.store.Scene(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.showScene(stulphttp.Context(request), definition))
	})
	s.mux.HandleFunc("PUT /api/stulp/scenes/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var definition store.Scene
		if err := decodeJSON(request, &definition); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		definition.ID = request.PathValue("id")
		previous, err := s.store.Scene(stulphttp.Context(request), definition.ID)
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		if previous.Active {
			writeError(response, stulphttp.StatusConflict, store.ErrSceneActive)
			return
		}
		canonical, err := s.canonicalScene(stulphttp.Context(request), definition, &previous)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		// A kind change swaps the device's capability from onoff to button or
		// back. A Flow card holding the old capability would silently stop
		// working, so it is refused the same way a delete is.
		if err := s.sceneKindChangeAllowed(stulphttp.Context(request), previous, canonical); err != nil {
			writeError(response, stulphttp.StatusConflict, err)
			return
		}
		updated, err := s.store.UpdateScene(stulphttp.Context(request), canonical)
		if err != nil {
			status := stulphttp.StatusBadRequest
			if errors.Is(err, store.ErrSceneChanged) || errors.Is(err, store.ErrSceneActive) {
				status = stulphttp.StatusConflict
			}
			writeError(response, status, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.showScene(stulphttp.Context(request), updated))
	})
	s.mux.HandleFunc("DELETE /api/stulp/scenes/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		id := request.PathValue("id")
		if err := s.deleteScene(stulphttp.Context(request), id); err != nil {
			status := stulphttp.StatusNotFound
			if errors.Is(err, store.ErrSceneActive) || errors.Is(err, store.ErrSceneInUse) {
				status = stulphttp.StatusConflict
			}
			writeError(response, status, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
}

func (s *Server) deleteScene(ctx context.Context, id string) error {
	definition, err := s.store.Scene(ctx, id)
	if err != nil {
		return err
	}
	usedBy, err := s.flowsUsingScene(ctx, id)
	if err != nil {
		return err
	}
	if len(usedBy) > 0 {
		names := make([]string, 0, len(usedBy))
		for _, flow := range usedBy {
			names = append(names, flow.Name)
		}
		return fmt.Errorf("%w: scene %q wordt gebruikt door %s; verwijder die apparaatstap eerst",
			store.ErrSceneInUse, definition.Name, strings.Join(names, ", "))
	}
	return s.store.DeleteScene(ctx, id)
}

func (s *Server) sceneKindChangeAllowed(ctx context.Context, previous, wanted store.Scene) error {
	if previous.CapabilityID() == wanted.CapabilityID() {
		return nil
	}
	usedBy, err := s.flowsUsingScene(ctx, previous.ID)
	if err != nil {
		return err
	}
	if len(usedBy) == 0 {
		return nil
	}
	names := make([]string, 0, len(usedBy))
	for _, flow := range usedBy {
		names = append(names, flow.Name)
	}
	return fmt.Errorf("%w: scene %q wordt gebruikt door %s; verwijder die apparaatstap eerst voordat je de soort wijzigt",
		store.ErrSceneInUse, previous.Name, strings.Join(names, ", "))
}

func (s *Server) flowsUsingScene(ctx context.Context, sceneID string) ([]store.Flow, error) {
	flows, err := s.store.Flows(ctx)
	if err != nil {
		return nil, err
	}
	used := make([]store.Flow, 0)
	deviceID := store.SceneDeviceID(sceneID)
	for _, definition := range flows {
		for _, step := range definition.Steps() {
			if referencesSceneDevice(step.Args, deviceID) || referencesSceneDevice(step.State, deviceID) {
				used = append(used, definition)
				break
			}
		}
	}
	return used, nil
}

func referencesSceneDevice(value any, deviceID string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if selected, _ := typed["$device"].(string); selected == deviceID {
			return true
		}
		for _, child := range typed {
			if referencesSceneDevice(child, deviceID) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if referencesSceneDevice(child, deviceID) {
				return true
			}
		}
	}
	return false
}

// canonicalScene validates against the live device metadata and changes values
// from the household's display units back to the canonical units apps use.
// The previous scene prevents rounding drift when a displayed value was not
// actually edited (Beaufort is intentionally not a perfectly reversible scale).
func (s *Server) canonicalScene(ctx context.Context, incoming store.Scene, previous *store.Scene) (store.Scene, error) {
	stored := make(map[string]store.SceneState)
	if previous != nil {
		for _, state := range previous.States {
			stored[state.DeviceID+"\x00"+state.CapabilityID] = state
		}
	}
	for index := range incoming.States {
		wanted := &incoming.States[index]
		device, err := s.store.Device(ctx, wanted.DeviceID)
		if err != nil {
			return store.Scene{}, fmt.Errorf("stand %d: %w", index+1, err)
		}
		if device.AppID == store.NativeSceneAppID {
			return store.Scene{}, fmt.Errorf("stand %d: een scene kan geen andere scene bedienen", index+1)
		}
		if !deviceHasCapability(device, wanted.CapabilityID) {
			return store.Scene{}, fmt.Errorf("stand %d: apparaat %q heeft geen capability %q", index+1, device.Name, wanted.CapabilityID)
		}
		definition := s.capabilityObject(device, wanted.CapabilityID, device.State[wanted.CapabilityID])
		setable, _ := definition["setable"].(bool)
		getable, _ := definition["getable"].(bool)
		if !setable {
			return store.Scene{}, fmt.Errorf("stand %d: %s van %s is alleen-lezen", index+1, capabilitySceneTitle(definition, wanted.CapabilityID), device.Name)
		}
		if !getable || statelessSceneCapability(wanted.CapabilityID) {
			return store.Scene{}, fmt.Errorf("stand %d: %s van %s is een actie en geen blijvende status", index+1, capabilitySceneTitle(definition, wanted.CapabilityID), device.Name)
		}
		if err := validateMCPCapabilityValue(definition, wanted.Value); err != nil {
			return store.Scene{}, fmt.Errorf("stand %d (%s van %s): %w", index+1, capabilitySceneTitle(definition, wanted.CapabilityID), device.Name, err)
		}

		key := wanted.DeviceID + "\x00" + wanted.CapabilityID
		beforeValue, hasBefore := device.State[wanted.CapabilityID]
		if before, exists := stored[key]; exists {
			beforeValue, hasBefore = before.Value, true
		}
		if hasBefore {
			incomingNumber, incomingIsNumber := numberOf(wanted.Value)
			shownBefore := s.capabilityObject(device, wanted.CapabilityID, beforeValue)["value"]
			beforeNumber, beforeIsNumber := numberOf(shownBefore)
			if incomingIsNumber && beforeIsNumber && nearly(incomingNumber, beforeNumber) {
				// Capturing a current value must keep the actual canonical reading.
				// In particular, 12 m/s displays as 6 Bft, whose inverse is 10.8;
				// blindly converting it back would make a newly captured scene drift.
				wanted.Value = beforeValue
				continue
			}
		}
		wanted.Value = s.canonicalCapabilityValue(ctx, device, wanted.CapabilityID, wanted.Value)
	}
	return incoming, nil
}

func (s *Server) showScenes(ctx context.Context, scenes []store.Scene) []store.Scene {
	shown := make([]store.Scene, 0, len(scenes))
	for _, definition := range scenes {
		shown = append(shown, s.showScene(ctx, definition))
	}
	return shown
}

func (s *Server) showScene(ctx context.Context, definition store.Scene) store.Scene {
	states := make([]store.SceneState, len(definition.States))
	copy(states, definition.States)
	for index := range states {
		device, err := s.store.Device(ctx, states[index].DeviceID)
		if err != nil || !deviceHasCapability(device, states[index].CapabilityID) {
			continue
		}
		states[index].Value = s.capabilityObject(device, states[index].CapabilityID, states[index].Value)["value"]
	}
	definition.States = states
	// Previous is an internal, canonical restore checkpoint. The UI only needs
	// the normal on/off state; exposing this list would mix canonical values into
	// an otherwise display-unit API and invite clients to edit runtime state.
	definition.Previous = nil
	return definition
}

func deviceHasCapability(device store.Device, capability string) bool {
	for _, id := range device.Capabilities {
		if id == capability {
			return true
		}
	}
	return false
}

func statelessSceneCapability(id string) bool {
	base, _, _ := strings.Cut(id, ".")
	return base == "button" || base == "speaker_prev" || base == "speaker_next"
}

func capabilitySceneTitle(definition map[string]any, fallback string) string {
	if title := localized(definition["title"], "nl"); title != "" {
		return title
	}
	return fallback
}
