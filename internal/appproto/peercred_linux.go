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
// SO_PEERCRED op Linux. De waarde wordt vastgelegd bij het verbinden en is niet
// te zetten door degene die verbindt, dus is dit geen bewering van de app maar
// een feit van het besturingssysteem.
func PeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("attach: peer credentials need a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Ucred
	var credentialsErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialsErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credentialsErr != nil {
		return 0, fmt.Errorf("attach: peer credentials: %w", credentialsErr)
	}
	return credentials.Uid, nil
}
