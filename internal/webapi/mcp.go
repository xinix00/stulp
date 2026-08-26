package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	flowengine "github.com/xinix00/stulp/internal/flow"
	"github.com/xinix00/stulp/internal/scene"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/stulphttp"
)

const (
	mcpProtocolVersion    = "2025-11-25"
	mcpToolTimeout        = 45 * time.Second
	maxMCPRequestBytes    = 256 << 10
	maxMCPStructuredBytes = 256 << 10
	// Leave ample room for the request metadata and counters around a projected
	// Scene result. State entries are added in order until this budget is spent.
	mcpSceneStatesBytes = 96 << 10

	// Output limits. Every string Stulp hands to a client is bounded, because
	// an app manifest and a device name are not under Stulp's control.
	mcpIDLimit    = 256
	mcpNameLimit  = 500
	mcpTextLimit  = 2048
	mcpErrorLimit = 4096

	// A group tree deeper than this is a cycle or a mistake; either way the walk
	// has to end.
	mcpMaximumGroupDepth = 16

	mcpInstructions = "Control this Stulp home through its devices and visual Flows. Start with system_context: it names the device groups (rooms) of this home. " +
		"Then narrow with devices_list (groupId, capabilityId, search) and flow_cards_list before changing anything; only set capabilities marked setable. " +
		"When a Flow needs durable boolean memory, devices_create can add a virtual_switch; use its returned device id in Flow cards. " +
		"A device with class=scene is a normal on/off scene device: the first on saves the current target states and applies the scene, repeated on keeps that original snapshot, and off restores it."
)

const (
	mcpVirtualDeviceType = "virtual_switch"
	virtualDevicesAppID  = "com.stulp.virtualdevices"
	virtualSwitchDriver  = "switch"
)

var supportedMCPVersions = [...]string{
	mcpProtocolVersion,
	"2025-06-18",
	"2025-03-26",
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

// mcpLimiter is deliberately instance-wide rather than client-wide. The MCP
// URL already carries the one access key for this Stulp, so a per-client map
// would retain attacker-controlled addresses without improving isolation.
type mcpLimiter struct {
	mu       sync.Mutex
	refilled time.Time
	tokens   float64
	active   int
}

func (s *Server) handleMCP() {
	s.mux.HandleFunc("GET /mcp/{key}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if !s.validMCPOrigin(request) {
			writeMCPError(response, stulphttp.StatusForbidden, nil, &mcpRPCError{Code: -32600, Message: "origin does not match this Stulp host"})
			return
		}
		response.Header().Set("Allow", "POST")
		writeMCPError(response, stulphttp.StatusMethodNotAllowed, nil, &mcpRPCError{Code: -32600, Message: "this stateless MCP server accepts POST only"})
	})
	s.mux.HandleFunc("POST /mcp/{key}", s.mcpPost)
	// leanhttp's UI root is intentionally a method-less fallback. This more
	// specific fallback keeps DELETE/PUT/PATCH from becoming index.html on a
	// node, while the method-specific routes above still win for GET and POST.
	s.mux.HandleFunc("/mcp/{key}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if !s.validMCPOrigin(request) {
			writeMCPError(response, stulphttp.StatusForbidden, nil, &mcpRPCError{Code: -32600, Message: "origin does not match this Stulp host"})
			return
		}
		response.Header().Set("Allow", "POST")
		writeMCPError(response, stulphttp.StatusMethodNotAllowed, nil, &mcpRPCError{Code: -32600, Message: "this MCP endpoint accepts POST only"})
	})
}

func (s *Server) mcpPost(response stulphttp.ResponseWriter, request *stulphttp.Request) {
	if !s.validMCPOrigin(request) {
		writeMCPError(response, stulphttp.StatusForbidden, nil, &mcpRPCError{Code: -32600, Message: "origin does not match this Stulp host"})
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/json" {
		writeMCPError(response, stulphttp.StatusUnsupportedMediaType, nil, &mcpRPCError{Code: -32600, Message: "MCP requests need Content-Type: application/json"})
		return
	}
	accept := request.Header.Get("Accept")
	if !mcpAccepts(accept, "application/json") || !mcpAccepts(accept, "text/event-stream") {
		writeMCPError(response, stulphttp.StatusBadRequest, nil, &mcpRPCError{Code: -32600, Message: "MCP requests must accept application/json and text/event-stream"})
		return
	}

	defer stulphttp.CloseBody(request)
	decoder := json.NewDecoder(stulphttp.LimitBody(response, request, maxMCPRequestBytes))
	decoder.UseNumber()
	var rawMessage json.RawMessage
	if err := decoder.Decode(&rawMessage); err != nil {
		writeMCPResponse(response, nil, nil, &mcpRPCError{Code: -32700, Message: "parse error", Data: err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeMCPResponse(response, nil, nil, &mcpRPCError{Code: -32600, Message: "the body must contain exactly one JSON-RPC message"})
		return
	}
	if !mcpJSONObject(rawMessage) {
		writeMCPResponse(response, nil, nil, &mcpRPCError{Code: -32600, Message: "JSON-RPC request must be an object"})
		return
	}
	var message mcpRequest
	if err := json.Unmarshal(rawMessage, &message); err != nil {
		writeMCPResponse(response, nil, nil, &mcpRPCError{Code: -32600, Message: "invalid request", Data: err.Error()})
		return
	}
	if message.JSONRPC != "2.0" || strings.TrimSpace(message.Method) == "" || !validMCPRequestID(message.ID) {
		writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32600, Message: "invalid request"})
		return
	}
	if len(message.Params) != 0 && !mcpJSONObject(message.Params) {
		writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32602, Message: "params must be an object"})
		return
	}

	// Een JSON-RPC-notificatie heeft geen id en dus geen JSON-RPC-antwoord.
	// initialized is de enige die deze stateless server zelf nodig heeft; andere
	// geldige notificaties mogen eveneens zonder toestand worden aangenomen.
	if len(message.ID) == 0 {
		response.WriteHeader(stulphttp.StatusAccepted)
		return
	}

	if version := strings.TrimSpace(request.Header.Get("MCP-Protocol-Version")); version != "" && !supportedMCPVersion(version) {
		writeMCPError(response, stulphttp.StatusBadRequest, message.ID, &mcpRPCError{
			Code: -32602, Message: "unsupported MCP-Protocol-Version",
			Data: map[string]any{"supported": supportedMCPVersions[:], "requested": version},
		})
		return
	}

	ctx := stulphttp.Context(request)
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil || params.ProtocolVersion == "" || params.Capabilities == nil || params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
			writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32602, Message: "protocolVersion, capabilities and clientInfo name/version are required"})
			return
		}
		version := mcpProtocolVersion
		if supportedMCPVersion(params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		writeMCPResponse(response, message.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "stulp", "version": s.options.StulpVersion},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		writeMCPResponse(response, message.ID, map[string]any{}, nil)
	case "tools/list":
		writeMCPResponse(response, message.ID, map[string]any{"tools": mcpTools()}, nil)
	case "tools/call":
		var params mcpCallParams
		if err := json.Unmarshal(message.Params, &params); err != nil || strings.TrimSpace(params.Name) == "" {
			writeMCPResponse(response, message.ID, nil, &mcpRPCError{Code: -32602, Message: "tool name is required"})
			return
		}
		release, retryAfter, allowed := s.beginMCPToolCall(time.Now())
		if !allowed {
			response.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeMCPError(response, stulphttp.StatusTooManyRequests, message.ID, &mcpRPCError{
				Code: -32000, Message: "MCP tool rate limit exceeded", Data: map[string]any{"retryAfterSeconds": retryAfter},
			})
			return
		}
		defer release()
		callContext, cancel := context.WithTimeout(ctx, mcpToolTimeout)
		defer cancel()
		result, known := s.callMCPTool(callContext, params.Name, params.Arguments)
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
	writeMCPResponseStatus(response, stulphttp.StatusOK, id, result, rpcError)
}

