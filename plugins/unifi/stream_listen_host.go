//go:build !tamago

package main

import "net"

// streamListen bindt de beeldserver op een host aan localhost: Stulp draait
// op dezelfde machine en verder heeft niemand hier iets te zoeken.
func streamListen() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
