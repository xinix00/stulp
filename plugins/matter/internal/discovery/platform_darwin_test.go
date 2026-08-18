//go:build darwin

package discovery

import "testing"

func TestParseDNSServiceZone(t *testing.T) {
	const zone = `_matterc._udp                                PTR     4C17FC0E930A269F._matterc._udp
4C17FC0E930A269F._matterc._udp             SRV     0 0 5540 B6E87FB8481098C3.local. ; comment
4C17FC0E930A269F._matterc._udp             TXT     "VP=4447+4104" "D=2813" "CM=2" "DN=Aqara switch"
`
	records := newCollector()
	parseDNSServiceZone(records, ServiceCommissionable, zone)
	host := "B6E87FB8481098C3.local."
	records.addresses[host] = []string{"fd1a:8120:96bb:1:eaba:3301:dd60:7841"}
	nodes := records.nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes: %#v", len(nodes), nodes)
	}
	node := nodes[0]
	if node.Instance != "4C17FC0E930A269F" || node.Host != "B6E87FB8481098C3.local" || node.Port != 5540 {
		t.Fatalf("SRV record parsed as %+v", node)
	}
	if node.VendorID != 4447 || node.ProductID != 4104 || node.Discriminator != 2813 || node.CommissioningMode != 2 {
		t.Fatalf("TXT record parsed as %+v", node)
	}
	if node.DeviceName != "Aqara switch" {
		t.Fatalf("quoted TXT value with a space parsed as %q", node.DeviceName)
	}
}

func TestMatterAddressRankPrefersRoutableIPv6(t *testing.T) {
	if matterAddressRank("fd00::1") >= matterAddressRank("192.168.1.2") {
		t.Fatal("routable IPv6 should be preferred for Matter/Thread")
	}
	if matterAddressRank("192.168.1.2") >= matterAddressRank("fe80::1%en0") {
		t.Fatal("IPv4 should be preferred over a scoped link-local fallback")
	}
}

func TestParseDNSServiceAddresses(t *testing.T) {
	const output = `Timestamp     A/R  Flags         IF  Hostname                               Address                                      TTL
 7:51:35.290  Add  40000002      14  B6E87FB8481098C3.local.                FD1A:8120:96BB:0001:EABA:3301:DD60:7841%<0>  120
 7:51:35.291  Add  40000002      14  B6E87FB8481098C3.local.                192.168.1.42                                120
`
	addresses := parseDNSServiceAddresses(output)
	if len(addresses) != 2 || addresses[0] != "fd1a:8120:96bb:1:eaba:3301:dd60:7841" || addresses[1] != "192.168.1.42" {
		t.Fatalf("addresses parsed as %v", addresses)
	}
}

// A TXT value is arbitrary octets. A Thread border router publishes its
// extended PAN ID as eight raw bytes, and decoding that as text would replace
// every byte that is not valid UTF-8 with a substitute character.
func TestZoneTXTValuesSurviveRawBytes(t *testing.T) {
	line := "Woonkamer._meshcop._udp TXT \"nn=MyHome12\" \"xp=\xb4\x44\x83\x6c\x32\x60\x4a\x7f\" \"tv=1.3.0\""
	values := quotedZoneValues(line)
	if len(values) != 3 {
		t.Fatalf("got %d values: %q", len(values), values)
	}
	want := "xp=" + string([]byte{0xb4, 0x44, 0x83, 0x6c, 0x32, 0x60, 0x4a, 0x7f})
	if values[1] != want {
		t.Fatalf("extended PAN ID = % x, want % x", values[1], want)
	}
	if values[0] != "nn=MyHome12" || values[2] != "tv=1.3.0" {
		t.Fatalf("neighbouring values were disturbed: %q", values)
	}
}
