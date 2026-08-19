package webapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

func mcpTestServer(t *testing.T) (*Server, *store.Store, store.Device) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.InstallMatterApp(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(context.Background(), store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Woonkamer", Class: "sensor",
		Data:     map[string]any{"id": "mcp-sensor", "secret": "DO_NOT_EXPOSE_DATA"},
		Settings: map[string]any{"token": "DO_NOT_EXPOSE_SETTINGS"}, Store: map[string]any{"credential": "DO_NOT_EXPOSE_STORE"},
		Capabilities: []string{"onoff", "measure_temperature"},
		State:        map[string]any{"onoff": false, "measure_temperature": 21.5}, Available: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	floor, err := database.CreateDeviceGroup(context.Background(), store.DeviceGroup{Name: "Beneden"})
	if err != nil {
		t.Fatal(err)
	}
	room, err := database.CreateDeviceGroup(context.Background(), store.DeviceGroup{Name: "Woonkamer", ParentID: floor.ID})
	if err != nil {
		t.Fatal(err)
	}
	device, err = database.SetDeviceGroup(context.Background(), device.ID, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(apps.Close)
	server := New(database, apps, Options{Token: "secret", Language: "nl", StulpVersion: "test"})
	t.Cleanup(server.Close)
	return server, database, device
}

func mcpRoundTrip(t *testing.T, handler http.Handler, path string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return mcpRawRoundTrip(t, handler, http.MethodPost, path, body, map[string]string{
		"Content-Type":         "application/json",
		"Accept":               "application/json, text/event-stream",
		"MCP-Protocol-Version": mcpProtocolVersion,
	})
}

func mcpToolCall(t *testing.T, handler http.Handler, name string, arguments map[string]any) map[string]any {
	t.Helper()
	response, decoded := mcpRoundTrip(t, handler, "/mcp/secret", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("tool %s returned HTTP %d: %s", name, response.Code, response.Body.String())
	}
	result, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("tool %s has no result: %#v", name, decoded)
	}
	return result
}

func mcpStructured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if result["isError"] == true {
		t.Fatalf("tool failed: %#v", result["content"])
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("tool has no structuredContent: %#v", result)
	}
	return structured
}

func TestMCPIsAKeyedStatelessStreamableHTTPEndpoint(t *testing.T) {
	server, _, _ := mcpTestServer(t)
	handler := server.Handler()

	wrong, _ := mcpRoundTrip(t, handler, "/mcp/wrong", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("wrong MCP key returned %d", wrong.Code)
	}
	get := httptest.NewRequest(http.MethodGet, "/mcp/secret", nil)
	get.Header.Set("Accept", "text/event-stream")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusMethodNotAllowed || getResponse.Header().Get("Allow") != "POST" {
		t.Fatalf("stateless MCP GET = %d Allow %q", getResponse.Code, getResponse.Header().Get("Allow"))
	}

	// A page on another site posting to this Stulp: the browser sends its own
	// Origin, which does not match the host it is talking to.
	evil := httptest.NewRequest(http.MethodPost, "/mcp/secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	evil.Header.Set("Content-Type", "application/json")
	evil.Header.Set("Accept", "application/json, text/event-stream")
	evil.Header.Set("Origin", "https://evil.example")
	evilResponse := httptest.NewRecorder()
	handler.ServeHTTP(evilResponse, evil)
	if evilResponse.Code != http.StatusForbidden {
		t.Fatalf("foreign MCP origin returned %d", evilResponse.Code)
	}

	initialized, decoded := mcpRoundTrip(t, handler, "/mcp/secret", map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "test", "version": "1"},
		},
	})
	result, _ := decoded["result"].(map[string]any)
	capabilities, _ := result["capabilities"].(map[string]any)
	if initialized.Code != http.StatusOK || result["protocolVersion"] != mcpProtocolVersion || capabilities["tools"] == nil {
		t.Fatalf("initialize = %d %#v", initialized.Code, decoded)
	}

	listed, decoded := mcpRoundTrip(t, handler, "/mcp/secret", map[string]any{"jsonrpc": "2.0", "id": 8, "method": "tools/list"})
	result, _ = decoded["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		names[tool["name"].(string)] = true
	}
	for _, name := range []string{"system_context", "devices_list", "devices_write", "flow_cards_list", "flow_card_autocomplete", "flow_action_run", "flows_create", "flows_add_cards", "flows_configure_card", "flows_connect_cards"} {
		if !names[name] {
			t.Errorf("tools/list misses %s", name)
		}
	}
	if listed.Code != http.StatusOK {
		t.Fatalf("tools/list returned %d", listed.Code)
	}

	notification, _ := mcpRoundTrip(t, handler, "/mcp/secret", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if notification.Code != http.StatusAccepted || notification.Body.Len() != 0 {
		t.Fatalf("initialized notification = %d %q", notification.Code, notification.Body.String())
	}
}

