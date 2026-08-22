package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	scenerunner "github.com/xinix00/stulp/internal/scene"
	"github.com/xinix00/stulp/internal/store"
)

func TestMCPDiscoversAndControlsSceneAsNormalOnOffDevice(t *testing.T) {
	server, physical := sceneServer(t)
	created := createAPIScene(t, server, "Film kijken",
		store.SceneState{DeviceID: physical.ID, CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: physical.ID, CapabilityID: "mode", Value: "movie"},
	)
	// sceneServer normally has no access key so its REST tests can call it
	// directly. MCP deliberately never exists without one.
	server.options.Token = "secret"

	var calls []store.SceneState
	server.scenes = scenerunner.New(server.store, func(_ context.Context, deviceID, capabilityID string, value any, options map[string]any) error {
		if len(options) != 0 {
			return errors.New("unexpected scene invocation options")
		}
		calls = append(calls, store.SceneState{DeviceID: deviceID, CapabilityID: capabilityID, Value: value})
		return nil
	})
	handler := server.Handler()
	deviceID := store.SceneDeviceID(created.ID)

	listed := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{"deviceId": deviceID}))
	devices, _ := listed["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("scene device lookup = %#v", listed)
	}
	device, _ := devices[0].(map[string]any)
	capabilities, _ := device["capabilities"].(map[string]any)
	onoff, _ := capabilities["onoff"].(map[string]any)
	if device["id"] != deviceID || device["name"] != "Film kijken" || device["class"] != "scene" ||
		device["appId"] != store.NativeSceneAppID || device["available"] != true ||
		onoff["type"] != "boolean" || onoff["getable"] != true || onoff["setable"] != true ||
		onoff["hasValue"] != true || onoff["value"] != false {
		t.Fatalf("MCP scene device metadata = %#v", device)
	}

	writable := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{
		"search": "film kijken", "writableOnly": true,
	}))
	writableDevices, _ := writable["devices"].([]any)
	if writable["total"] != float64(1) || len(writableDevices) != 1 {
		t.Fatalf("writable scene search = %#v", writable)
	}

	invalid := mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": "aan",
	})
	if invalid["isError"] != true || !strings.Contains(mcpToolText(invalid), "boolean") || len(calls) != 0 {
		t.Fatalf("invalid MCP scene write = %#v calls=%#v", invalid, calls)
	}
	assertMCPSceneActive(t, server, created.ID, false)

	turnedOn := mcpStructured(t, mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": true,
	}))
	if turnedOn["accepted"] != true || turnedOn["deviceId"] != deviceID || turnedOn["requestedValue"] != true {
		t.Fatalf("MCP scene on result = %#v", turnedOn)
	}
	activation := mcpSceneActivation(t, turnedOn)
	if activation["requestedOn"] != true || activation["active"] != true || activation["success"] != true ||
		activation["attempted"] != float64(2) || activation["succeeded"] != float64(2) || activation["failed"] != float64(0) {
		t.Fatalf("MCP scene activation detail = %#v", activation)
	}
	if len(calls) != 2 || calls[0].DeviceID != physical.ID || calls[0].CapabilityID != "onoff" || calls[0].Value != true ||
		calls[1].CapabilityID != "mode" || calls[1].Value != "movie" {
		t.Fatalf("MCP scene on calls = %#v", calls)
	}
	assertMCPSceneActive(t, server, created.ID, true)

	reported := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{"deviceId": deviceID}))
	reportedDevices, _ := reported["devices"].([]any)
	reportedDevice, _ := reportedDevices[0].(map[string]any)
	reportedCapabilities, _ := reportedDevice["capabilities"].(map[string]any)
	reportedOnOff, _ := reportedCapabilities["onoff"].(map[string]any)
	if reportedOnOff["value"] != true {
		t.Fatalf("MCP did not read the active scene state: %#v", reportedOnOff)
	}

	turnedOff := mcpStructured(t, mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": false,
	}))
	if turnedOff["accepted"] != true || turnedOff["requestedValue"] != false {
		t.Fatalf("MCP scene off result = %#v", turnedOff)
	}
	restored := mcpSceneActivation(t, turnedOff)
	if restored["requestedOn"] != false || restored["active"] != false || restored["success"] != true ||
		restored["attempted"] != float64(2) || restored["succeeded"] != float64(2) || restored["failed"] != float64(0) {
		t.Fatalf("MCP scene restore detail = %#v", restored)
	}
	if len(calls) != 4 || calls[2].CapabilityID != "mode" || calls[2].Value != "day" ||
		calls[3].CapabilityID != "onoff" || calls[3].Value != false {
		t.Fatalf("MCP scene restore calls = %#v", calls)
	}
	assertMCPSceneActive(t, server, created.ID, false)

	// MCP may also run the same generic DAN card directly or persist it in a
	// Flow; no scene-only action contract is needed.
	cards := mcpStructured(t, mcpToolCall(t, handler, "flow_cards_list", map[string]any{
		"kind": "action", "deviceId": deviceID,
	}))
	listedCards, _ := cards["cards"].([]any)
	foundGenericOnOff := false
	for _, raw := range listedCards {
		card, _ := raw.(map[string]any)
		if card["id"] == "capability.onoff.set" {
			foundGenericOnOff = true
			break
		}
	}
	if !foundGenericOnOff {
		t.Fatalf("MCP scene Flow cards omit the generic onoff action: %#v", cards)
	}
	action := mcpStructured(t, mcpToolCall(t, handler, "flow_action_run", map[string]any{
		"appId": "stulp", "cardId": "capability.onoff.set", "args": map[string]any{
			"device": map[string]any{"$device": deviceID}, "value": true,
		},
	}))
	actionResult, _ := action["action"].(map[string]any)
	if actionResult["cardId"] != "capability.onoff.set" || actionResult["result"] != true {
		t.Fatalf("MCP generic scene action = %#v", action)
	}
	assertMCPSceneActive(t, server, created.ID, true)
}

