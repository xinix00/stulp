package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/stulphttp"
)

const mcpProtocolVersion = "2025-11-25"

var supportedMCPVersions = map[string]bool{
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (s *Server) handleMCP() {
	s.mux.HandleFunc("GET /mcp/{key}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if !s.validMCPOrigin(request) {
			writeMCPHTTPError(response, stulphttp.StatusForbidden, "origin does not match this Stulp host")
			return
		}
		response.Header().Set("Allow", "POST")
		writeMCPHTTPError(response, stulphttp.StatusMethodNotAllowed, "this stateless MCP server does not open a server event stream")
	})
	s.mux.HandleFunc("POST /mcp/{key}", s.mcpPost)
}

func (s *Server) mcpPost(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	if !s.validMCPOrigin(request) {
		writeMCPHTTPError(response, stulphttp.StatusForbidden, "origin does not match this Stulp host")
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/json" {
		writeMCPHTTPError(response, stulphttp.StatusUnsupportedMediaType, "MCP requests need Content-Type: application/json")
		return
	}
	accept := strings.ToLower(request.Header.Get("Accept"))
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		writeMCPHTTPError(response, stulphttp.StatusBadRequest, "MCP requests must accept application/json and text/event-stream")
		return
	}
	if version := request.Header.Get("MCP-Protocol-Version"); version != "" && !supportedMCPVersions[version] {
		writeMCPHTTPError(response, stulphttp.StatusBadRequest, "unsupported MCP-Protocol-Version")
		return
	}

	defer stulphttp.CloseBody(request)
	decoder := json.NewDecoder(stulphttp.LimitBody(response, request, 1<<20))
	decoder.UseNumber()
	var message mcpRequest
	if err := decoder.Decode(&message); err != nil {
		writeMCPResponse(response, nil, nil, &mcpRPCError{Code: -32700, Message: "parse error", Data: err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeMCPResponse(response, nil, nil, &mcpRPCError{Code: -32600, Message: "the body must contain exactly one JSON-RPC message"})
		return
	}
	if message.JSONRPC != "2.0" || message.Method == "" {
		writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32600, Message: "invalid request"})
		return
	}

	// Een JSON-RPC-notificatie heeft geen id en dus geen JSON-RPC-antwoord.
	// initialized is de enige die deze stateless server zelf nodig heeft; andere
	// geldige notificaties mogen eveneens zonder toestand worden aangenomen.
	if len(message.ID) == 0 {
		response.WriteHeader(stulphttp.StatusAccepted)
		return
	}

	ctx := stulphttp.Context(request)
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil || params.ProtocolVersion == "" {
			writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32602, Message: "protocolVersion is required"})
			return
		}
		version := mcpProtocolVersion
		if supportedMCPVersions[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		writeMCPResponse(response, message.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "stulp", "version": s.options.StulpVersion},
			"instructions":    "Manage this Stulp home through its devices and visual Flows. Read devices and flow cards before writing so capability and card constraints are respected.",
		}, nil)
	case "ping":
		writeMCPResponse(response, message.ID, map[string]any{}, nil)
	case "tools/list":
		writeMCPResponse(response, message.ID, map[string]any{"tools": mcpTools()}, nil)
	case "tools/call":
		var params mcpCallParams
		if err := json.Unmarshal(message.Params, &params); err != nil || params.Name == "" {
			writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32602, Message: "tool name is required"})
			return
		}
		result, known := s.callMCPTool(ctx, params.Name, params.Arguments)
		if !known {
			writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32602, Message: "unknown tool " + params.Name})
			return
		}
		writeMCPResponse(response, message.ID, result, nil)
	default:
		writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32601, Message: "method not found"})
	}
}

func (s *Server) validMCPOrigin(request *stulphttp.Request) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return strings.EqualFold(parsed.Host, stulphttp.Host(request))
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeMCPResponse(response stulphttp.ResponseWriter, id json.RawMessage, result any, rpcError *mcpRPCError) {
	message := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage("null")}
	if len(id) != 0 {
		message["id"] = id
	}
	if rpcError != nil {
		message["error"] = rpcError
	} else {
		message["result"] = result
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(stulphttp.StatusOK)
	_ = json.NewEncoder(response).Encode(message)
}

