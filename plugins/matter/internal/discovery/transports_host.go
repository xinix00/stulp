//go:build !tamago

package discovery

// transports_host.go opent de mDNS-sockets in de proces-gedaante: één per
// LAN-interface, met x/net's IP_MULTICAST_IF-besturing. De node-gedaante
// (transports_tamago.go) heeft precies één interface en geen syscalls — de
// x/net-laag bestaat daar niet.

import (
	"net"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// openTransports opens one ephemeral-port socket per usable interface
// address and pairs it with the mDNS group as the query destination.
// Querying from a port other than 5353 makes this a "legacy" one-shot query
// (RFC 6762 §6.7): responders unicast their answers straight back to us.
// That sidesteps sharing port 5353 with a system daemon such as macOS
// mDNSResponder, which otherwise swallows the group traffic. Binding per
// interface address also reaches responders beyond the default route —
// smart-home devices tend to live on their own VLAN.
func openTransports() ([]transport, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []transport
	var lastErr error
	for _, candidate := range interfaces {
		if !multicastLANInterface(candidate.Flags) {
			continue
		}
		addresses, addressErr := candidate.Addrs()
		if addressErr != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			if address4 := network.IP.To4(); address4 != nil {
				// Use a wildcard local address and select the egress interface with
				// IP_MULTICAST_IF below. macOS network extensions can reject a
				// multicast send from an explicitly bound LAN address even when the
				// route and socket option both point at that same interface.
				connection, listenErr := net.ListenUDP("udp4", &net.UDPAddr{})
				if listenErr != nil {
					lastErr = listenErr
					continue
				}
				// Binding a source address is insufficient on macOS when a VPN adds
				// a competing multicast route. Pin IP_MULTICAST_IF so a LAN query
				// cannot be diverted into a Tailscale/WireGuard tunnel.
				if interfaceErr := ipv4.NewPacketConn(connection).SetMulticastInterface(&candidate); interfaceErr != nil {
					connection.Close()
					lastErr = interfaceErr
					continue
				}
				result = append(result, transport{
					connection:  connection,
					destination: &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort},
					zone:        candidate.Name,
				})
			} else if network.IP.IsLinkLocalUnicast() {
				connection, listenErr := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
				if listenErr != nil {
					lastErr = listenErr
					continue
				}
				if interfaceErr := ipv6.NewPacketConn(connection).SetMulticastInterface(&candidate); interfaceErr != nil {
					connection.Close()
					lastErr = interfaceErr
					continue
				}
				result = append(result, transport{
					connection:  connection,
					destination: &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: mdnsPort, Zone: candidate.Name},
					zone:        candidate.Name,
				})
			}
		}
	}
	return result, lastErr
}

func multicastLANInterface(flags net.Flags) bool {
	return flags&net.FlagUp != 0 && flags&net.FlagMulticast != 0 &&
		flags&net.FlagLoopback == 0 && flags&net.FlagPointToPoint == 0
}
