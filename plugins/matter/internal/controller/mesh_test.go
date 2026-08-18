package controller

import "testing"

func neighbourOf(extAddress string, lqi uint64, child bool) ThreadNeighbour {
	value, isChild := lqi, child
	return ThreadNeighbour{ExtAddress: extAddress, LQI: &value, IsChild: &isChild}
}

// A link both ends report confirms the address behind it. One only a single
// side reports is still drawn, but must not claim the same certainty.
func TestMutualLinksAreMarkedAndDeduplicated(t *testing.T) {
	nodes := []MeshNode{
		{NodeID: "0000000000000001", ExtAddress: "AAAAAAAAAAAAAAA1"},
		{NodeID: "0000000000000002", ExtAddress: "AAAAAAAAAAAAAAA2"},
		{NodeID: "0000000000000003", ExtAddress: "AAAAAAAAAAAAAAA3"},
	}
	neighbours := [][]ThreadNeighbour{
		{neighbourOf("AAAAAAAAAAAAAAA2", 200, false), neighbourOf("AAAAAAAAAAAAAAA3", 90, true)},
		{neighbourOf("AAAAAAAAAAAAAAA1", 198, false)},
		{}, // node 3 never answered
	}
	links, unidentified := buildLinks(nodes, neighbours)
	if unidentified != 0 {
		t.Fatalf("unidentified = %d, want 0", unidentified)
	}
	if len(links) != 2 {
		t.Fatalf("got %d links, want the mutual pair collapsed into one: %#v", len(links), links)
	}
	var mutual, single int
	for _, link := range links {
		if link.Mutual {
			mutual++
			continue
		}
		single++
		if link.To != "0000000000000003" || !link.IsChild {
			t.Fatalf("one-sided link = %#v", link)
		}
	}
	if mutual != 1 || single != 1 {
		t.Fatalf("got %d mutual and %d one-sided links", mutual, single)
	}
}

// A Thread mesh carries devices from other fabrics. They are neighbours of our
// nodes but not ours to draw, and the count says so rather than hiding them.
func TestNeighboursOutsideTheFabricAreCounted(t *testing.T) {
	nodes := []MeshNode{{NodeID: "0000000000000001", ExtAddress: "AAAAAAAAAAAAAAA1"}}
	links, unidentified := buildLinks(nodes, [][]ThreadNeighbour{{
		neighbourOf("BBBBBBBBBBBBBBBB", 170, false),
		neighbourOf("CCCCCCCCCCCCCCCC", 60, false),
	}})
	if unidentified != 2 {
		t.Fatalf("unidentified = %d, want 2", unidentified)
	}
	for _, link := range links {
		if link.To != "" {
			t.Fatalf("a foreign neighbour was resolved to a node: %#v", link)
		}
		if link.ToExtAddress == "" {
			t.Fatalf("a foreign neighbour lost its address: %#v", link)
		}
	}
}

// Without the DNS-SD hostnames nothing can be joined, and the map must still be
// drawable rather than claiming links it cannot support.
func TestLinksWithoutKnownAddressesStayUnresolved(t *testing.T) {
	nodes := []MeshNode{{NodeID: "0000000000000001"}, {NodeID: "0000000000000002"}}
	links, unidentified := buildLinks(nodes, [][]ThreadNeighbour{
		{neighbourOf("AAAAAAAAAAAAAAA2", 200, false)}, {},
	})
	if unidentified != 1 || len(links) != 1 || links[0].To != "" || links[0].Mutual {
		t.Fatalf("links = %#v, unidentified = %d", links, unidentified)
	}
}

// Neighbour tables report addresses in whatever case the stack prefers, and a
// case difference must not split one device into two.
func TestAddressMatchingIgnoresCase(t *testing.T) {
	nodes := []MeshNode{
		{NodeID: "0000000000000001", ExtAddress: "A1B2C3D4E5F60718"},
		{NodeID: "0000000000000002", ExtAddress: "00112233445566AA"},
	}
	links, unidentified := buildLinks(nodes, [][]ThreadNeighbour{
		{neighbourOf("00112233445566aa", 210, false)},
		{neighbourOf("a1b2c3d4e5f60718", 205, false)},
	})
	if unidentified != 0 {
		t.Fatalf("case difference broke the match: %d unidentified", unidentified)
	}
	if len(links) != 1 || !links[0].Mutual {
		t.Fatalf("links = %#v, want one mutual link", links)
	}
}

func TestIsHexRejectsHostnamesThatAreNotAddresses(t *testing.T) {
	for _, value := range []string{"", "GHIJKLMNOPQRSTUV", "a1b2c3d4e5f60718", "9013DABB240A!"} {
		if isHex(value) {
			t.Errorf("isHex(%q) accepted a non-address", value)
		}
	}
	if !isHex("A1B2C3D4E5F60718") {
		t.Error("a valid IEEE address was rejected")
	}
}

// Thread is a low-bandwidth radio behind one border router, and questioning
// several accessories at once has been observed to upset that router. The whole
// point of the mesh call is that it stays gentle.
func TestMeshQuestionsOneNodeAtATime(t *testing.T) {
	if meshWorkers != 1 {
		t.Fatalf("meshWorkers = %d; the mesh is meant to ask one node at a time", meshWorkers)
	}
}