func writeMCPHTTPError(response stulphttp.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]any{
		"jsonrpc": "2.0", "id": nil,
		"error": map[string]any{"code": -32600, "message": message},
	})
}

func mcpTools() []map[string]any {
	readOnly := map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true}
	write := map[string]any{"readOnlyHint": false, "destructiveHint": false}
	destructive := map[string]any{"readOnlyHint": false, "destructiveHint": true}
	return []map[string]any{
		mcpTool("devices_list", "Read devices", "List every device, or one device. Capability metadata includes setable, type, units, ranges and enum values; only capabilities with setable=true may be written.",
			mcpObject(map[string]any{"deviceId": mcpString("Optional exact device id")}), readOnly),
		mcpTool("devices_write", "Write a device", "Set one writable device capability. Read devices_list first and use only a capability whose setable field is true. Values use the displayed household units.",
			mcpObjectRequired(map[string]any{
				"deviceId": mcpString("Exact device id"), "capabilityId": mcpString("Exact capability id"),
				"value":   map[string]any{"description": "Value matching the capability type, range or enum"},
				"options": map[string]any{"type": "object", "description": "Optional device-specific transition options", "additionalProperties": true},
			}, "deviceId", "capabilityId", "value"), write),
		mcpTool("flow_cards_list", "Read Flow cards", "List the available ALS/trigger, EN/condition and DAN/action cards and their argument definitions. Device arguments are configured as {\"$device\":\"DEVICE_ID\"}.",
			mcpObject(map[string]any{
				"kind":          map[string]any{"type": "string", "enum": []string{"trigger", "condition", "action"}},
				"deviceId":      mcpString("Only cards usable with this device, plus global cards"),
				"search":        mcpString("Case-insensitive card id/title search"),
				"availableOnly": map[string]any{"type": "boolean", "default": true},
			}), readOnly),
		mcpTool("flow_card_autocomplete", "Complete a Flow card argument", "Get valid choices for an autocomplete argument on a Flow card. Pass the card and current args exactly as returned by flow_cards_list / configured in the step.",
			mcpObjectRequired(map[string]any{
				"appId": mcpString("Card appId"), "cardId": mcpString("Card id"),
				"cardType": map[string]any{"type": "string", "enum": []string{"trigger", "device-trigger", "condition", "action"}},
				"argument": mcpString("Autocomplete argument name"), "query": mcpString("Search text"),
				"args": map[string]any{"type": "object", "description": "Current values of all card arguments", "additionalProperties": true},
			}, "appId", "cardId", "cardType", "argument"), readOnly),
		mcpTool("flows_list", "Read Flows", "List all visual Flows, or one Flow, including every card position (x/y), configuration (step.args/state/inverted), and connection (edges).",
			mcpObject(map[string]any{"flowId": mcpString("Optional exact Flow id")}), readOnly),
		mcpTool("flows_create", "Create a Flow", "Create a complete visual Flow. Put every card anywhere in the canvas with x/y and connect cards with edges. A valid Flow needs at least one trigger and one action.",
			mcpObjectRequired(map[string]any{
				"name": mcpString("Flow name"), "enabled": map[string]any{"type": "boolean", "default": true},
				"nodes": map[string]any{"type": "array", "items": mcpFlowNodeSchema()},
				"edges": map[string]any{"type": "array", "items": mcpFlowEdgeSchema(), "default": []any{}},
			}, "name", "nodes"), write),
		mcpTool("flows_update", "Update Flow settings", "Rename a Flow and/or enable or disable it without replacing its cards.",
			mcpObjectRequired(map[string]any{
				"flowId": mcpString("Exact Flow id"), "name": mcpString("Optional new name"), "enabled": map[string]any{"type": "boolean"},
			}, "flowId"), write),
		mcpTool("flows_add_card", "Add a Flow card", "Add and configure one card at an arbitrary x/y canvas position. Use flow_cards_list first.",
			mcpObjectRequired(map[string]any{
				"flowId": mcpString("Exact Flow id"), "node": mcpFlowNodeSchema(),
			}, "flowId", "node"), write),
		mcpTool("flows_configure_card", "Configure a Flow card", "Replace a card's args/state and/or change inverted and its x/y canvas position. Omitted properties remain unchanged.",
			mcpObjectRequired(map[string]any{
				"flowId": mcpString("Exact Flow id"), "nodeId": mcpString("Exact card/node id"),
				"args":     map[string]any{"type": "object", "additionalProperties": true},
				"state":    map[string]any{"type": "object", "additionalProperties": true},
				"inverted": map[string]any{"type": "boolean"}, "x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"},
			}, "flowId", "nodeId"), write),
		mcpTool("flows_connect_cards", "Connect Flow cards", "Connect one card's output to another card's input. Connections must be acyclic and cannot point into a trigger.",
			mcpObjectRequired(map[string]any{
				"flowId": mcpString("Exact Flow id"), "fromNodeId": mcpString("Source card id"), "toNodeId": mcpString("Destination card id"),
				"edgeId": mcpString("Optional connection id"),
			}, "flowId", "fromNodeId", "toNodeId"), write),
		mcpTool("flows_disconnect_cards", "Disconnect Flow cards", "Remove a connection by edgeId, or by fromNodeId plus toNodeId.",
			mcpObjectRequired(map[string]any{
				"flowId": mcpString("Exact Flow id"), "edgeId": mcpString("Connection id"),
				"fromNodeId": mcpString("Source card id"), "toNodeId": mcpString("Destination card id"),
			}, "flowId"), destructive),
		mcpTool("flows_remove_card", "Remove a Flow card", "Remove one card and every connection touching it. A Flow must still contain a trigger and an action.",
			mcpObjectRequired(map[string]any{"flowId": mcpString("Exact Flow id"), "nodeId": mcpString("Exact card/node id")}, "flowId", "nodeId"), destructive),
		mcpTool("flows_run", "Run a Flow", "Run a Flow immediately and return its execution result.",
			mcpObjectRequired(map[string]any{"flowId": mcpString("Exact Flow id")}, "flowId"), write),
		mcpTool("flows_delete", "Delete a Flow", "Permanently delete a Flow.",
			mcpObjectRequired(map[string]any{"flowId": mcpString("Exact Flow id")}, "flowId"), destructive),
	}
}

