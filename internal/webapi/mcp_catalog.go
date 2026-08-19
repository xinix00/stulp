package webapi

import "encoding/json"

// The catalog is the single source of truth for what MCP may do: tools/list
// serves it and validateMCPToolArguments enforces it. It is built once and kept
// encoded, so a client calling tools/list does not rebuild a few hundred maps.
var (
	mcpToolCatalog      = buildMCPToolCatalog()
	mcpToolCatalogJSON  = mustEncodeMCPCatalog(mcpToolCatalog)
	mcpToolInputSchemas = func() map[string]map[string]any {
		schemas := make(map[string]map[string]any, len(mcpToolCatalog))
		for _, tool := range mcpToolCatalog {
			name, _ := tool["name"].(string)
			schema, _ := tool["inputSchema"].(map[string]any)
			if name == "" || schema == nil || schemas[name] != nil {
				panic("invalid or duplicate tool in the MCP catalog")
			}
			schemas[name] = schema
		}
		return schemas
	}()
)

func mcpTools() *json.RawMessage { return &mcpToolCatalogJSON }

func mustEncodeMCPCatalog(tools []map[string]any) json.RawMessage {
	encoded, err := json.Marshal(tools)
	if err != nil {
		panic("encode MCP tool catalog: " + err.Error())
	}
	return encoded
}

// The hints are advisory metadata for the client, not enforcement: what a tool
// may actually do is decided in callMCPTool.
var (
	mcpRead       = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	mcpReadRemote = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true}
	mcpAdd        = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}
	mcpChange     = map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false}
	mcpAct        = map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true}
)

