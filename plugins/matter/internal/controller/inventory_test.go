package controller

import (
	"context"
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

type metadataAttributeClient struct {
	values map[uint32]im.Value
}

type recordingAttributeWriter struct {
	writes []im.AttributeWrite
	status im.Status
}

func (w *recordingAttributeWriter) Write(_ context.Context, writes ...im.AttributeWrite) ([]im.AttributeWriteResult, error) {
	w.writes = append(w.writes, writes...)
	return []im.AttributeWriteResult{{Path: writes[0].Path, Status: w.status}}, nil
}

func (c metadataAttributeClient) Read(_ context.Context, paths ...im.AttributePath) ([]im.AttributeReport, error) {
	reports := make([]im.AttributeReport, 0, len(paths))
	for _, path := range paths {
		if path.Attribute == nil {
			continue
		}
		value, ok := c.values[*path.Attribute]
		if !ok {
			status := im.Status{Global: im.StatusUnsupportedAttribute}
			reports = append(reports, im.AttributeReport{Path: path, Status: &status})
			continue
		}
		reports = append(reports, im.AttributeReport{Path: path, Value: value})
	}
	return reports, nil
}

func uintArray(values ...uint64) im.Value {
	result := im.Value{Type: tlv.TypeArray, Children: make([]im.Value, 0, len(values))}
	for _, value := range values {
		result.Children = append(result.Children, im.Value{Type: tlv.TypeUint, Uint: value})
	}
	return result
}

func TestEndpointInventoryUsesMatterGlobalMetadata(t *testing.T) {
	client := metadataAttributeClient{values: map[uint32]im.Value{
		generatedCommandListAttribute: uintArray(3),
		acceptedCommandListAttribute:  uintArray(2, 1),
		eventListAttribute:            uintArray(4),
		attributeListAttribute:        uintArray(0, uint64(activePowerAttribute)),
		featureMapAttribute:           {Type: tlv.TypeUint, Uint: 0x12},
		clusterRevisionAttribute:      {Type: tlv.TypeUint, Uint: 3},
	}}
	inventory := inspectEndpointInventory(context.Background(), client, 7, []uint32{0x0100}, []uint32{electricalPowerCluster, 0x1234})
	if inventory.Endpoint != 7 || len(inventory.Clusters) != 2 {
		t.Fatalf("inventory = %#v", inventory)
	}
	power := inventory.Clusters[0]
	if power.Name != "Electrical Power Measurement" || power.Coverage != "partial" || power.FeatureMap != "0x12" ||
		power.Revision != 3 || len(power.Attributes) != 2 || power.AcceptedCommands[0] != "0x1" {
		t.Fatalf("power inventory = %#v", power)
	}
	if len(power.MappedAttributes) != 1 || power.MappedAttributes[0] != matterHexID(activePowerAttribute) ||
		len(power.UnmappedAttributes) != 1 || power.UnmappedAttributes[0] != "0x0" {
		t.Fatalf("power element coverage = %#v", power)
	}
	if inventory.Clusters[1].Coverage != "unmapped" {
		t.Fatalf("unknown cluster was hidden: %#v", inventory.Clusters[1])
	}
}

func TestSensitivitySettingComesFromFeatureAndAttributeLists(t *testing.T) {
	inventory := EndpointInventory{Endpoint: 4, Clusters: []ClusterInventory{{
		ID: matterHexID(booleanStateConfigurationCluster), FeatureMap: "0x8",
		Attributes: []string{matterHexID(0), matterHexID(1)},
	}}}
	client := metadataAttributeClient{values: map[uint32]im.Value{
		0: {Type: tlv.TypeUint, Uint: 1}, 1: {Type: tlv.TypeUint, Uint: 3},
	}}
	settings, descriptors := inspectEndpointSettings(context.Background(), client, 4, &inventory)
	if len(descriptors) != 1 || descriptors[0].Levels != 3 || descriptors[0].Endpoint != 4 ||
		settings[descriptors[0].ID] != uint64(1) {
		t.Fatalf("sensitivity = settings %#v descriptors %#v", settings, descriptors)
	}
}

func TestStoredInventoryRecomputesCoverageFromCurrentRegistry(t *testing.T) {
	stored := []any{map[string]any{
		"endpoint": float64(1), "deviceTypes": []any{"0x100"},
		"clusters": []any{map[string]any{
			"id": "0x6", "name": "On/Off", "coverage": "supported",
			"attributes":        []any{"0x0", "0x4003", "0xFFFB"},
			"acceptedCommands":  []any{"0x0", "0x1", "0x42"},
			"generatedCommands": []any{}, "events": []any{},
		}},
	}}
	inventories := storedEndpointInventories(stored)
	if len(inventories) != 1 || len(inventories[0].Clusters) != 1 {
		t.Fatalf("stored inventory = %#v", inventories)
	}
	cluster := inventories[0].Clusters[0]
	if cluster.Coverage != "partial" || len(cluster.UnmappedAttributes) != 1 || cluster.UnmappedAttributes[0] != "0x4003" ||
		len(cluster.UnmappedCommands) != 1 || cluster.UnmappedCommands[0] != "0x42" {
		t.Fatalf("recomputed coverage = %#v", cluster)
	}
}

func TestBooleanStateUsesTheEndpointDeviceType(t *testing.T) {
	for deviceType, want := range map[uint32]string{
		0x0015: "alarm_contact", 0x0107: "alarm_motion", 0x0043: "alarm_water", 0x0041: "alarm_generic",
	} {
		if got := booleanStateCapability([]uint32{deviceType}); got != want {
			t.Fatalf("device type 0x%04X mapped to %q, want %q", deviceType, got, want)
		}
	}
}

func TestColorCommandsComeFromCapabilityRegistry(t *testing.T) {
	command, timed, err := commandForCapability(nil, []uint32{colorControlCluster}, 2, "light_hue", 0.5)
	if err != nil || timed || command.Path != (im.CommandPath{Endpoint: 2, Cluster: colorControlCluster, Command: 0}) {
		t.Fatalf("hue command = %#v timed=%v err=%v", command.Path, timed, err)
	}
	payload, err := im.EncodeInvokeRequest([]im.Command{command}, false)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := im.DecodeInvokeRequest(payload)
	if err != nil || len(decoded) != 1 {
		t.Fatalf("decoded hue command = %#v, %v", decoded, err)
	}
	hue, ok := decoded[0].Fields.Field(0)
	if !ok || hue.Type != tlv.TypeUint || hue.Uint != 127 {
		t.Fatalf("encoded hue = %#v", hue)
	}
}

func TestSensitivitySettingWritesItsAdvertisedAttribute(t *testing.T) {
	writer := &recordingAttributeWriter{status: im.Status{Global: im.StatusSuccess}}
	setting := MatterSetting{
		ID: "matter.sensitivity.9", Kind: "sensitivity", Endpoint: 9,
		Cluster: matterHexID(booleanStateConfigurationCluster), Attribute: matterHexID(0), Levels: 3,
	}
	if err := writeMatterSetting(context.Background(), writer, setting, 2); err != nil {
		t.Fatal(err)
	}
	if len(writer.writes) != 1 {
		t.Fatalf("writes = %#v", writer.writes)
	}
	payload, err := im.EncodeWriteRequest(writer.writes, false)
	if err != nil {
		t.Fatal(err)
	}
	request, err := im.DecodeWriteRequest(payload)
	if err != nil || len(request.Writes) != 1 || request.Writes[0].Path.Endpoint == nil ||
		*request.Writes[0].Path.Endpoint != 9 || request.Writes[0].Value.Type != tlv.TypeUint || request.Writes[0].Value.Uint != 2 {
		t.Fatalf("sensitivity write = %#v, %v", request, err)
	}
}