func mcpTool(name, title, description string, schema, annotations map[string]any) map[string]any {
	return map[string]any{"name": name, "title": title, "description": description, "inputSchema": schema, "annotations": annotations}
}

func mcpString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func mcpObject(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
}

func mcpObjectRequired(properties map[string]any, required ...string) map[string]any {
	result := mcpObject(properties)
	result["required"] = required
	return result
}

func mcpFlowNodeSchema() map[string]any {
	return mcpObjectRequired(map[string]any{
		"id": mcpString("Optional stable card/node id; generated when omitted"),
		"x":  map[string]any{"type": "number", "description": "Horizontal canvas position"},
		"y":  map[string]any{"type": "number", "description": "Vertical canvas position"},
		"step": mcpObjectRequired(map[string]any{
			"appId": mcpString("Card appId from flow_cards_list"), "cardId": mcpString("Card id from flow_cards_list"),
			"cardType": map[string]any{"type": "string", "enum": []string{"trigger", "device-trigger", "condition", "action"}},
			"args":     map[string]any{"type": "object", "description": "Values keyed by the card argument names", "additionalProperties": true},
			"state":    map[string]any{"type": "object", "description": "Optional hidden card state", "additionalProperties": true},
			"inverted": map[string]any{"type": "boolean", "description": "Invert a condition"},
		}, "appId", "cardId", "cardType"),
	}, "x", "y", "step")
}

func mcpFlowEdgeSchema() map[string]any {
	return mcpObjectRequired(map[string]any{
		"id":   mcpString("Optional stable connection id; generated when omitted"),
		"from": mcpString("Source card/node id"), "to": mcpString("Destination card/node id"),
	}, "from", "to")
}

