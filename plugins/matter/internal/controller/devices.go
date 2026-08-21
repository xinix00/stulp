package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

type deviceReplacement struct {
	deviceID     string
	capabilities map[string]string
}

type capabilityOccurrence struct {
	deviceID, capability, base string
	endpoint                   uint16
	value                      any
	hasValue                   bool
}

// Matter exposes functionality through endpoints, but a native accessory is
// still one physical product. Stulp collapses every non-bridged endpoint on a
// node into one logical device. Repeated functions become numbered
// sub-capabilities (button.1, button.2, onoff.1, ...), retaining an exact
// capability-to-endpoint route for commands, attributes and events.
func combineNativeEndpoints(devices []Device) ([]Device, map[string]deviceReplacement) {
	indices := make([]int, 0, len(devices))
	var nodeID string
	for index, device := range devices {
		candidateNode, _ := device.Store["matter.nodeId"].(string)
		if candidateNode == "" || bridgedEndpoint(device) {
			continue
		}
		if nodeID == "" {
			nodeID = candidateNode
		}
		if candidateNode == nodeID {
			indices = append(indices, index)
		}
	}
	if len(indices) < 2 {
		return devices, nil
	}
	sort.SliceStable(indices, func(left, right int) bool {
		leftDevice, rightDevice := devices[indices[left]], devices[indices[right]]
		if physicalDeviceRank(leftDevice) != physicalDeviceRank(rightDevice) {
			return physicalDeviceRank(leftDevice) > physicalDeviceRank(rightDevice)
		}
		return primaryEndpoint(leftDevice) < primaryEndpoint(rightDevice)
	})
	baseIndex := indices[0]
	merged := devices[baseIndex]
	if merged.State == nil {
		merged.State = make(map[string]any)
	}
	if merged.Store == nil {
		merged.Store = make(map[string]any)
	}
	if merged.Data == nil {
		merged.Data = make(map[string]any)
	}
	merged.Capabilities = nil
	merged.State = make(map[string]any)

	occurrences := make([]capabilityOccurrence, 0)
	counts := make(map[string]int)
	allEndpoints := make([]uint16, 0, len(indices))
	for _, index := range indices {
		device := devices[index]
		allEndpoints = append(allEndpoints, deviceEndpoints(device)...)
		for _, capability := range device.Capabilities {
			base := baseCapability(capability)
			value, hasValue := device.State[capability]
			occurrences = append(occurrences, capabilityOccurrence{
				deviceID: device.ID, capability: capability, base: base,
				endpoint: capabilityEndpoint(device, capability, primaryEndpoint(device)),
				value:    value, hasValue: hasValue,
			})
			counts[base]++
		}
		mergePhysicalMetadata(&merged, device)
	}
	sort.SliceStable(occurrences, func(left, right int) bool {
		if occurrences[left].base != occurrences[right].base {
			return occurrences[left].base < occurrences[right].base
		}
		return occurrences[left].endpoint < occurrences[right].endpoint
	})
	ordinals := make(map[string]int)
	capabilityEndpoints := make(map[string]uint16, len(occurrences))
	replacements := make(map[string]deviceReplacement, len(indices))
	for _, index := range indices {
		device := devices[index]
		if device.ID != "" {
			replacements[device.ID] = deviceReplacement{deviceID: merged.ID, capabilities: make(map[string]string)}
		}
	}
	for _, occurrence := range occurrences {
		capability := occurrence.base
		if counts[occurrence.base] > 1 {
			ordinals[occurrence.base]++
			capability = fmt.Sprintf("%s.%d", occurrence.base, ordinals[occurrence.base])
		}
		merged.Capabilities = append(merged.Capabilities, capability)
		capabilityEndpoints[capability] = occurrence.endpoint
		if occurrence.hasValue {
			merged.State[capability] = occurrence.value
		}
		if occurrence.deviceID != "" {
			replacement := replacements[occurrence.deviceID]
			replacement.capabilities[occurrence.capability] = capability
			replacements[occurrence.deviceID] = replacement
		}
	}
	slices.Sort(allEndpoints)
	allEndpoints = slices.Compact(allEndpoints)
	merged.Store["matter.endpoints"] = allEndpoints
	merged.Store["matter.capabilityEndpoints"] = capabilityEndpoints
	merged.Data["endpoints"] = allEndpoints
	trimEndpointSuffix(&merged)

	combined := make([]Device, 0, len(devices)-len(indices)+1)
	for index, device := range devices {
		if index == baseIndex {
			combined = append(combined, merged)
			continue
		}
		if slices.Contains(indices, index) {
			continue
		}
		combined = append(combined, device)
	}
	return combined, replacements
}

