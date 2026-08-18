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

// ReplaceDeviceReferences repoints elke Flow-stap die naar een vervangen
// apparaat wees. De sleutel is het oude apparaat-id.
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
	return nil
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