func buildMCPToolCatalog() []map[string]any {
	page := map[string]any{
		"offset": map[string]any{"type": "integer", "minimum": 0, "default": 0},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": mcpMaximumPageLimit, "default": mcpDefaultPageLimit},
	}
	return []map[string]any{
		mcpTool("system_context", "Read home context", mcpRead,
			"Read Stulp's language, timezone, household units, version, statistics state and the device groups (rooms) of this home. "+
				"Read it before anything that names a room, and before time- or unit-sensitive Flows.",
			mcpObject(nil)),

		mcpTool("devices_list", "Read devices", mcpRead,
			"Find devices. Without deviceId or capabilityId this returns compact summaries: identity, availability and the capability ids a device has. "+
				"Pass deviceId for one device with full capability metadata, or capabilityId to read that one capability across devices. "+
				"Only capabilities with setable=true may be changed.",
			mcpObject(mcpFields(page,
				"deviceId", mcpID("One exact device, with full capability metadata"),
				"capabilityId", mcpID("Only devices with this capability, and only this capability in the result"),
				"groupId", mcpID("Only devices in this group or one nested under it; read system_context for the groups"),
				"search", mcpText("Case-insensitive name, id or group search", 500),
				"availableOnly", mcpFlag("Skip devices the app reports as unavailable", false),
				"writableOnly", mcpFlag("Only devices with a setable capability", false),
			))),

		mcpTool("devices_write", "Set a device capability", mcpAct,
			"Request one writable capability change. Read devices_list first and use only setable=true. "+
				"Values use household units; accepted does not mean the device has already reported the new state.",
			mcpRequired(mcpObject(map[string]any{
				"deviceId":     mcpID("Exact device id"),
				"capabilityId": mcpID("Exact capability id"),
				"value":        map[string]any{"description": "Value matching the capability type, range or enum"},
			}), "deviceId", "capabilityId", "value")),

		mcpTool("flow_cards_list", "Read Flow cards", mcpRead,
			"Find ALS/trigger, EN/condition and DAN/action cards. Without cardId this lists titles only; "+
				"pass cardId to read one card's arguments and tokens before configuring it. "+
				"Device arguments use {\"$device\":\"DEVICE_ID\"}; dropdowns store an id and tokens use {{token}}.",
			mcpObject(mcpFields(page,
				"kind", map[string]any{"type": "string", "enum": []any{"trigger", "condition", "action"}},
				"cardId", mcpID("One exact card id, with its argument and token definitions"),
				"appId", mcpID("Only cards from this app"),
				"deviceId", mcpID("Only cards usable with this device, plus global cards"),
				"search", mcpText("Case-insensitive card id/title search", 500),
				"availableOnly", mcpFlag("Skip cards whose app is not running", true),
			))),

		mcpTool("flow_card_autocomplete", "Complete a Flow card argument", mcpReadRemote,
			"Get bounded choices for one autocomplete argument. args is the current step.args object; store the entire selected choice under args[argument].",
			mcpRequired(mcpObject(map[string]any{
				"appId":    mcpID("Card appId"),
				"cardId":   mcpID("Card id"),
				"cardType": map[string]any{"type": "string", "enum": []any{"trigger", "device-trigger", "condition", "action"}},
				"argument": mcpID("Autocomplete argument name"),
				"query":    mcpText("Search text", 500),
				"args":     mcpFreeObject("Current step.args values"),
			}), "appId", "cardId", "cardType", "argument")),

		mcpTool("flow_action_run", "Run one action card", mcpAct,
			"Run one available DAN/action card immediately without persisting a Flow. This can control physical devices, contact external services or send notifications.",
			mcpRequired(mcpObject(map[string]any{
				"appId":  mcpID("Card appId from flow_cards_list"),
				"cardId": mcpID("Action card id from flow_cards_list"),
				"args":   mcpFreeObject(""),
			}), "appId", "cardId", "args")),

		mcpTool("flows_list", "Read Flows", mcpRead,
			"Read a bounded page of Flow summaries. With flowId, return one full graph in the same flows array, including positions, public args and edges; hidden runtime state is omitted.",
			mcpObject(mcpFields(page,
				"flowId", mcpID("Optional exact Flow id"),
				"search", mcpText("Case-insensitive name or id search", 500),
				"enabled", map[string]any{"type": "boolean"},
			))),

		mcpTool("flows_create", "Create a Flow", mcpAdd,
			"Create a Flow, empty or complete. Build it further with flows_add_cards and flows_connect_cards, in any order. "+
				"A Flow only runs once a path leads from an ALS/trigger card to a DAN/action card; every Flow tool reports what is still missing.",
			mcpRequired(mcpObject(map[string]any{
				"name":    mcpText("Flow name", 160),
				"enabled": mcpFlag("", true),
				"nodes":   mcpDefault(mcpArray(mcpFlowNodeSchema(), 0, 128), []any{}),
				"edges":   mcpDefault(mcpArray(mcpFlowEdgeSchema(), 0, 256), []any{}),
			}), "name")),

		mcpTool("flows_update", "Update Flow settings", mcpChange,
			"Rename a Flow and/or enable or disable it without replacing its cards.",
			mcpRequired(mcpObject(map[string]any{
				"flowId":  mcpID("Exact Flow id"),
				"name":    mcpText("New name", 160),
				"enabled": map[string]any{"type": "boolean"},
			}), "flowId")),

		mcpTool("flows_add_cards", "Add Flow cards", mcpAdd,
			"Add one card, or several at once, at their x/y canvas positions. Read the card with flow_cards_list first. "+
				"You choose each card's id, so edges can reference it here or in a later flows_connect_cards.",
			mcpRequired(mcpObject(map[string]any{
				"flowId": mcpID("Exact Flow id"),
				"nodes":  mcpArray(mcpFlowNodeSchema(), 1, 128),
				"edges":  mcpDefault(mcpArray(mcpFlowEdgeSchema(), 0, 256), []any{}),
			}), "flowId", "nodes")),

		mcpTool("flows_configure_card", "Configure a Flow card", mcpChange,
			"Merge args into one card and/or change inverted and its x/y position. Omitted args remain unchanged; an argument set to null is cleared.",
			mcpRequired(mcpObject(map[string]any{
				"flowId":   mcpID("Exact Flow id"),
				"nodeId":   mcpID("Exact card/node id"),
				"args":     mcpFreeObject(""),
				"inverted": map[string]any{"type": "boolean"},
				"x":        map[string]any{"type": "number"},
				"y":        map[string]any{"type": "number"},
			}), "flowId", "nodeId")),

		mcpTool("flows_connect_cards", "Connect Flow cards", mcpAdd,
			"Add an acyclic connection from one card to another; a connection cannot point into a trigger.",
			mcpRequired(mcpObject(map[string]any{
				"flowId":     mcpID("Exact Flow id"),
				"fromNodeId": mcpID("Source card id"),
				"toNodeId":   mcpID("Destination card id"),
				"edgeId":     mcpID("Optional connection id"),
			}), "flowId", "fromNodeId", "toNodeId")),

		mcpTool("flows_disconnect_cards", "Disconnect Flow cards", mcpChange,
			"Remove a connection by edgeId, or by fromNodeId plus toNodeId.",
			mcpRequired(mcpObject(map[string]any{
				"flowId":     mcpID("Exact Flow id"),
				"edgeId":     mcpID("Connection id"),
				"fromNodeId": mcpID("Source card id"),
				"toNodeId":   mcpID("Destination card id"),
			}), "flowId")),

		mcpTool("flows_remove_card", "Remove a Flow card", mcpChange,
			"Remove one card and every connection touching it. The Flow must remain valid.",
			mcpRequired(mcpObject(map[string]any{
				"flowId": mcpID("Exact Flow id"),
				"nodeId": mcpID("Exact card/node id"),
			}), "flowId", "nodeId")),

		mcpTool("flows_run", "Run a Flow", mcpAct,
			"Execute reachable conditions/actions immediately, ignoring triggers and enabled state. This can control devices, contact services or send notifications.",
			mcpRequired(mcpObject(map[string]any{"flowId": mcpID("Exact Flow id")}), "flowId")),

		mcpTool("flows_delete", "Delete a Flow", mcpChange,
			"Permanently delete a Flow.",
			mcpRequired(mcpObject(map[string]any{"flowId": mcpID("Exact Flow id")}), "flowId")),
	}
}