func writeMCPResponseStatus(response stulphttp.ResponseWriter, status int, id json.RawMessage, result any, rpcError *mcpRPCError) {
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
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(message)
}

func writeMCPError(response stulphttp.ResponseWriter, status int, id json.RawMessage, rpcError *mcpRPCError) {
	writeMCPResponseStatus(response, status, id, nil, rpcError)
}

func supportedMCPVersion(version string) bool {
	for _, supported := range supportedMCPVersions {
		if version == supported {
			return true
		}
	}
	return false
}

func validMCPRequestID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 || len(trimmed) > 1024 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	if trimmed[0] == '"' {
		var value string
		return json.Unmarshal(id, &value) == nil
	}
	var value json.Number
	return json.Unmarshal(id, &value) == nil
}

func mcpJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func mcpAccepts(header, mediaType string) bool {
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(entry, ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), mediaType) {
			continue
		}
		enabled := true
		for _, parameter := range parts[1:] {
			name, value, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if found && strings.EqualFold(name, "q") {
				quality, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(value), "\""), 64)
				if err != nil || quality <= 0 {
					enabled = false
				}
			}
		}
		if enabled {
			return true
		}
	}
	return false
}

func (s *Server) beginMCPToolCall(now time.Time) (release func(), retryAfter int, allowed bool) {
	const (
		burst       = 30.0
		perSecond   = 2.0
		maxParallel = 4
	)
	s.mcpLimit.mu.Lock()
	if s.mcpLimit.refilled.IsZero() {
		s.mcpLimit.refilled, s.mcpLimit.tokens = now, burst
	} else if elapsed := now.Sub(s.mcpLimit.refilled).Seconds(); elapsed > 0 {
		s.mcpLimit.tokens = math.Min(burst, s.mcpLimit.tokens+elapsed*perSecond)
		s.mcpLimit.refilled = now
	}
	if s.mcpLimit.tokens < 1 || s.mcpLimit.active >= maxParallel {
		retryAfter = 1
		if s.mcpLimit.tokens < 1 {
			retryAfter = int(math.Ceil((1 - s.mcpLimit.tokens) / perSecond))
			if retryAfter < 1 {
				retryAfter = 1
			}
		}
		s.mcpLimit.mu.Unlock()
		return func() {}, retryAfter, false
	}
	s.mcpLimit.tokens--
	s.mcpLimit.active++
	s.mcpLimit.mu.Unlock()
	return func() {
		s.mcpLimit.mu.Lock()
		s.mcpLimit.active--
		s.mcpLimit.mu.Unlock()
	}, 0, true
}

// mcpToolHandler answers one tool call: the structured value, a one-line
// summary for the text content, or an error the caller may see.
type mcpToolHandler func(*Server, context.Context, map[string]any) (any, string, error)

// mcpToolHandlers is the dispatcher, and the catalog is what it advertises. A
// test holds the two against each other so a tool cannot be advertised without
// an implementation, or implemented without being advertised.
var mcpToolHandlers = map[string]mcpToolHandler{
	"system_context":         (*Server).mcpSystemContextTool,
	"devices_list":           (*Server).mcpDevices,
	"devices_create":         (*Server).mcpCreateDevice,
	"devices_write":          (*Server).mcpWriteDevice,
	"flow_cards_list":        (*Server).mcpFlowCards,
	"flow_card_autocomplete": (*Server).mcpFlowCardAutocomplete,
	"flow_action_run":        (*Server).mcpRunFlowAction,
	"flows_list":             (*Server).mcpFlows,
	"flows_create":           (*Server).mcpCreateFlow,
	"flows_update":           (*Server).mcpUpdateFlow,
	"flows_add_cards":        (*Server).mcpAddFlowCards,
	"flows_configure_card":   (*Server).mcpConfigureFlowCard,
	"flows_connect_cards":    (*Server).mcpConnectFlowCards,
	"flows_disconnect_cards": (*Server).mcpDisconnectFlowCards,
	"flows_remove_card":      (*Server).mcpRemoveFlowCard,
	"flows_run":              (*Server).mcpRunFlow,
	"flows_delete":           (*Server).mcpDeleteFlow,
}

func (s *Server) callMCPTool(ctx context.Context, name string, arguments map[string]any) (map[string]any, bool) {
	handler, known := mcpToolHandlers[name]
	if !known {
		return nil, false
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if err := validateMCPToolArguments(name, arguments); err != nil {
		return mcpToolError(err), true
	}
	value, summary, err := handler(s, ctx, arguments)
	if err != nil {
		if value != nil {
			return mcpStructuredToolResult(value, summary, err), true
		}
		return mcpToolError(err), true
	}
	return mcpStructuredToolResult(value, summary, nil), true
}

func mcpStructuredToolResult(value any, summary string, toolErr error) map[string]any {
	structured, ok := value.(map[string]any)
	if !ok {
		structured = map[string]any{"result": value}
	}
	encoded, encodeErr := json.Marshal(structured)
	if encodeErr != nil {
		return mcpToolError(errors.Join(toolErr, fmt.Errorf("encode tool result: %w", encodeErr)))
	}
	if len(encoded) > maxMCPStructuredBytes {
		return mcpToolError(errors.Join(toolErr,
			fmt.Errorf("tool result exceeds %d KiB; use filters, pagination or an exact id", maxMCPStructuredBytes>>10)))
	}
	if toolErr != nil {
		errorText := mcpTrimString(toolErr.Error(), mcpErrorLimit)
		if summary == "" {
			summary = errorText
		} else {
			summary = mcpTrimString(summary+" Error: "+errorText, mcpErrorLimit)
		}
	}
	// structuredContent is useful to clients that expose it, but MCP explicitly
	// keeps TextContent as the compatibility path. Some connectors hand only
	// content to the model; putting a count-only summary there made every list
	// look empty even though the complete objects were present beside it. Keep
	// the serialized structured value first so even those clients receive ids,
	// names and graph contents. The concise prose remains a second block.
	content := []any{map[string]any{"type": "text", "text": string(encoded)}}
	if summary != "" {
		content = append(content, map[string]any{"type": "text", "text": summary})
	}
	return map[string]any{
		"content":           content,
		"structuredContent": json.RawMessage(encoded),
		"isError":           toolErr != nil,
	}
}

func mcpToolError(err error) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": mcpTrimString(err.Error(), mcpErrorLimit)}},
		"isError": true,
	}
}

func (s *Server) mcpSystemContextTool(ctx context.Context, _ map[string]any) (any, string, error) {
	system, err := s.store.System(ctx)
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{
		"stulpVersion": s.options.StulpVersion,
		"language":     s.options.Language,
		"timezone":     s.options.Timezone,
		"units":        system.Units.Filled(),
		"statistics":   system.Statistics,
	}
	if s.stats != nil {
		result["statisticsRunning"] = s.stats.Running()
	}
	// "Turn on the lights in the living room" is a question about groups, so the
	// groups are part of the context. They are few and small, and without them an
	// assistant has to guess which devices belong together.
	groups, err := s.mcpDeviceGroups(ctx)
	if err != nil {
		return nil, "", err
	}
	result["deviceGroups"] = groups
	return map[string]any{"context": result}, fmt.Sprintf("Read Stulp home context and %d device groups.", len(groups)), nil
}

