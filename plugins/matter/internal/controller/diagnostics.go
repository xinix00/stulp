package controller

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

// Matter's diagnostics clusters. A node implements the one matching its radio,
// so a Thread accessory has 0x0035 and a Wi-Fi one has 0x0036.
const (
	generalDiagnosticsCluster uint32 = 0x0033
	threadDiagnosticsCluster  uint32 = 0x0035
	wifiDiagnosticsCluster    uint32 = 0x0036
)

// NodeDiagnostics is what a node reports about itself when asked. Everything
// here comes from the accessory over its own operational connection; Stulp
// measures nothing and infers nothing.
//
// Every field is optional because every attribute is. A node that does not
// implement a cluster says so through Missing rather than through zeros, and a
// value outside its physically possible range is dropped rather than shown.
type NodeDiagnostics struct {
	NodeID string `json:"nodeId"`
	// Inventory is the last operational Descriptor/global-attribute snapshot.
	// It is refreshed when the device model is inspected, not guessed from the
	// capabilities Stulp happened to create.
	Inventory []EndpointInventory `json:"inventory,omitempty"`

	VendorName      string `json:"vendorName,omitempty"`
	ProductName     string `json:"productName,omitempty"`
	HardwareVersion string `json:"hardwareVersion,omitempty"`
	SoftwareVersion string `json:"softwareVersion,omitempty"`
	SerialNumber    string `json:"serialNumber,omitempty"`

	UpTimeSeconds         *uint64  `json:"upTimeSeconds,omitempty"`
	TotalOperationalHours *uint64  `json:"totalOperationalHours,omitempty"`
	RebootCount           *uint64  `json:"rebootCount,omitempty"`
	BootReason            string   `json:"bootReason,omitempty"`
	ActiveFaults          []string `json:"activeFaults,omitempty"`

	Thread *ThreadDiagnostics `json:"thread,omitempty"`
	WiFi   *WiFiDiagnostics   `json:"wifi,omitempty"`

	// Missing names the clusters this node does not implement, so an empty
	// section reads as "not offered" instead of "nothing to report".
	Missing []string `json:"missing,omitempty"`
	// Errors records what could not be read. A node answering some clusters and
	// refusing others is normal and should not hide the parts that worked.
	Errors []string `json:"errors,omitempty"`
}

// ThreadDiagnostics is the mesh as the node sees it. The neighbour table is the
// interesting part: it is the only view of the Thread topology available to a
// controller that owns no radio.
type ThreadDiagnostics struct {
	Channel        *uint64 `json:"channel,omitempty"`
	RoutingRole    string  `json:"routingRole,omitempty"`
	NetworkName    string  `json:"networkName,omitempty"`
	PanID          *uint64 `json:"panId,omitempty"`
	ExtendedPanID  string  `json:"extendedPanId,omitempty"`
	PartitionID    *uint64 `json:"partitionId,omitempty"`
	LeaderRouterID *uint64 `json:"leaderRouterId,omitempty"`
	OverrunCount   *uint64 `json:"overrunCount,omitempty"`

	Neighbours []ThreadNeighbour `json:"neighbours,omitempty"`
	Routes     []ThreadRoute     `json:"routes,omitempty"`
}

// ThreadNeighbour is one radio link the node can see. LQI and RSSI describe how
// good that link is; the error rates describe how often it fails.
type ThreadNeighbour struct {
	ExtAddress       string  `json:"extAddress,omitempty"`
	Rloc16           *uint64 `json:"rloc16,omitempty"`
	AgeSeconds       *uint64 `json:"ageSeconds,omitempty"`
	LQI              *uint64 `json:"lqi,omitempty"`
	AverageRSSI      *int64  `json:"averageRssi,omitempty"`
	LastRSSI         *int64  `json:"lastRssi,omitempty"`
	FrameErrorRate   *uint64 `json:"frameErrorRate,omitempty"`
	MessageErrorRate *uint64 `json:"messageErrorRate,omitempty"`
	RxOnWhenIdle     *bool   `json:"rxOnWhenIdle,omitempty"`
	FullThreadDevice *bool   `json:"fullThreadDevice,omitempty"`
	IsChild          *bool   `json:"isChild,omitempty"`
}

