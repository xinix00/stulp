//go:build !tamago

// Package udp is de UDP-kant die ontdekkingsprotocollen nodig hebben.
//
// Go's net-pakket geeft je een UDP-socket en verder niets: geen multicast join,
// geen broadcast, geen keuze van uitgaande interface. Dat zijn setsockopt-opties
// op de rauwe descriptor, en elke plugin die SSDP, mDNS of een broadcast-zoekerij
// doet zou ze anders zelf opnieuw schrijven.
//
// Dit pakket bestond eerst als onderdeel van de dgram-shim voor gecompileerde
// JavaScript-apps. Het is er uitgetild omdat het niets met JavaScript te maken
// heeft en een Go-plugin er evenveel aan heeft.
//
// De opties gaan via x/sys/unix (Stulp draait Linux, de ontwikkelmachine
// macOS). Op tamago/HopOS bestaat multicast niet eens in de netstack: dáár
// linkt de stub (udp_tamago.go) die bij het EERSTE gebruik luid weigert —
// zo kan één plugin-binary host én node bedienen en degradeert alleen zijn
// discovery, in plaats van dat de hele app niet compileert (open punt 6 van
// nacht 12-08).
package udp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Socket is een UDP-socket met de opties eromheen. De ingesloten *net.UDPConn
// blijft gewoon bruikbaar voor lezen en schrijven.
type Socket struct {
	*net.UDPConn
	network string
}

// Options zijn de keuzes die vóór het binden gemaakt moeten worden.
type Options struct {
	// ReuseAddr laat meerdere sockets dezelfde poort delen. Voor SSDP is dat
	// geen luxe: poort 1900 is gedeeld, en zonder deze optie mislukt de bind
	// zodra er iets anders op de machine luistert.
	ReuseAddr bool
}