// mcpDevices answers two different questions with one tool. Asking which
// devices exist returns summaries: identity, availability and the capability
// ids. Asking about one device (deviceId) or one capability across devices
// (capabilityId) returns the full metadata for exactly what was asked. Dumping
// every capability of every device is both the largest and the least useful
// answer, and it is the one an assistant reaches for by default.
func (s *Server) mcpDevices(ctx context.Context, arguments map[string]any) (any, string, error) {
	deviceID := stringArgument(arguments, "deviceId")
	capabilityID := stringArgument(arguments, "capabilityId")
	groups, err := s.deviceGroupIndex(ctx)
	if err != nil {
		return nil, "", err
	}
	if deviceID != "" {
		device, err := s.store.Device(ctx, deviceID)
		if err != nil {
			return nil, "", err
		}
		if capabilityID != "" && !mcpDeviceHasCapability(device, capabilityID) {
			return nil, "", fmt.Errorf("device %q has no capability %q", deviceID, capabilityID)
		}
		return map[string]any{"devices": []any{s.mcpDeviceDetail(device, capabilityID, groups)}, "total": 1},
			fmt.Sprintf("Read device %s.", device.Name), nil
	}
	devices, err := s.store.Devices(ctx, "")
	if err != nil {
		return nil, "", err
	}
	search := strings.ToLower(stringArgument(arguments, "search"))
	groupID := stringArgument(arguments, "groupId")
	if groupID != "" {
		if _, exists := groups[groupID]; !exists {
			return nil, "", fmt.Errorf("device group %q does not exist; read system_context for the groups", groupID)
		}
	}
	availableOnly, _ := arguments["availableOnly"].(bool)
	writableOnly, _ := arguments["writableOnly"].(bool)
	filtered := make([]store.Device, 0, len(devices))
	for _, device := range devices {
		if capabilityID != "" && !mcpDeviceHasCapability(device, capabilityID) {
			continue
		}
		if groupID != "" && !mcpDeviceInGroup(device, groupID, groups) {
			continue
		}
		if availableOnly && !device.Available {
			continue
		}
		if search != "" {
			haystack := device.ID + " " + device.Name + " " + device.HardwareName() + " " + mcpDeviceGroupPath(device.GroupID, groups)
			if !strings.Contains(strings.ToLower(haystack), search) {
				continue
			}
		}
		if writableOnly && !s.mcpDeviceHasWritableCapability(device, capabilityID) {
			continue
		}
		filtered = append(filtered, device)
	}
	total := len(filtered)
	offset, limit := mcpPage(arguments)
	page, next := mcpPageValues(filtered, offset, limit)
	projected := make([]any, 0, len(page))
	for _, device := range page {
		if capabilityID != "" {
			projected = append(projected, s.mcpDeviceDetail(device, capabilityID, groups))
			continue
		}
		projected = append(projected, mcpDeviceSummary(device, groups))
	}
	result := map[string]any{"devices": projected, "total": total}
	if next >= 0 {
		result["nextOffset"] = next
	}
	summary := fmt.Sprintf("Found %d matching devices; returned %d summaries. Pass deviceId for capability details.", total, len(projected))
	if capabilityID != "" {
		summary = fmt.Sprintf("Found %d devices with %s; returned %d.", total, capabilityID, len(projected))
	}
	return result, summary, nil
}

// mcpCreateDevice is deliberately narrower than the normal pairing API. An MCP
// client may ask for one advertised kind, but it cannot choose an arbitrary app,
// driver or candidate object. That keeps private device data and hardware
// pairing firmly behind each plugin's own UI while still giving Flows a durable
// boolean to use as a variable.
//
// The virtual-device plugin remains the sole owner of identity, validation and
// initial state. Running its real create/list_devices pairing conversation is
// important: duplicating that logic here would eventually create devices that
// the plugin itself could not restore.
func (s *Server) mcpCreateDevice(ctx context.Context, arguments map[string]any) (any, string, error) {
	if kind := stringArgument(arguments, "type"); kind != mcpVirtualDeviceType {
		return nil, "", fmt.Errorf("unsupported virtual device type %q", kind)
	}
	name := strings.TrimSpace(stringArgument(arguments, "name"))
	if name == "" {
		return nil, "", errors.New("virtual device name cannot be empty")
	}

	sessionID, err := randomID()
	if err != nil {
		return nil, "", fmt.Errorf("create virtual-device pairing session: %w", err)
	}
	defer func() {
		// Arm cleanup before pair.start. Its response may be lost after the app
		// already created the session; pair.close is idempotent when it did not.
		// Give it a short independent window so a canceled MCP request cannot
		// leave that session in the app until its next restart.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.supervisor.ClosePairSession(cleanup, virtualDevicesAppID, sessionID)
	}()
	handlers, err := s.supervisor.StartPairSession(ctx, virtualDevicesAppID, virtualSwitchDriver, sessionID)
	if err != nil {
		return nil, "", fmt.Errorf("start Virtual devices pairing (install and start %s first): %w", virtualDevicesAppID, err)
	}
	if !slices.Contains(handlers, "create") || !slices.Contains(handlers, "list_devices") {
		return nil, "", errors.New("the installed Virtual devices app does not support MCP-safe switch creation; update it first")
	}
	if _, err := s.supervisor.PairEmit(ctx, virtualDevicesAppID, sessionID, "create", map[string]any{"name": name}); err != nil {
		return nil, "", fmt.Errorf("prepare virtual switch: %w", err)
	}
	found, err := s.supervisor.PairEmit(ctx, virtualDevicesAppID, sessionID, "list_devices", nil)
	if err != nil {
		return nil, "", fmt.Errorf("read prepared virtual switch: %w", err)
	}
	var candidates []map[string]any
	encoded, err := json.Marshal(found)
	if err == nil {
		err = json.Unmarshal(encoded, &candidates)
	}
	if err != nil {
		return nil, "", fmt.Errorf("decode prepared virtual switch: %w", err)
	}
	if len(candidates) != 1 {
		return nil, "", fmt.Errorf("Virtual devices pairing returned %d candidates, expected exactly one", len(candidates))
	}
	created, err := s.supervisor.AddPairedDevice(ctx, virtualDevicesAppID, virtualSwitchDriver, candidates[0])
	if err != nil {
		return nil, "", fmt.Errorf("add virtual switch: %w", err)
	}
	// AddPairedDevice returns the snapshot from before device.init. Re-read it
	// so the result includes the durable onoff=false state restored by OnInit.
	created, err = s.store.Device(ctx, created.ID)
	if err != nil {
		return nil, "", fmt.Errorf("read created virtual switch: %w", err)
	}
	groups, err := s.deviceGroupIndex(ctx)
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{
		"created": true,
		"type":    mcpVirtualDeviceType,
		"device":  s.mcpDeviceDetail(created, "", groups),
	}
	return result, fmt.Sprintf("Created virtual switch %s (%s), initially off.", created.Name, created.ID), nil
}