// ThreadRoute is one router the node knows how to reach.
type ThreadRoute struct {
	ExtAddress      string  `json:"extAddress,omitempty"`
	Rloc16          *uint64 `json:"rloc16,omitempty"`
	RouterID        *uint64 `json:"routerId,omitempty"`
	PathCost        *uint64 `json:"pathCost,omitempty"`
	LQIIn           *uint64 `json:"lqiIn,omitempty"`
	LQIOut          *uint64 `json:"lqiOut,omitempty"`
	AgeSeconds      *uint64 `json:"ageSeconds,omitempty"`
	Allocated       *bool   `json:"allocated,omitempty"`
	LinkEstablished *bool   `json:"linkEstablished,omitempty"`
}

// WiFiDiagnostics is the radio state of a node on Wi-Fi rather than Thread.
type WiFiDiagnostics struct {
	BSSID           string  `json:"bssid,omitempty"`
	SecurityType    string  `json:"securityType,omitempty"`
	Version         string  `json:"version,omitempty"`
	Channel         *uint64 `json:"channel,omitempty"`
	RSSI            *int64  `json:"rssi,omitempty"`
	BeaconLostCount *uint64 `json:"beaconLostCount,omitempty"`
	BeaconRxCount   *uint64 `json:"beaconRxCount,omitempty"`
	PacketUnicastRx *uint64 `json:"packetUnicastRx,omitempty"`
	PacketUnicastTx *uint64 `json:"packetUnicastTx,omitempty"`
	OverrunCount    *uint64 `json:"overrunCount,omitempty"`
	CurrentMaxRate  *uint64 `json:"currentMaxRate,omitempty"`
}

// Diagnostics asks one node what it knows about itself and its radio. It reads
// three clusters with wildcard attribute paths, so it costs three round trips
// regardless of how much the node implements.
//
// A node is only asked when the caller wants it; nothing here runs on a timer.
func (c *Controller) Diagnostics(ctx context.Context, deviceID string) (NodeDiagnostics, error) {
	device, err := c.store.Device(ctx, deviceID)
	if err != nil {
		return NodeDiagnostics{}, err
	}
	info, err := deviceConnection(device)
	if err != nil {
		return NodeDiagnostics{}, err
	}
	session, err := c.session(ctx, info)
	if err != nil {
		return NodeDiagnostics{}, err
	}
	client := im.Client{Transport: c.node, Session: session}
	result := NodeDiagnostics{
		NodeID:    fmt.Sprintf("%016X", info.nodeID),
		Inventory: storedEndpointInventories(device.Store["~matter.endpointInventory"]),
	}

	// Endpoint 0 is the node itself: Basic Information and the diagnostics
	// clusters live there, never on an application endpoint.
	basic, err := readCluster(ctx, client, 0, basicCluster)
	switch {
	case err != nil:
		result.Errors = append(result.Errors, "Basisinformatie: "+err.Error())
	default:
		result.readBasic(basic)
	}

	general, err := readCluster(ctx, client, 0, generalDiagnosticsCluster)
	switch {
	case err != nil:
		result.Errors = append(result.Errors, "Algemene diagnostiek: "+err.Error())
	case len(general) == 0:
		result.Missing = append(result.Missing, "Algemene diagnostiek")
	default:
		result.readGeneral(general)
	}

	thread, threadErr := readCluster(ctx, client, 0, threadDiagnosticsCluster)
	wifi, wifiErr := readCluster(ctx, client, 0, wifiDiagnosticsCluster)
	switch {
	case threadErr == nil && len(thread) > 0:
		result.Thread = readThread(thread)
	case wifiErr == nil && len(wifi) > 0:
		result.WiFi = readWiFi(wifi)
	default:
		// A node has exactly one radio diagnostics cluster; not finding either
		// is worth saying, because it means the mesh view is unavailable rather
		// than empty.
		result.Missing = append(result.Missing, "Radiodiagnostiek (Thread of Wi-Fi)")
	}
	return result, nil
}

// readCluster reads every attribute of one cluster in a single exchange. A
// cluster the node does not implement yields no reports rather than an error,
// which is how Missing gets filled in.
func readCluster(ctx context.Context, client attributeClient, endpoint uint16, cluster uint32) (map[uint32]im.Value, error) {
	path := im.AttributePath{Endpoint: &endpoint, Cluster: &cluster}
	reports, err := client.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	values := make(map[uint32]im.Value, len(reports))
	for _, report := range reports {
		if report.Status != nil || report.Path.Attribute == nil {
			continue
		}
		values[*report.Path.Attribute] = report.Value
	}
	return values, nil
}

