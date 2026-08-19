//go:build !tamago

package transport

import (
	"fmt"
	"log/slog"
	"net"
)

// listenSockets op een host: één "udp"-socket, die het OS dual-stack bindt —
// v4 én v6 over dezelfde fd, dus geen tweede baan nodig (conn6 = nil).
func listenSockets(address string, _ *slog.Logger) (*net.UDPConn, *net.UDPConn, error) {
	resolved, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %q: %w", address, err)
	}
	conn, err := net.ListenUDP("udp", resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %q: %w", address, err)
	}
	return conn, nil, nil
}
