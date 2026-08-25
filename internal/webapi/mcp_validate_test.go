package webapi

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestValidateMCPToolArgumentsAcceptsAdvertisedShapes(t *testing.T) {
	t.Parallel()
	node := func(cardType string) map[string]any {
		return map[string]any{
			"id": cardType, "x": 10.0, "y": 20.0,
			"step": map[string]any{
				"appId": "com.example", "cardId": "card", "cardType": cardType,
				"args": map[string]any{"nested": []any{true, "text", 4.0}},
			},
		}
	}
	tests := []struct {
		name      string
		arguments map[string]any
	}{
		{"system_context", nil},
		{"devices_list", map[string]any{"search": "lamp", "availableOnly": true, "offset": 0.0, "limit": 25.0}},
		{"devices_create", map[string]any{"type": "virtual_switch", "name": "Alarm ingeschakeld"}},
		{"devices_write", map[string]any{"deviceId": "lamp", "capabilityId": "onoff", "value": true}},
		{"flow_cards_list", map[string]any{"kind": "condition", "availableOnly": false}},
		{"flow_card_autocomplete", map[string]any{"appId": "app", "cardId": "card", "cardType": "action", "argument": "device", "args": map[string]any{}}},
		{"flow_action_run", map[string]any{"appId": "app", "cardId": "card", "args": map[string]any{}}},
		{"flows_list", map[string]any{"enabled": true, "offset": json.Number("2"), "limit": json.Number("10")}},
		{"flows_create", map[string]any{
			"name": "Morning", "nodes": []any{node("trigger"), node("action")},
			"edges": []any{map[string]any{"from": "one", "to": "two"}},
		}},
		{"flows_update", map[string]any{"flowId": "flow", "name": "Evening"}},
		{"flows_add_cards", map[string]any{"flowId": "flow", "nodes": []any{node("action")}}},
		{"flows_configure_card", map[string]any{"flowId": "flow", "nodeId": "node", "args": map[string]any{}, "x": -5.5}},
		{"flows_connect_cards", map[string]any{"flowId": "flow", "fromNodeId": "one", "toNodeId": "two"}},
		{"flows_disconnect_cards", map[string]any{"flowId": "flow"}},
		{"flows_remove_card", map[string]any{"flowId": "flow", "nodeId": "node"}},
		{"flows_run", map[string]any{"flowId": "flow"}},
		{"flows_delete", map[string]any{"flowId": "flow"}},
	}
	covered := make(map[string]bool, len(tests))
	for _, test := range tests {
		test := test
		covered[test.name] = true
		t.Run(test.name, func(t *testing.T) {
			if err := validateMCPToolArguments(test.name, test.arguments); err != nil {
				t.Fatal(err)
			}
		})
	}
	if len(tests) != len(mcpToolInputSchemas) {
		t.Fatalf("tested %d tool schemas, package advertises %d", len(tests), len(mcpToolInputSchemas))
	}
	for name := range mcpToolInputSchemas {
		if !covered[name] {
			t.Errorf("advertised tool %q has no valid-shape test", name)
		}
	}
}