func mcpTool(name, title string, annotations map[string]any, description string, schema map[string]any) map[string]any {
	return map[string]any{
		"name": name, "title": title, "description": description,
		"inputSchema": schema, "annotations": annotations,
	}
}

// mcpID is the shape of every identifier crossing this API: a bounded,
// non-empty string that must match something Stulp already advertised.
func mcpID(description string) map[string]any {
	return mcpText(description, mcpMaximumObjectKey)
}

func mcpText(description string, maximum int) map[string]any {
	schema := map[string]any{"type": "string", "maxLength": maximum}
	if maximum > 0 {
		schema["minLength"] = 1
	}
	if description != "" {
		schema["description"] = description
	}
	return schema
}

func mcpFlag(description string, fallback bool) map[string]any {
	schema := map[string]any{"type": "boolean", "default": fallback}
	if description != "" {
		schema["description"] = description
	}
	return schema
}

// mcpFreeObject is an object whose keys come from an app manifest rather than
// from this catalog, so the schema cannot name them. The value budget in
// validateMCPSchema still bounds depth, size and count.
func mcpFreeObject(description string) map[string]any {
	schema := map[string]any{"type": "object", "additionalProperties": true}
	if description != "" {
		schema["description"] = description
	}
	return schema
}

func mcpObject(properties map[string]any) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
}

func mcpRequired(schema map[string]any, required ...string) map[string]any {
	names := make([]any, 0, len(required))
	for _, name := range required {
		names = append(names, name)
	}
	schema["required"] = names
	return schema
}

func mcpArray(items map[string]any, minimum, maximum int) map[string]any {
	schema := map[string]any{"type": "array", "items": items, "maxItems": maximum}
	if minimum > 0 {
		schema["minItems"] = minimum
	}
	return schema
}

func mcpDefault(schema map[string]any, fallback any) map[string]any {
	schema["default"] = fallback
	return schema
}

// mcpFields adds fields to a shared base without mutating it, so the paging
// fields can be reused by every list tool.
func mcpFields(base map[string]any, pairs ...any) map[string]any {
	fields := make(map[string]any, len(base)+len(pairs)/2)
	for name, schema := range base {
		fields[name] = schema
	}
	for index := 0; index+1 < len(pairs); index += 2 {
		name, _ := pairs[index].(string)
		fields[name] = pairs[index+1]
	}
	return fields
}

func mcpFlowNodeSchema() map[string]any {
	return mcpRequired(mcpObject(map[string]any{
		"id": mcpID("Card id you choose, unique within the Flow; connections reference it"),
		"x":  map[string]any{"type": "number", "description": "Horizontal canvas position"},
		"y":  map[string]any{"type": "number", "description": "Vertical canvas position"},
		"step": mcpRequired(mcpObject(map[string]any{
			"appId":    mcpID("Card appId from flow_cards_list"),
			"cardId":   mcpID("Card id from flow_cards_list"),
			"cardType": map[string]any{"type": "string", "enum": []any{"trigger", "device-trigger", "condition", "action"}},
			"args":     mcpFreeObject("Values keyed by the card argument names"),
			"inverted": map[string]any{"type": "boolean", "description": "Invert a condition"},
		}), "appId", "cardId", "cardType"),
	}), "id", "x", "y", "step")
}

func mcpFlowEdgeSchema() map[string]any {
	return mcpRequired(mcpObject(map[string]any{
		"id":   mcpID("Optional connection id; generated when omitted"),
		"from": mcpID("Source card/node id"),
		"to":   mcpID("Destination card/node id"),
	}), "from", "to")
}
