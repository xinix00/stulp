package controller

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/discovery"
)

// meshWorkers bounds how many nodes are questioned at once, and one is
// deliberate.
//
// Thread is a low-bandwidth radio and every exchange with a Thread node is
// relayed by a border router. Asking several accessories at the same time puts
// that burst through one relay, which has been observed to upset the border
// router rather than the accessories. One at a time is slower and kinder, and
// the answers stream out as they arrive so the wait is visible instead of
// silent. Raise this only with evidence that a particular network tolerates it.
const meshWorkers = 1

// MeshNode is one accessory as the drawing needs it: who it is, how Stulp
// reaches it, and what its radio reports.
type MeshNode struct {
	NodeID      string `json:"nodeId"`
	Name        string `json:"name"`
	DeviceID    string `json:"deviceId"`
	Address     string `json:"address,omitempty"`
	SessionOpen bool   `json:"sessionOpen"`
	Subscribed  bool   `json:"subscribed"`
	Endpoints   int    `json:"endpoints"`

	// ExtAddress is the node's IEEE address as advertised in DNS-SD. It is what
	// makes a neighbour table entry resolvable to a node on the map.
	ExtAddress  string `json:"extAddress,omitempty"`
	NetworkName string `json:"networkName,omitempty"`
	RoutingRole string `json:"routingRole,omitempty"`
	Radio       string `json:"radio,omitempty"` // thread | wifi
	RSSI        *int64 `json:"rssi,omitempty"`
	Neighbours  int    `json:"neighbours"`
	// Pending marks a node that is still being questioned, so the drawing can
	// show it as known-but-unanswered rather than as having nothing to report.
	Pending bool   `json:"pending,omitempty"`
	Error   string `json:"error,omitempty"`
}

// MeshLink is one radio link between two nodes, as reported by at least one of
// them.
type MeshLink struct {
	From string `json:"from"` // node ID
	To   string `json:"to"`   // node ID, empty when the neighbour is not one of ours
	// ToExtAddress names the far end when it could not be resolved to a node
	// Stulp knows. Thread meshes contain devices from other fabrics.
	ToExtAddress string  `json:"toExtAddress,omitempty"`
	LQI          *uint64 `json:"lqi,omitempty"`
	RSSI         *int64  `json:"rssi,omitempty"`
	FrameErrors  *uint64 `json:"frameErrorRate,omitempty"`
	IsChild      bool    `json:"isChild"`
	// Kind is "radio" for a neighbour a node reported, and "border" for the
	// link between a node and the border router serving its Thread network.
	Kind string `json:"kind"`
	// Mutual means both ends reported each other. A one-sided link is still
	// real — a sleepy child may simply not have been asked — but only a mutual
	// one confirms that the DNS-SD address actually is that node's radio
	// address, so the drawing shows the two differently.
	Mutual bool `json:"mutual"`
}

// MeshRouter is a Thread border router. It is not a Matter node and Stulp has
// no session with it, but every Thread node's path to Stulp runs through one,
// so leaving it off the map leaves out how the mesh is actually reached.
type MeshRouter struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	NetworkName   string   `json:"networkName,omitempty"`
	ExtendedPanID string   `json:"extendedPanId,omitempty"`
	Vendor        string   `json:"vendor,omitempty"`
	Model         string   `json:"model,omitempty"`
	Addresses     []string `json:"addresses,omitempty"`
}

// MeshMap is the whole picture: the controller, its nodes, the border routers
// carrying them, and the links between them all.
type MeshMap struct {
	Nodes   []MeshNode   `json:"nodes"`
	Routers []MeshRouter `json:"routers"`
	Links   []MeshLink   `json:"links"`
	// Unidentified counts neighbours that belong to no node Stulp knows. They
	// are part of the mesh but not part of this fabric.
	Unidentified int      `json:"unidentified"`
	Warnings     []string `json:"warnings,omitempty"`
}

// MeshUpdate is one node's answer together with everything that answer already
// proves about its connections. The links travel with the node rather than at
// the end, so a drawing grows as a graph instead of as loose dots that snap
// together at the last moment.
type MeshUpdate struct {
	Node  MeshNode   `json:"node"`
	Links []MeshLink `json:"links"`
	Index int        `json:"index"`
	Total int        `json:"total"`
}