// mcpDeviceGroups lists every room/group with its full path and how many
// devices sit in it, directly and including nested groups.
func (s *Server) mcpDeviceGroups(ctx context.Context) ([]any, error) {
	index, err := s.deviceGroupIndex(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := s.store.Devices(ctx, "")
	if err != nil {
		return nil, err
	}
	direct := make(map[string]int, len(index))
	nested := make(map[string]int, len(index))
	for _, device := range devices {
		if device.GroupID == "" {
			continue
		}
		direct[device.GroupID]++
		for _, id := range mcpGroupAncestry(device.GroupID, index) {
			nested[id]++
		}
	}
	result := make([]any, 0, len(index))
	for _, group := range index {
		projected := map[string]any{
			"id": mcpTrimString(group.ID, mcpIDLimit), "name": mcpTrimString(group.Name, mcpNameLimit),
			"path": mcpDeviceGroupPath(group.ID, index), "deviceCount": direct[group.ID],
		}
		if nested[group.ID] != direct[group.ID] {
			projected["deviceCountIncludingSubgroups"] = nested[group.ID]
		}
		if group.ParentID != "" {
			projected["parentId"] = mcpTrimString(group.ParentID, mcpIDLimit)
		}
		result = append(result, projected)
	}
	return result, nil
}

// mcpGroupAncestry is a group and every group above it, so a device in
// "Beneden / Woonkamer" counts for the living room and for downstairs.
func mcpGroupAncestry(groupID string, groups map[string]store.DeviceGroup) []string {
	ancestry := make([]string, 0, 4)
	for groupID != "" && len(ancestry) < mcpMaximumGroupDepth {
		group, exists := groups[groupID]
		if !exists {
			break
		}
		ancestry = append(ancestry, group.ID)
		groupID = group.ParentID
	}
	return ancestry
}

func (s *Server) deviceGroupIndex(ctx context.Context) (map[string]store.DeviceGroup, error) {
	groups, err := s.store.DeviceGroups(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]store.DeviceGroup, len(groups))
	for _, group := range groups {
		index[group.ID] = group
	}
	return index, nil
}

// mcpDeviceInGroup answers "is this device in the living room", including a
// device that sits in a group nested under it.
func mcpDeviceInGroup(device store.Device, groupID string, groups map[string]store.DeviceGroup) bool {
	for _, id := range mcpGroupAncestry(device.GroupID, groups) {
		if id == groupID {
			return true
		}
	}
	return false
}

// mcpDeviceSummary is what a device looks like in a list: enough to recognise it
// and to see what it can do, without a single capability lookup.
func mcpDeviceSummary(device store.Device, groups map[string]store.DeviceGroup) map[string]any {
	capabilities := make([]any, 0, len(device.Capabilities))
	for _, id := range device.Capabilities {
		capabilities = append(capabilities, mcpTrimString(id, mcpIDLimit))
	}
	result := map[string]any{
		"id": mcpTrimString(device.ID, mcpIDLimit), "name": mcpTrimString(device.Name, mcpNameLimit),
		"class": mcpTrimString(device.Class, mcpIDLimit), "available": device.Available,
		"capabilities": capabilities,
	}
	if path := mcpDeviceGroupPath(device.GroupID, groups); path != "" {
		result["group"] = path
	}
	if !device.Available && device.Message != "" {
		result["unavailableMessage"] = mcpTrimString(device.Message, mcpTextLimit)
	}
	return result
}

// mcpDeviceDetail is the answer to a targeted question: one device, or one
// capability across devices, with the metadata needed to write it.
func (s *Server) mcpDeviceDetail(device store.Device, onlyCapability string, groups map[string]store.DeviceGroup) map[string]any {
	capabilities := make(map[string]any, len(device.Capabilities))
	for _, id := range device.Capabilities {
		if onlyCapability != "" && id != onlyCapability {
			continue
		}
		value, hasValue := device.State[id]
		definition := s.capabilityObject(device, id, value)
		capability := map[string]any{
			"id": id, "title": mcpTrimString(capabilityDisplayTitle(id, definition["title"], s.options.Language), mcpNameLimit),
			"type": definition["type"], "getable": definition["getable"], "setable": definition["setable"], "hasValue": false,
		}
		if hasValue {
			// capabilityObject has already converted numeric values to household units.
			if safe, ok := mcpCapabilityValue(definition["value"]); ok {
				capability["value"], capability["hasValue"] = safe, true
			}
		}
		for _, key := range []string{"min", "max", "step", "units"} {
			if definition[key] != nil {
				capability[key] = definition[key]
			}
		}
		if values := mcpEnumValues(definition["values"], s.options.Language); len(values) > 0 {
			capability["values"] = values
		}
		capabilities[id] = capability
	}
	result := map[string]any{
		"id": mcpTrimString(device.ID, mcpIDLimit), "name": mcpTrimString(device.Name, mcpNameLimit),
		"hardwareName": mcpTrimString(device.HardwareName(), mcpNameLimit), "appId": mcpTrimString(device.AppID, mcpIDLimit),
		"class": mcpTrimString(device.Class, mcpIDLimit), "available": device.Available, "capabilities": capabilities,
	}
	if device.Message != "" {
		result["unavailableMessage"] = mcpTrimString(device.Message, mcpTextLimit)
	}
	if path := mcpDeviceGroupPath(device.GroupID, groups); path != "" {
		result["group"] = path
	}
	return result
}

func mcpDeviceGroupPath(groupID string, groups map[string]store.DeviceGroup) string {
	if groupID == "" {
		return ""
	}
	parts := make([]string, 0, 4)
	for groupID != "" && len(parts) < mcpMaximumGroupDepth {
		group, exists := groups[groupID]
		if !exists {
			break
		}
		parts = append(parts, mcpTrimString(group.Name, mcpNameLimit))
		groupID = group.ParentID
	}
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	return strings.Join(parts, " / ")
}

func mcpCapabilityValue(value any) (any, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		if len(typed) > 2048 {
			return mcpTrimString(typed, mcpTextLimit), true
		}
		return typed, true
	case int, int64, float32, float64, json.Number:
		if _, ok := mcpFiniteNumber(typed); ok {
			return typed, true
		}
	}
	return nil, false
}

func mcpDeviceHasCapability(device store.Device, id string) bool {
	for _, capability := range device.Capabilities {
		if capability == id {
			return true
		}
	}
	return false
}

func (s *Server) mcpDeviceHasWritableCapability(device store.Device, onlyCapability string) bool {
	for _, id := range device.Capabilities {
		if onlyCapability != "" && id != onlyCapability {
			continue
		}
		capability := s.capabilityObject(device, id, device.State[id])
		if setable, _ := capability["setable"].(bool); setable {
			return true
		}
	}
	return false
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
	if err := validateMCPCapabilityValue(capability, value); err != nil {
		return nil, "", fmt.Errorf("value for capability %q: %w", capabilityID, err)
	}
	canonical := s.canonicalCapabilityValue(ctx, device, capabilityID, value)
	activation, invokeErr := s.invokeCapabilityDetailed(ctx, deviceID, capabilityID, canonical, map[string]any{})
	accepted := invokeErr == nil
	if activation != nil && activation.Attempted > 0 {
		// A partial Scene result still means the request reached its state plan.
		// A pre-execution cancellation or persistence failure remains unaccepted.
		accepted = true
	}
	result := map[string]any{
		"accepted": accepted, "deviceId": deviceID, "capabilityId": capabilityID,
		"requestedValue": value,
	}
	summary := fmt.Sprintf("Accepted the %s change request for %s; the device has not necessarily reported it yet.", capabilityID, device.Name)
	if activation != nil {
		result["sceneActivation"] = mcpSceneActivationObject(*activation)
		summary = mcpSceneActivationSummary(*activation)
	}
	if reported, ok := mcpCapabilityValue(capability["value"]); ok {
		result["lastReportedValue"] = reported
	}
	if capability["units"] != nil {
		result["requestedUnits"] = capability["units"]
	}
	if invokeErr != nil {
		if activation != nil {
			return result, summary, invokeErr
		}
		return nil, "", invokeErr
	}
	return result, summary, nil
}

