package controller

import (
	"context"
	"fmt"
	"net"
	"slices"
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// pathAttributeClient answers by concrete path, like a node does. Anything it
// does not know is an unsupported attribute, which is also what a node says.
type pathAttributeClient struct {
	values map[string]im.Value
	reads  []string
}

func pathKey(endpoint uint16, cluster, attribute uint32) string {
	return fmt.Sprintf("%d/%X/%X", endpoint, cluster, attribute)
}

func (c *pathAttributeClient) Read(_ context.Context, paths ...im.AttributePath) ([]im.AttributeReport, error) {
	reports := make([]im.AttributeReport, 0, len(paths))
	for _, path := range paths {
		if path.Endpoint == nil || path.Cluster == nil || path.Attribute == nil {
			continue
		}
		key := pathKey(*path.Endpoint, *path.Cluster, *path.Attribute)
		c.reads = append(c.reads, key)
		value, ok := c.values[key]
		if !ok {
			status := im.Status{Global: im.StatusUnsupportedAttribute}
			reports = append(reports, im.AttributeReport{Path: path, Status: &status})
			continue
		}
		reports = append(reports, im.AttributeReport{Path: path, Value: value})
	}
	return reports, nil
}

func tlvString(text string) im.Value { return im.Value{Type: tlv.TypeString, Data: []byte(text)} }

func deviceTypeList(types ...uint64) im.Value {
	list := im.Value{Type: tlv.TypeArray}
	for _, deviceType := range types {
		list.Children = append(list.Children, im.Value{Type: tlv.TypeStructure, Children: []im.Value{
			{Tag: tlv.Context(0), Type: tlv.TypeUint, Uint: deviceType},
			{Tag: tlv.Context(1), Type: tlv.TypeUint, Uint: 1},
		}})
	}
	return list
}

// alpstugaNode is the IKEA ALPSTUGA as its public node dump shows it: one
// application endpoint typed Air Quality Sensor that serves On/Off for the
// display next to Air Quality, Temperature, Humidity, CO2 and PM2.5.
func alpstugaNode() *pathAttributeClient {
	values := map[string]im.Value{
		pathKey(0, descriptorCluster, 3):    uintArray(1),
		pathKey(0, basicCluster, 1):         tlvString("IKEA of Sweden"),
		pathKey(0, basicCluster, 2):         {Type: tlv.TypeUint, Uint: 0x117C},
		pathKey(0, basicCluster, 3):         tlvString("ALPSTUGA air quality monitor"),
		pathKey(0, basicCluster, 4):         {Type: tlv.TypeUint, Uint: 0x3001},
		pathKey(1, descriptorCluster, 0):    deviceTypeList(0x002C),
		pathKey(1, descriptorCluster, 1):    uintArray(3, 6, 29, 91, 1026, 1029, 1037, 1066),
		pathKey(1, onOffCluster, 0):         {Type: tlv.TypeBool, Bool: true},
		pathKey(1, airQualityCluster, 0):    {Type: tlv.TypeUint, Uint: 1},
		pathKey(1, temperatureCluster, 0):   {Type: tlv.TypeInt, Int: 2341},
		pathKey(1, humidityCluster, 0):      {Type: tlv.TypeUint, Uint: 6216},
		pathKey(1, carbonDioxideCluster, 0): {Type: tlv.TypeFloat, Float: 812},
		pathKey(1, pm25Cluster, 0):          {Type: tlv.TypeFloat, Float: float64(float32(3.1))},
	}
	attributes := map[uint32][]uint64{
		0x0003: {0, 1}, descriptorCluster: {0, 1, 2, 3},
		onOffCluster: {0}, airQualityCluster: {0},
		temperatureCluster: {0, 1, 2}, humidityCluster: {0, 1, 2},
		carbonDioxideCluster: {0, 1, 2, 7, 8, 9, 10}, pm25Cluster: {0, 1, 2, 7, 8, 9, 10},
	}
	for cluster, list := range attributes {
		values[pathKey(1, cluster, attributeListAttribute)] = uintArray(append(list, 0xFFF8, 0xFFF9, 0xFFFB, 0xFFFC, 0xFFFD)...)
		values[pathKey(1, cluster, acceptedCommandListAttribute)] = uintArray()
		values[pathKey(1, cluster, generatedCommandListAttribute)] = uintArray()
		values[pathKey(1, cluster, featureMapAttribute)] = im.Value{Type: tlv.TypeUint, Uint: 0}
		values[pathKey(1, cluster, clusterRevisionAttribute)] = im.Value{Type: tlv.TypeUint, Uint: 3}
	}
	values[pathKey(1, onOffCluster, acceptedCommandListAttribute)] = uintArray(0, 1, 2)
	values[pathKey(1, onOffCluster, featureMapAttribute)] = im.Value{Type: tlv.TypeUint, Uint: 0x2} // DeadFrontBehavior
	values[pathKey(1, carbonDioxideCluster, featureMapAttribute)] = im.Value{Type: tlv.TypeUint, Uint: 0x3}
	values[pathKey(1, pm25Cluster, featureMapAttribute)] = im.Value{Type: tlv.TypeUint, Uint: 0x3}
	return &pathAttributeClient{values: values}
}

func TestAirQualityMonitorRefreshTurnsTheLampIntoASensor(t *testing.T) {
	ctx := context.Background()
	remote := &net.UDPAddr{IP: net.ParseIP("fd00::1"), Port: 5540}
	prototypes, err := inspectNode(ctx, alpstugaNode(), onboarding.Payload{VendorID: 0x117C, ProductID: 0x3001}, remote, 0x1234, 1, []byte{1})
	if err != nil {
		t.Fatalf("inspect ALPSTUGA: %v", err)
	}
	if len(prototypes) != 1 {
		t.Fatalf("ALPSTUGA became %d devices, want one", len(prototypes))
	}
	fresh := prototypes[0]
	if fresh.Class != "sensor" {
		t.Fatalf("ALPSTUGA class = %q, want sensor", fresh.Class)
	}
	for capability, want := range map[string]any{
		"onoff": true, "measure_temperature": 23.41, "measure_humidity": 62.16,
		"measure_co2": float64(812), "measure_pm25": 3.1, "air_quality_state": "good",
	} {
		if !slices.Contains(fresh.Capabilities, capability) || fresh.State[capability] != want {
			t.Fatalf("%s = %v (present %v), want %v", capability, fresh.State[capability], slices.Contains(fresh.Capabilities, capability), want)
		}
	}

	// The device as it was paired by a build that knew neither the device type
	// nor the air clusters: a lamp with temperature and humidity.
	paired := testMatterSensor("lucht", 1, "onoff", true, onOffCluster)
	paired.Class, paired.Name = "light", "Lucht"
	paired.Capabilities = []string{"onoff", "measure_temperature", "measure_humidity"}
	paired.State = map[string]any{"onoff": true, "measure_temperature": 24.39, "measure_humidity": 67.42}
	paired.Store["matter.nodeId"] = "0000000000001234"
	paired.Store["matter.modelVersion"] = 3
	paired.Store["matter.deviceTypes"] = []string{"0x2C"}
	paired.Store["matter.serverClusters"] = hexIDs([]uint32{3, 6, 29, 91, 1026, 1029, 1037, 1066})
	database := newBacking()
	added, err := database.AddDevice(ctx, paired)
	if err != nil {
		t.Fatal(err)
	}
	if !modelRefreshRequired([]Device{added}) {
		t.Fatal("a model-3 device was not marked for the model-4 refresh")
	}
	if err := persistRefreshedNodeModel(ctx, database, []Device{added}, prototypes); err != nil {
		t.Fatalf("persist refreshed ALPSTUGA: %v", err)
	}
	updated, err := database.Device(ctx, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Class != "sensor" || updated.Name != "Lucht" {
		t.Fatalf("refreshed device = class %q, name %q", updated.Class, updated.Name)
	}
	for _, capability := range []string{"measure_co2", "measure_pm25", "air_quality_state"} {
		if !slices.Contains(updated.Capabilities, capability) {
			t.Fatalf("refreshed capabilities = %v, missing %s", updated.Capabilities, capability)
		}
	}
	if updated.State["measure_co2"] != float64(812) || updated.State["air_quality_state"] != "good" {
		t.Fatalf("refreshed state = %#v", updated.State)
	}
	if modelRefreshRequired([]Device{updated}) {
		t.Fatal("refreshed device is still marked for refresh")
	}
}