func TestMCPListsAnnotatedToolsAndRejectsUnknownMethods(t *testing.T) {
	server, _, _ := mcpTestServer(t)
	handler := server.Handler()

	listed, decoded := mcpRoundTrip(t, handler, "/mcp/secret", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	result, _ := decoded["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if listed.Code != http.StatusOK || len(tools) != len(mcpToolCatalog) {
		t.Fatalf("tools/list = %d %#v", listed.Code, decoded)
	}
	annotations := map[string]map[string]any{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		annotations[name], _ = tool["annotations"].(map[string]any)
		schema, _ := tool["inputSchema"].(map[string]any)
		if len(tool) != 5 || tool["title"] == nil || tool["description"] == nil || schema["type"] != "object" {
			t.Errorf("tool %q does not have the advertised tools/list shape: %#v", name, tool)
		}
	}
	if annotations["devices_write"]["destructiveHint"] != true || annotations["flow_action_run"]["openWorldHint"] != true ||
		annotations["flows_create"]["destructiveHint"] != false || annotations["devices_list"]["readOnlyHint"] != true {
		t.Fatalf("unsafe tool annotations: %#v", annotations)
	}

	unknown, decoded := mcpRoundTrip(t, handler, "/mcp/secret", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "unknown/method",
	})
	if unknown.Code != http.StatusOK || mcpRPCErrorCode(decoded) != -32601 {
		t.Fatalf("unknown method = %d %#v", unknown.Code, decoded)
	}
}

func TestMCPRejectsMalformedEnvelopesAndHeaders(t *testing.T) {
	server, _, _ := mcpTestServer(t)
	handler := server.Handler()
	baseHeaders := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}

	tests := []struct {
		name    string
		body    any
		headers map[string]string
		status  int
		code    int
	}{
		{"valid JSON array", []any{}, baseHeaders, http.StatusOK, -32600},
		{"boolean id", map[string]any{"jsonrpc": "2.0", "id": true, "method": "ping"}, baseHeaders, http.StatusOK, -32600},
		{"unsupported version", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, map[string]string{
			"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2099-01-01",
		}, http.StatusBadRequest, -32602},
		{"params must be an object", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping", "params": []any{1}}, baseHeaders, http.StatusOK, -32602},
		{"near media type", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, map[string]string{
			"Content-Type": "application/json", "Accept": "application/jsonfoo, text/event-stream",
		}, http.StatusBadRequest, -32600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, decoded := mcpRawRoundTrip(t, handler, http.MethodPost, "/mcp/secret", test.body, test.headers)
			if response.Code != test.status || mcpRPCErrorCode(decoded) != test.code {
				t.Fatalf("response = %d %#v", response.Code, decoded)
			}
		})
	}

	deleted, decoded := mcpRawRoundTrip(t, handler, http.MethodDelete, "/mcp/secret", nil, nil)
	if deleted.Code != http.StatusMethodNotAllowed || deleted.Header().Get("Allow") != "POST" || mcpRPCErrorCode(decoded) != -32600 {
		t.Fatalf("DELETE MCP = %d Allow %q %#v", deleted.Code, deleted.Header().Get("Allow"), decoded)
	}
	deletedWithOrigin, decoded := mcpRawRoundTrip(t, handler, http.MethodDelete, "/mcp/secret", nil, map[string]string{"Origin": "https://evil.example"})
	if deletedWithOrigin.Code != http.StatusForbidden || mcpRPCErrorCode(decoded) != -32600 {
		t.Fatalf("DELETE MCP with Origin = %d %#v", deletedWithOrigin.Code, decoded)
	}
}