// Mesh questions every node and joins the answers into a graph.
//
// The join is deliberate about what it can prove. A neighbour is reported by
// its IEEE address; a node's own IEEE address is only known from the hostname
// it publishes in DNS-SD. That mapping is an inference, so a link both ends
// report is marked mutual and one only a single side reports is not, and the
// drawing distinguishes them instead of presenting a guess as a measurement.
// onStart, when given, receives the shape of the map before any node is
// questioned: every node as a placeholder plus the border routers. onNode then
// receives each answer with its links. Both may be called from worker
// goroutines and must be safe to call concurrently.
func (c *Controller) Mesh(ctx context.Context, window time.Duration,
	onStart func(MeshMap), onNode func(MeshUpdate)) (MeshMap, error) {
	fabric, statuses, err := c.Topology(ctx)
	if err != nil {
		return MeshMap{}, err
	}
	result := MeshMap{
		Nodes: make([]MeshNode, 0, len(statuses)),
		Links: []MeshLink{}, Routers: []MeshRouter{},
	}
	if len(fabric.Unplaceable) > 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("%d apparaten hebben geen bruikbare node-identiteit en staan niet op de kaart", len(fabric.Unplaceable)))
	}
	if len(statuses) == 0 {
		if onStart != nil {
			onStart(result)
		}
		return result, nil
	}

	// One browse serves two purposes: the hostname of an operational record is
	// the only place a node's radio address is visible from outside, and the
	// border routers announce which Thread network carries which node.
	addresses := c.operationalExtAddresses(ctx, window, &result)
	result.Routers = c.borderRouters(ctx, window)

	nodes := make([]MeshNode, len(statuses))
	for index, status := range statuses {
		nodes[index] = placeholderNode(status, addresses)
	}
	if onStart != nil {
		start := result
		start.Nodes = append([]MeshNode(nil), nodes...)
		onStart(start)
	}

	neighbourLists := make([][]ThreadNeighbour, len(statuses))
	var group sync.WaitGroup
	work := make(chan int)
	for range min(meshWorkers, len(statuses)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range work {
				status := statuses[index]
				node := placeholderNode(status, addresses)
				node.Pending = false
				var neighbours []ThreadNeighbour
				if node.DeviceID != "" {
					// Sequential questioning makes a long per-node timeout
					// expensive for everyone behind it, so an unresponsive node
					// is given less rope.
					nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					diagnostics, diagErr := c.Diagnostics(nodeCtx, node.DeviceID)
					cancel()
					switch {
					case diagErr != nil:
						node.Error = diagErr.Error()
					case diagnostics.Thread != nil:
						node.Radio = "thread"
						node.RoutingRole = diagnostics.Thread.RoutingRole
						node.NetworkName = diagnostics.Thread.NetworkName
						node.Neighbours = len(diagnostics.Thread.Neighbours)
						neighbours = diagnostics.Thread.Neighbours
						neighbourLists[index] = neighbours
					case diagnostics.WiFi != nil:
						node.Radio = "wifi"
						node.RSSI = diagnostics.WiFi.RSSI
					}
				}
				nodes[index] = node
				if onNode != nil {
					// Everything this answer proves is already known: the
					// neighbour addresses were resolved before any node was
					// asked, and the routers were browsed at the same time.
					onNode(MeshUpdate{
						Node:  node,
						Links: append(nodeLinks(node, neighbours, byExtAddress(addresses)), routerLinks([]MeshNode{node}, result.Routers)...),
						Index: index, Total: len(statuses),
					})
				}
			}
		}()
	}
	for index := range statuses {
		select {
		case work <- index:
		case <-ctx.Done():
		}
	}
	close(work)
	group.Wait()

	result.Nodes = nodes
	result.Links, result.Unidentified = buildLinks(nodes, neighbourLists)
	result.Links = append(result.Links, routerLinks(nodes, result.Routers)...)
	return result, nil
}

// placeholderNode is what the map can say about a node before it has answered:
// who it is and how Stulp reaches it, with the radio still unknown.
func placeholderNode(status NodeStatus, addresses map[string]string) MeshNode {
	node := MeshNode{
		NodeID: status.NodeID, Address: status.Address,
		SessionOpen: status.SessionOpen, Subscribed: status.Subscribed,
		Endpoints: len(status.Devices), ExtAddress: addresses[status.NodeID],
		Pending: true,
	}
	if len(status.Devices) > 0 {
		node.Name, node.DeviceID = status.Devices[0].Name, status.Devices[0].ID
	}
	return node
}

func byExtAddress(addresses map[string]string) map[string]string {
	index := make(map[string]string, len(addresses))
	for nodeID, address := range addresses {
		index[address] = nodeID
	}
	return index
}

// nodeLinks turns one node's neighbour table into edges. Mutual cannot be known
// yet — the other end may not have answered — so these arrive unconfirmed and
// the final map settles them.
func nodeLinks(node MeshNode, neighbours []ThreadNeighbour, byAddress map[string]string) []MeshLink {
	links := make([]MeshLink, 0, len(neighbours))
	for _, neighbour := range neighbours {
		far := strings.ToUpper(neighbour.ExtAddress)
		links = append(links, MeshLink{
			Kind: "radio", From: node.NodeID, To: byAddress[far], ToExtAddress: far,
			LQI: neighbour.LQI, RSSI: neighbour.AverageRSSI,
			FrameErrors: neighbour.FrameErrorRate,
			IsChild:     neighbour.IsChild != nil && *neighbour.IsChild,
		})
	}
	return links
}

