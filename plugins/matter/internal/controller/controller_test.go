package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/discovery"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

type stubAttributeClient struct {
	reports []im.AttributeReport
	err     error
}

func (s stubAttributeClient) Read(context.Context, ...im.AttributePath) ([]im.AttributeReport, error) {
	return s.reports, s.err
}

// endpointCapabilities reads an endpoint without the descriptor metadata a real
// node supplies, which is what these tests exercise: the capability registry on
// its own, driven only by the server cluster list.
func endpointCapabilities(ctx context.Context, client attributeClient, endpoint uint16, servers []uint32) ([]string, map[string]any, error) {
	return endpointCapabilitiesFromRegistry(ctx, client, endpoint, servers, nil, EndpointInventory{})
}

func TestMatchesCommissionableAdvertisement(t *testing.T) {
	payload := onboarding.Payload{
		Discriminator: 0xF00, ShortDiscriminator: true,
		VendorID: 0x1234, ProductID: 0x5678,
	}
	matching := discovery.Node{Discriminator: 0xF42, VendorID: 0x1234, ProductID: 0x5678}
	if !matches(payload, matching) {
		t.Fatal("short discriminator and VID/PID should match")
	}
	wrongDiscriminator := matching
	wrongDiscriminator.Discriminator = 0xE42
	if matches(payload, wrongDiscriminator) {
		t.Fatal("different high discriminator nibble matched")
	}
	wrongVendor := matching
	wrongVendor.VendorID++
	if matches(payload, wrongVendor) {
		t.Fatal("different advertised vendor matched")
	}
	payload.ShortDiscriminator = false
	if matches(payload, matching) {
		t.Fatal("full discriminator accepted a partial match")
	}
}

func TestLoadOrCreateFabricPersistsIdentity(t *testing.T) {
	database := newBacking()
	ctx := context.Background()
	backing := database
	first, err := loadOrCreateFabric(ctx, backing)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateFabric(ctx, backing)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.RootID != second.RootID || first.ControllerNodeID != second.ControllerNodeID {
		t.Fatalf("fabric identity changed after reload: first=%#v second=%#v", first, second)
	}
	if !first.RootCertificate.Equal(second.RootCertificate) || !first.ControllerNOC.Equal(second.ControllerNOC) {
		t.Fatal("fabric certificates changed after reload")
	}
}

func TestMatterDeviceConnectionRoundTrip(t *testing.T) {
	wantNOC := []byte{1, 2, 3, 4}
	device := Device{Store: map[string]any{
		"matter.nodeId":            "0000000000012345",
		"matter.endpoint":          float64(7),
		"matter.fabricIndex":       float64(3),
		"matter.address":           "[fe80::1%en0]:5540",
		"matter.noc":               base64.StdEncoding.EncodeToString(wantNOC),
		"matter.mrpIdleInterval":   float64(15_800),
		"matter.mrpActiveInterval": float64(2500),
	}}
	got, err := deviceConnection(device)
	if err != nil {
		t.Fatal(err)
	}
	if got.nodeID != 0x12345 || got.endpoint != 7 || got.fabricIndex != 3 || got.remote.Port != 5540 || got.remote.Zone != "en0" ||
		string(got.noc) != string(wantNOC) || got.timing.Idle != 15_800*time.Millisecond ||
		got.timing.Active != 2500*time.Millisecond {
		t.Fatalf("unexpected connection: %#v", got)
	}
}

// All three values matter, and each for a different moment: SII for a peer that
// may be asleep, SAI for one that just spoke, SAT for how long that lasts.
func TestAdvertisedMRPTimingCarriesAllThreeValues(t *testing.T) {
	text := map[string]string{"SAI": "2500", "sii": "15800", "SAT": "1000"}
	got := advertisedMRPTiming(text)
	if got.Idle != 15_800*time.Millisecond || got.Active != 2500*time.Millisecond ||
		got.ActiveThreshold != time.Second {
		t.Fatalf("advertised MRP timing = %+v", got)
	}
	stored := make(map[string]any)
	copyMRPTXT(stored, text)
	if stored["matter.mrpIdleInterval"] != int64(15_800) || stored["matter.mrpActiveInterval"] != int64(2500) {
		t.Fatalf("stored MRP metadata = %#v", stored)
	}
}

func TestDescriptorArrayAndClassMapping(t *testing.T) {
	deviceTypes := im.Value{Children: []im.Value{
		{Type: tlv.TypeStructure, Children: []im.Value{{Tag: tlv.Context(0), Type: tlv.TypeUint, Uint: 0x010A}}},
		{Type: tlv.TypeStructure, Children: []im.Value{{Tag: tlv.Context(0), Type: tlv.TypeUint, Uint: 0x0302}}},
	}}
	parsed := deviceTypeArray(deviceTypes)
	if len(parsed) != 2 || parsed[0] != 0x010A || parsed[1] != 0x0302 {
		t.Fatalf("unexpected device types: %#v", parsed)
	}
	if got := endpointClass(parsed, []uint32{onOffCluster}); got != "socket" {
		t.Fatalf("socket mapped as %q", got)
	}
	if got := endpointClass(nil, []uint32{doorLockCluster}); got != "lock" {
		t.Fatalf("door-lock fallback mapped as %q", got)
	}
}