// Basic Information attributes (Matter core specification, cluster 0x0028).
func (d *NodeDiagnostics) readBasic(values map[uint32]im.Value) {
	d.VendorName = stringOf(values, 0x0001)
	d.ProductName = stringOf(values, 0x0003)
	d.HardwareVersion = stringOf(values, 0x0008)
	d.SoftwareVersion = stringOf(values, 0x000A)
	d.SerialNumber = stringOf(values, 0x000F)
}

// General Diagnostics attributes (cluster 0x0033).
func (d *NodeDiagnostics) readGeneral(values map[uint32]im.Value) {
	d.RebootCount = unsignedOf(values, 0x0001, 1<<32)
	// A device claiming more than a century of uptime has told us something
	// other than seconds.
	d.UpTimeSeconds = unsignedOf(values, 0x0002, 100*365*24*3600)
	d.TotalOperationalHours = unsignedOf(values, 0x0003, 100*365*24)
	if reason, ok := values[0x0004]; ok {
		d.BootReason = codeName(bootReasons, reason.Uint)
	}
	for attribute, label := range map[uint32]string{
		0x0005: "hardware", 0x0006: "radio", 0x0007: "netwerk",
	} {
		if faults, ok := values[attribute]; ok && len(faults.Children) > 0 {
			d.ActiveFaults = append(d.ActiveFaults,
				fmt.Sprintf("%d actieve %sfout(en)", len(faults.Children), label))
		}
	}
	sort.Strings(d.ActiveFaults)
}

// Thread Network Diagnostics attributes (cluster 0x0035).
func readThread(values map[uint32]im.Value) *ThreadDiagnostics {
	result := &ThreadDiagnostics{
		// Thread runs on IEEE 802.15.4 channels 11 to 26.
		Channel:        boundedUnsigned(values, 0x0000, 11, 26),
		NetworkName:    stringOf(values, 0x0002),
		PanID:          unsignedOf(values, 0x0003, 0xFFFF),
		ExtendedPanID:  bytesOf(values, 0x0004),
		OverrunCount:   unsignedOf(values, 0x0006, 1<<63),
		PartitionID:    unsignedOf(values, 0x0009, 1<<32),
		LeaderRouterID: unsignedOf(values, 0x000D, 62),
	}
	if role, ok := values[0x0001]; ok {
		result.RoutingRole = codeName(routingRoles, role.Uint)
	}
	for _, entry := range values[0x0007].Children {
		result.Neighbours = append(result.Neighbours, readNeighbour(entry))
	}
	for _, entry := range values[0x0008].Children {
		result.Routes = append(result.Routes, readRoute(entry))
	}
	return result
}

func readNeighbour(entry im.Value) ThreadNeighbour {
	fields := fieldsOf(entry)
	return ThreadNeighbour{
		ExtAddress: hexOf(fields, 0),
		AgeSeconds: unsignedOf(fields, 1, 1<<32),
		Rloc16:     unsignedOf(fields, 2, 0xFFFF),
		// LQI is a link quality indicator on a 0-255 scale.
		LQI:              unsignedOf(fields, 5, 255),
		AverageRSSI:      dBm(fields, 6),
		LastRSSI:         dBm(fields, 7),
		FrameErrorRate:   unsignedOf(fields, 8, 100),
		MessageErrorRate: unsignedOf(fields, 9, 100),
		RxOnWhenIdle:     booleanOf(fields, 10),
		FullThreadDevice: booleanOf(fields, 11),
		IsChild:          booleanOf(fields, 13),
	}
}

func readRoute(entry im.Value) ThreadRoute {
	fields := fieldsOf(entry)
	return ThreadRoute{
		ExtAddress:      hexOf(fields, 0),
		Rloc16:          unsignedOf(fields, 1, 0xFFFF),
		RouterID:        unsignedOf(fields, 2, 62),
		PathCost:        unsignedOf(fields, 4, 16),
		LQIIn:           unsignedOf(fields, 5, 255),
		LQIOut:          unsignedOf(fields, 6, 255),
		AgeSeconds:      unsignedOf(fields, 7, 1<<32),
		Allocated:       booleanOf(fields, 8),
		LinkEstablished: booleanOf(fields, 9),
	}
}