func bridgedEndpoint(device Device) bool {
	bridged, _ := device.Store["matter.bridged"].(bool)
	return bridged || storedDeviceHasCluster(device, bridgedBasicCluster)
}

func physicalDeviceRank(device Device) int {
	for _, capability := range device.Capabilities {
		switch baseCapability(capability) {
		case "onoff", "dim", "locked":
			return 3
		}
	}
	switch device.Class {
	case "light", "socket", "lock", "thermostat":
		return 2
	case "sensor":
		return 1
	default:
		return 0
	}
}

func mergePhysicalMetadata(destination *Device, source Device) {
	endpoints := append(deviceEndpoints(*destination), deviceEndpoints(source)...)
	slices.Sort(endpoints)
	destination.Store["matter.endpoints"] = slices.Compact(endpoints)
	destination.Store["matter.deviceTypes"] = mergeStringValues(destination.Store["matter.deviceTypes"], source.Store["matter.deviceTypes"])
	destination.Store["matter.serverClusters"] = mergeStringValues(destination.Store["matter.serverClusters"], source.Store["matter.serverClusters"])
	destination.Store["~matter.endpointInventory"] = mergeEndpointInventories(destination.Store["~matter.endpointInventory"], source.Store["~matter.endpointInventory"])
	destination.Store["matter.settings"] = mergeMatterSettings(destination.Store["matter.settings"], source.Store["matter.settings"])
	if destination.Settings == nil {
		destination.Settings = make(map[string]any)
	}
	for id, value := range source.Settings {
		destination.Settings[id] = value
	}
	if destination.GroupID == "" {
		destination.GroupID = source.GroupID
	}
	destination.Available = destination.Available || source.Available
	if lastEventNumber(source) > lastEventNumber(*destination) {
		destination.Store["matter.lastEventNumber"] = strconv.FormatUint(lastEventNumber(source), 10)
	}
}

func lastEventNumber(device Device) uint64 {
	value, _ := device.Store["matter.lastEventNumber"].(string)
	parsed, _ := strconv.ParseUint(value, 10, 64)
	return parsed
}

func baseCapability(capability string) string {
	base, _, _ := strings.Cut(capability, ".")
	return base
}

func capabilityForEndpoint(device Device, base string, endpoint uint16) string {
	for _, capability := range device.Capabilities {
		if baseCapability(capability) == base && capabilityEndpoint(device, capability, primaryEndpoint(device)) == endpoint {
			return capability
		}
	}
	return ""
}

func trimEndpointSuffix(device *Device) {
	endpoint := primaryEndpoint(*device)
	device.Name = strings.TrimSuffix(device.Name, fmt.Sprintf(" · %d", endpoint))
}

func primaryEndpoint(device Device) uint16 {
	value, ok := number(device.Store["matter.endpoint"])
	if !ok || value < 0 || value > math.MaxUint16 {
		return 0
	}
	return uint16(value)
}

func storedCapabilityEndpoints(device Device) map[string]uint16 {
	result := make(map[string]uint16, len(device.Capabilities))
	switch values := device.Store["matter.capabilityEndpoints"].(type) {
	case map[string]uint16:
		for capability, endpoint := range values {
			result[capability] = endpoint
		}
	case map[string]any:
		for capability, raw := range values {
			if endpoint, ok := number(raw); ok && endpoint >= 0 && endpoint <= math.MaxUint16 {
				result[capability] = uint16(endpoint)
			}
		}
	}
	fallback := primaryEndpoint(device)
	for _, capability := range device.Capabilities {
		if _, exists := result[capability]; !exists {
			result[capability] = fallback
		}
	}
	return result
}

func capabilityEndpoint(device Device, capability string, fallback uint16) uint16 {
	if endpoint, exists := storedCapabilityEndpoints(device)[capability]; exists {
		return endpoint
	}
	return fallback
}

