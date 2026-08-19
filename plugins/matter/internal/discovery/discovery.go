// Package discovery browses the LAN for Matter nodes with DNS-SD over mDNS.
// Commissionable nodes advertise _matterc._udp (with the discriminator and
// commissioning mode in TXT), operational nodes advertise _matter._tcp with
// an instance name of <compressed-fabric-id>-<node-id>. Thread devices are
// visible too: the Thread border router (an Apple TV, for example) runs an
// advertising proxy that republishes them on the LAN.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	ServiceCommissionable = "_matterc._udp.local."
	ServiceOperational    = "_matter._tcp.local."
	// ServiceBorderRouter is Thread's commissioning service. A Matter map wants
	// it because a Thread node's path to Stulp runs through one of these, and
	// the border router's TXT record names the Thread network the node is on.
	ServiceBorderRouter = "_meshcop._udp.local."
	mdnsPort            = 5353

	// maxQuestionsPerQuery keeps one multicast packet comfortably inside a
	// single Ethernet frame's worth of DNS.
	maxQuestionsPerQuery = 12
)

// Node is one advertised Matter service instance.
type Node struct {
	// Kind is commissionable or operational for the two Matter services, and
	// the service type itself for anything else a sweep turns up.
	Kind string `json:"kind"`
	// Service is the DNS-SD type the instance was advertised under, such as
	// _hap._tcp.
	Service   string            `json:"service,omitempty"`
	Instance  string            `json:"instance"`
	Host      string            `json:"host,omitempty"`
	Port      uint16            `json:"port,omitempty"`
	Addresses []string          `json:"addresses,omitempty"`
	Text      map[string]string `json:"text,omitempty"`

	// Decoded from TXT for commissionable nodes.
	Discriminator     uint16 `json:"discriminator,omitempty"`
	CommissioningMode int    `json:"commissioningMode,omitempty"`
	VendorID          uint16 `json:"vendorId,omitempty"`
	ProductID         uint16 `json:"productId,omitempty"`
	DeviceName        string `json:"deviceName,omitempty"`

	// Split from the instance name for operational nodes.
	CompressedFabricID string `json:"compressedFabricId,omitempty"`
	NodeID             string `json:"nodeId,omitempty"`
}

// Browse queries both Matter services and collects responses for the given
// window. IPv4 and IPv6 multicast are both attempted; one working stack is
// enough. Note that on-network commissioning ultimately needs IPv6 — Thread
// devices are IPv6-only — but their DNS-SD records (including AAAA) also
// arrive over IPv4 from dual-stack advertising proxies.
func Browse(ctx context.Context, window time.Duration) ([]Node, error) {
	return BrowseServices(ctx, window, ServiceCommissionable, ServiceOperational)
}

// BrowseServices queries an explicit set of DNS-SD services. The Matter map
// adds the Thread border router service to the two Matter ones; nothing else is
// asked for, because a general survey of the LAN is not what Stulp is for.
func BrowseServices(ctx context.Context, window time.Duration, services ...string) ([]Node, error) {
	records, err := sweep(ctx, window, services)
	if err != nil {
		if records == nil {
			return platformBrowseOrError(ctx, window, services, err)
		}
		return records.nodes(), err
	}
	return records.nodes(), nil
}