// borderRouters lists the Thread border routers on the link. A failed browse is
// silent here: operationalExtAddresses already warned about the same failure,
// and repeating it would only make the page noisier.
func (c *Controller) borderRouters(ctx context.Context, window time.Duration) []MeshRouter {
	if window <= 0 {
		window = 3 * time.Second
	}
	advertised, _ := discovery.BrowseServices(ctx, window, discovery.ServiceBorderRouter)
	routers := make([]MeshRouter, 0, len(advertised))
	for _, node := range advertised {
		router := MeshRouter{
			ID: "router:" + node.Instance, Name: node.Instance,
			NetworkName: node.Text["nn"], Vendor: node.Text["vn"], Model: node.Text["mn"],
			Addresses: node.Addresses,
		}
		// The extended PAN ID is eight raw bytes in the TXT record.
		if raw := node.Text["xp"]; raw != "" {
			router.ExtendedPanID = strings.ToUpper(hex.EncodeToString([]byte(raw)))
		}
		routers = append(routers, router)
	}
	sort.Slice(routers, func(left, right int) bool { return routers[left].Name < routers[right].Name })
	return routers
}

// routerLinks joins a Thread node to the border routers of its own network. The
// node reports the network it is on and the router advertises the network it
// serves, so this match is read from both ends rather than inferred.
func routerLinks(nodes []MeshNode, routers []MeshRouter) []MeshLink {
	links := make([]MeshLink, 0)
	for _, node := range nodes {
		if node.NetworkName == "" {
			continue
		}
		for _, router := range routers {
			if !strings.EqualFold(router.NetworkName, node.NetworkName) {
				continue
			}
			links = append(links, MeshLink{From: node.NodeID, To: router.ID, Kind: "border", Mutual: true})
		}
	}
	return links
}

// operationalExtAddresses maps node IDs to the IEEE address in their DNS-SD
// hostname. A failed browse is a warning, not an error: the map is still worth
// drawing without link identification.
func (c *Controller) operationalExtAddresses(ctx context.Context, window time.Duration, result *MeshMap) map[string]string {
	addresses := make(map[string]string)
	if window <= 0 {
		window = 3 * time.Second
	}
	advertised, err := discovery.BrowseServices(ctx, window, discovery.ServiceOperational)
	if err != nil {
		result.Warnings = append(result.Warnings,
			"radiobuurt kon niet worden opgezocht, dus verbindingen blijven onbenoemd: "+err.Error())
	}
	for _, node := range advertised {
		if node.NodeID == "" {
			continue
		}
		host := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(node.Host, "."), ".local"))
		// Only a 64-bit IEEE address can match a Thread neighbour entry; a
		// shorter hostname belongs to a node on Wi-Fi or Ethernet.
		if len(host) != 16 || !isHex(host) {
			continue
		}
		addresses[strings.ToUpper(node.NodeID)] = host
	}
	return addresses
}

func isHex(value string) bool {
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9', character >= 'A' && character <= 'F':
		default:
			return false
		}
	}
	return value != ""
}

// buildLinks turns per-node neighbour tables into edges, resolving each
// neighbour to a node where the IEEE address is known and counting the rest.
func buildLinks(nodes []MeshNode, neighbourLists [][]ThreadNeighbour) ([]MeshLink, int) {
	byExtAddress := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node.ExtAddress != "" {
			byExtAddress[node.ExtAddress] = node.NodeID
		}
	}
	// reported[a][b] means a listed b as a neighbour.
	reported := make(map[string]map[string]bool)
	links := make([]MeshLink, 0)
	unidentified := 0
	for index, neighbours := range neighbourLists {
		from := nodes[index].NodeID
		for _, neighbour := range neighbours {
			far := strings.ToUpper(neighbour.ExtAddress)
			to := byExtAddress[far]
			if to == "" {
				unidentified++
			} else {
				if reported[from] == nil {
					reported[from] = make(map[string]bool)
				}
				reported[from][to] = true
			}
			links = append(links, MeshLink{
				Kind: "radio", From: from, To: to, ToExtAddress: far,
				LQI: neighbour.LQI, RSSI: neighbour.AverageRSSI,
				FrameErrors: neighbour.FrameErrorRate,
				IsChild:     neighbour.IsChild != nil && *neighbour.IsChild,
			})
		}
	}
	// A link both ends report confirms the hostname-to-radio-address mapping
	// for both of them, so it is the only kind drawn as certain.
	deduplicated := make([]MeshLink, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, link := range links {
		if link.To != "" {
			link.Mutual = reported[link.To][link.From]
			// One edge per pair: keep the direction that sorts first so a
			// mutual link is drawn once.
			key := link.From + "|" + link.To
			if link.From > link.To {
				key = link.To + "|" + link.From
			}
			if link.Mutual && seen[key] {
				continue
			}
			seen[key] = true
		}
		deduplicated = append(deduplicated, link)
	}
	sort.SliceStable(deduplicated, func(left, right int) bool {
		if deduplicated[left].From != deduplicated[right].From {
			return deduplicated[left].From < deduplicated[right].From
		}
		return deduplicated[left].ToExtAddress < deduplicated[right].ToExtAddress
	})
	return deduplicated, unidentified
}
