package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// Matter's on-device data model is authoritative. Every server cluster
// exposes these global attributes, so Stulp can distinguish an unsupported
// feature from a capability mapping it simply does not know yet.
//
// Names and cluster semantics are checked against Project CHIP's generated
// data-model XML (the open implementation source for the CSA specification):
// https://github.com/project-chip/connectedhomeip/tree/master/src/app/zap-templates/zcl/data-model/chip
const (
	generatedCommandListAttribute uint32 = 0xFFF8
	acceptedCommandListAttribute  uint32 = 0xFFF9
	eventListAttribute            uint32 = 0xFFFA
	attributeListAttribute        uint32 = 0xFFFB
	featureMapAttribute           uint32 = 0xFFFC
	clusterRevisionAttribute      uint32 = 0xFFFD
)

type EndpointInventory struct {
	Endpoint    uint16             `json:"endpoint"`
	DeviceTypes []string           `json:"deviceTypes"`
	Clusters    []ClusterInventory `json:"clusters"`
}

type ClusterInventory struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Coverage           string   `json:"coverage"`
	FeatureMap         string   `json:"featureMap,omitempty"`
	Revision           uint16   `json:"revision,omitempty"`
	Attributes         []string `json:"attributes"`
	AcceptedCommands   []string `json:"acceptedCommands"`
	GeneratedCommands  []string `json:"generatedCommands"`
	Events             []string `json:"events"`
	MappedAttributes   []string `json:"mappedAttributes,omitempty"`
	UnmappedAttributes []string `json:"unmappedAttributes,omitempty"`
	MappedCommands     []string `json:"mappedCommands,omitempty"`
	UnmappedCommands   []string `json:"unmappedCommands,omitempty"`
	MappedEvents       []string `json:"mappedEvents,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

var clusterNames = map[uint32]string{
	0x0003: "Identify", 0x0004: "Groups", 0x0005: "Scenes", 0x0006: "On/Off", 0x0008: "Level Control",
	0x001D: "Descriptor", 0x001E: "Binding", 0x001F: "Access Control", 0x0028: "Basic Information",
	0x002A: "OTA Software Update Requestor", 0x002F: "Power Source", 0x0030: "General Commissioning",
	0x0031: "Network Commissioning", 0x0033: "General Diagnostics", 0x0035: "Thread Network Diagnostics",
	0x0036: "Wi-Fi Network Diagnostics", 0x0039: "Bridged Device Basic Information", 0x003B: "Switch",
	0x003E: "Operational Credentials", 0x0045: "Boolean State", 0x0046: "ICD Management",
	0x005B: "Air Quality", 0x005C: "Smoke CO Alarm", 0x0080: "Boolean State Configuration",
	0x0081: "Valve Configuration and Control", 0x0090: "Electrical Power Measurement",
	0x0091: "Electrical Energy Measurement", 0x0101: "Door Lock", 0x0102: "Window Covering",
	0x0200: "Pump Configuration and Control", 0x0201: "Thermostat", 0x0202: "Fan Control",
	0x0300: "Color Control", 0x0400: "Illuminance Measurement", 0x0402: "Temperature Measurement",
	0x0403: "Pressure Measurement", 0x0404: "Flow Measurement", 0x0405: "Relative Humidity Measurement",
	0x0406: "Occupancy Sensing", 0x040C: "Carbon Monoxide Concentration Measurement",
	0x040D: "Carbon Dioxide Concentration Measurement", 0x0413: "Nitrogen Dioxide Concentration Measurement",
	0x0415: "Ozone Concentration Measurement", 0x042A: "PM2.5 Concentration Measurement",
	0x042B: "Formaldehyde Concentration Measurement", 0x042C: "PM1 Concentration Measurement",
	0x042D: "PM10 Concentration Measurement", 0x042E: "Total VOC Concentration Measurement",
	0x042F: "Radon Concentration Measurement",
}

var infrastructureClusters = map[uint32]bool{
	0x0003: true, 0x0004: true, 0x0005: true, 0x001D: true, 0x001E: true, 0x001F: true,
	0x0028: true, 0x002A: true, 0x0030: true, 0x0031: true, 0x0033: true, 0x0035: true,
	0x0036: true, 0x0039: true, 0x003E: true, 0x0046: true,
}

func clusterName(cluster uint32) string {
	if name := clusterNames[cluster]; name != "" {
		return name
	}
	return "Unknown cluster"
}

func clusterCoverage(cluster uint32) string {
	if infrastructureClusters[cluster] {
		return "infrastructure"
	}
	if cluster == booleanStateConfigurationCluster {
		return "configuration"
	}
	if clusterHasCapabilityMapping(cluster) {
		return "supported"
	}
	return "unmapped"
}

func inspectEndpointInventory(ctx context.Context, client attributeClient, endpoint uint16, deviceTypes, servers []uint32) EndpointInventory {
	inventory := EndpointInventory{Endpoint: endpoint, DeviceTypes: hexIDs(deviceTypes), Clusters: make([]ClusterInventory, 0, len(servers))}
	clusterIndex := make(map[uint32]int, len(servers))
	metadataAttributes := []uint32{
		generatedCommandListAttribute, acceptedCommandListAttribute, eventListAttribute,
		attributeListAttribute, featureMapAttribute, clusterRevisionAttribute,
	}
	paths := make([]im.AttributePath, 0, len(servers)*len(metadataAttributes))
	for _, cluster := range servers {
		clusterIndex[cluster] = len(inventory.Clusters)
		inventory.Clusters = append(inventory.Clusters, ClusterInventory{
			ID: matterHexID(cluster), Name: clusterName(cluster), Coverage: clusterCoverage(cluster),
			Attributes: []string{}, AcceptedCommands: []string{}, GeneratedCommands: []string{}, Events: []string{},
		})
		for _, attribute := range metadataAttributes {
			paths = append(paths, im.ConcreteAttributePath(endpoint, cluster, attribute))
		}
	}
	if len(paths) == 0 {
		return inventory
	}
	// Bound request size for Thread/UDP while still avoiding one roundtrip per
	// cluster. ReportData may chunk its response; ReadRequest itself cannot.
	const metadataPathsPerRead = 36
	reports := make([]im.AttributeReport, 0, len(paths))
	failedClusters := make(map[uint32]bool)
	for start := 0; start < len(paths); start += metadataPathsPerRead {
		end := min(start+metadataPathsPerRead, len(paths))
		batchReports, err := client.Read(ctx, paths[start:end]...)
		if err == nil {
			reports = append(reports, batchReports...)
			continue
		}
		for _, path := range paths[start:end] {
			if path.Cluster == nil || failedClusters[*path.Cluster] {
				continue
			}
			failedClusters[*path.Cluster] = true
			inventory.Clusters[clusterIndex[*path.Cluster]].Errors = append(
				inventory.Clusters[clusterIndex[*path.Cluster]].Errors, err.Error())
		}
	}
	seen := make(map[uint32]map[uint32]bool, len(servers))
	for _, report := range reports {
		if report.Path.Cluster == nil || report.Path.Attribute == nil {
			continue
		}
		index, exists := clusterIndex[*report.Path.Cluster]
		if !exists {
			continue
		}
		entry := &inventory.Clusters[index]
		if seen[*report.Path.Cluster] == nil {
			seen[*report.Path.Cluster] = make(map[uint32]bool)
		}
		seen[*report.Path.Cluster][*report.Path.Attribute] = true
		if report.Status != nil {
			if report.Status.Global != im.StatusUnsupportedAttribute {
				entry.Errors = append(entry.Errors, fmt.Sprintf("0x%04X: %v", *report.Path.Attribute, *report.Status))
			}
			continue
		}
		switch *report.Path.Attribute {
		case generatedCommandListAttribute:
			entry.GeneratedCommands = valueHexIDs(report.Value)
		case acceptedCommandListAttribute:
			entry.AcceptedCommands = valueHexIDs(report.Value)
		case eventListAttribute:
			entry.Events = valueHexIDs(report.Value)
		case attributeListAttribute:
			entry.Attributes = valueHexIDs(report.Value)
		case featureMapAttribute:
			if report.Value.Type == tlv.TypeUint {
				entry.FeatureMap = fmt.Sprintf("0x%X", report.Value.Uint)
			} else {
				entry.Errors = append(entry.Errors, "FeatureMap is not unsigned")
			}
		case clusterRevisionAttribute:
			if report.Value.Type == tlv.TypeUint && report.Value.Uint <= 0xFFFF {
				entry.Revision = uint16(report.Value.Uint)
			} else {
				entry.Errors = append(entry.Errors, "ClusterRevision is not uint16")
			}
		}
	}
	for cluster, index := range clusterIndex {
		if failedClusters[cluster] {
			continue
		}
		for _, attribute := range metadataAttributes {
			if !seen[cluster][attribute] {
				inventory.Clusters[index].Errors = append(inventory.Clusters[index].Errors,
					fmt.Sprintf("metadata 0x%04X missing from response", attribute))
			}
		}
		classifyInventoryCoverage(&inventory.Clusters[index], cluster)
	}
	return inventory
}

func classifyInventoryCoverage(entry *ClusterInventory, cluster uint32) {
	if infrastructureClusters[cluster] {
		return
	}
	mappedAttributes := make(map[uint32]bool)
	mappedCommands := make(map[uint32]bool)
	for _, mapping := range capabilityMappings {
		if mapping.Cluster != cluster {
			continue
		}
		mappedAttributes[mapping.Attribute] = true
		for _, command := range mapping.Commands {
			mappedCommands[command] = true
		}
	}
	if cluster == booleanStateConfigurationCluster {
		mappedAttributes[0], mappedAttributes[1] = true, true
	}
	for _, id := range entry.Attributes {
		attribute, ok := parseClusterHex(id)
		if !ok || attribute >= generatedCommandListAttribute {
			continue
		}
		if mappedAttributes[attribute] {
			entry.MappedAttributes = append(entry.MappedAttributes, id)
		} else {
			entry.UnmappedAttributes = append(entry.UnmappedAttributes, id)
		}
	}
	for _, id := range entry.AcceptedCommands {
		command, ok := parseClusterHex(id)
		if ok && mappedCommands[command] {
			entry.MappedCommands = append(entry.MappedCommands, id)
		} else {
			entry.UnmappedCommands = append(entry.UnmappedCommands, id)
		}
	}
	// Every event is retained with stable IDs and reaches matter_event even
	// when Stulp has no friendly name for it.
	entry.MappedEvents = append(entry.MappedEvents, entry.Events...)
	if entry.Coverage == "supported" && (len(entry.UnmappedAttributes) > 0 || len(entry.UnmappedCommands) > 0) {
		entry.Coverage = "partial"
	}
}

func valueHexIDs(value im.Value) []string {
	if value.Type != tlv.TypeArray {
		return []string{}
	}
	result := make([]uint32, 0, len(value.Children))
	for _, child := range value.Children {
		if child.Type == tlv.TypeUint && child.Uint <= 0xFFFFFFFF {
			result = append(result, uint32(child.Uint))
		}
	}
	slices.Sort(result)
	result = slices.Compact(result)
	return hexIDs(result)
}

func inventoryHasAttribute(inventory EndpointInventory, cluster, attribute uint32) (known, present bool) {
	for _, entry := range inventory.Clusters {
		if entry.ID != matterHexID(cluster) {
			continue
		}
		if len(entry.Attributes) == 0 {
			return false, false
		}
		return true, slices.Contains(entry.Attributes, matterHexID(attribute))
	}
	return false, false
}

func mergeEndpointInventories(left, right any) []EndpointInventory {
	byEndpoint := make(map[uint16]EndpointInventory)
	for _, inventory := range append(storedEndpointInventories(left), storedEndpointInventories(right)...) {
		byEndpoint[inventory.Endpoint] = inventory
	}
	endpoints := make([]int, 0, len(byEndpoint))
	for endpoint := range byEndpoint {
		endpoints = append(endpoints, int(endpoint))
	}
	sort.Ints(endpoints)
	result := make([]EndpointInventory, 0, len(endpoints))
	for _, endpoint := range endpoints {
		result = append(result, byEndpoint[uint16(endpoint)])
	}
	return result
}

func storedEndpointInventories(raw any) []EndpointInventory {
	if values, ok := raw.([]EndpointInventory); ok {
		return append([]EndpointInventory(nil), values...)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]EndpointInventory, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		endpointNumber, ok := number(object["endpoint"])
		if !ok || endpointNumber < 0 || endpointNumber > 0xFFFF {
			continue
		}
		inventory := EndpointInventory{Endpoint: uint16(endpointNumber), DeviceTypes: storedStrings(object["deviceTypes"])}
		clusters, _ := object["clusters"].([]any)
		for _, rawCluster := range clusters {
			clusterObject, ok := rawCluster.(map[string]any)
			if !ok {
				continue
			}
			entry := ClusterInventory{
				ID: inventoryString(clusterObject["id"]), Name: inventoryString(clusterObject["name"]), Coverage: inventoryString(clusterObject["coverage"]),
				FeatureMap: inventoryString(clusterObject["featureMap"]), Attributes: storedStrings(clusterObject["attributes"]),
				AcceptedCommands: storedStrings(clusterObject["acceptedCommands"]), GeneratedCommands: storedStrings(clusterObject["generatedCommands"]),
				Events: storedStrings(clusterObject["events"]), Errors: storedStrings(clusterObject["errors"]),
				MappedAttributes: storedStrings(clusterObject["mappedAttributes"]), UnmappedAttributes: storedStrings(clusterObject["unmappedAttributes"]),
				MappedCommands: storedStrings(clusterObject["mappedCommands"]), UnmappedCommands: storedStrings(clusterObject["unmappedCommands"]),
				MappedEvents: storedStrings(clusterObject["mappedEvents"]),
			}
			if revision, ok := number(clusterObject["revision"]); ok && revision >= 0 && revision <= 0xFFFF {
				entry.Revision = uint16(revision)
			}
			if clusterID, ok := parseClusterHex(entry.ID); ok {
				// Coverage is derived from today's registry, not frozen into the
				// device snapshot. Adding a mapping updates diagnostics immediately.
				entry.Coverage = clusterCoverage(clusterID)
				entry.MappedAttributes, entry.UnmappedAttributes = nil, nil
				entry.MappedCommands, entry.UnmappedCommands, entry.MappedEvents = nil, nil, nil
				classifyInventoryCoverage(&entry, clusterID)
			}
			inventory.Clusters = append(inventory.Clusters, entry)
		}
		result = append(result, inventory)
	}
	return result
}

func inventoryString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func matterHexID(value uint32) string { return fmt.Sprintf("0x%X", value) }

func parseClusterHex(value string) (uint32, bool) {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && strings.EqualFold(value[:2], "0x") {
		value = value[2:]
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	return uint32(parsed), err == nil
}