// sweep sends one PTR query per service type and collects every answer for the
// window. It returns the collector so a caller can read either the instances or
// the service types the link advertised.
func sweep(ctx context.Context, window time.Duration, services []string) (*collector, error) {
	if window <= 0 {
		window = 3 * time.Second
	}
	deadline := time.Now().Add(window)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}

	queries, err := buildQueries(services)
	if err != nil {
		return nil, err
	}
	if len(queries) == 0 {
		return newCollector(), nil
	}
	transports, err := openTransports()
	if len(transports) == 0 {
		return nil, fmt.Errorf("open mDNS socket: %w", err)
	}
	working := transports[:0]
	var sendErrors []error
	for _, candidate := range transports {
		_ = candidate.connection.SetReadDeadline(deadline)
		sent := false
		for _, query := range queries {
			if _, writeErr := candidate.connection.WriteToUDP(query, candidate.destination); writeErr != nil {
				sendErrors = append(sendErrors, fmt.Errorf("%s: %w", candidate.zone, writeErr))
				break
			}
			sent = true
		}
		if !sent {
			candidate.connection.Close()
			continue
		}
		working = append(working, candidate)
	}
	transports = working
	if len(transports) == 0 {
		// Seen in the wild: VPN or endpoint-security software installs
		// reject routes for 224.0.0.0/4 and ff02::, so every multicast send
		// returns EHOSTUNREACH while regular LAN traffic works fine.
		return nil, fmt.Errorf("mDNS query could not be sent on any LAN interface (multicast blocked by VPN or firewall?): %w", errors.Join(sendErrors...))
	}
	type received struct {
		data []byte
		zone string // interface the packet arrived on, for link-local scoping
	}
	packets := make(chan received, 64)
	done := make(chan struct{}, len(transports))
	for _, candidate := range transports {
		go func(connection *net.UDPConn, destination *net.UDPAddr, zone string) {
			defer connection.Close()
			defer func() { done <- struct{}{} }()
			// One repeat query midway helps slow responders without waiting
			// for the full mDNS retry schedule.
			repeat := time.AfterFunc(time.Second, func() {
				for _, query := range queries {
					_, _ = connection.WriteToUDP(query, destination)
				}
			})
			defer repeat.Stop()
			buffer := make([]byte, 9000)
			for {
				count, _, readErr := connection.ReadFromUDP(buffer)
				if readErr != nil {
					return
				}
				packet := make([]byte, count)
				copy(packet, buffer[:count])
				select {
				case packets <- received{data: packet, zone: zone}:
				default:
				}
			}
		}(candidate.connection, candidate.destination, candidate.zone)
	}
	active := len(transports)

	records := newCollector()
	finished := 0
	for finished < active {
		select {
		case packet := <-packets:
			records.consume(packet.data, packet.zone)
		case <-done:
			finished++
		case <-ctx.Done():
			return records, ctx.Err()
		}
	}
	for {
		select {
		case packet := <-packets:
			records.consume(packet.data, packet.zone)
		default:
			return records, nil
		}
	}
}

func platformBrowseOrError(ctx context.Context, window time.Duration, services []string, multicastErr error) ([]Node, error) {
	nodes, err := platformBrowse(ctx, window, services)
	if err == nil {
		return nodes, nil
	}
	return nil, errors.Join(multicastErr, fmt.Errorf("platform mDNS fallback: %w", err))
}

type transport struct {
	connection  *net.UDPConn
	destination *net.UDPAddr
	zone        string // interface name, used to scope link-local answers
}

// buildQueries turns a list of service types into one or more mDNS packets,
// split so no single query outgrows a normal frame.
func buildQueries(services []string) ([][]byte, error) {
	queries := make([][]byte, 0, 1+len(services)/maxQuestionsPerQuery)
	for start := 0; start < len(services); start += maxQuestionsPerQuery {
		end := min(start+maxQuestionsPerQuery, len(services))
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
		builder.EnableCompression()
		if err := builder.StartQuestions(); err != nil {
			return nil, err
		}
		for _, service := range services[start:end] {
			name, err := dnsmessage.NewName(service)
			if err != nil {
				return nil, err
			}
			question := dnsmessage.Question{Name: name, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}
			if err := builder.Question(question); err != nil {
				return nil, err
			}
		}
		query, err := builder.Finish()
		if err != nil {
			return nil, err
		}
		queries = append(queries, query)
	}
	return queries, nil
}

type serviceRecord struct {
	host string
	port uint16
}

type collector struct {
	kinds     map[string]string // instance name → service type it was advertised under
	services  map[string]serviceRecord
	texts     map[string][]string
	addresses map[string][]string // host name → IP strings
}

func newCollector() *collector {
	return &collector{
		kinds:     make(map[string]string),
		services:  make(map[string]serviceRecord),
		texts:     make(map[string][]string),
		addresses: make(map[string][]string),
	}
}

func (c *collector) consume(packet []byte, zone string) {
	var parser dnsmessage.Parser
	if _, err := parser.Start(packet); err != nil {
		return
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return
	}
	sections := []func() ([]dnsmessage.Resource, error){
		parser.AllAnswers, parser.AllAuthorities, parser.AllAdditionals,
	}
	for _, section := range sections {
		resources, err := section()
		if err != nil {
			return
		}
		for _, resource := range resources {
			c.record(resource, zone)
		}
	}
}