func TestInitialAttributeReadFailuresAreErrors(t *testing.T) {
	networkFailure := errors.New("router restarted")
	failedStatus := im.Status{Global: im.StatusFailure}
	tests := []struct {
		name   string
		client stubAttributeClient
		cause  error
	}{
		{name: "network", client: stubAttributeClient{err: networkFailure}, cause: networkFailure},
		{name: "device status", client: stubAttributeClient{reports: []im.AttributeReport{{Status: &failedStatus}}}},
		{name: "empty report", client: stubAttributeClient{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ok, err := readAttribute(context.Background(), test.client, 1, temperatureCluster, 0)
			if err == nil || ok {
				t.Fatalf("failed attribute read became ok=%v err=%v", ok, err)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("attribute error lost its cause: %v", err)
			}
		})
	}
}

func TestInitialCapabilityStateDistinguishesZeroNullAndFailure(t *testing.T) {
	zero := stubAttributeClient{reports: []im.AttributeReport{{Value: im.Value{Type: tlv.TypeInt, Int: 0}}}}
	capabilities, state, err := endpointCapabilities(context.Background(), zero, 1, []uint32{temperatureCluster})
	if err != nil || !slices.Contains(capabilities, "measure_temperature") || state["measure_temperature"] != float64(0) {
		t.Fatalf("real zero was not retained: capabilities=%v state=%#v err=%v", capabilities, state, err)
	}
	unsignedZero := stubAttributeClient{reports: []im.AttributeReport{{Value: im.Value{Type: tlv.TypeUint, Uint: 0}}}}
	capabilities, state, err = endpointCapabilities(context.Background(), unsignedZero, 1, []uint32{humidityCluster})
	if err != nil || !slices.Contains(capabilities, "measure_humidity") || state["measure_humidity"] != float64(0) {
		t.Fatalf("real unsigned zero was not retained: capabilities=%v state=%#v err=%v", capabilities, state, err)
	}

	null := stubAttributeClient{reports: []im.AttributeReport{{Value: im.Value{Type: tlv.TypeNull}}}}
	capabilities, state, err = endpointCapabilities(context.Background(), null, 1, []uint32{temperatureCluster})
	if err != nil || !slices.Contains(capabilities, "measure_temperature") {
		t.Fatalf("nullable temperature capability was rejected: capabilities=%v err=%v", capabilities, err)
	}
	if _, exists := state["measure_temperature"]; exists {
		t.Fatalf("null temperature was persisted as a value: %#v", state)
	}

	failure := errors.New("temporary read timeout")
	capabilities, state, err = endpointCapabilities(context.Background(), stubAttributeClient{err: failure}, 1, []uint32{temperatureCluster})
	if !errors.Is(err, failure) || capabilities != nil || state != nil {
		t.Fatalf("read failure was not propagated: capabilities=%v state=%#v err=%v", capabilities, state, err)
	}
}

func TestIlluminanceMeasurementUsesMatterLogarithmicLuxEncoding(t *testing.T) {
	tests := []struct {
		raw  uint64
		want float64
	}{
		{raw: 0, want: 0},
		{raw: 1, want: 1},
		{raw: 10001, want: 10},
		{raw: 20001, want: 100},
	}
	for _, test := range tests {
		got, ok := decodeIlluminance(im.Value{Type: tlv.TypeUint, Uint: test.raw})
		if !ok || math.Abs(got.(float64)-test.want) > 1e-9 {
			t.Fatalf("illuminance raw %d = %v, %v; want %v", test.raw, got, ok, test.want)
		}
	}
	if _, ok := decodeIlluminance(im.Value{Type: tlv.TypeUint, Uint: 0xFFFF}); ok {
		t.Fatal("reserved illuminance value 0xFFFF was accepted as lux")
	}

	capabilities, state, err := endpointCapabilities(context.Background(), stubAttributeClient{
		reports: []im.AttributeReport{{Value: im.Value{Type: tlv.TypeUint, Uint: 20001}}},
	}, 3, []uint32{illuminanceCluster})
	if err != nil || !slices.Contains(capabilities, "measure_luminance") || math.Abs(state["measure_luminance"].(float64)-100) > 1e-9 {
		t.Fatalf("initial illuminance mapping: capabilities=%v state=%#v err=%v", capabilities, state, err)
	}
}

func TestUnsupportedOptionalBatteryAttributeIsAbsent(t *testing.T) {
	unsupported := im.Status{Global: im.StatusUnsupportedAttribute}
	capabilities, state, err := endpointCapabilities(context.Background(), stubAttributeClient{
		reports: []im.AttributeReport{{Status: &unsupported}},
	}, 1, []uint32{powerSourceCluster})
	if err != nil || slices.Contains(capabilities, "measure_battery") || len(state) != 0 {
		t.Fatalf("unsupported optional battery attribute was not skipped: capabilities=%v state=%#v err=%v", capabilities, state, err)
	}
}

