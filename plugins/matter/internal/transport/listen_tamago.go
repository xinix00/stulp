//go:build tamago

package transport

import (
	"fmt"
	"log/slog"
	"net"
)

// listenSockets op een HopOS-node: v4 en v6 zijn gescheiden sockets — leannet
// bindt per familie (IPV6_V6ONLY-semantiek), en de v6-baan (leanipv6) ontstaat
// pas doordat wij hem openen. Dat is de opt-in: matter is de reden dat deze
// node IPv6 spreekt, en een node zonder matter betaalt er niets voor.
//
// Faalt de v6-baan, dan draait v4 door met een luide logregel: een kern van
// vóór de leanipv6-release doet dan tenminste nog on-link v4-matter in plaats
// van helemaal niets.
func listenSockets(address string, logger *slog.Logger) (*net.UDPConn, *net.UDPConn, error) {
	resolved, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %q: %w", address, err)
	}
	conn, err := net.ListenUDP("udp", resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %q: %w", address, err)
	}
	resolved6, err := net.ResolveUDPAddr("udp6", address)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("resolve %q (v6): %w", address, err)
	}
	conn6, err := net.ListenUDP("udp6", resolved6)
	if err != nil {
		logger.Warn("Matter IPv6 lane unavailable; Thread devices out of reach", "error", err)
		return conn, nil, nil
	}
	return conn, conn6, nil
}
