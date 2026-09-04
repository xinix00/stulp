package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

type nodeMetadata struct {
	vendorName, productName, nodeLabel string
	vendorID, productID                uint16
}

type attributeClient interface {
	Read(context.Context, ...im.AttributePath) ([]im.AttributeReport, error)
}

func inspectNode(ctx context.Context, client attributeClient, onboardingPayload onboarding.Payload, remote *net.UDPAddr,
	nodeID uint64, fabricIndex uint8, noc []byte) ([]Device, error) {
	metadata := nodeMetadata{vendorID: onboardingPayload.VendorID, productID: onboardingPayload.ProductID}
	if err := readBasicMetadata(ctx, client, 0, basicCluster, &metadata); err != nil {
		return nil, err
	}

	value, _, err := readAttribute(ctx, client, 0, descriptorCluster, 3)
	if err != nil {
		return nil, fmt.Errorf("read Matter descriptor parts: %w", err)
	}
	if value.Type != tlv.TypeArray {
		return nil, fmt.Errorf("Matter descriptor parts have TLV type %d, want array", value.Type)
	}
	parts := uint16Array(value)
	if len(parts) == 0 {
		return nil, errors.New("Matter descriptor exposes no endpoint parts")
	}
	parts = uniqueUint16(parts)
	prototypes := make([]Device, 0, len(parts))
	for _, endpoint := range parts {
		value, _, err := readAttribute(ctx, client, endpoint, descriptorCluster, 0)
		if err != nil {
			return nil, fmt.Errorf("read Matter endpoint %d device types: %w", endpoint, err)
		}
		if value.Type != tlv.TypeArray {
			return nil, fmt.Errorf("Matter endpoint %d device types have TLV type %d, want array", endpoint, value.Type)
		}
		deviceTypes := deviceTypeArray(value)
		value, _, err = readAttribute(ctx, client, endpoint, descriptorCluster, 1)
		if err != nil {
			return nil, fmt.Errorf("read Matter endpoint %d server list: %w", endpoint, err)
		}
		if value.Type != tlv.TypeArray {
			return nil, fmt.Errorf("Matter endpoint %d server list has TLV type %d, want array", endpoint, value.Type)
		}
		servers := uint32Array(value)
		endpointMetadata := metadata
		bridged := slices.Contains(servers, bridgedBasicCluster)
		if bridged {
			if err := readBasicMetadata(ctx, client, endpoint, bridgedBasicCluster, &endpointMetadata); err != nil {
				return nil, err
			}
		}
		inventory := inspectEndpointInventory(ctx, client, endpoint, deviceTypes, servers)
		capabilities, state, err := endpointCapabilitiesFromRegistry(ctx, client, endpoint, servers, deviceTypes, inventory)
		if err != nil {
			return nil, fmt.Errorf("read Matter endpoint %d initial state: %w", endpoint, err)
		}
		settings, matterSettings := inspectEndpointSettings(ctx, client, endpoint, &inventory)
		capabilityEndpoints := make(map[string]uint16, len(capabilities))
		for _, capability := range capabilities {
			capabilityEndpoints[capability] = endpoint
		}
		name := strings.TrimSpace(endpointMetadata.productName)
		if bridged && strings.TrimSpace(endpointMetadata.nodeLabel) != "" {
			name = strings.TrimSpace(endpointMetadata.nodeLabel)
		}
		if name == "" {
			name = strings.TrimSpace(endpointMetadata.nodeLabel)
		}
		if name == "" {
			name = fmt.Sprintf("Matter %04X:%04X", endpointMetadata.vendorID, endpointMetadata.productID)
		}
		if len(parts) > 1 && !bridged {
			name += fmt.Sprintf(" · %d", endpoint)
		}
		manufacturer := strings.TrimSpace(endpointMetadata.vendorName)
		if manufacturer == "" {
			manufacturer = fmt.Sprintf("Matter 0x%04X", endpointMetadata.vendorID)
		}
		prototypes = append(prototypes, Device{
			DriverID: "matter", Name: name,
			Class: endpointClass(deviceTypes, servers), Capabilities: capabilities, State: state, Settings: settings,
			Data: map[string]any{
				"id": fmt.Sprintf("%016X-%d", nodeID, endpoint), "nodeId": fmt.Sprintf("%016X", nodeID),
				"endpoint": endpoint, "vendorId": endpointMetadata.vendorID, "productId": endpointMetadata.productID,
			},
			Store: map[string]any{
				"manufacturer":       manufacturer,
				"matter.attestation": "dac-pai-verified",
				"matter.nodeId":      fmt.Sprintf("%016X", nodeID), "matter.endpoint": endpoint,
				"matter.endpoints": []uint16{endpoint}, "matter.capabilityEndpoints": capabilityEndpoints,
				"matter.address": remote.String(), "matter.noc": base64.StdEncoding.EncodeToString(noc),
				"matter.fabricIndex": fabricIndex, "matter.deviceTypes": hexIDs(deviceTypes),
				"matter.serverClusters": hexIDs(servers), "matter.bridged": bridged,
				"~matter.endpointInventory": []EndpointInventory{inventory},
				"matter.settings":           matterSettings,
				"matter.modelVersion":       matterModelVersion,
			},
		})
	}
	prototypes, _ = combineNativeEndpoints(prototypes)
	for index := range prototypes {
		prototypes[index].PreserveHardwareName()
	}
	if len(prototypes) == 0 {
		return nil, fmt.Errorf("Matter node exposes no manageable endpoint (descriptor parts: %v)", parts)
	}
	return prototypes, nil
}

