package store

import (
	"context"
	"fmt"
	"strings"
)

// Een apparaat dat door een ander vervangen wordt.
//
// Dat gebeurt als een app zijn eigen model bijwerkt: twee rijen die hetzelfde
// apparaat bleken te zijn worden er één, of een capability krijgt een andere
// naam. De Flows van de gebruiker wijzen dan naar iets dat niet meer bestaat.
//
// Het herschrijven hoort hier en niet bij de app. Een app die dit zelf zou doen
// heeft toegang tot álle Flows nodig -- ook die van andere apps, ook die er
// niets mee te maken hebben. Zo hoeft hij alleen te zeggen wat er veranderd is,
// en blijft het bewerken van Flows bij degene die ze bezit.

// DeviceReplacement zegt waar een apparaat naartoe ging, en welke van zijn
// capabilities een andere naam kregen.
type DeviceReplacement struct {
	DeviceID     string
	Capabilities map[string]string
}

// ReplaceDeviceReferences repoints every Flow step and Scene state that refers
// to a replaced device. The map key is the old device id.
func (s *Store) ReplaceDeviceReferences(ctx context.Context, replacements map[string]DeviceReplacement) error {
	if len(replacements) == 0 {
		return nil
	}
	flows, err := s.Flows(ctx)
	if err != nil {
		return err
	}
	for _, flow := range flows {
		changed := false
		for index := range flow.Nodes {
			step := &flow.Nodes[index].Step
			oldDeviceID := nestedDeviceID(step.Args)
			replacement, applies := replacements[oldDeviceID]
			if !applies {
				continue
			}
			changed = replaceDeviceIDValues(step.Args, oldDeviceID, replacement.DeviceID) || changed
			changed = replaceDeviceIDValues(step.State, oldDeviceID, replacement.DeviceID) || changed
			if capability, ok := step.Args["capability"].(string); ok {
				if renamed := replacement.Capabilities[capability]; renamed != "" && renamed != capability {
					step.Args["capability"] = renamed
					changed = true
				}
			}
			for oldCapability, newCapability := range replacement.Capabilities {
				prefix := "capability." + oldCapability + "."
				if strings.HasPrefix(step.CardID, prefix) && oldCapability != newCapability {
					step.CardID = "capability." + newCapability + "." + strings.TrimPrefix(step.CardID, prefix)
					changed = true
				}
			}
		}
		if changed {
			if _, err := s.UpdateFlow(ctx, flow); err != nil {
				return fmt.Errorf("repoint replaced device in flow %q: %w", flow.ID, err)
			}
		}
	}

	scenes, err := s.Scenes(ctx)
	if err != nil {
		return err
	}
	for _, scene := range scenes {
		if err := s.replaceSceneReferences(ctx, scene.ID, replacements); err != nil {
			return fmt.Errorf("repoint replaced device in scene %q: %w", scene.ID, err)
		}
	}
	return nil
}

// replaceSceneReferences is a system migration, not an editor update. It may
// therefore rewrite an active Scene, but does so under the Scene lock and also
// rewrites the persisted restore snapshot so turning it off remains possible.
func (s *Store) replaceSceneReferences(ctx context.Context, id string, replacements map[string]DeviceReplacement) error {
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
	definition := cloneScene(s.doc.Scenes[index])
	changed := replaceSceneStateReferences(definition.States, replacements)
	changed = replaceSceneStateReferences(definition.Previous, replacements) || changed
	if !changed {
		s.mu.Unlock()
		return nil
	}
	if err := definition.normalize(); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := normalizeSceneStates(definition.Previous); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("scene previous states: %w", err)
	}
	definition.UpdatedAt, definition.Revision = nowRFC3339(), definition.Revision+1
	scenes := append([]Scene(nil), s.doc.Scenes...)
	scenes[index] = cloneScene(definition)
	devices := append([]deviceRecord(nil), s.doc.Devices...)
	deviceID := SceneDeviceID(id)
	deviceIndex := indexOfDeviceRecord(devices, deviceID)
	var previous *deviceRecord
	if deviceIndex >= 0 {
		previous = &devices[deviceIndex]
	}
	record := newSceneDeviceRecord(definition, previous, s.nextDeviceOrderLocked("", ""))
	if deviceIndex >= 0 {
		devices[deviceIndex] = record
	} else {
		devices = append(devices, record)
	}
	if err := s.commitScenesLocked(scenes, devices); err != nil {
		s.mu.Unlock()
		return err
	}
	s.state[deviceID] = sceneDeviceState(definition)
	device := record.device(cloneMap(s.state[deviceID]))
	s.mu.Unlock()
	s.publish(Event{Manager: "scene", Type: "scene.update", ID: id, Data: cloneScene(definition)})
	s.publish(Event{Manager: "devices", Type: "device.update", ID: deviceID, Data: device})
	return nil
}

func replaceSceneStateReferences(states []SceneState, replacements map[string]DeviceReplacement) bool {
	changed := false
	for index := range states {
		state := &states[index]
		replacement, applies := replacements[state.DeviceID]
		if !applies {
			continue
		}
		if renamed := replacement.Capabilities[state.CapabilityID]; renamed != "" && renamed != state.CapabilityID {
			state.CapabilityID = renamed
			changed = true
		}
		if replacement.DeviceID != state.DeviceID {
			state.DeviceID = replacement.DeviceID
			changed = true
		}
	}
	return changed
}

// nestedDeviceID vindt het apparaat waar een stap over gaat. Een device-argument
// staat als {"$device": "<id>"} in de argumenten.
func nestedDeviceID(args map[string]any) string {
	for _, value := range args {
		if device, ok := value.(map[string]any); ok {
			if id, _ := device["$device"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

func replaceDeviceIDValues(value any, oldID, newID string) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok {
				if text == oldID && oldID != newID {
					typed[key], changed = newID, true
				}
				continue
			}
			changed = replaceDeviceIDValues(child, oldID, newID) || changed
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				if text == oldID && oldID != newID {
					typed[index], changed = newID, true
				}
				continue
			}
			changed = replaceDeviceIDValues(child, oldID, newID) || changed
		}
	}
	return changed
}
