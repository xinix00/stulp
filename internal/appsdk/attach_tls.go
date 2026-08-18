//go:build !stulp_notls

package appsdk

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
)

// TLS om een attach over een poort.
//
// Dit staat apart omdat crypto/tls bijna een megabyte aan een Go-binary toevoegt,
// en de meeste apps het hier niet nodig hebben: een app die Stulp zelf start praat
// over een socketpair, en een app naast Stulp over een unix-socket. Wie die
// megabyte niet wil dragen bouwt met -tags stulp_notls.
//
// De standaard is met TLS, want een standaard die geheimhouding weglaat is een
// standaard die het op het slechtste moment weglaat.
//
// Wat TLS hier toevoegt is geheimhouding en het merken van gerommel onderweg, niet
// wie er binnenkomt -- dat regelt het bewijs met de nonce, ook zonder TLS. Zie
// internal/appproto/token.go.
func dialTLS(config AttachConfig) (net.Conn, error) {
	settings := &tls.Config{MinVersion: tls.VersionTLS13}
	switch {
	case config.Insecure:
		settings.InsecureSkipVerify = true
	case config.CACert != "":
		pem, err := os.ReadFile(config.CACert)
		if err != nil {
			return nil, fmt.Errorf("attach: reading %s: %w", config.CACert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("attach: %s holds no certificate", config.CACert)
		}
		settings.RootCAs = pool
	}
	conn, err := tls.Dial("tcp", config.Target, settings)
	if err != nil {
		return nil, fmt.Errorf("attach: cannot reach stulp at %s: %w", config.Target, err)
	}
	return conn, nil
}