// mcpSceneActivationObject projects the runner's internal result through the
// same output boundary as every other MCP object. Plugin errors and imported
// Scene values are not trusted to be small, scalar or shallow.
func mcpSceneActivationObject(activation scene.ActivationResult) map[string]any {
	result := map[string]any{
		"sceneId":     mcpTrimString(activation.SceneID, mcpIDLimit),
		"sceneName":   mcpTrimString(activation.SceneName, mcpNameLimit),
		"requestedOn": activation.RequestedOn,
		"active":      activation.Active,
		"success":     activation.Success,
		"attempted":   activation.Attempted,
		"succeeded":   activation.Succeeded,
		"failed":      activation.Failed,
	}
	states := make([]any, 0, len(activation.States))
	used := 0
	for index, state := range activation.States {
		projected := map[string]any{
			"deviceId":     mcpTrimString(state.DeviceID, mcpIDLimit),
			"capabilityId": mcpTrimString(state.CapabilityID, mcpIDLimit),
			"success":      state.Success,
		}
		if state.Error != "" {
			projected["error"] = mcpTrimString(state.Error, mcpErrorLimit)
		}
		if value, ok := mcpCapabilityValue(state.Value); ok {
			projected["value"] = value
		} else if state.Value != nil {
			projected["valueOmitted"] = true
		}
		encoded, err := json.Marshal(projected)
		if err != nil || used+len(encoded) > mcpSceneStatesBytes {
			result["statesOmitted"] = len(activation.States) - index
			break
		}
		used += len(encoded)
		states = append(states, projected)
	}
	result["states"] = states
	return result
}

func mcpSceneActivationSummary(activation scene.ActivationResult) string {
	direction := "on"
	if !activation.RequestedOn {
		direction = "off"
	}
	summary := fmt.Sprintf("Scene %q requested %s; durable active=%t; %d of %d attempted states succeeded and %d failed.",
		mcpTrimString(activation.SceneName, mcpNameLimit), direction, activation.Active,
		activation.Succeeded, activation.Attempted, activation.Failed)
	switch {
	case !activation.RequestedOn && activation.Active:
		summary += " Restore is incomplete; writing onoff=false again safely retries the remaining states."
	case activation.RequestedOn && activation.Active && !activation.Success:
		summary += " The successfully applied part is active; writing onoff=false restores it."
	}
	return mcpTrimString(summary, mcpErrorLimit)
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
	// cardId turns the listing into a targeted read: only then are the argument
	// definitions worth their size, and only for the card actually being filled in.
	cardID := stringArgument(arguments, "cardId")
	appID := stringArgument(arguments, "appId")
	search := strings.ToLower(strings.TrimSpace(stringArgument(arguments, "search")))
	availableOnly := true
	if value, exists := arguments["availableOnly"].(bool); exists {
		availableOnly = value
	}
	filtered := make([]map[string]any, 0, 32)
	for _, entry := range []struct{ kind, key string }{{"trigger", "triggers"}, {"condition", "conditions"}, {"action", "actions"}} {
		if kind != "" && kind != entry.kind {
			continue
		}
		for _, card := range cards[entry.key] {
			available, _ := card["available"].(bool)
			if availableOnly && !available {
				continue
			}
			if deviceID != "" && !mcpCardAllowsDevice(card, deviceID) {
				continue
			}
			if cardID != "" && fmt.Sprint(card["id"]) != cardID {
				continue
			}
			if appID != "" && fmt.Sprint(card["appId"]) != appID {
				continue
			}
			haystack := strings.ToLower(fmt.Sprint(card["id"]) + " " + fmt.Sprint(card["title"]) + " " + fmt.Sprint(card["appName"]))
			if search != "" && !strings.Contains(haystack, search) {
				continue
			}
			filtered = append(filtered, card)
		}
	}
	total := len(filtered)
	offset, limit := mcpPage(arguments)
	filtered, next := mcpPageValues(filtered, offset, limit)
	projected := make([]any, 0, len(filtered))
	for _, card := range filtered {
		projected = append(projected, mcpCardObject(card, s.options.Language, cardID != ""))
	}
	result := map[string]any{"cards": projected, "total": total}
	if next >= 0 {
		result["nextOffset"] = next
	}
	if cardID != "" {
		return result, fmt.Sprintf("Read %d Flow cards with id %s.", len(projected), cardID), nil
	}
	return result, fmt.Sprintf("Found %d matching Flow cards; returned %d titles. Pass cardId for its arguments and tokens.", total, len(projected)), nil
}

// mcpCardObject projects one card. A listing shows what a card is and whether
// it can be used; the argument and token definitions -- the bulk of a card, and
// a manifest's own vocabulary -- follow only for a card asked for by id.
func mcpCardObject(card map[string]any, language string, detail bool) map[string]any {
	result := make(map[string]any, 14)
	for _, key := range []string{"appId", "id", "type"} {
		if value, _ := card[key].(string); value != "" {
			result[key] = mcpTrimString(value, mcpIDLimit)
		}
	}
	for _, key := range []string{"appName", "title"} {
		if value, _ := card[key].(string); value != "" {
			result[key] = mcpTrimString(value, mcpTextLimit)
		}
	}
	if available, ok := card["available"].(bool); ok {
		result["available"] = available
	}
	if !detail {
		return result
	}
	for _, key := range []string{"scope", "deviceArgument", "capability"} {
		if value, _ := card[key].(string); value != "" {
			result[key] = mcpTrimString(value, mcpIDLimit)
		}
	}
	for _, key := range []string{"titleFormatted", "hint"} {
		if value, _ := card[key].(string); value != "" {
			result[key] = mcpTrimString(value, mcpTextLimit)
		}
	}
	if args := mcpPublicDefinitions(card["args"], language); len(args) > 0 {
		result["args"] = args
	}
	if tokens := mcpPublicDefinitions(card["tokens"], language); len(tokens) > 0 {
		result["tokens"] = tokens
	}
	return result
}

