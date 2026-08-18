package controller

import (
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func unsignedValue(value uint64) im.Value {
	return im.Value{Type: tlv.TypeUint, Uint: value}
}

func signedValue(value int64) im.Value {
	return im.Value{Type: tlv.TypeInt, Int: value}
}

func stringValue(value string) im.Value {
	return im.Value{Type: tlv.TypeString, Data: []byte(value)}
}

func boolValue(value bool) im.Value {
	return im.Value{Type: tlv.TypeBool, Bool: value}
}

// structValue builds a TLV struct the way a device encodes a list entry, with
// context tags numbering its fields.
func structValue(fields map[uint8]im.Value) im.Value {
	entry := im.Value{Type: tlv.TypeStructure}
	for number, value := range fields {
		value.Tag = tlv.Context(number)
		entry.Children = append(entry.Children, value)
	}
	return entry
}

func TestThreadDiagnosticsDecodeTheMeshView(t *testing.T) {
	values := map[uint32]im.Value{
		0x0000: unsignedValue(15),           // Channel
		0x0001: unsignedValue(6),            // RoutingRole: router
		0x0002: stringValue("Derek-Thread"), // NetworkName
		0x0003: unsignedValue(0x1A2B),       // PanId
		0x0009: unsignedValue(4242),         // PartitionId
		0x000D: unsignedValue(12),           // LeaderRouterId
		0x0007: {Type: tlv.TypeArray, Children: []im.Value{ // NeighborTable
			structValue(map[uint8]im.Value{
				0:  unsignedValue(0xA1B2C3D4E5F60718),
				1:  unsignedValue(37),
				2:  unsignedValue(0x4401),
				5:  unsignedValue(180),
				6:  signedValue(-62),
				7:  signedValue(-58),
				8:  unsignedValue(2),
				9:  unsignedValue(1),
				10: boolValue(false),
				11: boolValue(false),
				13: boolValue(true),
			}),
		}},
	}
	thread := readThread(values)
	if thread.Channel == nil || *thread.Channel != 15 {
		t.Fatalf("channel = %v", thread.Channel)
	}
	if thread.RoutingRole != "router" {
		t.Fatalf("routing role = %q", thread.RoutingRole)
	}
	if thread.NetworkName != "Derek-Thread" || thread.PanID == nil || *thread.PanID != 0x1A2B {
		t.Fatalf("network = %#v", thread)
	}
	if len(thread.Neighbours) != 1 {
		t.Fatalf("neighbours = %#v", thread.Neighbours)
	}
	neighbour := thread.Neighbours[0]
	if neighbour.ExtAddress != "A1B2C3D4E5F60718" {
		t.Fatalf("neighbour address = %q", neighbour.ExtAddress)
	}
	if neighbour.LQI == nil || *neighbour.LQI != 180 {
		t.Fatalf("LQI = %v", neighbour.LQI)
	}
	if neighbour.AverageRSSI == nil || *neighbour.AverageRSSI != -62 {
		t.Fatalf("average RSSI = %v", neighbour.AverageRSSI)
	}
	if neighbour.IsChild == nil || !*neighbour.IsChild {
		t.Fatalf("isChild = %v", neighbour.IsChild)
	}
	if neighbour.RxOnWhenIdle == nil || *neighbour.RxOnWhenIdle {
		t.Fatalf("rxOnWhenIdle = %v", neighbour.RxOnWhenIdle)
	}
}

// A value that cannot be true must not reach the page. Either the device is
// reporting nonsense or the attribute was read from the wrong identifier, and
// both are better shown as a gap than as a measurement.
func TestImpossibleValuesAreDroppedRatherThanShown(t *testing.T) {
	thread := readThread(map[uint32]im.Value{
		0x0000: unsignedValue(3),       // Thread has no channel 3
		0x0003: unsignedValue(0x1FFFF), // PAN IDs are 16 bits
		0x000D: unsignedValue(200),     // router IDs stop at 62
	})
	if thread.Channel != nil {
		t.Errorf("an impossible channel was reported: %v", *thread.Channel)
	}
	if thread.PanID != nil {
		t.Errorf("an oversized PAN ID was reported: %v", *thread.PanID)
	}
	if thread.LeaderRouterID != nil {
		t.Errorf("an impossible router ID was reported: %v", *thread.LeaderRouterID)
	}

	neighbour := readNeighbour(structValue(map[uint8]im.Value{
		5: unsignedValue(900), // LQI is a byte
		6: signedValue(40),    // no receiver reports +40 dBm
		8: unsignedValue(150), // a percentage
	}))
	if neighbour.LQI != nil || neighbour.AverageRSSI != nil || neighbour.FrameErrorRate != nil {
		t.Fatalf("impossible neighbour values survived: %#v", neighbour)
	}
}

// A device may answer with the wrong TLV type. Reading it as if it were right
// would put a zero on the page.
func TestWrongTypesAreTreatedAsAbsent(t *testing.T) {
	values := map[uint32]im.Value{
		0x0000: stringValue("15"),      // channel as text
		0x0002: unsignedValue(7),       // network name as a number
		0x0004: stringValue("not hex"), // extended PAN ID as text
	}
	thread := readThread(values)
	if thread.Channel != nil || thread.NetworkName != "" || thread.ExtendedPanID != "" {
		t.Fatalf("mistyped attributes were accepted: %#v", thread)
	}
}

func TestWiFiDiagnosticsDecodeTheRadioState(t *testing.T) {
	wifi := readWiFi(map[uint32]im.Value{
		0x0000: {Type: tlv.TypeBytes, Data: []byte{0xA0, 0xB1, 0xC2, 0xD3, 0xE4, 0xF5}},
		0x0001: unsignedValue(4), // WPA2
		0x0002: unsignedValue(3), // 802.11n
		0x0003: unsignedValue(11),
		0x0004: signedValue(-67),
		0x0005: unsignedValue(3),
	})
	if wifi.BSSID != "a0b1c2d3e4f5" {
		t.Fatalf("BSSID = %q", wifi.BSSID)
	}
	if wifi.SecurityType != "WPA2" || wifi.Version != "802.11n" {
		t.Fatalf("radio = %#v", wifi)
	}
	if wifi.Channel == nil || *wifi.Channel != 11 || wifi.RSSI == nil || *wifi.RSSI != -67 {
		t.Fatalf("channel/RSSI = %#v", wifi)
	}
	if wifi.BeaconLostCount == nil || *wifi.BeaconLostCount != 3 {
		t.Fatalf("beacon loss = %v", wifi.BeaconLostCount)
	}
}

func TestGeneralDiagnosticsReadUptimeAndBootReason(t *testing.T) {
	var result NodeDiagnostics
	result.readGeneral(map[uint32]im.Value{
		0x0001: unsignedValue(4),
		0x0002: unsignedValue(93_784),
		0x0003: unsignedValue(1200),
		0x0004: unsignedValue(3), // hardware watchdog
		0x0006: {Type: tlv.TypeArray, Children: []im.Value{unsignedValue(1), unsignedValue(2)}},
	})
	if result.RebootCount == nil || *result.RebootCount != 4 {
		t.Fatalf("reboot count = %v", result.RebootCount)
	}
	if result.UpTimeSeconds == nil || *result.UpTimeSeconds != 93_784 {
		t.Fatalf("uptime = %v", result.UpTimeSeconds)
	}
	if result.BootReason != "hardwarewatchdog" {
		t.Fatalf("boot reason = %q", result.BootReason)
	}
	if len(result.ActiveFaults) != 1 {
		t.Fatalf("faults = %#v", result.ActiveFaults)
	}
}

// An uptime of centuries is a unit mismatch, not a very reliable lamp.
func TestAbsurdUptimeIsRejected(t *testing.T) {
	var result NodeDiagnostics
	result.readGeneral(map[uint32]im.Value{0x0002: unsignedValue(1 << 62)})
	if result.UpTimeSeconds != nil {
		t.Fatalf("an absurd uptime was accepted: %v", *result.UpTimeSeconds)
	}
}

func TestEnumNamesFallBackToTheirCode(t *testing.T) {
	if got := codeName(routingRoles, 6); got != "router" {
		t.Fatalf("routing role 6 = %q", got)
	}
	// A future Matter revision may add values; naming them "code N" is honest
	// where inventing a label would not be.
	if got := codeName(routingRoles, 99); got != "code 99" {
		t.Fatalf("unknown routing role = %q", got)
	}
	if got := codeName(bootReasons, 255); got != "code 255" {
		t.Fatalf("unknown boot reason = %q", got)
	}
}