func TestMCPReturnsStructuredPartialSceneResultAndRetryState(t *testing.T) {
	server, physical := sceneServer(t)
	created := createAPIScene(t, server, "Gedeeltelijk",
		store.SceneState{DeviceID: physical.ID, CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: physical.ID, CapabilityID: "mode", Value: "movie"},
	)
	server.options.Token = "secret"
	handler := server.Handler()
	deviceID := store.SceneDeviceID(created.ID)

	server.scenes = scenerunner.New(server.store, func(_ context.Context, _ string, capabilityID string, _ any, _ map[string]any) error {
		if capabilityID == "mode" {
			return errors.New("mode is offline")
		}
		return nil
	})
	partial := mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": true,
	})
	partialText := mcpToolText(partial)
	if partial["isError"] != true || !strings.Contains(partialText, "mode is offline") ||
		!strings.Contains(partialText, "durable active=true") || !strings.Contains(partialText, "writing onoff=false") {
		t.Fatalf("partial MCP scene result = %#v", partial)
	}
	structured, ok := partial["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("partial MCP scene has no structuredContent: %#v", partial)
	}
	if structured["accepted"] != true || structured["requestedValue"] != true {
		t.Fatalf("partial MCP scene request metadata = %#v", structured)
	}
	activation := mcpSceneActivation(t, structured)
	states, _ := activation["states"].([]any)
	if activation["requestedOn"] != true || activation["active"] != true || activation["success"] != false ||
		activation["attempted"] != float64(2) || activation["succeeded"] != float64(1) || activation["failed"] != float64(1) || len(states) != 2 {
		t.Fatalf("partial MCP scene activation detail = %#v", activation)
	}
	failedState, _ := states[1].(map[string]any)
	failedMessage, _ := failedState["error"].(string)
	if failedState["capabilityId"] != "mode" || failedState["success"] != false || !strings.Contains(failedMessage, "mode is offline") {
		t.Fatalf("partial MCP scene failed state = %#v", failedState)
	}
	definition, err := server.store.Scene(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !definition.Active || len(definition.Previous) != 1 || definition.Previous[0].CapabilityID != "onoff" {
		t.Fatalf("partial MCP scene durable retry state = %#v", definition)
	}
	assertMCPSceneActive(t, server, created.ID, true)

	server.scenes = scenerunner.New(server.store, func(_ context.Context, _ string, _ string, _ any, _ map[string]any) error { return nil })
	retried := mcpStructured(t, mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": false,
	}))
	retryActivation := mcpSceneActivation(t, retried)
	if retryActivation["active"] != false || retryActivation["success"] != true || retryActivation["attempted"] != float64(1) {
		t.Fatalf("partial MCP scene retry = %#v", retryActivation)
	}
	assertMCPSceneActive(t, server, created.ID, false)
}

func TestMCPReportsPartialSceneRestoreAndKeepsItRetryable(t *testing.T) {
	server, physical := sceneServer(t)
	created := createAPIScene(t, server, "Herstelbaar",
		store.SceneState{DeviceID: physical.ID, CapabilityID: "onoff", Value: true},
		store.SceneState{DeviceID: physical.ID, CapabilityID: "mode", Value: "movie"},
	)
	server.options.Token = "secret"
	handler := server.Handler()
	deviceID := store.SceneDeviceID(created.ID)

	server.scenes = scenerunner.New(server.store, func(_ context.Context, _ string, _ string, _ any, _ map[string]any) error { return nil })
	mcpStructured(t, mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": true,
	}))

	server.scenes = scenerunner.New(server.store, func(_ context.Context, _ string, capabilityID string, _ any, _ map[string]any) error {
		if capabilityID == "mode" {
			return errors.New("mode restore failed")
		}
		return nil
	})
	partial := mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": false,
	})
	partialText := mcpToolText(partial)
	if partial["isError"] != true || !strings.Contains(partialText, "mode restore failed") ||
		!strings.Contains(partialText, "durable active=true") || !strings.Contains(partialText, "safely retries") {
		t.Fatalf("partial MCP restore result = %#v", partial)
	}
	structured, _ := partial["structuredContent"].(map[string]any)
	activation := mcpSceneActivation(t, structured)
	if activation["requestedOn"] != false || activation["active"] != true || activation["success"] != false ||
		activation["attempted"] != float64(2) || activation["succeeded"] != float64(1) || activation["failed"] != float64(1) {
		t.Fatalf("partial MCP restore detail = %#v", activation)
	}
	definition, err := server.store.Scene(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !definition.Active || len(definition.Previous) != 1 || definition.Previous[0].CapabilityID != "mode" {
		t.Fatalf("partial MCP restore checkpoint = %#v", definition)
	}
	assertMCPSceneActive(t, server, created.ID, true)

	server.scenes = scenerunner.New(server.store, func(_ context.Context, _ string, _ string, _ any, _ map[string]any) error { return nil })
	retry := mcpStructured(t, mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": deviceID, "capabilityId": "onoff", "value": false,
	}))
	if activation := mcpSceneActivation(t, retry); activation["active"] != false || activation["attempted"] != float64(1) {
		t.Fatalf("MCP restore retry = %#v", activation)
	}
	assertMCPSceneActive(t, server, created.ID, false)
}