// "Turn on the lights in the living room" is the whole journey: read the rooms,
// narrow to the room and the capability, then write one device.
func TestMCPFindsDevicesByRoomAndCapability(t *testing.T) {
	server, database, device := mcpTestServer(t)
	handler := server.Handler()

	systemContext := mcpStructured(t, mcpToolCall(t, handler, "system_context", map[string]any{}))
	home, _ := systemContext["context"].(map[string]any)
	groups, _ := home["deviceGroups"].([]any)
	rooms := map[string]map[string]any{}
	for _, raw := range groups {
		group, _ := raw.(map[string]any)
		name, _ := group["name"].(string)
		rooms[name] = group
	}
	if len(groups) != 2 || rooms["Woonkamer"]["path"] != "Beneden / Woonkamer" || rooms["Woonkamer"]["deviceCount"] != 1.0 {
		t.Fatalf("device groups = %#v", groups)
	}
	// The device sits in the living room, which sits downstairs, so downstairs
	// counts it too -- but only through its subgroup.
	if rooms["Beneden"]["deviceCount"] != 0.0 || rooms["Beneden"]["deviceCountIncludingSubgroups"] != 1.0 {
		t.Fatalf("nested group counts = %#v", rooms["Beneden"])
	}

	roomID, _ := rooms["Woonkamer"]["id"].(string)
	floorID, _ := rooms["Beneden"]["id"].(string)
	for _, groupID := range []string{roomID, floorID} {
		found := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{
			"groupId": groupID, "capabilityId": "onoff",
		}))
		devices, _ := found["devices"].([]any)
		if len(devices) != 1 {
			t.Fatalf("group %s has %d devices with onoff", groupID, len(devices))
		}
		first, _ := devices[0].(map[string]any)
		if first["id"] != device.ID {
			t.Fatalf("group %s returned %#v", groupID, first)
		}
	}

	unknownGroup := mcpToolCall(t, handler, "devices_list", map[string]any{"groupId": "no-such-room"})
	if unknownGroup["isError"] != true || !strings.Contains(mcpToolText(unknownGroup), "system_context") {
		t.Fatalf("unknown group was not refused with a hint: %#v", unknownGroup)
	}

	// The write itself needs the owning app to be running, which is covered by
	// TestMCPReadsDevicesAndRefusesReadOnlyWrites; what matters here is that the
	// assistant could get from a room name to one exact device id.
	stored, err := database.Device(context.Background(), device.ID)
	if err != nil || stored.GroupID != roomID {
		t.Fatalf("device group = %q err=%v", stored.GroupID, err)
	}
}

func TestMCPReadsDevicesAndRefusesReadOnlyWrites(t *testing.T) {
	server, database, device := mcpTestServer(t)
	handler := server.Handler()

	structured := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{"deviceId": device.ID}))
	devices, _ := structured["devices"].([]any)
	object, _ := devices[0].(map[string]any)
	capabilities, _ := object["capabilities"].(map[string]any)
	temperature, _ := capabilities["measure_temperature"].(map[string]any)
	onoff, _ := capabilities["onoff"].(map[string]any)
	if object["name"] != "Woonkamer" || temperature["value"] != 21.5 || temperature["setable"] != false || onoff["setable"] != true {
		t.Fatalf("device metadata is incomplete: %#v", object)
	}
	if object["group"] != "Beneden / Woonkamer" {
		t.Fatalf("device group context is incomplete: %#v", object["group"])
	}
	encoded, _ := json.Marshal(structured)
	if strings.Contains(string(encoded), "DO_NOT_EXPOSE_") || object["data"] != nil || object["settings"] != nil || object["store"] != nil {
		t.Fatalf("private plugin device fields leaked through MCP: %s", encoded)
	}

	// A list is a list: names and capability ids, no per-capability metadata.
	listed := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{"search": "woonkamer"}))
	summaries, _ := listed["devices"].([]any)
	summary, _ := summaries[0].(map[string]any)
	ids, _ := summary["capabilities"].([]any)
	if len(summaries) != 1 || summary["name"] != "Woonkamer" || len(ids) == 0 || ids[0] == nil {
		t.Fatalf("device summary = %#v", summary)
	}
	if _, isObject := summary["capabilities"].(map[string]any); isObject || summary["hardwareName"] != nil || summary["appId"] != nil {
		t.Fatalf("device summary carries detail fields: %#v", summary)
	}

	// One capability across devices is the targeted read, so it does carry the
	// metadata needed to write it.
	perCapability := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{"capabilityId": "onoff"}))
	perDevice, _ := perCapability["devices"].([]any)
	first, _ := perDevice[0].(map[string]any)
	onlyCapability, _ := first["capabilities"].(map[string]any)
	if len(onlyCapability) != 1 || onlyCapability["onoff"] == nil {
		t.Fatalf("capability-filtered device = %#v", first)
	}

	failed := mcpToolCall(t, handler, "devices_write", map[string]any{
		"deviceId": device.ID, "capabilityId": "measure_temperature", "value": 99,
	})
	if failed["isError"] != true || !strings.Contains(mcpToolText(failed), "read-only") {
		t.Fatalf("read-only write was not refused: %#v", failed)
	}
	stored, err := database.Device(context.Background(), device.ID)
	if err != nil || stored.State["measure_temperature"] != 21.5 {
		t.Fatalf("read-only write changed the device: %#v err=%v", stored.State, err)
	}

	cards := mcpStructured(t, mcpToolCall(t, handler, "flow_cards_list", map[string]any{
		"kind": "action", "deviceId": device.ID,
	}))
	actions, _ := cards["cards"].([]any)
	foundSet := false
	for _, raw := range actions {
		card, _ := raw.(map[string]any)
		if card["id"] == "set_device_capability" {
			foundSet = true
		}
	}
	if !foundSet {
		t.Fatal("device-filtered Flow cards omit the writable capability action")
	}
}

