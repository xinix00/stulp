//go:build !linux && !darwin

package appproto

import (
	"errors"
	"net"
)

// PeerUID bestaat hier niet, en dat is geen gat maar de aard van het doel.
//
// Op HopOS (GOOS=tamago) is er geen kernel met gebruikers en geen unix-socket om
// een uid aan te vragen: een app is een slot met een eigen IP, en Stulp bereikt
// hem over TCP. Het slot dat op linux en darwin de uid-controle is, is daar de
// token-uitwisseling van AttachPort -- een nonce heen, een HMAC terug, in beide
// richtingen, zodat een meelezer niets heeft om te hergebruiken.
//
// Luid falen is hier het punt. Zou dit stil 0 teruggeven, dan zou CheckPeer
// "dezelfde gebruiker" concluderen op een platform waar die vraag niet bestaat,
// en dan was de uid-controle een dichte deur zonder muur eromheen.
func PeerUID(net.Conn) (uint32, error) {
	return 0, errors.New("attach: this platform has no unix sockets to ask a uid of -- use the port with a token (--attach-port)")
}