func (s *Server) callMCPTool(ctx context.Context, name string, arguments map[string]any) (map[string]any, bool) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	var value any
	var summary string
	var err error

	switch name {
	case "devices_list":
		value, summary, err = s.mcpDevices(ctx, stringArgument(arguments, "deviceId"))
	case "devices_write":
		value, summary, err = s.mcpWriteDevice(ctx, arguments)
	case "flow_cards_list":
		value, summary, err = s.mcpFlowCards(ctx, arguments)
	case "flow_card_autocomplete":
		value, summary, err = s.mcpFlowCardAutocomplete(ctx, arguments)
	case "flows_list":
		value, summary, err = s.mcpFlows(ctx, stringArgument(arguments, "flowId"))
	case "flows_create":
		value, summary, err = s.mcpCreateFlow(ctx, arguments)
	case "flows_update":
		value, summary, err = s.mcpUpdateFlow(ctx, arguments)
	case "flows_add_card":
		value, summary, err = s.mcpAddFlowCard(ctx, arguments)
	case "flows_configure_card":
		value, summary, err = s.mcpConfigureFlowCard(ctx, arguments)
	case "flows_connect_cards":
		value, summary, err = s.mcpConnectFlowCards(ctx, arguments)
	case "flows_disconnect_cards":
		value, summary, err = s.mcpDisconnectFlowCards(ctx, arguments)
	case "flows_remove_card":
		value, summary, err = s.mcpRemoveFlowCard(ctx, arguments)
	case "flows_run":
		value, summary, err = s.mcpRunFlow(ctx, arguments)
	case "flows_delete":
		value, summary, err = s.mcpDeleteFlow(ctx, arguments)
	default:
		return nil, false
	}
	if err != nil {
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			"isError": true,
		}, true
	}
	structured, ok := value.(map[string]any)
	if !ok {
		structured = map[string]any{"result": value}
	}
	if summary == "" {
		encoded, _ := json.MarshalIndent(structured, "", "  ")
		summary = string(encoded)
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": summary}},
		"structuredContent": structured,
		"isError":           false,
	}, true
}

func (s *Server) mcpDevices(ctx context.Context, deviceID string) (any, string, error) {
	if deviceID != "" {
		device, err := s.store.Device(ctx, deviceID)
		if err != nil {
			return nil, "", err
		}
		return map[string]any{"device": s.deviceObject(device)}, "", nil
	}
	devices, err := s.store.Devices(ctx, "")
	if err != nil {
		return nil, "", err
	}
	result := make([]any, 0, len(devices))
	for _, device := range devices {
		result = append(result, s.deviceObject(device))
	}
	return map[string]any{"devices": result}, "", nil
}

func (s *Server) mcpWriteDevice(ctx context.Context, arguments map[string]any) (any, string, error) {
	deviceID, capabilityID := stringArgument(arguments, "deviceId"), stringArgument(arguments, "capabilityId")
	value, hasValue := arguments["value"]
	if deviceID == "" || capabilityID == "" || !hasValue {
		return nil, "", errors.New("deviceId, capabilityId and value are required")
	}
	device, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return nil, "", err
	}
	known := false
	for _, id := range device.Capabilities {
		if id == capabilityID {
			known = true
			break
		}
	}
	if !known {
		return nil, "", fmt.Errorf("device %q has no capability %q", deviceID, capabilityID)
	}
	capability := s.capabilityObject(device, capabilityID, device.State[capabilityID])
	setable, _ := capability["setable"].(bool)
	if !setable {
		return nil, "", fmt.Errorf("capability %q on device %q is read-only", capabilityID, deviceID)
	}
	options, optionsOK := arguments["options"].(map[string]any)
	if arguments["options"] != nil && !optionsOK {
		return nil, "", errors.New("options must be an object")
	}
	if options == nil {
		options = map[string]any{}
	}
	canonical := s.canonicalCapabilityValue(ctx, device, capabilityID, value)
	if err := s.invokeCapability(ctx, deviceID, capabilityID, canonical, options); err != nil {
		return nil, "", err
	}
	return map[string]any{
		"written": true, "deviceId": deviceID, "capabilityId": capabilityID, "value": value,
	}, fmt.Sprintf("Wrote %s on %s.", capabilityID, device.Name), nil
}