func TestMCPSceneActivationProjectionBoundsUntrustedOutput(t *testing.T) {
	untrusted := strings.Repeat("\x01", 16<<10)
	states := make([]scenerunner.StateResult, 256)
	for index := range states {
		states[index] = scenerunner.StateResult{
			DeviceID: strings.Repeat("device", 100), CapabilityID: strings.Repeat("capability", 100),
			Value: untrusted, Error: untrusted,
		}
	}
	states[0].Value = map[string]any{"nested": []any{map[string]any{"private": untrusted}}}
	projected := mcpSceneActivationObject(scenerunner.ActivationResult{
		SceneID: strings.Repeat("scene", 100), SceneName: strings.Repeat("name", 1000),
		RequestedOn: true, Active: true, Attempted: len(states), Failed: len(states), States: states,
	})
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= maxMCPStructuredBytes {
		t.Fatalf("projected Scene result is %d bytes, limit is %d", len(encoded), maxMCPStructuredBytes)
	}
	if len(projected["sceneId"].(string)) > mcpIDLimit || len(projected["sceneName"].(string)) > mcpNameLimit {
		t.Fatalf("Scene identity was not bounded: %#v", projected)
	}
	projectedStates, _ := projected["states"].([]any)
	omitted, _ := projected["statesOmitted"].(int)
	if len(projectedStates) == 0 || omitted == 0 || len(projectedStates)+omitted != len(states) {
		t.Fatalf("bounded state page = %d included + %d omitted, want %d total", len(projectedStates), omitted, len(states))
	}
	first, _ := projectedStates[0].(map[string]any)
	if first["valueOmitted"] != true || len(first["deviceId"].(string)) > mcpIDLimit ||
		len(first["capabilityId"].(string)) > mcpIDLimit || len(first["error"].(string)) > mcpErrorLimit {
		t.Fatalf("unsafe projected Scene state = %#v", first)
	}
	if len(projectedStates) > 1 {
		second, _ := projectedStates[1].(map[string]any)
		if value, _ := second["value"].(string); len(value) > mcpTextLimit {
			t.Fatalf("projected Scene value has %d bytes, want at most %d", len(value), mcpTextLimit)
		}
	}
}

func TestMCPSceneWriteBeforeExecutionIsNotAccepted(t *testing.T) {
	server, physical := sceneServer(t)
	created := createAPIScene(t, server, "Te laat",
		store.SceneState{DeviceID: physical.ID, CapabilityID: "onoff", Value: true})
	invoked := false
	server.scenes = scenerunner.New(server.store, func(_ context.Context, _ string, _ string, _ any, _ map[string]any) error {
		invoked = true
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	value, _, err := server.mcpWriteDevice(ctx, map[string]any{
		"deviceId": store.SceneDeviceID(created.ID), "capabilityId": "onoff", "value": true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled Scene write error = %v", err)
	}
	result, _ := value.(map[string]any)
	activation := mcpSceneActivation(t, result)
	if result["accepted"] != false || activation["attempted"] != 0 || invoked {
		t.Fatalf("pre-cancelled Scene write = %#v invoked=%t", result, invoked)
	}
	assertMCPSceneActive(t, server, created.ID, false)
}

func mcpSceneActivation(t *testing.T, structured map[string]any) map[string]any {
	t.Helper()
	activation, ok := structured["sceneActivation"].(map[string]any)
	if !ok {
		t.Fatalf("MCP result has no sceneActivation: %#v", structured)
	}
	return activation
}

func assertMCPSceneActive(t *testing.T, server *Server, sceneID string, want bool) {
	t.Helper()
	definition, err := server.store.Scene(context.Background(), sceneID)
	if err != nil {
		t.Fatal(err)
	}
	device, err := server.store.Device(context.Background(), store.SceneDeviceID(sceneID))
	if err != nil {
		t.Fatal(err)
	}
	if definition.Active != want || device.State["onoff"] != want {
		t.Fatalf("scene active=%t device onoff=%#v, want %t", definition.Active, device.State["onoff"], want)
	}
}
