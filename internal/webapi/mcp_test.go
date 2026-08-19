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
		Data: map[string]any{"id": "mcp-sensor"}, Capabilities: []string{"onoff", "measure_temperature"},
		State: map[string]any{"onoff": false, "measure_temperature": 21.5}, Available: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(apps.Close)
	server := New(database, apps, Options{Token: "secret", Language: "nl", StulpVersion: "test"})
	t.Cleanup(server.Close)
	return server, database, device
}

func mcpRoundTrip(t *testing.T, handler http.Handler, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
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
	for _, name := range []string{"devices_list", "devices_write", "flow_cards_list", "flow_card_autocomplete", "flows_create", "flows_add_card", "flows_configure_card", "flows_connect_cards"} {
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

func TestMCPReadsDevicesAndRefusesReadOnlyWrites(t *testing.T) {
	server, database, device := mcpTestServer(t)
	handler := server.Handler()

	structured := mcpStructured(t, mcpToolCall(t, handler, "devices_list", map[string]any{"deviceId": device.ID}))
	object, _ := structured["device"].(map[string]any)
	capabilities, _ := object["capabilitiesObj"].(map[string]any)
	temperature, _ := capabilities["measure_temperature"].(map[string]any)
	onoff, _ := capabilities["onoff"].(map[string]any)
	if object["name"] != "Woonkamer" || temperature["value"] != 21.5 || temperature["setable"] != false || onoff["setable"] != true {
		t.Fatalf("device metadata is incomplete: %#v", object)
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
	groups, _ := cards["cards"].(map[string]any)
	actions, _ := groups["actions"].([]any)
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
	flowObject, _ = configured["flow"].(map[string]any)
	if node := mcpResultNode(flowObject, "message"); node["x"] != float64(760) || mcpResultNodeArgs(node)["excerpt"] != "Tijd om op te staan" {
		t.Fatalf("configured card did not retain position and args: %#v", node)
	}

	added := mcpStructured(t, mcpToolCall(t, handler, "flows_add_card", map[string]any{
		"flowId": flowID,
		"node": map[string]any{"id": "pause", "x": 410, "y": 300, "step": map[string]any{
			"appId": "stulp", "cardId": "delay", "cardType": "action", "args": map[string]any{"seconds": 1.5},
		}},
	}))
	flowObject, _ = added["flow"].(map[string]any)
	if node := mcpResultNode(flowObject, "pause"); node == nil || node["y"] != float64(300) {
		t.Fatalf("added card was not placed on the canvas: %#v", flowObject)
	}

	connected := mcpStructured(t, mcpToolCall(t, handler, "flows_connect_cards", map[string]any{
		"flowId": flowID, "edgeId": "clock-pause", "fromNodeId": "clock", "toNodeId": "pause",
	}))
	flowObject, _ = connected["flow"].(map[string]any)
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