func (s *Server) mcpFlowCards(ctx context.Context, arguments map[string]any) (any, string, error) {
	cards, err := s.flowCards(ctx)
	if err != nil {
		return nil, "", err
	}
	kind := stringArgument(arguments, "kind")
	if kind != "" && kind != "trigger" && kind != "condition" && kind != "action" {
		return nil, "", errors.New("kind must be trigger, condition or action")
	}
	deviceID := stringArgument(arguments, "deviceId")
	if deviceID != "" {
		if _, err := s.store.Device(ctx, deviceID); err != nil {
			return nil, "", err
		}
	}
	search := strings.ToLower(strings.TrimSpace(stringArgument(arguments, "search")))
	availableOnly := true
	if value, exists := arguments["availableOnly"].(bool); exists {
		availableOnly = value
	}
	result := map[string]any{}
	for _, entry := range []struct{ kind, key string }{{"trigger", "triggers"}, {"condition", "conditions"}, {"action", "actions"}} {
		if kind != "" && kind != entry.kind {
			continue
		}
		filtered := make([]any, 0, len(cards[entry.key]))
		for _, card := range cards[entry.key] {
			available, _ := card["available"].(bool)
			if availableOnly && !available {
				continue
			}
			if deviceID != "" && !mcpCardAllowsDevice(card, deviceID) {
				continue
			}
			haystack := strings.ToLower(fmt.Sprint(card["id"]) + " " + fmt.Sprint(card["title"]) + " " + fmt.Sprint(card["appName"]))
			if search != "" && !strings.Contains(haystack, search) {
				continue
			}
			filtered = append(filtered, card)
		}
		result[entry.key] = filtered
	}
	return map[string]any{"cards": result}, "", nil
}

func (s *Server) mcpFlowCardAutocomplete(ctx context.Context, arguments map[string]any) (any, string, error) {
	appID, cardID := stringArgument(arguments, "appId"), stringArgument(arguments, "cardId")
	cardType, argument := stringArgument(arguments, "cardType"), stringArgument(arguments, "argument")
	if appID == "" || cardID == "" || cardType == "" || argument == "" {
		return nil, "", errors.New("appId, cardId, cardType and argument are required")
	}
	args, argsOK := arguments["args"].(map[string]any)
	if arguments["args"] != nil && !argsOK {
		return nil, "", errors.New("args must be an object")
	}
	if args == nil {
		args = map[string]any{}
	}
	values, err := s.supervisor.InvokeFlowAutocomplete(ctx, appID, cardType, cardID, argument, stringArgument(arguments, "query"), args)
	if err != nil {
		return nil, "", err
	}
	return map[string]any{"values": values}, "", nil
}

func (s *Server) mcpFlows(ctx context.Context, flowID string) (any, string, error) {
	if flowID != "" {
		flow, err := s.store.Flow(ctx, flowID)
		if err != nil {
			return nil, "", err
		}
		return map[string]any{"flow": s.showFlow(ctx, flow, s.declaredArgumentUnits(ctx))}, "", nil
	}
	flows, err := s.store.Flows(ctx)
	if err != nil {
		return nil, "", err
	}
	return map[string]any{"flows": s.showFlows(ctx, flows)}, "", nil
}

func (s *Server) mcpCreateFlow(ctx context.Context, arguments map[string]any) (any, string, error) {
	var params struct {
		Name    string           `json:"name"`
		Enabled *bool            `json:"enabled"`
		Nodes   []store.FlowNode `json:"nodes"`
		Edges   []store.FlowEdge `json:"edges"`
	}
	if err := remarshal(arguments, &params); err != nil {
		return nil, "", err
	}
	enabled := true
	if params.Enabled != nil {
		enabled = *params.Enabled
	}
	definition := store.Flow{Name: params.Name, Enabled: enabled, Nodes: params.Nodes, Edges: params.Edges}
	if err := s.normalizeAndValidateMCPNodes(ctx, definition.Nodes, true); err != nil {
		return nil, "", err
	}
	created, err := s.store.CreateFlow(ctx, s.canonicalFlow(ctx, definition, nil))
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, created, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Created Flow %q (%s).", shown.Name, shown.ID), nil
}

