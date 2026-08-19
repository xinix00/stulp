package webapi

// Card arguments are the one place where an app's own vocabulary reaches Stulp:
// what a card accepts is declared in its manifest, so validation here reads that
// declaration instead of a fixed schema.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/store"
)

func (s *Server) validateMCPCardArguments(ctx context.Context, node *store.FlowNode, card map[string]any, allowUnknown bool) error {
	definitions, _ := card["args"].([]any)
	known := make(map[string]map[string]any, len(definitions))
	for _, raw := range definitions {
		definition, _ := raw.(map[string]any)
		name, _ := definition["name"].(string)
		if name != "" {
			known[name] = definition
		}
	}
	for name := range node.Step.Args {
		if known[name] == nil && !allowUnknown {
			return fmt.Errorf("card %s has no argument %q", node.Step.CardID, name)
		}
	}

	devices := make(map[string]store.Device)
	for _, raw := range definitions {
		definition, _ := raw.(map[string]any)
		name, _ := definition["name"].(string)
		if name == "" || definition["type"] != "device" {
			continue
		}
		value, exists := node.Step.Args[name]
		if text, ok := value.(string); ok && text != "" {
			value = map[string]any{"$device": text}
			node.Step.Args[name] = value
		}
		reference, ok := value.(map[string]any)
		deviceID, idOK := reference["$device"].(string)
		if !exists || !ok || !idOK || strings.TrimSpace(deviceID) == "" || len(reference) != 1 {
			if optional, _ := definition["optional"].(bool); optional && !exists {
				continue
			}
			return fmt.Errorf("card %s argument %q needs {\"$device\":\"DEVICE_ID\"}", node.Step.CardID, name)
		}
		deviceID = strings.TrimSpace(deviceID)
		reference["$device"] = deviceID
		device, err := s.store.Device(ctx, deviceID)
		if err != nil {
			return fmt.Errorf("card %s argument %q: %w", node.Step.CardID, name, err)
		}
		if !mcpCardAllowsDevice(card, deviceID) {
			return fmt.Errorf("card %s cannot be used with device %s", node.Step.CardID, deviceID)
		}
		devices[name] = device
	}

	for _, raw := range definitions {
		definition, _ := raw.(map[string]any)
		name, _ := definition["name"].(string)
		typeName, _ := definition["type"].(string)
		if name == "" || typeName == "device" {
			continue
		}
		value, exists := node.Step.Args[name]
		optional, _ := definition["optional"].(bool)
		if !exists || value == nil {
			if optional {
				continue
			}
			return fmt.Errorf("card %s argument %q is required", node.Step.CardID, name)
		}
		if err := s.validateMCPCardArgumentValue(node, card, definition, value, devices); err != nil {
			return fmt.Errorf("card %s argument %q: %w", node.Step.CardID, name, err)
		}
	}
	if node.Step.Inverted && node.Step.CardType != "condition" {
		return fmt.Errorf("card %s can only be inverted when it is a condition", node.Step.CardID)
	}
	return nil
}

func (s *Server) validateMCPCardArgumentValue(node *store.FlowNode, card, definition map[string]any, value any, devices map[string]store.Device) error {
	typeName, _ := definition["type"].(string)
	if text, ok := value.(string); ok && mcpHoldsToken(text) {
		return nil
	}
	switch typeName {
	case "text":
		if _, ok := value.(string); !ok {
			return errors.New("must be a string")
		}
	case "number":
		number, ok := mcpFiniteNumber(value)
		if !ok {
			return errors.New("must be a finite number or token")
		}
		if minimum, ok := mcpFiniteNumber(definition["min"]); ok && number < minimum {
			return fmt.Errorf("must be at least %v", definition["min"])
		}
		if maximum, ok := mcpFiniteNumber(definition["max"]); ok && number > maximum {
			return fmt.Errorf("must be at most %v", definition["max"])
		}
	case "time":
		text, ok := value.(string)
		if !ok {
			return errors.New("must use HH:MM")
		}
		if _, err := time.Parse("15:04", text); err != nil {
			return errors.New("must use a valid 24-hour HH:MM time")
		}
	case "dropdown":
		if !mcpEnumContains(definition["values"], value) {
			return errors.New("must be one of the advertised dropdown ids")
		}
	case "autocomplete":
		choice, ok := value.(map[string]any)
		id, idOK := choice["id"].(string)
		if !ok || !idOK || strings.TrimSpace(id) == "" {
			return errors.New("must be an autocomplete choice object with an id")
		}
	case "capability":
		capabilityID, ok := value.(string)
		if !ok || strings.TrimSpace(capabilityID) == "" {
			return errors.New("must be a capability id")
		}
		device, ok := mcpArgumentDevice(card, devices)
		if !ok || !mcpDeviceHasCapability(device, capabilityID) {
			return fmt.Errorf("device does not expose capability %q", capabilityID)
		}
	case "capability-value":
		device, ok := mcpArgumentDevice(card, devices)
		if !ok {
			return errors.New("needs a selected device")
		}
		capabilityID, _ := card["capability"].(string)
		if capabilityID == "" {
			capabilityID, _ = node.Step.Args["capability"].(string)
		}
		if capabilityID == "" || !mcpDeviceHasCapability(device, capabilityID) {
			return errors.New("needs a capability exposed by the selected device")
		}
		capability := s.capabilityObject(device, capabilityID, device.State[capabilityID])
		if node.Step.CardType == "action" {
			setable, _ := capability["setable"].(bool)
			if !setable {
				return fmt.Errorf("capability %q is read-only", capabilityID)
			}
		}
		if err := validateMCPCapabilityValue(capability, value); err != nil {
			return err
		}
	default:
		return fmt.Errorf("uses unsupported argument type %q", typeName)
	}
	return nil
}

func mcpArgumentDevice(card map[string]any, devices map[string]store.Device) (store.Device, bool) {
	if name, _ := card["deviceArgument"].(string); name != "" {
		device, ok := devices[name]
		return device, ok
	}
	for _, device := range devices {
		return device, true
	}
	return store.Device{}, false
}

func mcpHoldsToken(value string) bool {
	start := strings.Index(value, "{{")
	return start >= 0 && strings.Contains(value[start+2:], "}}")
}

func mcpValueHoldsToken(value any) bool {
	switch value := value.(type) {
	case string:
		return mcpHoldsToken(value)
	case map[string]any:
		for _, child := range value {
			if mcpValueHoldsToken(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if mcpValueHoldsToken(child) {
				return true
			}
		}
	}
	return false
}