// Listen bindt een UDP-socket. network is "udp4" of "udp6"; een leeg adres
// betekent alle interfaces, poort 0 betekent een vrije poort.
func Listen(network, address string, port int, options Options) (*Socket, error) {
	if network != "udp4" && network != "udp6" {
		return nil, fmt.Errorf("udp: %q is not udp4 or udp6", network)
	}
	config := net.ListenConfig{}
	if options.ReuseAddr {
		config.Control = func(_, _ string, c syscall.RawConn) error {
			var opErr error
			if err := c.Control(func(fd uintptr) {
				if opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); opErr != nil {
					return
				}
				// BSD (en dus macOS) heeft SO_REUSEPORT nodig om een poort echt
				// te delen; Linux accepteert hem ook.
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return opErr
		}
	}
	packet, err := config.ListenPacket(context.Background(), network, net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	conn, ok := packet.(*net.UDPConn)
	if !ok {
		packet.Close()
		return nil, fmt.Errorf("udp: %s did not give a UDP socket", network)
	}
	return &Socket{UDPConn: conn, network: network}, nil
}

func (s *Socket) v6() bool { return s.network == "udp6" }

// SetBroadcast staat verzenden naar een broadcastadres toe. Zonder deze optie
// weigert de kernel een pakket naar 255.255.255.255, dus wie hem overslaat
// stuurt een broadcast die nooit vertrekt.
func (s *Socket) SetBroadcast(on bool) error {
	return s.setInt(unix.SOL_SOCKET, unix.SO_BROADCAST, boolInt(on))
}

// JoinGroup meldt de socket aan bij een multicastgroep.
//
// Zonder interface gaat de join naar élke interface die aan staat en multicast
// kan, en slaagt de aanroep alleen als er minstens één gelukt is. De kernel zou
// anders de interface van de default route kiezen, en op een machine met meer
// dan één netwerk is dat regelmatig niet die van het apparaat. Succes melden bij
// nul joins zou een plugin laten luisteren naar niets.
func (s *Socket) JoinGroup(group, iface net.IP) error { return s.membership(group, iface, false) }

// LeaveGroup is het omgekeerde van JoinGroup.
func (s *Socket) LeaveGroup(group, iface net.IP) error { return s.membership(group, iface, true) }

// SetMulticastInterface kiest waar multicast naar buiten gaat. Zonder die keuze
// doet de kernel een route-lookup op de groep, en op een machine met een VPN
// eindigt dat in een reject-route -- dan vertrekt een M-SEARCH nooit.
func (s *Socket) SetMulticastInterface(address net.IP) error {
	if address == nil {
		return fmt.Errorf("udp: no interface address")
	}
	return s.control(func(fd int) error {
		if s.v6() {
			found, err := interfaceFor(address)
			if err != nil {
				return err
			}
			return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, found.Index)
		}
		var v4 [4]byte
		copy(v4[:], address.To4())
		return unix.SetsockoptInet4Addr(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, v4)
	})
}

// SetMulticastTTL bepaalt hoeveel routers een multicastpakket mag passeren. 1 is
// het eigen netwerk, en dat is wat ontdekking in huis nodig heeft.
func (s *Socket) SetMulticastTTL(ttl int) error {
	if s.v6() {
		return s.setInt(unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, ttl)
	}
	return s.setInt(unix.IPPROTO_IP, unix.IP_MULTICAST_TTL, ttl)
}

// SetMulticastLoopback bepaalt of de machine zijn eigen multicast terugziet.
func (s *Socket) SetMulticastLoopback(on bool) error {
	if s.v6() {
		return s.setInt(unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_LOOP, boolInt(on))
	}
	return s.setInt(unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, boolInt(on))
}

func (s *Socket) membership(group, iface net.IP, leave bool) error {
	if group == nil || !group.IsMulticast() {
		return fmt.Errorf("udp: %v is not a multicast address", group)
	}
	if iface != nil {
		index := 0
		if s.v6() {
			found, err := interfaceFor(iface)
			if err != nil {
				return err
			}
			index = found.Index
		}
		return s.control(func(fd int) error { return s.join(fd, group, iface, index, leave) })
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	joined := 0
	var lastErr error
	if err := s.control(func(fd int) error {
		for i := range interfaces {
			candidate := &interfaces[i]
			if candidate.Flags&net.FlagUp == 0 || candidate.Flags&net.FlagMulticast == 0 {
				continue
			}
			local := interfaceAddress(candidate, s.v6())
			if !s.v6() && local == nil {
				continue
			}
			if err := s.join(fd, group, local, candidate.Index, leave); err != nil {
				lastErr = err
				continue
			}
			joined++
		}
		return nil
	}); err != nil {
		return err
	}
	if joined > 0 {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("udp: no interface is up and multicast-capable")
}

func (s *Socket) join(fd int, group, iface net.IP, index int, leave bool) error {
	if s.v6() {
		request := &unix.IPv6Mreq{Interface: uint32(index)}
		copy(request.Multiaddr[:], group.To16())
		option := unix.IPV6_JOIN_GROUP
		if leave {
			option = unix.IPV6_LEAVE_GROUP
		}
		return unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, option, request)
	}
	request := &unix.IPMreq{}
	copy(request.Multiaddr[:], group.To4())
	if iface != nil {
		copy(request.Interface[:], iface.To4())
	}
	option := unix.IP_ADD_MEMBERSHIP
	if leave {
		option = unix.IP_DROP_MEMBERSHIP
	}
	return unix.SetsockoptIPMreq(fd, unix.IPPROTO_IP, option, request)
}

func (s *Socket) setInt(level, option, value int) error {
	return s.control(func(fd int) error { return unix.SetsockoptInt(fd, level, option, value) })
}

func (s *Socket) control(fn func(fd int) error) error {
	raw, err := s.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := raw.Control(func(fd uintptr) { opErr = fn(int(fd)) }); err != nil {
		return err
	}
	return opErr
}

// InterfaceAddress levert het adres van een interface in de gevraagde familie.
func interfaceAddress(ifi *net.Interface, wantV6 bool) net.IP {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		network, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if isV4 := network.IP.To4() != nil; isV4 == wantV6 {
			continue
		}
		return network.IP
	}
	return nil
}

// interfaceFor zoekt de interface die dit adres draagt.
func interfaceFor(ip net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range interfaces {
		addrs, err := interfaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if network, ok := addr.(*net.IPNet); ok && network.IP.Equal(ip) {
				return &interfaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("udp: no interface has address %s", ip)
}

func boolInt(on bool) int {
	if on {
		return 1
	}
	return 0
}