func (s *Server) mcpUpdateFlow(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID := stringArgument(arguments, "flowId")
	if flowID == "" {
		return nil, "", errors.New("flowId is required")
	}
	previous, err := s.store.Flow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	definition, err := cloneMCPFlow(previous)
	if err != nil {
		return nil, "", err
	}
	changed := false
	if name, exists := arguments["name"]; exists {
		text, ok := name.(string)
		if !ok {
			return nil, "", errors.New("name must be a string")
		}
		definition.Name, changed = text, true
	}
	if enabled, exists := arguments["enabled"]; exists {
		value, ok := enabled.(bool)
		if !ok {
			return nil, "", errors.New("enabled must be a boolean")
		}
		definition.Enabled, changed = value, true
	}
	if !changed {
		return nil, "", errors.New("provide name and/or enabled")
	}
	updated, err := s.store.UpdateFlow(ctx, definition)
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, updated, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Updated Flow %q.", shown.Name), nil
}

func (s *Server) mcpAddFlowCard(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID := stringArgument(arguments, "flowId")
	if flowID == "" || arguments["node"] == nil {
		return nil, "", errors.New("flowId and node are required")
	}
	previous, err := s.store.Flow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	definition, err := cloneMCPFlow(previous)
	if err != nil {
		return nil, "", err
	}
	var node store.FlowNode
	if err := remarshal(arguments["node"], &node); err != nil {
		return nil, "", fmt.Errorf("node: %w", err)
	}
	definition.Nodes = append(definition.Nodes, node)
	if err := s.normalizeAndValidateMCPNodes(ctx, definition.Nodes[len(definition.Nodes)-1:], true); err != nil {
		return nil, "", err
	}
	node = definition.Nodes[len(definition.Nodes)-1]
	definition = s.canonicalFlow(ctx, definition, &previous)
	updated, err := s.store.UpdateFlow(ctx, definition)
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, updated, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Added card %s to Flow %q.", node.Step.CardID, shown.Name), nil
}

func (s *Server) mcpConfigureFlowCard(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID, nodeID := stringArgument(arguments, "flowId"), stringArgument(arguments, "nodeId")
	if flowID == "" || nodeID == "" {
		return nil, "", errors.New("flowId and nodeId are required")
	}
	previous, err := s.store.Flow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	definition, err := cloneMCPFlow(previous)
	if err != nil {
		return nil, "", err
	}
	index := mcpNodeIndex(definition.Nodes, nodeID)
	if index < 0 {
		return nil, "", fmt.Errorf("Flow %q has no card %q", flowID, nodeID)
	}
	node := &definition.Nodes[index]
	changed := false
	if raw, exists := arguments["args"]; exists {
		args, ok := raw.(map[string]any)
		if !ok {
			return nil, "", errors.New("args must be an object")
		}
		node.Step.Args, changed = args, true
	}
	if raw, exists := arguments["state"]; exists {
		state, ok := raw.(map[string]any)
		if !ok {
			return nil, "", errors.New("state must be an object")
		}
		node.Step.State, changed = state, true
	}
	if raw, exists := arguments["inverted"]; exists {
		value, ok := raw.(bool)
		if !ok {
			return nil, "", errors.New("inverted must be a boolean")
		}
		node.Step.Inverted, changed = value, true
	}
	if raw, exists := arguments["x"]; exists {
		value, ok := numberArgument(raw)
		if !ok {
			return nil, "", errors.New("x must be a number")
		}
		node.X, changed = value, true
	}
	if raw, exists := arguments["y"]; exists {
		value, ok := numberArgument(raw)
		if !ok {
			return nil, "", errors.New("y must be a number")
		}
		node.Y, changed = value, true
	}
	if !changed {
		return nil, "", errors.New("provide args, state, inverted, x and/or y")
	}
	if err := s.normalizeAndValidateMCPNodes(ctx, definition.Nodes[index:index+1], false); err != nil {
		return nil, "", err
	}
	definition = s.canonicalFlow(ctx, definition, &previous)
	updated, err := s.store.UpdateFlow(ctx, definition)
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, updated, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Configured card %s in Flow %q.", nodeID, shown.Name), nil
}

