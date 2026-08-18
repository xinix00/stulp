package controller

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// capabilityMapping is the one registry for initial reads, subscriptions and
// live reports. Adding an attribute in only one of those paths is therefore no
// longer possible. Optionality follows the generated Project CHIP XML linked
// from inventory.go; AttributeList on the accessory remains authoritative.
type capabilityMapping struct {
	Capability string
	Cluster    uint32
	Attribute  uint32
	Optional   bool
	Decode     func(im.Value) (any, bool)
	Applies    func(mappingContext) bool
	Command    func(uint16, any) (im.Command, bool, error)
	Commands   []uint32
}

type mappingContext struct {
	deviceTypes []uint32
	servers     []uint32
}

const (
	colorControlCluster              uint32 = 0x0300
	smokeCOAlarmCluster              uint32 = 0x005C
	booleanStateConfigurationCluster uint32 = 0x0080
)

var capabilityMappings = []capabilityMapping{
	{Capability: "onoff", Cluster: onOffCluster, Attribute: 0, Decode: decodeBool, Command: onOffCommand, Commands: []uint32{0, 1}},
	{Capability: "dim", Cluster: levelCluster, Attribute: 0, Decode: decodeUnsignedScale(254), Command: levelCommand, Commands: []uint32{4}},
	{Capability: "locked", Cluster: doorLockCluster, Attribute: 0, Decode: decodeLockState, Command: lockCommand, Commands: []uint32{0, 1}},
	{Capability: "measure_temperature", Cluster: temperatureCluster, Attribute: 0, Decode: decodeSignedScale(100)},
	{Capability: "measure_temperature", Cluster: thermostatCluster, Attribute: 0, Decode: decodeSignedScale(100),
		Applies: func(ctx mappingContext) bool { return !slices.Contains(ctx.servers, temperatureCluster) }},
	{Capability: "measure_luminance", Cluster: illuminanceCluster, Attribute: 0, Decode: decodeIlluminance},
	{Capability: "measure_humidity", Cluster: humidityCluster, Attribute: 0, Decode: decodeUnsignedScale(100)},
	{Capability: "measure_pressure", Cluster: pressureCluster, Attribute: 0, Decode: decodeSignedScale(10)},
	{Capability: "measure_water", Cluster: flowCluster, Attribute: 0, Decode: decodeUnsignedScale(10)},
	{Capability: "alarm_motion", Cluster: occupancyCluster, Attribute: 0, Decode: decodeBitmapBool(1)},
	{Capability: "alarm_contact", Cluster: booleanStateCluster, Attribute: 0, Decode: decodeBool,
		Applies: func(ctx mappingContext) bool { return booleanStateCapability(ctx.deviceTypes) == "alarm_contact" }},
	{Capability: "alarm_motion", Cluster: booleanStateCluster, Attribute: 0, Decode: decodeBool,
		Applies: func(ctx mappingContext) bool { return booleanStateCapability(ctx.deviceTypes) == "alarm_motion" }},
	{Capability: "alarm_water", Cluster: booleanStateCluster, Attribute: 0, Decode: decodeBool,
		Applies: func(ctx mappingContext) bool { return booleanStateCapability(ctx.deviceTypes) == "alarm_water" }},
	{Capability: "alarm_generic", Cluster: booleanStateCluster, Attribute: 0, Decode: decodeBool,
		Applies: func(ctx mappingContext) bool { return booleanStateCapability(ctx.deviceTypes) == "alarm_generic" }},
	{Capability: "measure_battery", Cluster: powerSourceCluster, Attribute: 0x000C, Optional: true, Decode: decodeBattery},
	{Capability: "measure_voltage", Cluster: electricalPowerCluster, Attribute: 0x0004, Optional: true, Decode: decodeSignedScale(1000)},
	{Capability: "measure_current", Cluster: electricalPowerCluster, Attribute: 0x0005, Optional: true, Decode: decodeSignedScale(1000)},
	{Capability: "measure_power", Cluster: electricalPowerCluster, Attribute: activePowerAttribute, Decode: decodeSignedScale(1000)},
	{Capability: "meter_power", Cluster: electricalEnergyCluster, Attribute: cumulativeEnergyImportedAttribute, Optional: true, Decode: decodeEnergy},
	{Capability: "light_hue", Cluster: colorControlCluster, Attribute: 0x0000, Optional: true, Decode: decodeUnsignedScale(254), Command: hueCommand, Commands: []uint32{0}},
	{Capability: "light_saturation", Cluster: colorControlCluster, Attribute: 0x0001, Optional: true, Decode: decodeUnsignedScale(254), Command: saturationCommand, Commands: []uint32{3}},
	{Capability: "alarm_smoke", Cluster: smokeCOAlarmCluster, Attribute: 0x0001, Optional: true, Decode: decodeEnumAlarm},
	{Capability: "alarm_co", Cluster: smokeCOAlarmCluster, Attribute: 0x0002, Optional: true, Decode: decodeEnumAlarm},
	{Capability: "alarm_battery", Cluster: smokeCOAlarmCluster, Attribute: 0x0003, Decode: decodeEnumAlarm},
}