// readBasicMetadata overlays one Basic Information cluster onto what is already
// known about a node. The node's root cluster and a bridged endpoint's own
// cluster number these attributes identically, so both are read the same way;
// an attribute the accessory does not expose leaves the existing value alone.
func readBasicMetadata(ctx context.Context, client attributeClient, endpoint uint16, cluster uint32, metadata *nodeMetadata) error {
	for attribute, destination := range map[uint32]*string{
		1: &metadata.vendorName, 3: &metadata.productName, 5: &metadata.nodeLabel,
	} {
		value, ok, err := optionalStringAttribute(ctx, client, endpoint, cluster, attribute)
		if err != nil {
			return err
		}
		if ok {
			*destination = value
		}
	}
	for attribute, destination := range map[uint32]*uint16{2: &metadata.vendorID, 4: &metadata.productID} {
		value, ok, err := optionalUint16Attribute(ctx, client, endpoint, cluster, attribute)
		if err != nil {
			return err
		}
		if ok {
			*destination = value
		}
	}
	return nil
}

func readAttribute(ctx context.Context, client attributeClient, endpoint uint16, cluster, attribute uint32) (im.Value, bool, error) {
	reports, err := client.Read(ctx, im.ConcreteAttributePath(endpoint, cluster, attribute))
	if err != nil {
		return im.Value{}, false, fmt.Errorf("read attribute %d/0x%04X/0x%04X: %w", endpoint, cluster, attribute, err)
	}
	for _, report := range reports {
		if report.Status != nil {
			return im.Value{}, false, fmt.Errorf("read attribute %d/0x%04X/0x%04X: %w", endpoint, cluster, attribute, *report.Status)
		}
		return report.Value, true, nil
	}
	return im.Value{}, false, fmt.Errorf("read attribute %d/0x%04X/0x%04X: empty report", endpoint, cluster, attribute)
}

func readOptionalAttribute(ctx context.Context, client attributeClient, endpoint uint16, cluster, attribute uint32) (im.Value, bool, error) {
	value, ok, err := readOptionalNullableAttribute(ctx, client, endpoint, cluster, attribute)
	if err != nil || !ok {
		return im.Value{}, false, err
	}
	if value.Type == tlv.TypeNull {
		return im.Value{}, false, nil
	}
	return value, true, nil
}

// readOptionalNullableAttribute distinguishes an unsupported optional
// attribute from a supported attribute whose current value is null. The
// distinction matters for nullable measurement attributes: the capability
// exists even when the accessory has no sample yet.
func readOptionalNullableAttribute(ctx context.Context, client attributeClient, endpoint uint16, cluster, attribute uint32) (im.Value, bool, error) {
	value, ok, err := readAttribute(ctx, client, endpoint, cluster, attribute)
	if err != nil {
		var status im.Status
		if errors.As(err, &status) && status.Global == im.StatusUnsupportedAttribute {
			return im.Value{}, false, nil
		}
		return im.Value{}, false, err
	}
	return value, ok, nil
}

func optionalStringAttribute(ctx context.Context, client attributeClient, endpoint uint16, cluster, attribute uint32) (string, bool, error) {
	value, ok, err := readOptionalAttribute(ctx, client, endpoint, cluster, attribute)
	if err != nil || !ok {
		return "", false, err
	}
	if value.Type != tlv.TypeString {
		return "", false, fmt.Errorf("attribute %d/0x%04X/0x%04X has TLV type %d, want string", endpoint, cluster, attribute, value.Type)
	}
	return string(value.Data), true, nil
}