func (s *Server) mcpConnectFlowCards(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID := stringArgument(arguments, "flowId")
	from, to := stringArgument(arguments, "fromNodeId"), stringArgument(arguments, "toNodeId")
	if flowID == "" || from == "" || to == "" {
		return nil, "", errors.New("flowId, fromNodeId and toNodeId are required")
	}
	definition, err := s.mutableMCPFlow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	definition.Edges = append(definition.Edges, store.FlowEdge{ID: stringArgument(arguments, "edgeId"), From: from, To: to})
	updated, err := s.store.UpdateFlow(ctx, definition)
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, updated, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Connected %s to %s in Flow %q.", from, to, shown.Name), nil
}

func (s *Server) mcpDisconnectFlowCards(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID, edgeID := stringArgument(arguments, "flowId"), stringArgument(arguments, "edgeId")
	from, to := stringArgument(arguments, "fromNodeId"), stringArgument(arguments, "toNodeId")
	if flowID == "" || (edgeID == "" && (from == "" || to == "")) {
		return nil, "", errors.New("flowId and either edgeId or fromNodeId plus toNodeId are required")
	}
	definition, err := s.mutableMCPFlow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	kept := make([]store.FlowEdge, 0, len(definition.Edges))
	removed := false
	for _, edge := range definition.Edges {
		match := edgeID != "" && edge.ID == edgeID || edgeID == "" && edge.From == from && edge.To == to
		if match {
			removed = true
			continue
		}
		kept = append(kept, edge)
	}
	if !removed {
		return nil, "", errors.New("connection does not exist")
	}
	definition.Edges = kept
	updated, err := s.store.UpdateFlow(ctx, definition)
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, updated, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Disconnected cards in Flow %q.", shown.Name), nil
}

func (s *Server) mcpRemoveFlowCard(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID, nodeID := stringArgument(arguments, "flowId"), stringArgument(arguments, "nodeId")
	if flowID == "" || nodeID == "" {
		return nil, "", errors.New("flowId and nodeId are required")
	}
	definition, err := s.mutableMCPFlow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	if mcpNodeIndex(definition.Nodes, nodeID) < 0 {
		return nil, "", fmt.Errorf("Flow %q has no card %q", flowID, nodeID)
	}
	nodes := make([]store.FlowNode, 0, len(definition.Nodes)-1)
	for _, node := range definition.Nodes {
		if node.ID != nodeID {
			nodes = append(nodes, node)
		}
	}
	edges := make([]store.FlowEdge, 0, len(definition.Edges))
	for _, edge := range definition.Edges {
		if edge.From != nodeID && edge.To != nodeID {
			edges = append(edges, edge)
		}
	}
	definition.Nodes, definition.Edges = nodes, edges
	updated, err := s.store.UpdateFlow(ctx, definition)
	if err != nil {
		return nil, "", err
	}
	shown := s.showFlow(ctx, updated, s.declaredArgumentUnits(ctx))
	return map[string]any{"flow": shown}, fmt.Sprintf("Removed card %s from Flow %q.", nodeID, shown.Name), nil
}

func (s *Server) mcpRunFlow(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID := stringArgument(arguments, "flowId")
	if flowID == "" {
		return nil, "", errors.New("flowId is required")
	}
	runContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result, err := s.flows.Run(runContext, flowID)
	if err != nil {
		return nil, "", err
	}
	return map[string]any{"execution": result}, fmt.Sprintf("Ran Flow %s.", flowID), nil
}

func (s *Server) mcpDeleteFlow(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID := stringArgument(arguments, "flowId")
	if flowID == "" {
		return nil, "", errors.New("flowId is required")
	}
	flow, err := s.store.Flow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	if err := s.store.DeleteFlow(ctx, flowID); err != nil {
		return nil, "", err
	}
	return map[string]any{"deleted": true, "flowId": flowID}, fmt.Sprintf("Deleted Flow %q.", flow.Name), nil
}