func commandForCapability(deviceTypes, servers []uint32, endpoint uint16, capability string, value any) (im.Command, bool, error) {
	mapping, ok := mappingForCapability(deviceTypes, servers, capability)
	if !ok || mapping.Command == nil {
		return im.Command{}, false, fmt.Errorf("Matter capability %q is read-only or unsupported", capability)
	}
	return mapping.Command(endpoint, value)
}

func onOffCommand(endpoint uint16, value any) (im.Command, bool, error) {
	enabled, ok := value.(bool)
	if !ok {
		return im.Command{}, false, fmt.Errorf("onoff needs a boolean")
	}
	command := uint32(0)
	if enabled {
		command = 1
	}
	return im.Command{Path: im.CommandPath{Endpoint: endpoint, Cluster: onOffCluster, Command: command}}, false, nil
}

func levelCommand(endpoint uint16, value any) (im.Command, bool, error) {
	level, ok := number(value)
	if !ok || level < 0 || level > 1 {
		return im.Command{}, false, fmt.Errorf("dim needs a number between 0 and 1")
	}
	return im.Command{Path: im.CommandPath{Endpoint: endpoint, Cluster: levelCluster, Command: 4},
		Fields: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(math.Round(level*254)), 1)
			writer.PutUintWidth(tlv.Context(1), 0, 2)
			writer.EndContainer()
		}}, false, nil
}

func lockCommand(endpoint uint16, value any) (im.Command, bool, error) {
	locked, ok := value.(bool)
	if !ok {
		return im.Command{}, false, fmt.Errorf("locked needs a boolean")
	}
	command := uint32(1)
	if locked {
		command = 0
	}
	return im.Command{Path: im.CommandPath{Endpoint: endpoint, Cluster: doorLockCluster, Command: command}}, true, nil
}

func hueCommand(endpoint uint16, value any) (im.Command, bool, error) {
	hue, ok := number(value)
	if !ok || hue < 0 || hue > 1 {
		return im.Command{}, false, fmt.Errorf("light_hue needs a number between 0 and 1")
	}
	return im.Command{Path: im.CommandPath{Endpoint: endpoint, Cluster: colorControlCluster, Command: 0x00},
		Fields: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(math.Round(hue*254)), 1)
			writer.PutUintWidth(tlv.Context(1), 0, 1) // shortest direction
			writer.PutUintWidth(tlv.Context(2), 0, 2) // immediate transition
			writer.PutUintWidth(tlv.Context(3), 0, 1)
			writer.PutUintWidth(tlv.Context(4), 0, 1)
			writer.EndContainer()
		}}, false, nil
}

func saturationCommand(endpoint uint16, value any) (im.Command, bool, error) {
	saturation, ok := number(value)
	if !ok || saturation < 0 || saturation > 1 {
		return im.Command{}, false, fmt.Errorf("light_saturation needs a number between 0 and 1")
	}
	return im.Command{Path: im.CommandPath{Endpoint: endpoint, Cluster: colorControlCluster, Command: 0x03},
		Fields: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUintWidth(tlv.Context(0), uint64(math.Round(saturation*254)), 1)
			writer.PutUintWidth(tlv.Context(1), 0, 2)
			writer.PutUintWidth(tlv.Context(2), 0, 1)
			writer.PutUintWidth(tlv.Context(3), 0, 1)
			writer.EndContainer()
		}}, false, nil
}

func clusterHasCapabilityMapping(cluster uint32) bool {
	if cluster == switchCluster {
		return true
	}
	for _, mapping := range capabilityMappings {
		if mapping.Cluster == cluster {
			return true
		}
	}
	return false
}

func endpointCapabilitiesFromRegistry(ctx context.Context, client attributeClient, endpoint uint16, servers, deviceTypes []uint32,
	inventory EndpointInventory) ([]string, map[string]any, error) {
	capabilities := make([]string, 0, 12)
	state := make(map[string]any)
	mappingContext := mappingContext{deviceTypes: deviceTypes, servers: servers}
	for _, mapping := range capabilityMappings {
		if !slices.Contains(servers, mapping.Cluster) || mapping.Applies != nil && !mapping.Applies(mappingContext) {
			continue
		}
		known, present := inventoryHasAttribute(inventory, mapping.Cluster, mapping.Attribute)
		if known && !present {
			continue
		}
		var value im.Value
		var supported bool
		var err error
		if mapping.Optional || known {
			value, supported, err = readOptionalNullableAttribute(ctx, client, endpoint, mapping.Cluster, mapping.Attribute)
		} else {
			value, supported, err = readAttribute(ctx, client, endpoint, mapping.Cluster, mapping.Attribute)
		}
		if err != nil {
			return nil, nil, err
		}
		if !supported {
			continue
		}
		if !slices.Contains(capabilities, mapping.Capability) {
			capabilities = append(capabilities, mapping.Capability)
		}
		decoded, hasValue := mapping.Decode(value)
		if value.Type != tlv.TypeNull && !hasValue {
			return nil, nil, fmt.Errorf("attribute %d/0x%04X/0x%04X has unsupported TLV type %d", endpoint, mapping.Cluster, mapping.Attribute, value.Type)
		}
		if hasValue {
			state[mapping.Capability] = decoded
		}
	}
	if slices.Contains(servers, switchCluster) {
		capabilities = append(capabilities, "button")
		state["button"] = false
	}
	return capabilities, state, nil
}

