package appproto

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID levert de uid van de andere kant van een unix-socket, zoals de kernel
// hem kent.
//
// LOCAL_PEERCRED op macOS, wat op Linux SO_PEERCRED is. Beide leveren een uid
// die vastligt bij het verbinden en die de andere kant niet zelf kan zetten.
//
// macOS is een ontwikkeldoel en geen doel om op te draaien; dit bestand staat er
// zodat attach werkt waar apps geschreven worden.
func PeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("attach: peer credentials need a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Xucred
	var credentialsErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialsErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credentialsErr != nil {
		return 0, fmt.Errorf("attach: peer credentials: %w", credentialsErr)
	}
	return credentials.Uid, nil
}
