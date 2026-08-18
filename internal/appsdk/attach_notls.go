//go:build stulp_notls

package appsdk

import (
	"errors"
	"net"
)

// Gebouwd met -tags stulp_notls: deze binary draagt crypto/tls niet.
//
// Dat scheelt bijna een megabyte, en het is de goede keuze voor een app die Stulp
// zelf start of die naast hem staat -- daar is de verbinding een socketpair of een
// unix-socket, en TLS eromheen zou niets beschermen wat de kernel niet al
// beschermt.
//
// Vraagt zo'n binary tóch om een poort met TLS, dan moet dat hier stoppen met een
// zin die zegt waarom. Stil terugvallen op een verbinding zonder TLS is het ene
// geval dat niet mag: dan denkt degene die het uitrolt dat er geheimhouding is
// terwijl die er niet is.
func dialTLS(config AttachConfig) (net.Conn, error) {
	return nil, errors.New("attach: this app was built with -tags stulp_notls and cannot speak TLS; " +
		"use a unix socket, or set STULP_ATTACH_PLAINTEXT=1 to accept that everything after the handshake is readable")
}