func deviceEndpoints(device Device) []uint16 {
	result := make([]uint16, 0, len(device.Capabilities)+1)
	appendEndpoint := func(raw any) {
		if endpoint, ok := number(raw); ok && endpoint >= 0 && endpoint <= math.MaxUint16 && !slices.Contains(result, uint16(endpoint)) {
			result = append(result, uint16(endpoint))
		}
	}
	appendEndpoint(device.Store["matter.endpoint"])
	switch values := device.Store["matter.endpoints"].(type) {
	case []uint16:
		for _, endpoint := range values {
			appendEndpoint(endpoint)
		}
	case []any:
		for _, endpoint := range values {
			appendEndpoint(endpoint)
		}
	}
	for _, endpoint := range storedCapabilityEndpoints(device) {
		appendEndpoint(endpoint)
	}
	slices.Sort(result)
	return result
}

func mergeStringValues(left, right any) []string {
	result := storedStrings(left)
	for _, value := range storedStrings(right) {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func storedStrings(raw any) []string {
	var result []string
	switch values := raw.(type) {
	case []string:
		result = append(result, values...)
	case []any:
		for _, rawValue := range values {
			if value, ok := rawValue.(string); ok {
				result = append(result, value)
			}
		}
	}
	return result
}

func modelRefreshRequired(devices []Device) bool {
	for _, device := range devices {
		version, ok := number(device.Store["matter.modelVersion"])
		if !ok || version < float64(matterModelVersion) {
			return true
		}
	}
	return false
}

// refreshNodeModel re-reads the operational Descriptor over CASE. This is a
// deliberately non-destructive migration: commissioning and the installed
// fabric stay untouched, while endpoints skipped by an older Stulp build can
// become visible when support for their clusters is added.
func (c *Controller) refreshNodeModel(ctx context.Context, nodeID uint64, existing []Device,
	info connectionInfo, session *transport.SecureSession) ([]Device, error) {
	payload := onboarding.Payload{}
	for _, device := range existing {
		if value, ok := number(device.Data["vendorId"]); ok && value >= 0 && value <= math.MaxUint16 {
			payload.VendorID = uint16(value)
		}
		if value, ok := number(device.Data["productId"]); ok && value >= 0 && value <= math.MaxUint16 {
			payload.ProductID = uint16(value)
		}
		if payload.VendorID != 0 || payload.ProductID != 0 {
			break
		}
	}

	prototypes, err := inspectNode(ctx, im.Client{Transport: c.node, Session: session}, payload,
		info.remote, nodeID, info.fabricIndex, info.noc)
	if err != nil {
		return nil, err
	}
	if err := persistRefreshedNodeModel(ctx, c.store, existing, prototypes); err != nil {
		return nil, err
	}
	refreshed, _, err := c.nodeDevices(ctx, nodeID)
	return refreshed, err
}

func persistRefreshedNodeModel(ctx context.Context, database Backing, existing, prototypes []Device) error {
	used := make(map[int]bool, len(existing))
	for _, prototype := range prototypes {
		match := -1
		for index, current := range existing {
			if used[index] || bridgedEndpoint(current) != bridgedEndpoint(prototype) {
				continue
			}
			// A native accessory is collapsed into one logical device and is
			// therefore matched by node. Bridge children retain endpoint identity.
			if !bridgedEndpoint(prototype) || primaryEndpoint(current) == primaryEndpoint(prototype) {
				match = index
				break
			}
		}
		if match < 0 {
			if _, err := database.AddDevice(ctx, prototype); err != nil {
				return fmt.Errorf("add refreshed Matter endpoint %d: %w", primaryEndpoint(prototype), err)
			}
			continue
		}
		used[match] = true
		updated := mergeRefreshedDevice(existing[match], prototype)
		if err := database.UpdateDevice(ctx, updated); err != nil {
			return fmt.Errorf("update refreshed Matter device %q: %w", updated.ID, err)
		}
	}
	return nil
}

func mergeRefreshedDevice(existing, prototype Device) Device {
	updated := prototype
	updated.ID = existing.ID
	updated.Name = existing.Name
	updated.GroupID = existing.GroupID
	updated.Settings = copyDeviceMap(existing.Settings)
	for key, value := range prototype.Settings {
		updated.Settings[key] = value
	}

	updated.Data = copyDeviceMap(existing.Data)
	for _, key := range []string{"id", "nodeId", "endpoint", "endpoints", "vendorId", "productId"} {
		if value, exists := prototype.Data[key]; exists {
			updated.Data[key] = value
		}
	}

	updated.Store = copyDeviceMap(existing.Store)
	for _, key := range []string{
		"manufacturer", "matter.attestation", "matter.bridged", "matter.endpoint", "matter.endpoints",
		"matter.capabilityEndpoints", "matter.deviceTypes", "matter.serverClusters", "~matter.endpointInventory", "matter.settings", "matter.modelVersion",
	} {
		if value, exists := prototype.Store[key]; exists {
			updated.Store[key] = value
		}
	}

	updated.State = copyDeviceMap(existing.State)
	for capability, value := range prototype.State {
		updated.State[capability] = value
	}
	updated.Available = true
	updated.Message = ""
	return updated
}

// reconcileNativeDevices upgrades existing device records. The main endpoint
// keeps its device ID and group. Flow references follow through Stulp, which
// owns them: the controller only reports what was replaced by what.
func reconcileNativeDevices(ctx context.Context, database Backing) error {
	devices, err := database.Devices(ctx)
	if err != nil {
		return err
	}
	// Older Stulp versions retained the endpoint's cluster list, so newly
	// supported read-only capabilities can be added without recommissioning.
	// Their first subscription report supplies the current value.
	for index := range devices {
		if !upgradeMappedCapabilities(&devices[index]) {
			continue
		}
		if err := database.UpdateDevice(ctx, devices[index]); err != nil {
			return fmt.Errorf("upgrade native Matter device %q: %w", devices[index].ID, err)
		}
	}
	byNode := make(map[string][]Device)
	for _, device := range devices {
		nodeID, _ := device.Store["matter.nodeId"].(string)
		if nodeID != "" {
			byNode[nodeID] = append(byNode[nodeID], device)
		}
	}
	for _, nodeDevices := range byNode {
		combined, replacements := combineNativeEndpoints(nodeDevices)
		if len(replacements) == 0 {
			continue
		}
		var merged Device
		for _, candidate := range combined {
			if replacement, exists := replacements[candidate.ID]; exists && replacement.deviceID == candidate.ID {
				merged = candidate
				break
			}
		}
		if merged.ID == "" {
			continue
		}
		if err := database.ReplaceDeviceReferences(ctx, storeReplacements(replacements)); err != nil {
			return err
		}
		if err := database.UpdateDevice(ctx, merged); err != nil {
			return err
		}
		for oldID, replacement := range replacements {
			if oldID != replacement.deviceID {
				if err := database.DeleteDevice(ctx, oldID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func upgradeMappedCapabilities(device *Device) bool {
	if !storedDeviceHasCluster(*device, illuminanceCluster) {
		return false
	}
	for _, capability := range device.Capabilities {
		if baseCapability(capability) == "measure_luminance" {
			return false
		}
	}
	endpoint := primaryEndpoint(*device)
	// Existing combined motion sensors did not retain a cluster-to-endpoint
	// index. Their illuminance server normally accompanies Occupancy Sensing,
	// whose exact route is retained and is the strongest available hint.
	for _, capability := range device.Capabilities {
		if baseCapability(capability) == "alarm_motion" {
			endpoint = capabilityEndpoint(*device, capability, endpoint)
			break
		}
	}
	device.Capabilities = append(device.Capabilities, "measure_luminance")
	if device.Store == nil {
		device.Store = make(map[string]any)
	}
	capabilityEndpoints := storedCapabilityEndpoints(*device)
	capabilityEndpoints["measure_luminance"] = endpoint
	device.Store["matter.capabilityEndpoints"] = capabilityEndpoints
	return true
}

func copyDeviceMap(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

// storeReplacements vertaalt wat de controller weet naar wat Stulp nodig heeft:
// welk apparaat door welk ander vervangen is, en welke capabilities meeverhuisden.
func storeReplacements(replacements map[string]deviceReplacement) map[string]DeviceReplacement {
	out := make(map[string]DeviceReplacement, len(replacements))
	for oldID, replacement := range replacements {
		out[oldID] = DeviceReplacement{
			DeviceID: replacement.deviceID, Capabilities: replacement.capabilities,
		}
	}
	return out
}