func TestValidateMCPToolArgumentsRejectsSchemaAndBudgetViolations(t *testing.T) {
	t.Parallel()
	deep := any("leaf")
	for range mcpMaximumValueDepth + 2 {
		deep = map[string]any{"next": deep}
	}
	largeArgs := make(map[string]any, 5)
	for index := range 5 {
		largeArgs[string(rune('a'+index))] = strings.Repeat("x", mcpMaximumStringBytes)
	}
	tests := []struct {
		name      string
		arguments map[string]any
		contains  string
	}{
		{"system_context", map[string]any{"extra": true}, "unknown field"},
		{"devices_create", map[string]any{"type": "virtual_switch"}, "name is required"},
		{"devices_create", map[string]any{"type": "physical_switch", "name": "Lamp"}, "advertised enum"},
		{"devices_create", map[string]any{"type": "virtual_switch", "name": ""}, "at least 1"},
		{"devices_create", map[string]any{"type": "virtual_switch", "name": strings.Repeat("x", 161)}, "at most 160"},
		{"devices_create", map[string]any{"type": "virtual_switch", "name": "Lamp", "appId": "com.example.unsafe"}, "unknown field"},
		{"devices_write", map[string]any{"deviceId": "lamp", "value": true}, "capabilityId is required"},
		{"devices_list", map[string]any{"deviceId": 12.0}, "deviceId must be a string"},
		{"devices_list", map[string]any{"offset": 1.5}, "whole number"},
		{"devices_list", map[string]any{"limit": 101.0}, "at most 100"},
		{"flow_cards_list", map[string]any{"kind": "device-trigger"}, "advertised enum"},
		{"flows_create", map[string]any{"name": "Flow", "nodes": []any{map[string]any{
			"x": 0.0, "y": 0.0,
			"step": map[string]any{"appId": "app", "cardId": "card", "cardType": "trigger"},
		}}}, "id is required"},
		{"flows_add_cards", map[string]any{"flowId": "flow", "nodes": []any{}}, "at least 1"},
		{"flows_add_cards", map[string]any{"flowId": "flow", "nodes": []any{map[string]any{
			"id": "one", "x": 0.0, "y": 0.0, "extra": true,
			"step": map[string]any{"appId": "app", "cardId": "card", "cardType": "action"},
		}}}, "unknown field"},
		{"flows_add_cards", map[string]any{"flowId": "flow", "nodes": []any{map[string]any{
			"id": "one", "x": math.Inf(1), "y": 0.0,
			"step": map[string]any{"appId": "app", "cardId": "card", "cardType": "action"},
		}}}, "finite number"},
		{"flows_configure_card", map[string]any{"flowId": "flow", "nodeId": "node", "state": map[string]any{}}, "unknown field"},
		{"flow_action_run", map[string]any{"appId": "app", "cardId": "card", "args": map[string]any{
			"text": strings.Repeat("x", mcpMaximumStringBytes+1),
		}}, "larger than"},
		{"flow_action_run", map[string]any{"appId": "app", "cardId": "card", "args": map[string]any{
			"items": make([]any, mcpMaximumContainer+1),
		}}, "more than"},
		{"flow_action_run", map[string]any{"appId": "app", "cardId": "card", "args": largeArgs}, "total value budget"},
		{"flow_action_run", map[string]any{"appId": "app", "cardId": "card", "args": map[string]any{"deep": deep}}, "nesting depth"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name+"/"+test.contains, func(t *testing.T) {
			err := validateMCPToolArguments(test.name, test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want substring %q", err, test.contains)
			}
		})
	}
	if err := validateMCPToolArguments("not_a_tool", nil); err == nil {
		t.Fatal("unknown tool was accepted")
	}
}

func TestMCPPageAndPageValuesAreBounded(t *testing.T) {
	t.Parallel()
	if offset, limit := mcpPage(nil); offset != 0 || limit != 50 {
		t.Fatalf("default page = (%d, %d), want (0, 50)", offset, limit)
	}
	if offset, limit := mcpPage(map[string]any{"offset": 2.0, "limit": 500.0}); offset != 2 || limit != 100 {
		t.Fatalf("bounded page = (%d, %d), want (2, 100)", offset, limit)
	}
	page, next := mcpPageValues([]int{0, 1, 2, 3, 4}, 1, 2)
	if !reflect.DeepEqual(page, []int{1, 2}) || next != 3 {
		t.Fatalf("page = %v next=%d, want [1 2] next=3", page, next)
	}
	page, next = mcpPageValues([]int{0, 1}, 20, 2)
	if len(page) != 0 || next != -1 {
		t.Fatalf("past-end page = %v next=%d", page, next)
	}
	if offset, limit := mcpPage(map[string]any{"limit": json.Number("99999999999999999999")}); offset != 0 || limit != mcpDefaultPageLimit {
		t.Fatalf("unusable limit = (%d, %d), want (0, %d)", offset, limit, mcpDefaultPageLimit)
	}
	page, next = mcpPageValues([]int{0, 1}, 0, mcpMaximumPageLimit)
	if len(page) != 2 || next != -1 {
		t.Fatalf("short page = %v next=%d", page, next)
	}
}