func mcpPublicDefinitions(raw any, language string) []any {
	values, _ := raw.([]any)
	result := make([]any, 0, len(values))
	for _, rawValue := range values {
		value, _ := rawValue.(map[string]any)
		if value == nil {
			continue
		}
		projected := make(map[string]any, 12)
		for _, key := range []string{"name", "type"} {
			if text, _ := value[key].(string); text != "" {
				projected[key] = mcpTrimString(text, mcpIDLimit)
			}
		}
		for _, key := range []string{"title", "hint", "placeholder", "filter"} {
			if text := localized(value[key], language); text != "" {
				projected[key] = mcpTrimString(text, mcpTextLimit)
			}
		}
		if optional, ok := value["optional"].(bool); ok {
			projected["optional"] = optional
		}
		for _, key := range []string{"min", "max", "step"} {
			if number, ok := mcpFiniteNumber(value[key]); ok {
				projected[key] = number
			}
		}
		if units := localized(value["units"], language); units != "" {
			projected["units"] = mcpTrimString(units, mcpIDLimit)
		}
		if values := mcpEnumValues(value["values"], language); len(values) > 0 {
			projected["values"] = values
		}
		if len(projected) > 0 {
			result = append(result, projected)
		}
	}
	return result
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
	cards, err := s.mcpCardIndex(ctx)
	if err != nil {
		return nil, "", err
	}
	card := cards[mcpCardKey(appID, cardType, cardID)]
	if card == nil {
		return nil, "", fmt.Errorf("card %s/%s/%s does not exist; call flow_cards_list first", appID, cardType, cardID)
	}
	if available, _ := card["available"].(bool); !available {
		return nil, "", fmt.Errorf("card %s is currently unavailable", cardID)
	}
	argumentFound := false
	definitions, _ := card["args"].([]any)
	knownArguments := make(map[string]map[string]any, len(definitions))
	for _, raw := range definitions {
		definition, _ := raw.(map[string]any)
		name, _ := definition["name"].(string)
		if name != "" {
			knownArguments[name] = definition
		}
		if definition["name"] == argument && definition["type"] == "autocomplete" {
			argumentFound = true
		}
	}
	if !argumentFound {
		return nil, "", fmt.Errorf("card %s has no autocomplete argument %q", cardID, argument)
	}
	for name, value := range args {
		definition := knownArguments[name]
		if definition == nil {
			return nil, "", fmt.Errorf("card %s has no argument %q", cardID, name)
		}
		if definition["type"] != "device" {
			continue
		}
		if text, ok := value.(string); ok {
			value = map[string]any{"$device": strings.TrimSpace(text)}
			args[name] = value
		}
		reference, _ := value.(map[string]any)
		deviceID, _ := reference["$device"].(string)
		if strings.TrimSpace(deviceID) == "" || len(reference) != 1 {
			return nil, "", fmt.Errorf("card %s argument %q needs {\"$device\":\"DEVICE_ID\"}", cardID, name)
		}
		if _, err := s.store.Device(ctx, deviceID); err != nil {
			return nil, "", fmt.Errorf("card %s argument %q: %w", cardID, name, err)
		}
		if !mcpCardAllowsDevice(card, deviceID) {
			return nil, "", fmt.Errorf("card %s cannot be used with device %s", cardID, deviceID)
		}
	}
	values, err := s.supervisor.InvokeFlowAutocomplete(ctx, appID, cardType, cardID, argument, stringArgument(arguments, "query"), args)
	if err != nil {
		return nil, "", err
	}
	bounded := mcpAutocompleteValues(values, 50)
	return map[string]any{"values": bounded, "total": len(bounded)}, fmt.Sprintf("Found %d autocomplete choices.", len(bounded)), nil
}

func mcpAutocompleteValues(raw any, limit int) []any {
	values := make([]map[string]any, 0, limit)
	switch typed := raw.(type) {
	case []any:
		for _, rawValue := range typed {
			if value, ok := rawValue.(map[string]any); ok {
				values = append(values, value)
			}
			if len(values) == limit {
				break
			}
		}
	case []map[string]any:
		if len(typed) > limit {
			typed = typed[:limit]
		}
		values = append(values, typed...)
	}
	result := make([]any, 0, len(values))
	for _, value := range values {
		id, _ := value["id"].(string)
		name, _ := value["name"].(string)
		if id == "" || name == "" {
			continue
		}
		item := map[string]any{"id": mcpTrimString(id, mcpNameLimit), "name": mcpTrimString(name, mcpNameLimit)}
		if description, _ := value["description"].(string); description != "" {
			item["description"] = mcpTrimString(description, mcpTextLimit)
		}
		result = append(result, item)
	}
	return result
}

func mcpTrimString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "")
}

func (s *Server) mcpRunFlowAction(ctx context.Context, arguments map[string]any) (any, string, error) {
	step := store.FlowStep{
		AppID: stringArgument(arguments, "appId"), CardID: stringArgument(arguments, "cardId"),
		CardType: "action",
	}
	step.Args, _ = arguments["args"].(map[string]any)
	if mcpValueHoldsToken(step.Args) {
		return nil, "", errors.New("flow_action_run cannot use Flow tokens because an immediate action has no trigger values")
	}
	nodes := []store.FlowNode{{Step: step}}
	if err := s.normalizeAndValidateMCPNodes(ctx, nodes, true); err != nil {
		return nil, "", err
	}
	canonical := s.canonicalFlow(ctx, store.Flow{Nodes: nodes}, nil)
	result, err := s.flows.RunAction(ctx, canonical.Nodes[0].Step)
	if err != nil {
		return nil, "", err
	}
	return map[string]any{"action": mcpStepResultObject(result)}, fmt.Sprintf("Ran action %s/%s.", step.AppID, step.CardID), nil
}

func (s *Server) mcpFlows(ctx context.Context, arguments map[string]any) (any, string, error) {
	flowID := stringArgument(arguments, "flowId")
	if flowID != "" {
		flow, err := s.store.Flow(ctx, flowID)
		if err != nil {
			return nil, "", err
		}
		cards, err := s.mcpCardIndex(ctx)
		if err != nil {
			return nil, "", err
		}
		return map[string]any{"flows": []any{s.mcpFlowObject(ctx, flow, cards)}, "total": 1}, "Found 1 Flow.", nil
	}
	flows, err := s.store.FlowSummaries(ctx)
	if err != nil {
		return nil, "", err
	}
	search := strings.ToLower(stringArgument(arguments, "search"))
	enabled, filterEnabled := arguments["enabled"].(bool)
	filtered := make([]store.FlowSummary, 0, len(flows))
	for _, flow := range flows {
		if search != "" && !strings.Contains(strings.ToLower(flow.ID+" "+flow.Name), search) {
			continue
		}
		if filterEnabled && flow.Enabled != enabled {
			continue
		}
		filtered = append(filtered, flow)
	}
	total := len(filtered)
	offset, limit := mcpPage(arguments)
	filtered, next := mcpPageValues(filtered, offset, limit)
	summaries := make([]any, 0, len(filtered))
	for _, flow := range filtered {
		summaries = append(summaries, mcpFlowSummary(flow))
	}
	result := map[string]any{"flows": summaries, "total": total}
	if next >= 0 {
		result["nextOffset"] = next
	}
	return result, fmt.Sprintf("Found %d matching Flows; returned %d summaries.", total, len(summaries)), nil
}

func mcpFlowSummary(flow store.FlowSummary) map[string]any {
	result := map[string]any{
		"id": flow.ID, "name": flow.Name, "enabled": flow.Enabled,
		"nodeCount": flow.NodeCount, "edgeCount": flow.EdgeCount,
		"createdAt": flow.CreatedAt, "updatedAt": flow.UpdatedAt,
	}
	if flow.LastRunAt != "" {
		result["lastRunAt"] = flow.LastRunAt
	}
	if flow.LastError != "" {
		result["lastError"] = mcpTrimString(flow.LastError, mcpTextLimit)
	}
	return result
}

