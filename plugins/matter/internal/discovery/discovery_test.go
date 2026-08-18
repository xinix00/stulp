package discovery

import (
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestMulticastLANInterfaceRejectsVPNAndLoopback(t *testing.T) {
	lan := net.FlagUp | net.FlagMulticast | net.FlagBroadcast
	if !multicastLANInterface(lan) {
		t.Fatal("ordinary multicast LAN interface was rejected")
	}
	for name, flags := range map[string]net.Flags{
		"down":     net.FlagMulticast,
		"loopback": lan | net.FlagLoopback,
		"vpn":      lan | net.FlagPointToPoint,
	} {
		if multicastLANInterface(flags) {
			t.Fatalf("%s interface was accepted for LAN mDNS", name)
		}
	}
}

func name(t *testing.T, value string) dnsmessage.Name {
	t.Helper()
	parsed, err := dnsmessage.NewName(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// Build a realistic mDNS response and check the collector assembles it into
// one commissionable and one operational node.
func TestCollector(t *testing.T) {
	instance := "A1B2C3D4E5F60708._matterc._udp.local."
	operational := "ABCDEF1234567890-0000000000000001._matter._tcp.local."
	host := "bedroom-sensor.local."

	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	builder.EnableCompression()
	if err := builder.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	header := func(owner string, resourceType dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{
			Name: name(t, owner), Type: resourceType, Class: dnsmessage.ClassINET, TTL: 120,
		}
	}
	checks := []error{
		builder.PTRResource(header(ServiceCommissionable, dnsmessage.TypePTR),
			dnsmessage.PTRResource{PTR: name(t, instance)}),
		builder.PTRResource(header(ServiceOperational, dnsmessage.TypePTR),
			dnsmessage.PTRResource{PTR: name(t, operational)}),
		builder.SRVResource(header(instance, dnsmessage.TypeSRV),
			dnsmessage.SRVResource{Target: name(t, host), Port: 5540}),
		builder.TXTResource(header(instance, dnsmessage.TypeTXT),
			dnsmessage.TXTResource{TXT: []string{"D=3840", "CM=1", "VP=65521+32768", "DN=Bedroom Sensor"}}),
		builder.AResource(header(host, dnsmessage.TypeA),
			dnsmessage.AResource{A: [4]byte{192, 168, 1, 50}}),
		builder.AAAAResource(header(host, dnsmessage.TypeAAAA),
			dnsmessage.AAAAResource{AAAA: [16]byte{0: 0xfd, 1: 0x11, 15: 0x01}}),
		builder.AAAAResource(header(host, dnsmessage.TypeAAAA),
			dnsmessage.AAAAResource{AAAA: [16]byte{0: 0xfe, 1: 0x80, 15: 0x02}}),
	}
	for _, err := range checks {
		if err != nil {
			t.Fatal(err)
		}
	}
	packet, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}

	records := newCollector()
	records.consume(packet, "en0")
	nodes := records.nodes()
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2: %#v", len(nodes), nodes)
	}

	commissionable := nodes[0]
	if commissionable.Kind != "commissionable" {
		t.Fatalf("first node kind = %s", commissionable.Kind)
	}
	if commissionable.Discriminator != 3840 || commissionable.CommissioningMode != 1 {
		t.Fatalf("TXT decoding failed: %+v", commissionable)
	}
	if commissionable.VendorID != 65521 || commissionable.ProductID != 32768 {
		t.Fatalf("VP decoding failed: %+v", commissionable)
	}
	if commissionable.DeviceName != "Bedroom Sensor" {
		t.Fatalf("DN decoding failed: %+v", commissionable)
	}
	if commissionable.Host != "bedroom-sensor.local" || commissionable.Port != 5540 {
		t.Fatalf("SRV assembly failed: %+v", commissionable)
	}
	if len(commissionable.Addresses) != 3 {
		t.Fatalf("addresses = %v, want IPv4 + ULA + link-local", commissionable.Addresses)
	}
	// The routable ULA stays bare; the link-local answer must carry the
	// zone of the interface it arrived on, or it is undialable.
	for _, address := range commissionable.Addresses {
		if strings.HasPrefix(address, "fd11") && strings.Contains(address, "%") {
			t.Fatalf("routable address %q must not get a zone", address)
		}
		if strings.HasPrefix(address, "fe80") && !strings.HasSuffix(address, "%en0") {
			t.Fatalf("link-local address %q is missing its %%en0 zone", address)
		}
	}

	operationalNode := nodes[1]
	if operationalNode.Kind != "operational" {
		t.Fatalf("second node kind = %s", operationalNode.Kind)
	}
	if operationalNode.CompressedFabricID != "ABCDEF1234567890" || operationalNode.NodeID != "0000000000000001" {
		t.Fatalf("operational instance split failed: %+v", operationalNode)
	}
}

func TestCollectorIgnoresGarbage(t *testing.T) {
	records := newCollector()
	records.consume([]byte{0x01, 0x02, 0x03}, "en0")
	records.consume(nil, "")
	if nodes := records.nodes(); len(nodes) != 0 {
		t.Fatalf("garbage produced nodes: %#v", nodes)
	}
}

// DNS-SD escapes spaces and dots in instance names; a map should show the name
// its owner typed.
func TestInstanceNamesAreUnescaped(t *testing.T) {
	cases := map[string]string{
		`AthomBV\032HomeyPro\032#FBD0`: "AthomBV HomeyPro #FBD0",
		`Keuken\.lamp`:                 "Keuken.lamp",
		"Woonkamer":                    "Woonkamer",
		`trailing\`:                    `trailing\`,
	}
	for input, want := range cases {
		if got := dnsUnescape(input); got != want {
			t.Errorf("dnsUnescape(%q) = %q, want %q", input, got, want)
		}
	}
}