// A Flow is built the way a person builds one on the canvas: start empty, add a
// card, add another, then connect them. No step may be refused for being
// incomplete, and every step has to say what is still missing.
func TestMCPBuildsAFlowOneCardAtATime(t *testing.T) {
	server, database, _ := mcpTestServer(t)
	handler := server.Handler()

	created := mcpToolCall(t, handler, "flows_create", map[string]any{"name": "Stap voor stap"})
	flowObject, _ := mcpStructured(t, created)["flow"].(map[string]any)
	flowID, _ := flowObject["id"].(string)
	if flowID == "" || !strings.Contains(mcpToolText(created), "add an ALS/trigger card") {
		t.Fatalf("empty Flow was not created with a next step: %#v", created)
	}

	trigger := mcpToolCall(t, handler, "flows_add_cards", map[string]any{
		"flowId": flowID,
		"nodes": []any{map[string]any{"id": "clock", "x": 40, "y": 70, "step": map[string]any{
			"appId": "stulp", "cardId": "time_at", "cardType": "trigger", "args": map[string]any{"time": "08:00"},
		}}},
	})
	if trigger["isError"] == true || !strings.Contains(mcpToolText(trigger), "add a DAN/action card") {
		t.Fatalf("trigger-only Flow was refused or unexplained: %#v", trigger)
	}

	action := mcpToolCall(t, handler, "flows_add_cards", map[string]any{
		"flowId": flowID,
		"nodes": []any{map[string]any{"id": "message", "x": 620, "y": 70, "step": map[string]any{
			"appId": "stulp", "cardId": "notification", "cardType": "action", "args": map[string]any{"excerpt": "Opstaan"},
		}}},
	})
	if action["isError"] == true || !strings.Contains(mcpToolText(action), "connect the ALS/trigger card") {
		t.Fatalf("unconnected Flow was refused or unexplained: %#v", action)
	}

	connected := mcpToolCall(t, handler, "flows_connect_cards", map[string]any{
		"flowId": flowID, "fromNodeId": "clock", "toNodeId": "message",
	})
	if connected["isError"] == true || !strings.Contains(mcpToolText(connected), "complete and enabled") {
		t.Fatalf("connected Flow was not reported as complete: %#v", connected)
	}
	stored, err := database.Flow(context.Background(), flowID)
	if err != nil || len(stored.Nodes) != 2 || len(stored.Edges) != 1 {
		t.Fatalf("Flow did not persist intact: %#v err=%v", stored, err)
	}
	if runnable, missing := stored.Runnable(); !runnable {
		t.Fatalf("stored Flow is not runnable: %s", missing)
	}

	// The same operation, in one call, for a caller that already knows the shape.
	batch := mcpStructured(t, mcpToolCall(t, handler, "flows_add_cards", map[string]any{
		"flowId": flowID,
		"nodes": []any{
			map[string]any{"id": "pause", "x": 300, "y": 300, "step": map[string]any{
				"appId": "stulp", "cardId": "delay", "cardType": "action", "args": map[string]any{"seconds": 1.0},
			}},
			map[string]any{"id": "second", "x": 620, "y": 300, "step": map[string]any{
				"appId": "stulp", "cardId": "notification", "cardType": "action", "args": map[string]any{"excerpt": "Nogmaals"},
			}},
		},
		"edges": []any{
			map[string]any{"from": "clock", "to": "pause"},
			map[string]any{"from": "pause", "to": "second"},
		},
	}))
	ids, _ := batch["nodeIds"].([]any)
	summary, _ := batch["flow"].(map[string]any)
	if len(ids) != 2 || summary["nodeCount"] != float64(4) || summary["edgeCount"] != float64(3) {
		t.Fatalf("batch add = %#v", batch)
	}
}

