//go:build linux || darwin

package appproto

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Pair is a connected pair of sockets: one end stays with Stulp, the other is
// handed to the app process as fd 3.
//
// A socketpair rather than a socket file: no path to choose, no permissions to
// get right, nothing left behind after a crash, and no window between spawning
// the child and it connecting. It also means an app process has no way to reach
// another app's channel -- there is nothing to address.
type Pair struct {
	// Parent is Stulp's end.
	Parent *os.File
	// Child is handed to the app through exec.Cmd.ExtraFiles and closed by the
	// caller once the process has started.
	Child *os.File
}

// NewPair creates the socket pair.
func NewPair() (*Pair, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	return &Pair{
		Parent: os.NewFile(uintptr(fds[0]), "stulp-parent"),
		Child:  os.NewFile(uintptr(fds[1]), "stulp-child"),
	}, nil
}

// Conn wraps Stulp's end so the protocol can be spoken over it.
func (p *Pair) Conn() (net.Conn, error) { return net.FileConn(p.Parent) }