func mappingForCapability(deviceTypes, servers []uint32, capability string) (capabilityMapping, bool) {
	base := baseCapability(capability)
	ctx := mappingContext{deviceTypes: deviceTypes, servers: servers}
	for _, mapping := range capabilityMappings {
		if mapping.Capability != base || len(servers) > 0 && !slices.Contains(servers, mapping.Cluster) || mapping.Applies != nil && !mapping.Applies(ctx) {
			continue
		}
		return mapping, true
	}
	return capabilityMapping{}, false
}

func mappingForReport(deviceTypes, servers []uint32, cluster, attribute uint32) (capabilityMapping, bool) {
	ctx := mappingContext{deviceTypes: deviceTypes, servers: servers}
	for _, mapping := range capabilityMappings {
		if mapping.Cluster == cluster && mapping.Attribute == attribute &&
			(mapping.Applies == nil || mapping.Applies(ctx)) {
			return mapping, true
		}
	}
	return capabilityMapping{}, false
}

func booleanStateCapability(deviceTypes []uint32) string {
	for _, deviceType := range deviceTypes {
		switch deviceType {
		case 0x0107: // Occupancy Sensor
			return "alarm_motion"
		case 0x0043, 0x0044: // Water Leak Detector, Rain Sensor
			return "alarm_water"
		case 0x0041: // Water Freeze Detector has no frost capability here.
			return "alarm_generic"
		case 0x0015: // Contact Sensor
			return "alarm_contact"
		}
	}
	return "alarm_contact"
}

func decodeBool(value im.Value) (any, bool) {
	if value.Type == tlv.TypeBool {
		return value.Bool, true
	}
	if value.Type == tlv.TypeUint {
		return value.Uint != 0, true
	}
	return nil, false
}

func decodeLockState(value im.Value) (any, bool) {
	if value.Type == tlv.TypeUint {
		return value.Uint == 1, true
	}
	return nil, false
}

func decodeUnsignedScale(divisor float64) func(im.Value) (any, bool) {
	return func(value im.Value) (any, bool) {
		if value.Type == tlv.TypeUint {
			return float64(value.Uint) / divisor, true
		}
		return nil, false
	}
}

func decodeSignedScale(divisor float64) func(im.Value) (any, bool) {
	return func(value im.Value) (any, bool) {
		switch value.Type {
		case tlv.TypeInt:
			return float64(value.Int) / divisor, true
		case tlv.TypeUint:
			return float64(value.Uint) / divisor, true
		default:
			return nil, false
		}
	}
}

func decodeBitmapBool(mask uint64) func(im.Value) (any, bool) {
	return func(value im.Value) (any, bool) {
		if value.Type == tlv.TypeUint {
			return value.Uint&mask != 0, true
		}
		return nil, false
	}
}

func decodeBattery(value im.Value) (any, bool) {
	if value.Type != tlv.TypeUint {
		return nil, false
	}
	return math.Min(100, float64(value.Uint)/2), true
}

// Matter encodes Illuminance Measurement logarithmically:
// raw = 10,000 * log10(lux) + 1. Zero means the level is below the sensor's
// measurable minimum; 0xFFFF is reserved and nullable values use TLV null.
func decodeIlluminance(value im.Value) (any, bool) {
	if value.Type != tlv.TypeUint || value.Uint > 0xFFFE {
		return nil, false
	}
	if value.Uint == 0 {
		return float64(0), true
	}
	return math.Pow(10, (float64(value.Uint)-1)/10000), true
}

// meter_power capability uses cumulative kilowatt-hours.
func decodeEnergy(value im.Value) (any, bool) {
	if value.Type != tlv.TypeStructure {
		return nil, false
	}
	energy, ok := value.Field(0)
	if !ok {
		return nil, false
	}
	switch energy.Type {
	case tlv.TypeInt:
		if energy.Int < 0 {
			return nil, false
		}
		return float64(energy.Int) / 1_000_000, true
	case tlv.TypeUint:
		return float64(energy.Uint) / 1_000_000, true
	default:
		return nil, false
	}
}

func decodeEnumAlarm(value im.Value) (any, bool) {
	if value.Type == tlv.TypeUint && value.Uint <= 2 {
		return value.Uint != 0, true
	}
	return nil, false
}