func optionalUint16Attribute(ctx context.Context, client attributeClient, endpoint uint16, cluster, attribute uint32) (uint16, bool, error) {
	value, ok, err := readOptionalAttribute(ctx, client, endpoint, cluster, attribute)
	if err != nil || !ok {
		return 0, false, err
	}
	if value.Type != tlv.TypeUint || value.Uint > math.MaxUint16 {
		return 0, false, fmt.Errorf("attribute %d/0x%04X/0x%04X is not a uint16", endpoint, cluster, attribute)
	}
	return uint16(value.Uint), true, nil
}

func uint16Array(value im.Value) []uint16 {
	result := make([]uint16, 0, len(value.Children))
	for _, child := range value.Children {
		if child.Type == tlv.TypeUint && child.Uint <= math.MaxUint16 {
			result = append(result, uint16(child.Uint))
		}
	}
	return result
}

func uint32Array(value im.Value) []uint32 {
	result := make([]uint32, 0, len(value.Children))
	for _, child := range value.Children {
		if child.Type == tlv.TypeUint && child.Uint <= math.MaxUint32 {
			result = append(result, uint32(child.Uint))
		}
	}
	return result
}

func deviceTypeArray(value im.Value) []uint32 {
	result := make([]uint32, 0, len(value.Children))
	for _, child := range value.Children {
		field, ok := child.Field(0)
		if ok && field.Type == tlv.TypeUint && field.Uint <= math.MaxUint32 {
			result = append(result, uint32(field.Uint))
		}
	}
	return result
}

func endpointClass(deviceTypes, servers []uint32) string {
	for _, deviceType := range deviceTypes {
		switch deviceType {
		case 0x0100, 0x010D:
			return "light"
		case 0x010A, 0x010B:
			return "socket"
		case 0x000A:
			return "lock"
		case 0x0301:
			return "thermostat"
		// Sensor device types come before the server-cluster fallback below: an
		// air quality monitor also serves On/Off for its display, and that
		// server alone would make it a lamp.
		case 0x0302, 0x0307, 0x0015, // Temperature, Humidity, Contact Sensor
			0x002C, 0x0106, 0x0107, 0x0305, 0x0306, // Air Quality, Light, Occupancy, Pressure, Flow Sensor
			0x0041, 0x0043, 0x0044, 0x0076: // Water Freeze, Water Leak, Rain, Smoke CO Alarm
			return "sensor"
		}
	}
	switch {
	case slices.Contains(servers, doorLockCluster):
		return "lock"
	case slices.Contains(servers, thermostatCluster):
		return "thermostat"
	case slices.Contains(servers, onOffCluster):
		return "light"
	default:
		return "sensor"
	}
}

func hexIDs(values []uint32) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = "0x" + strings.ToUpper(strconv.FormatUint(uint64(value), 16))
	}
	return result
}

// StoredClass rekent de apparaatsoort opnieuw uit wat het koppelen in de store
// bewaarde (matter.deviceTypes/serverClusters, als hex-teksten). Hij bestaat om
// apparaten te helen die gekoppeld zijn toen de koppelstroom de soort nog liet
// vallen en daardoor op de driver-default "other" staan. Een lege store geeft
// een lege soort terug — de endpointClass-fallback zou van élk apparaat zonder
// opgeslagen model "sensor" maken, en niets weten is geen sensor.
func StoredClass(store map[string]any) string {
	deviceTypes := storedHexIDs(store["matter.deviceTypes"])
	servers := storedHexIDs(store["matter.serverClusters"])
	if len(deviceTypes) == 0 && len(servers) == 0 {
		return ""
	}
	return endpointClass(deviceTypes, servers)
}

// storedHexIDs leest wat hexIDs schreef, ná een JSON-rondreis: een lijst van
// "0x..."-teksten, als []string of als []any.
func storedHexIDs(value any) []uint32 {
	var texts []string
	switch list := value.(type) {
	case []string:
		texts = list
	case []any:
		for _, raw := range list {
			if text, ok := raw.(string); ok {
				texts = append(texts, text)
			}
		}
	}
	result := make([]uint32, 0, len(texts))
	for _, text := range texts {
		if parsed, err := strconv.ParseUint(text, 0, 32); err == nil {
			result = append(result, uint32(parsed))
		}
	}
	return result
}