func TestElectricalMeasurementsUseMatterUnitsAndNullableSupport(t *testing.T) {
	powerCapabilities, powerState, err := endpointCapabilities(context.Background(), stubAttributeClient{
		reports: []im.AttributeReport{{Value: im.Value{Type: tlv.TypeInt, Int: 12_345}}},
	}, 1, []uint32{electricalPowerCluster})
	if err != nil || !slices.Contains(powerCapabilities, "measure_power") ||
		math.Abs(powerState["measure_power"].(float64)-12.345) > 1e-9 {
		t.Fatalf("initial power mapping: capabilities=%v state=%#v err=%v", powerCapabilities, powerState, err)
	}

	energyValue := im.Value{Type: tlv.TypeStructure, Children: []im.Value{{
		Tag: tlv.Context(0), Type: tlv.TypeInt, Int: 2_345_678,
	}}}
	energyCapabilities, energyState, err := endpointCapabilities(context.Background(), stubAttributeClient{
		reports: []im.AttributeReport{{Value: energyValue}},
	}, 1, []uint32{electricalEnergyCluster})
	if err != nil || !slices.Contains(energyCapabilities, "meter_power") ||
		math.Abs(energyState["meter_power"].(float64)-2.345678) > 1e-9 {
		t.Fatalf("initial energy mapping: capabilities=%v state=%#v err=%v", energyCapabilities, energyState, err)
	}

	nullCapabilities, nullState, err := endpointCapabilities(context.Background(), stubAttributeClient{
		reports: []im.AttributeReport{{Value: im.Value{Type: tlv.TypeNull}}},
	}, 1, []uint32{electricalEnergyCluster})
	if err != nil || !slices.Contains(nullCapabilities, "meter_power") {
		t.Fatalf("nullable cumulative energy was rejected: capabilities=%v err=%v", nullCapabilities, err)
	}
	if _, exists := nullState["meter_power"]; exists {
		t.Fatalf("null cumulative energy became a reading: %#v", nullState)
	}

	unsupported := im.Status{Global: im.StatusUnsupportedAttribute}
	unsupportedCapabilities, _, err := endpointCapabilities(context.Background(), stubAttributeClient{
		reports: []im.AttributeReport{{Status: &unsupported}},
	}, 1, []uint32{electricalEnergyCluster})
	if err != nil || slices.Contains(unsupportedCapabilities, "meter_power") {
		t.Fatalf("unsupported cumulative energy became a capability: capabilities=%v err=%v", unsupportedCapabilities, err)
	}
}

// De melding is het enige wat een gebruiker van een mislukte browse te zien
// krijgt, dus hij moet de twee oorzaken uit elkaar houden: er stond niets open,
// of er stond wel wat open maar niet dit.
func TestNoMatchError(t *testing.T) {
	payload, err := onboarding.Parse("3497 011 2332")
	if err != nil {
		t.Fatalf("parse code: %v", err)
	}
	empty := noMatchError(payload, nil).Error()
	if !strings.Contains(empty, "Apple Home") {
		t.Fatalf("lege browse noemt de route niet: %s", empty)
	}
	busy := noMatchError(payload, []discovery.Node{
		{Instance: "ABC", DeviceName: "Stekker gang", Discriminator: 1234},
	}).Error()
	if !strings.Contains(busy, "Stekker gang") || !strings.Contains(busy, "1234") {
		t.Fatalf("gevonden apparaat wordt niet genoemd: %s", busy)
	}
	if strings.Contains(busy, "Apple Home") {
		t.Fatalf("verkeerde uitleg bij een browse die wél iets zag: %s", busy)
	}
}

// De soort moet uit de store terug te rekenen zijn: apparaten die gekoppeld
// zijn toen de koppelstroom de soort nog liet vallen staan op "other", en hun
// eigen store weet beter. Een lege store geeft bewust niets terug -- de
// endpointClass-fallback zou er "sensor" van maken, en niets weten is geen
// sensor.
func TestStoredClassRecomputesTheKind(t *testing.T) {
	socket := map[string]any{
		// Zoals het na een JSON-rondreis binnenkomt: []any met teksten.
		"matter.deviceTypes":    []any{"0x510", "0x10A"},
		"matter.serverClusters": []any{"0x6"},
	}
	if got := StoredClass(socket); got != "socket" {
		t.Fatalf("StoredClass(stekker) = %q, wilde socket", got)
	}
	light := map[string]any{"matter.deviceTypes": []string{"0x100"}}
	if got := StoredClass(light); got != "light" {
		t.Fatalf("StoredClass(lamp) = %q, wilde light", got)
	}
	if got := StoredClass(map[string]any{}); got != "" {
		t.Fatalf("StoredClass(leeg) = %q, wilde leeg: niets weten is geen sensor", got)
	}
}