func TestMCPBuildsConfiguresAndConnectsAVisualFlow(t *testing.T) {
	server, database, _ := mcpTestServer(t)
	handler := server.Handler()

	created := mcpStructured(t, mcpToolCall(t, handler, "flows_create", map[string]any{
		"name": "Goedemorgen", "enabled": true,
		"nodes": []any{
			map[string]any{"id": "clock", "x": 40, "y": 70, "step": map[string]any{
				"appId": "stulp", "cardId": "time_at", "cardType": "trigger", "args": map[string]any{"time": "08:00"},
			}},
			map[string]any{"id": "message", "x": 620, "y": 90, "step": map[string]any{
				"appId": "stulp", "cardId": "notification", "cardType": "action", "args": map[string]any{"excerpt": "Goedemorgen"},
			}},
		},
		"edges": []any{map[string]any{"id": "direct", "from": "clock", "to": "message"}},
	}))
	flowObject, _ := created["flow"].(map[string]any)
	flowID, _ := flowObject["id"].(string)
	if flowID == "" {
		t.Fatalf("created Flow has no id: %#v", created)
	}

	configured := mcpStructured(t, mcpToolCall(t, handler, "flows_configure_card", map[string]any{
		"flowId": flowID, "nodeId": "message", "x": 760, "y": 180,
		"args": map[string]any{"excerpt": "Tijd om op te staan"},
	}))
	if configured["nodeId"] != "message" {
		t.Fatalf("configure result misses its delta: %#v", configured)
	}
	flowObject = mcpReadFlow(t, handler, flowID)
	if node := mcpResultNode(flowObject, "message"); node["x"] != float64(760) || mcpResultNodeArgs(node)["excerpt"] != "Tijd om op te staan" {
		t.Fatalf("configured card did not retain position and args: %#v", node)
	}

	added := mcpStructured(t, mcpToolCall(t, handler, "flows_add_cards", map[string]any{
		"flowId": flowID,
		"nodes": []any{map[string]any{"id": "pause", "x": 410, "y": 300, "step": map[string]any{
			"appId": "stulp", "cardId": "delay", "cardType": "action", "args": map[string]any{"seconds": 1.5},
		}}},
	}))
	addedIDs, _ := added["nodeIds"].([]any)
	if len(addedIDs) != 1 || addedIDs[0] != "pause" {
		t.Fatalf("add result misses the card ids: %#v", added)
	}
	flowObject = mcpReadFlow(t, handler, flowID)
	if node := mcpResultNode(flowObject, "pause"); node == nil || node["y"] != float64(300) {
		t.Fatalf("added card was not placed on the canvas: %#v", flowObject)
	}

	connected := mcpStructured(t, mcpToolCall(t, handler, "flows_connect_cards", map[string]any{
		"flowId": flowID, "edgeId": "clock-pause", "fromNodeId": "clock", "toNodeId": "pause",
	}))
	if connected["edgeId"] != "clock-pause" {
		t.Fatalf("connect result misses its edge delta: %#v", connected)
	}
	flowObject = mcpReadFlow(t, handler, flowID)
	edges, _ := flowObject["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("card connection was not saved: %#v", edges)
	}

	invalid := mcpToolCall(t, handler, "flows_connect_cards", map[string]any{
		"flowId": flowID, "fromNodeId": "pause", "toNodeId": "clock",
	})
	if invalid["isError"] != true || !strings.Contains(mcpToolText(invalid), "ALS card") {
		t.Fatalf("invalid connection was not explained: %#v", invalid)
	}
	stored, err := database.Flow(context.Background(), flowID)
	if err != nil || len(stored.Nodes) != 3 || len(stored.Edges) != 2 {
		t.Fatalf("Flow did not persist intact: %#v err=%v", stored, err)
	}

	updated := mcpStructured(t, mcpToolCall(t, handler, "flows_update", map[string]any{
		"flowId": flowID, "name": "Ochtend",
	}))
	updatedFlow, _ := updated["flow"].(map[string]any)
	if updatedFlow["name"] != "Ochtend" {
		t.Fatalf("Flow rename was not returned: %#v", updated)
	}

	disconnected := mcpStructured(t, mcpToolCall(t, handler, "flows_disconnect_cards", map[string]any{
		"flowId": flowID, "edgeId": "clock-pause",
	}))
	if disconnected["removedConnections"] != float64(1) {
		t.Fatalf("disconnect result = %#v", disconnected)
	}
	removed := mcpStructured(t, mcpToolCall(t, handler, "flows_remove_card", map[string]any{
		"flowId": flowID, "nodeId": "pause",
	}))
	if removed["removedNodeId"] != "pause" {
		t.Fatalf("remove result = %#v", removed)
	}

	execution := mcpStructured(t, mcpToolCall(t, handler, "flows_run", map[string]any{"flowId": flowID}))
	run, _ := execution["execution"].(map[string]any)
	actions, _ := run["actions"].([]any)
	if run["success"] != true || len(actions) != 1 {
		t.Fatalf("Flow execution = %#v", execution)
	}
	summaries := mcpStructured(t, mcpToolCall(t, handler, "flows_list", map[string]any{"search": "ochtend"}))
	listed, _ := summaries["flows"].([]any)
	if summaries["total"] != float64(1) || len(listed) != 1 {
		t.Fatalf("Flow summary page = %#v", summaries)
	}

	deleted := mcpStructured(t, mcpToolCall(t, handler, "flows_delete", map[string]any{"flowId": flowID}))
	if deleted["deleted"] != true || deleted["flowId"] != flowID {
		t.Fatalf("delete result = %#v", deleted)
	}
	if _, err := database.Flow(context.Background(), flowID); err == nil {
		t.Fatal("deleted Flow still exists")
	}
}

