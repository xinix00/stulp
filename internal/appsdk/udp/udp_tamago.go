//go:build tamago

// De tamago-gedaante van package udp: dezelfde API, en élk gebruik weigert
// luid met de reden. Multicast en broadcast bestaan niet in leannet (bewuste
// KAM-grens), dus SSDP/mDNS-ontdekking kan op een HopOS-node niet — maar een
// plugin die naast discovery ook gewone apparaten draagt hoort daar wél te
// LINKEN en te draaien. Zelfde naad-stijl als appspike's smp_riscv64: weigeren
// met een reden, niet stil half werken en niet de hele build blokkeren.
package udp

import (
	"errors"
	"net"
)

// Socket bestaat op tamago alleen als type; Listen geeft hem nooit uit.
type Socket struct {
	*net.UDPConn
	network string
}

// Options spiegelt de host-vorm, zodat gedeelde plugin-code compileert.
type Options struct {
	ReuseAddr bool
}

var errNoMulticast = errors.New("appsdk/udp: multicast/broadcast discovery is not available on a HopOS node (leannet has no multicast); guard discovery behind a capability check")

// Listen weigert luid: er is geen multicast om op te luisteren.
func Listen(network, address string, port int, options Options) (*Socket, error) {
	return nil, errNoMulticast
}

func (s *Socket) SetBroadcast(on bool) error                 { return errNoMulticast }
func (s *Socket) JoinGroup(group, iface net.IP) error        { return errNoMulticast }
func (s *Socket) LeaveGroup(group, iface net.IP) error       { return errNoMulticast }
func (s *Socket) SetMulticastInterface(address net.IP) error { return errNoMulticast }
func (s *Socket) SetMulticastTTL(ttl int) error              { return errNoMulticast }
func (s *Socket) SetMulticastLoopback(on bool) error         { return errNoMulticast }