// Wi-Fi Network Diagnostics attributes (cluster 0x0036).
func readWiFi(values map[uint32]im.Value) *WiFiDiagnostics {
	result := &WiFiDiagnostics{
		BSSID: bytesOf(values, 0x0000),
		// 2.4 GHz starts at channel 1; 6 GHz reaches 233.
		Channel:         boundedUnsigned(values, 0x0003, 1, 233),
		RSSI:            dBm(values, 0x0004),
		BeaconLostCount: unsignedOf(values, 0x0005, 1<<32),
		BeaconRxCount:   unsignedOf(values, 0x0006, 1<<32),
		PacketUnicastRx: unsignedOf(values, 0x0009, 1<<32),
		PacketUnicastTx: unsignedOf(values, 0x000A, 1<<32),
		CurrentMaxRate:  unsignedOf(values, 0x000B, 1<<40),
		OverrunCount:    unsignedOf(values, 0x000C, 1<<63),
	}
	if security, ok := values[0x0001]; ok {
		result.SecurityType = codeName(wifiSecurityTypes, security.Uint)
	}
	if version, ok := values[0x0002]; ok {
		result.Version = codeName(wifiVersions, version.Uint)
	}
	return result
}

func fieldsOf(entry im.Value) map[uint32]im.Value {
	fields := make(map[uint32]im.Value, len(entry.Children))
	for _, child := range entry.Children {
		if number, ok := child.Tag.ContextNumber(); ok {
			fields[uint32(number)] = child
		}
	}
	return fields
}

func stringOf(values map[uint32]im.Value, attribute uint32) string {
	value, ok := values[attribute]
	if !ok || value.Type != tlv.TypeString {
		return ""
	}
	return string(value.Data)
}

func bytesOf(values map[uint32]im.Value, attribute uint32) string {
	value, ok := values[attribute]
	if !ok || value.Type != tlv.TypeBytes || len(value.Data) == 0 {
		return ""
	}
	return hex.EncodeToString(value.Data)
}

func hexOf(values map[uint32]im.Value, attribute uint32) string {
	value, ok := values[attribute]
	if !ok {
		return ""
	}
	if value.Type == tlv.TypeBytes {
		return hex.EncodeToString(value.Data)
	}
	if value.Type == tlv.TypeUint {
		return fmt.Sprintf("%016X", value.Uint)
	}
	return ""
}

// unsignedOf returns an attribute only when it is an unsigned integer within a
// plausible range. Silence beats a number that cannot be true: an attribute
// read from the wrong identifier, or a device reporting nonsense, must not
// arrive on the page looking like a measurement.
func unsignedOf(values map[uint32]im.Value, attribute uint32, maximum uint64) *uint64 {
	value, ok := values[attribute]
	if !ok || value.Type != tlv.TypeUint || value.Uint > maximum {
		return nil
	}
	result := value.Uint
	return &result
}

func boundedUnsigned(values map[uint32]im.Value, attribute uint32, minimum, maximum uint64) *uint64 {
	result := unsignedOf(values, attribute, maximum)
	if result == nil || *result < minimum {
		return nil
	}
	return result
}

// dBm reads a signed radio measurement. Anything outside the range a real
// receiver can report is treated as absent.
func dBm(values map[uint32]im.Value, attribute uint32) *int64 {
	value, ok := values[attribute]
	if !ok || value.Type != tlv.TypeInt || value.Int < -128 || value.Int > 20 {
		return nil
	}
	result := value.Int
	return &result
}

func booleanOf(values map[uint32]im.Value, attribute uint32) *bool {
	value, ok := values[attribute]
	if !ok || value.Type != tlv.TypeBool {
		return nil
	}
	result := value.Bool
	return &result
}

var (
	bootReasons = []string{"onbekend", "spanning ingeschakeld", "brownout", "hardwarewatchdog",
		"softwarewatchdog", "software-update", "software gaf opdracht"}
	routingRoles = []string{"onbekend", "niet toegewezen", "losgekoppeld", "slapend eindapparaat",
		"eindapparaat", "router-kandidaat", "router", "leider"}
	wifiSecurityTypes = []string{"onbepaald", "geen", "WEP", "WPA", "WPA2", "WPA3"}
	wifiVersions      = []string{"802.11a", "802.11b", "802.11g", "802.11n", "802.11ac", "802.11ax", "802.11ah"}
)

// codeName names an enumerated Matter value. A code a newer Matter revision
// added is shown as its number rather than dropped, so an unfamiliar value
// stays visible instead of reading as the first entry in the table.
func codeName(names []string, value uint64) string {
	if value < uint64(len(names)) {
		return names[value]
	}
	return fmt.Sprintf("code %d", value)
}