func TestMCPSafeValueRejectsUnsafeTrees(t *testing.T) {
	t.Parallel()
	original := map[string]any{"nested": []any{map[string]any{"value": "kept"}}, "number": 3.5}
	safe, ok := mcpSafeValue(original, 0).(map[string]any)
	if !ok || !reflect.DeepEqual(safe, original) {
		t.Fatalf("safe value = %#v", safe)
	}
	if got := mcpSafeValue(math.NaN(), 0); got != nil {
		t.Fatalf("NaN became %#v, want nil", got)
	}
	if got := mcpSafeValue(errors.New("private"), 0); got != nil {
		t.Fatalf("Go error became %#v, want nil", got)
	}
	if got := mcpSafeValue(strings.Repeat("x", mcpMaximumStringBytes+1), 0); got != nil {
		t.Fatalf("oversized string was retained")
	}
	large := make([]any, 5)
	for index := range large {
		large[index] = strings.Repeat("x", mcpMaximumStringBytes)
	}
	if got := mcpSafeValue(large, 0); got != nil {
		t.Fatalf("aggregate oversized value was retained")
	}
}

func TestMCPEnumValuesProjectsLocalizedBoundedChoices(t *testing.T) {
	t.Parallel()
	values := []any{
		map[string]any{"id": "up", "title": map[string]any{"en": "Up", "nl": "Omhoog"}, "private": "hidden"},
		"idle",
		map[string]any{"title": "missing id"},
	}
	got := mcpEnumValues(values, "nl")
	want := []any{
		map[string]any{"id": "up", "title": "Omhoog"},
		map[string]any{"id": "idle", "title": "idle"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enum values = %#v, want %#v", got, want)
	}
	many := make([]any, mcpMaximumEnumValues+20)
	for index := range many {
		many[index] = map[string]any{"id": index, "title": "Value"}
	}
	if got := len(mcpEnumValues(many, "en")); got != mcpMaximumEnumValues {
		t.Fatalf("enum values returned %d entries, want %d", got, mcpMaximumEnumValues)
	}
}

func TestValidateMCPCapabilityValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		capability map[string]any
		value      any
		wantError  bool
	}{
		{"boolean", map[string]any{"type": "boolean"}, true, false},
		{"boolean mismatch", map[string]any{"type": "boolean"}, 1.0, true},
		{"number in range", map[string]any{"type": "number", "min": 1.0, "max": 3.0}, 2.0, false},
		{"number below min", map[string]any{"type": "number", "min": 1.0}, 0.5, true},
		{"number above max", map[string]any{"type": "number", "max": 3.0}, 4.0, true},
		{"number non-finite", map[string]any{"type": "number"}, math.NaN(), true},
		{"string", map[string]any{"type": "string"}, "weather", false},
		{"string mismatch", map[string]any{"type": "string"}, false, true},
		{"enum", map[string]any{"type": "enum", "values": []any{map[string]any{"id": "up"}, map[string]any{"id": "down"}}}, "down", false},
		{"enum unknown", map[string]any{"type": "enum", "values": []any{"up", "down"}}, "sideways", true},
		{"numeric enum", map[string]any{"type": "enum", "values": []any{map[string]any{"id": 2.0}}}, json.Number("2"), false},
		{"unsupported", map[string]any{"type": "object"}, map[string]any{}, true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := validateMCPCapabilityValue(test.capability, test.value)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%t", err, test.wantError)
			}
		})
	}
}