func TestMCPValidatesCardsAndRunsAnActionWithoutPersistingAFlow(t *testing.T) {
	server, database, _ := mcpTestServer(t)
	handler := server.Handler()

	invalidTime := mcpToolCall(t, handler, "flows_create", map[string]any{
		"name": "Broken", "enabled": true,
		"nodes": []any{
			map[string]any{"id": "clock", "x": 0, "y": 0, "step": map[string]any{
				"appId": "stulp", "cardId": "time_at", "cardType": "trigger", "args": map[string]any{"time": "29:99"},
			}},
			map[string]any{"id": "wait", "x": 1, "y": 1, "step": map[string]any{
				"appId": "stulp", "cardId": "delay", "cardType": "action", "args": map[string]any{"seconds": 99},
			}},
		},
		"edges": []any{map[string]any{"from": "clock", "to": "wait"}},
	})
	if invalidTime["isError"] != true || !strings.Contains(mcpToolText(invalidTime), "valid 24-hour") {
		t.Fatalf("invalid card arguments were accepted: %#v", invalidTime)
	}
	invalidToken := mcpToolCall(t, handler, "flow_action_run", map[string]any{
		"appId": "stulp", "cardId": "delay", "args": map[string]any{"seconds": "{{temperature}}"},
	})
	if invalidToken["isError"] != true || !strings.Contains(mcpToolText(invalidToken), "has no trigger values") {
		t.Fatalf("token was accepted for an immediate action: %#v", invalidToken)
	}

	before, err := database.Flows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	action := mcpStructured(t, mcpToolCall(t, handler, "flow_action_run", map[string]any{
		"appId": "stulp", "cardId": "delay", "args": map[string]any{"seconds": 0},
	}))
	result, _ := action["action"].(map[string]any)
	if result["cardId"] != "delay" || result["cardType"] != "action" {
		t.Fatalf("ad-hoc action result = %#v", action)
	}
	after, err := database.Flows(context.Background())
	if err != nil || len(after) != len(before) {
		t.Fatalf("ad-hoc action persisted a Flow: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestMCPLimiterBoundsBurstAndConcurrency(t *testing.T) {
	server := &Server{}
	now := time.Unix(1_700_000_000, 0)
	releases := make([]func(), 0, 4)
	for range 4 {
		release, _, allowed := server.beginMCPToolCall(now)
		if !allowed {
			t.Fatal("one of the first four calls was refused")
		}
		releases = append(releases, release)
	}
	if _, retry, allowed := server.beginMCPToolCall(now); allowed || retry < 1 {
		t.Fatalf("fifth concurrent call allowed=%v retry=%d", allowed, retry)
	}
	for _, release := range releases {
		release()
	}
	// Four tokens were spent above; the remaining 26 are the rest of the burst.
	for range 26 {
		release, _, allowed := server.beginMCPToolCall(now)
		if !allowed {
			t.Fatal("burst was exhausted too early")
		}
		release()
	}
	if _, retry, allowed := server.beginMCPToolCall(now); allowed || retry != 1 {
		t.Fatalf("31st burst call allowed=%v retry=%d", allowed, retry)
	}
	release, _, allowed := server.beginMCPToolCall(now.Add(500 * time.Millisecond))
	if !allowed {
		t.Fatal("limiter did not refill at two calls per second")
	}
	release()
}

func TestMCPFlowArgumentProjectionOmitsPrivateAndUnknownFields(t *testing.T) {
	card := map[string]any{"args": []any{
		map[string]any{"name": "device", "type": "device"},
		map[string]any{"name": "track", "type": "autocomplete"},
		map[string]any{"name": "volume", "type": "number"},
	}}
	projected := mcpPublicFlowArgs(map[string]any{
		"device": map[string]any{"$device": "speaker", "credential": "DEVICE_SECRET"},
		"track": map[string]any{
			"id": "track:1", "name": "Song", "description": "Album",
			"image": "https://example.invalid/?token=IMAGE_SECRET", "accessToken": "TOKEN_SECRET",
		},
		"volume": 0.5, "removedManifestArgument": "UNKNOWN_SECRET",
	}, card)
	encoded, _ := json.Marshal(projected)
	for _, secret := range []string{"DEVICE_SECRET", "IMAGE_SECRET", "TOKEN_SECRET", "UNKNOWN_SECRET"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("Flow argument projection leaked %s: %s", secret, encoded)
		}
	}
	device, _ := projected["device"].(map[string]any)
	choice, _ := projected["track"].(map[string]any)
	if device["$device"] != "speaker" || len(device) != 1 || choice["id"] != "track:1" || choice["image"] != nil || projected["volume"] != 0.5 {
		t.Fatalf("public Flow args are incomplete: %#v", projected)
	}

	values := mcpAutocompleteValues([]any{map[string]any{
		"id": "track:1", "name": "Song", "description": "Album", "image": "IMAGE_SECRET",
	}}, 50)
	autocomplete, _ := values[0].(map[string]any)
	if autocomplete["id"] != "track:1" || autocomplete["image"] != nil {
		t.Fatalf("autocomplete projection = %#v", autocomplete)
	}
}

func mcpReadFlow(t *testing.T, handler http.Handler, flowID string) map[string]any {
	t.Helper()
	structured := mcpStructured(t, mcpToolCall(t, handler, "flows_list", map[string]any{"flowId": flowID}))
	flows, _ := structured["flows"].([]any)
	if len(flows) != 1 {
		t.Fatalf("flows_list returned %#v for %s", structured, flowID)
	}
	flow, _ := flows[0].(map[string]any)
	return flow
}

func mcpToolText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	text, _ := content[0].(map[string]any)
	value, _ := text["text"].(string)
	return value
}

func mcpRawRoundTrip(t *testing.T, handler http.Handler, method, path string, body any, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(data))
	}
	request := httptest.NewRequest(method, path, reader)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	decoded := map[string]any{}
	if response.Body.Len() > 0 && strings.Contains(response.Header().Get("Content-Type"), "json") {
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("invalid MCP response (%d): %v\n%s", response.Code, err, response.Body.String())
		}
	}
	return response, decoded
}

func mcpRPCErrorCode(response map[string]any) int {
	rpcError, _ := response["error"].(map[string]any)
	code, _ := rpcError["code"].(float64)
	return int(code)
}

func mcpResultNode(flow map[string]any, id string) map[string]any {
	nodes, _ := flow["nodes"].([]any)
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		if node["id"] == id {
			return node
		}
	}
	return nil
}

func mcpResultNodeArgs(node map[string]any) map[string]any {
	step, _ := node["step"].(map[string]any)
	args, _ := step["args"].(map[string]any)
	return args
}