func (c *collector) record(resource dnsmessage.Resource, zone string) {
	owner := resource.Header.Name.String()
	switch body := resource.Body.(type) {
	case *dnsmessage.PTRResource:
		instance := body.PTR.String()
		c.kinds[instance] = owner
	case *dnsmessage.SRVResource:
		c.services[owner] = serviceRecord{host: body.Target.String(), port: body.Port}
	case *dnsmessage.TXTResource:
		c.texts[owner] = append(c.texts[owner], body.TXT...)
	case *dnsmessage.AResource:
		ip := net.IP(body.A[:])
		c.addresses[owner] = appendUnique(c.addresses[owner], ip.String())
	case *dnsmessage.AAAAResource:
		ip := net.IP(body.AAAA[:])
		address := ip.String()
		// A link-local answer is only reachable through the interface it
		// arrived on; without the %zone the address is undialable.
		if ip.IsLinkLocalUnicast() && zone != "" {
			address += "%" + zone
		}
		c.addresses[owner] = appendUnique(c.addresses[owner], address)
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// ResolveHost resolves a DNS-SD target returned by Browse. Raw mDNS replies
// normally include their A/AAAA additionals, but platform discovery APIs may
// return SRV records first and require a separate address lookup.
func ResolveHost(ctx context.Context, host string, window time.Duration) ([]string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("Matter service host is empty")
	}
	if window <= 0 {
		window = 2 * time.Second
	}
	resolveCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	addresses, err := platformResolve(resolveCtx, strings.TrimSuffix(host, "."))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(addresses, func(left, right int) bool {
		return matterAddressRank(addresses[left]) < matterAddressRank(addresses[right])
	})
	return addresses, nil
}

func matterAddressRank(address string) int {
	host := address
	if before, _, ok := strings.Cut(address, "%"); ok {
		host = before
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return 4
	case ip.To4() == nil && !ip.IsLinkLocalUnicast():
		return 0 // Thread ULA or global IPv6
	case ip.To4() != nil:
		return 1
	case ip.IsLinkLocalUnicast():
		return 2
	default:
		return 3
	}
}

func (c *collector) nodes() []Node {
	result := make([]Node, 0, len(c.kinds))
	for instance, service := range c.kinds {
		node := Node{
			Kind:     kindOf(service),
			Service:  strings.TrimSuffix(strings.TrimSuffix(service, "local."), "."),
			Instance: dnsUnescape(strings.TrimSuffix(trimService(instance, service), ".")),
		}
		if service, ok := c.services[instance]; ok {
			node.Host = strings.TrimSuffix(service.host, ".")
			node.Port = service.port
			node.Addresses = c.addresses[service.host]
		}
		if entries := c.texts[instance]; len(entries) > 0 {
			node.Text = make(map[string]string, len(entries))
			for _, entry := range entries {
				key, value, _ := strings.Cut(entry, "=")
				node.Text[key] = value
			}
			node.decodeText()
		}
		if node.Kind == "operational" {
			if fabric, nodeID, ok := strings.Cut(node.Instance, "-"); ok {
				node.CompressedFabricID, node.NodeID = fabric, nodeID
			}
		}
		result = append(result, node)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Kind != result[right].Kind {
			return result[left].Kind < result[right].Kind
		}
		return result[left].Instance < result[right].Instance
	})
	return result
}

// kindOf keeps the two Matter services under the names the commissioning code
// already matches on, and lets every other service describe itself.
func kindOf(service string) string {
	switch service {
	case ServiceCommissionable:
		return "commissionable"
	case ServiceOperational:
		return "operational"
	default:
		return strings.TrimSuffix(strings.TrimSuffix(service, "local."), ".")
	}
}

// dnsUnescape resolves the \DDD and \c escapes DNS-SD uses in instance names,
// so "AthomBV\032HomeyPro" reads as the name its owner gave it.
func dnsUnescape(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var result []byte
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 >= len(value) {
			result = append(result, value[index])
			continue
		}
		if index+3 < len(value) {
			if code, err := strconv.Atoi(value[index+1 : index+4]); err == nil && code < 256 {
				result = append(result, byte(code))
				index += 3
				continue
			}
		}
		index++
		result = append(result, value[index])
	}
	return string(result)
}

func trimService(instance, service string) string {
	if trimmed, ok := strings.CutSuffix(instance, "."+service); ok {
		return trimmed
	}
	return instance
}

func (n *Node) decodeText() {
	if value, err := strconv.ParseUint(n.Text["D"], 10, 16); err == nil {
		n.Discriminator = uint16(value)
	}
	if value, err := strconv.Atoi(n.Text["CM"]); err == nil {
		n.CommissioningMode = value
	}
	// VP mag "vendor+product" zijn maar ook alleen "vendor": het product-ID is
	// optioneel. Cut moest daarom niet over het bestaan van de plus beslissen --
	// dan hield een apparaat dat alleen zijn vendor meldt er geen van tweeën aan
	// over.
	if text, ok := n.Text["VP"]; ok {
		vendor, product, _ := strings.Cut(text, "+")
		if value, err := strconv.ParseUint(vendor, 10, 16); err == nil {
			n.VendorID = uint16(value)
		}
		if value, err := strconv.ParseUint(product, 10, 16); err == nil {
			n.ProductID = uint16(value)
		}
	}
	n.DeviceName = n.Text["DN"]
}