func (s *Server) mutableMCPFlow(ctx context.Context, flowID string) (store.Flow, error) {
	if flowID == "" {
		return store.Flow{}, errors.New("flowId is required")
	}
	flow, err := s.store.Flow(ctx, flowID)
	if err != nil {
		return store.Flow{}, err
	}
	return cloneMCPFlow(flow)
}

func cloneMCPFlow(flow store.Flow) (store.Flow, error) {
	var cloned store.Flow
	if err := remarshal(flow, &cloned); err != nil {
		return store.Flow{}, fmt.Errorf("copy Flow: %w", err)
	}
	return cloned, nil
}

func (s *Server) normalizeAndValidateMCPNodes(ctx context.Context, nodes []store.FlowNode, requireAvailable bool) error {
	cards, err := s.mcpCardIndex(ctx)
	if err != nil {
		return err
	}
	for index := range nodes {
		node := &nodes[index]
		key := mcpCardKey(node.Step.AppID, node.Step.CardType, node.Step.CardID)
		card := cards[key]
		if card == nil {
			return fmt.Errorf("card %s/%s/%s does not exist; call flow_cards_list first", node.Step.AppID, node.Step.CardType, node.Step.CardID)
		}
		if requireAvailable {
			available, _ := card["available"].(bool)
			if !available {
				return fmt.Errorf("card %s is currently unavailable", node.Step.CardID)
			}
		}
		if node.Step.Args == nil {
			node.Step.Args = map[string]any{}
		}
		arguments, _ := card["args"].([]any)
		for _, raw := range arguments {
			argument, _ := raw.(map[string]any)
			if argument["type"] != "device" {
				continue
			}
			name, _ := argument["name"].(string)
			value := node.Step.Args[name]
			if text, ok := value.(string); ok {
				value = map[string]any{"$device": text}
				node.Step.Args[name] = value
			}
			reference, _ := value.(map[string]any)
			deviceID, _ := reference["$device"].(string)
			optional, _ := argument["optional"].(bool)
			if deviceID == "" {
				if optional {
					continue
				}
				return fmt.Errorf("card %s argument %q needs {\"$device\":\"DEVICE_ID\"}", node.Step.CardID, name)
			}
			if _, err := s.store.Device(ctx, deviceID); err != nil {
				return fmt.Errorf("card %s argument %q: %w", node.Step.CardID, name, err)
			}
			if !mcpCardAllowsDevice(card, deviceID) {
				return fmt.Errorf("card %s cannot be used with device %s", node.Step.CardID, deviceID)
			}
		}
	}
	return nil
}

func (s *Server) mcpCardIndex(ctx context.Context) (map[string]map[string]any, error) {
	groups, err := s.flowCards(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]map[string]any{}
	for _, cards := range groups {
		for _, card := range cards {
			appID, _ := card["appId"].(string)
			cardType, _ := card["type"].(string)
			cardID, _ := card["id"].(string)
			result[mcpCardKey(appID, cardType, cardID)] = card
		}
	}
	return result, nil
}

func mcpCardKey(appID, cardType, cardID string) string {
	return appID + "\x00" + cardType + "\x00" + cardID
}

func mcpCardAllowsDevice(card map[string]any, deviceID string) bool {
	if card["scope"] != "device" {
		return true
	}
	switch ids := card["deviceIds"].(type) {
	case []string:
		for _, id := range ids {
			if id == deviceID {
				return true
			}
		}
	case []any:
		for _, raw := range ids {
			if raw == deviceID {
				return true
			}
		}
	}
	return false
}

func mcpNodeIndex(nodes []store.FlowNode, id string) int {
	for index := range nodes {
		if nodes[index].ID == id {
			return index
		}
	}
	return -1
}

func stringArgument(arguments map[string]any, name string) string {
	value, _ := arguments[name].(string)
	return strings.TrimSpace(value)
}

func numberArgument(value any) (float64, bool) {
	var result float64
	switch typed := value.(type) {
	case float64:
		result = typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		result = parsed
	default:
		return 0, false
	}
	return result, !math.IsNaN(result) && !math.IsInf(result, 0)
}

func remarshal(value, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	return nil
}