func (s *Server) mcpFlowObject(ctx context.Context, flow store.Flow, cards map[string]map[string]any) map[string]any {
	shown := s.showFlow(ctx, flow, s.declaredArgumentUnits(ctx))
	nodes := make([]any, 0, len(shown.Nodes))
	for _, node := range shown.Nodes {
		step := map[string]any{"appId": node.Step.AppID, "cardId": node.Step.CardID, "cardType": node.Step.CardType}
		card := cards[mcpCardKey(node.Step.AppID, node.Step.CardType, node.Step.CardID)]
		if args := mcpPublicFlowArgs(node.Step.Args, card); len(args) > 0 {
			step["args"] = args
		}
		if node.Step.Inverted {
			step["inverted"] = true
		}
		nodes = append(nodes, map[string]any{"id": node.ID, "x": node.X, "y": node.Y, "step": step})
	}
	edges := make([]any, 0, len(shown.Edges))
	for _, edge := range shown.Edges {
		edges = append(edges, map[string]any{"id": edge.ID, "from": edge.From, "to": edge.To})
	}
	result := mcpFlowSummary(shown.Summary())
	result["nodes"], result["edges"] = nodes, edges
	return result
}

func mcpPublicFlowArgs(args map[string]any, card map[string]any) map[string]any {
	result := map[string]any{}
	definitions, _ := card["args"].([]any)
	for _, raw := range definitions {
		definition, _ := raw.(map[string]any)
		name, _ := definition["name"].(string)
		value, exists := args[name]
		if name == "" || !exists {
			continue
		}
		switch definition["type"] {
		case "device":
			reference, _ := value.(map[string]any)
			if deviceID, _ := reference["$device"].(string); deviceID != "" {
				result[name] = map[string]any{"$device": deviceID}
			}
		case "autocomplete":
			choice, _ := value.(map[string]any)
			projected := map[string]any{}
			for _, key := range []string{"id", "name", "description"} {
				if safe := mcpSafeValue(choice[key], 0); safe != nil && safe != "" {
					projected[key] = safe
				}
			}
			if projected["id"] != nil {
				result[name] = projected
			}
		default:
			if safe := mcpSafeValue(value, 0); safe != nil {
				result[name] = safe
			}
		}
	}
	return result
}

// editMCPFlow is the one way an MCP tool changes a stored Flow: read it, let the
// tool change that copy, save it against the revision it was read at. Every Flow
// tool therefore behaves the same way -- same result shape, same conflict
// handling, same closing line about what the Flow still needs.
func (s *Server) updateMCPFlow(ctx context.Context, definition store.Flow, expectedRevision uint64) (store.Flow, error) {
	updated, err := s.store.UpdateFlowIfUnchanged(ctx, definition, expectedRevision)
	if errors.Is(err, store.ErrFlowChanged) {
		return store.Flow{}, errors.New("Flow changed concurrently; read it again with flows_list and retry the edit")
	}
	return updated, err
}
func (s *Server) editMCPFlow(ctx context.Context, flowID, action string,
	edit func(flow *store.Flow) (map[string]any, error)) (any, string, error) {
	if flowID == "" {
		return nil, "", errors.New("flowId is required")
	}
	previous, err := s.store.Flow(ctx, flowID)
	if err != nil {
		return nil, "", err
	}
	definition := previous
	delta, err := edit(&definition)
	if err != nil {
		return nil, "", err
	}
	definition = s.canonicalFlow(ctx, definition, &previous)
	updated, err := s.updateMCPFlow(ctx, definition, previous.Revision)
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{"flow": mcpFlowSummary(updated.Summary())}
	for name, value := range delta {
		result[name] = value
	}
	return result, fmt.Sprintf("%s in Flow %q. %s", action, updated.Name, mcpFlowNextStep(updated)), nil
}

// mcpFlowNextStep says what a Flow still needs. A Flow is built one card at a
// time, so an incomplete Flow is stored and reported rather than refused -- but
// the caller has to hear that it will not run yet.
func mcpFlowNextStep(flow store.Flow) string {
	if runnable, missing := flow.Runnable(); !runnable {
		return "It cannot run yet: " + missing + "."
	}
	if !flow.Enabled {
		return "It is complete but disabled; enable it with flows_update."
	}
	return "It is complete and enabled."
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
	return map[string]any{"flow": mcpFlowSummary(created.Summary())},
		fmt.Sprintf("Created Flow %q (%s). %s", created.Name, created.ID, mcpFlowNextStep(created)), nil
}

func (s *Server) mcpUpdateFlow(ctx context.Context, arguments map[string]any) (any, string, error) {
	return s.editMCPFlow(ctx, stringArgument(arguments, "flowId"), "Updated the settings", func(flow *store.Flow) (map[string]any, error) {
		changed := false
		if name, exists := arguments["name"]; exists {
			text, ok := name.(string)
			if !ok {
				return nil, errors.New("name must be a string")
			}
			flow.Name, changed = text, true
		}
		if enabled, exists := arguments["enabled"]; exists {
			value, ok := enabled.(bool)
			if !ok {
				return nil, errors.New("enabled must be a boolean")
			}
			flow.Enabled, changed = value, true
		}
		if !changed {
			return nil, errors.New("provide name and/or enabled")
		}
		return nil, nil
	})
}

// mcpAddFlowCards adds cards, and optionally the connections between them. One
// card per call is how a person builds a Flow on the canvas; several at once is
// how a caller that already knows the shape does it. Same argument either way,
// so there is one code path and one thing to learn.
func (s *Server) mcpAddFlowCards(ctx context.Context, arguments map[string]any) (any, string, error) {
	return s.editMCPFlow(ctx, stringArgument(arguments, "flowId"), "Added cards", func(flow *store.Flow) (map[string]any, error) {
		var added []store.FlowNode
		if err := remarshal(arguments["nodes"], &added); err != nil {
			return nil, fmt.Errorf("nodes: %w", err)
		}
		if len(added) == 0 {
			return nil, errors.New("nodes needs at least one card")
		}
		var edges []store.FlowEdge
		if arguments["edges"] != nil {
			if err := remarshal(arguments["edges"], &edges); err != nil {
				return nil, fmt.Errorf("edges: %w", err)
			}
		}
		first := len(flow.Nodes)
		flow.Nodes = append(flow.Nodes, added...)
		flow.Edges = append(flow.Edges, edges...)
		if err := s.normalizeAndValidateMCPNodes(ctx, flow.Nodes[first:], true); err != nil {
			return nil, err
		}
		ids := make([]any, 0, len(added))
		for _, node := range added {
			ids = append(ids, node.ID)
		}
		return map[string]any{"nodeIds": ids}, nil
	})
}

func (s *Server) mcpConfigureFlowCard(ctx context.Context, arguments map[string]any) (any, string, error) {
	nodeID := stringArgument(arguments, "nodeId")
	return s.editMCPFlow(ctx, stringArgument(arguments, "flowId"), "Configured card "+nodeID, func(flow *store.Flow) (map[string]any, error) {
		index := mcpNodeIndex(flow.Nodes, nodeID)
		if nodeID == "" || index < 0 {
			return nil, fmt.Errorf("Flow %q has no card %q", flow.ID, nodeID)
		}
		node := &flow.Nodes[index]
		cards, err := s.mcpCardIndex(ctx)
		if err != nil {
			return nil, err
		}
		card := cards[mcpCardKey(node.Step.AppID, node.Step.CardType, node.Step.CardID)]
		if card == nil {
			return nil, fmt.Errorf("card %s/%s/%s does not exist; call flow_cards_list first", node.Step.AppID, node.Step.CardType, node.Step.CardID)
		}
		changed := false
		if raw, exists := arguments["args"]; exists {
			args, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("args must be an object")
			}
			if node.Step.Args == nil {
				node.Step.Args = map[string]any{}
			}
			for name, value := range args {
				if value == nil {
					delete(node.Step.Args, name)
					continue
				}
				node.Step.Args[name] = value
			}
			changed = true
		}
		if raw, exists := arguments["inverted"]; exists {
			value, ok := raw.(bool)
			if !ok {
				return nil, errors.New("inverted must be a boolean")
			}
			node.Step.Inverted, changed = value, true
		}
		for _, axis := range []struct {
			name  string
			field *float64
		}{{"x", &node.X}, {"y", &node.Y}} {
			raw, exists := arguments[axis.name]
			if !exists {
				continue
			}
			value, ok := numberArgument(raw)
			if !ok {
				return nil, fmt.Errorf("%s must be a number", axis.name)
			}
			*axis.field, changed = value, true
		}
		if !changed {
			return nil, errors.New("provide args, inverted, x and/or y")
		}
		// The incoming args are checked against the card; args already stored are
		// not, so a card whose app added or renamed an argument stays editable.
		if err := s.validateMCPCardArguments(ctx, node, card, true); err != nil {
			return nil, err
		}
		return map[string]any{"nodeId": nodeID}, nil
	})
}

func (s *Server) mcpConnectFlowCards(ctx context.Context, arguments map[string]any) (any, string, error) {
	from, to := stringArgument(arguments, "fromNodeId"), stringArgument(arguments, "toNodeId")
	return s.editMCPFlow(ctx, stringArgument(arguments, "flowId"), fmt.Sprintf("Connected %s to %s", from, to), func(flow *store.Flow) (map[string]any, error) {
		if from == "" || to == "" {
			return nil, errors.New("fromNodeId and toNodeId are required")
		}
		edge := store.FlowEdge{ID: stringArgument(arguments, "edgeId"), From: from, To: to}
		flow.Edges = append(flow.Edges, edge)
		if edge.ID == "" {
			return nil, nil
		}
		return map[string]any{"edgeId": edge.ID}, nil
	})
}

func (s *Server) mcpDisconnectFlowCards(ctx context.Context, arguments map[string]any) (any, string, error) {
	edgeID := stringArgument(arguments, "edgeId")
	from, to := stringArgument(arguments, "fromNodeId"), stringArgument(arguments, "toNodeId")
	return s.editMCPFlow(ctx, stringArgument(arguments, "flowId"), "Disconnected cards", func(flow *store.Flow) (map[string]any, error) {
		if edgeID == "" && (from == "" || to == "") {
			return nil, errors.New("provide edgeId, or fromNodeId plus toNodeId")
		}
		kept := make([]store.FlowEdge, 0, len(flow.Edges))
		removed := 0
		for _, edge := range flow.Edges {
			if edgeID != "" && edge.ID == edgeID || edgeID == "" && edge.From == from && edge.To == to {
				removed++
				continue
			}
			kept = append(kept, edge)
		}
		if removed == 0 {
			return nil, errors.New("connection does not exist")
		}
		flow.Edges = kept
		return map[string]any{"removedConnections": removed}, nil
	})
}

func (s *Server) mcpRemoveFlowCard(ctx context.Context, arguments map[string]any) (any, string, error) {
	nodeID := stringArgument(arguments, "nodeId")
	return s.editMCPFlow(ctx, stringArgument(arguments, "flowId"), "Removed card "+nodeID, func(flow *store.Flow) (map[string]any, error) {
		if nodeID == "" || mcpNodeIndex(flow.Nodes, nodeID) < 0 {
			return nil, fmt.Errorf("Flow %q has no card %q", flow.ID, nodeID)
		}
		nodes := make([]store.FlowNode, 0, len(flow.Nodes)-1)
		for _, node := range flow.Nodes {
			if node.ID != nodeID {
				nodes = append(nodes, node)
			}
		}
		// A card takes its connections with it; leaving them would reference a
		// card that is gone.
		edges := make([]store.FlowEdge, 0, len(flow.Edges))
		for _, edge := range flow.Edges {
			if edge.From != nodeID && edge.To != nodeID {
				edges = append(edges, edge)
			}
		}
		flow.Nodes, flow.Edges = nodes, edges
		return map[string]any{"removedNodeId": nodeID}, nil
	})
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
	return map[string]any{"execution": mcpRunResultObject(result)}, fmt.Sprintf("Ran Flow %s.", flowID), nil
}

func mcpRunResultObject(result flowengine.RunResult) map[string]any {
	conditions := make([]any, 0, len(result.Conditions))
	for _, step := range result.Conditions {
		conditions = append(conditions, mcpStepResultObject(step))
	}
	actions := make([]any, 0, len(result.Actions))
	for _, step := range result.Actions {
		actions = append(actions, mcpStepResultObject(step))
	}
	projected := map[string]any{
		"flowId": result.FlowID, "success": result.Success, "stopped": result.Stopped,
		"conditions": conditions, "actions": actions, "ranAt": result.RanAt,
	}
	if result.Error != "" {
		projected["error"] = mcpTrimString(result.Error, mcpErrorLimit)
	}
	return projected
}

func mcpStepResultObject(result flowengine.StepResult) map[string]any {
	projected := map[string]any{"appId": result.AppID, "cardId": result.CardID, "cardType": result.CardType}
	if result.Passed != nil {
		projected["passed"] = *result.Passed
	}
	if result.Result != nil {
		if safe := mcpSafeValueLimit(result.Result, 0, 512, 8<<10); safe != nil {
			projected["result"] = safe
		} else {
			projected["resultOmitted"] = true
		}
	}
	return projected
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

// normalizeAndValidateMCPNodes checks cards the caller just supplied: the card
// exists, its app is running, and its arguments match what the card declares.
func (s *Server) normalizeAndValidateMCPNodes(ctx context.Context, nodes []store.FlowNode, requireAvailable bool) error {
	cards, err := s.mcpCardIndex(ctx)
	if err != nil {
		return err
	}
	for index := range nodes {
		node := &nodes[index]
		card := cards[mcpCardKey(node.Step.AppID, node.Step.CardType, node.Step.CardID)]
		if card == nil {
			return fmt.Errorf("card %s/%s/%s does not exist; call flow_cards_list first", node.Step.AppID, node.Step.CardType, node.Step.CardID)
		}
		if requireAvailable {
			if available, _ := card["available"].(bool); !available {
				return fmt.Errorf("card %s is currently unavailable", node.Step.CardID)
			}
		}
		if node.Step.Args == nil {
			node.Step.Args = map[string]any{}
		}
		if err := s.validateMCPCardArguments(ctx, node, card, !requireAvailable); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) mcpCardIndex(ctx context.Context) (map[string]map[string]any, error) {
	groups, err := s.flowCards(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]map[string]any, 64)
	for _, key := range []string{"triggers", "conditions", "actions"} {
		for _, card := range groups[key] {
			appID, _ := card["appId"].(string)
			cardID, _ := card["id"].(string)
			cardType, _ := card["type"].(string)
			if appID == "" || cardID == "" || cardType == "" {
				continue
			}
			index[mcpCardKey(appID, cardType, cardID)] = card
		}
	}
	return index, nil
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
