//go:build tamago

package discovery

// transports_tamago.go is de node-gedaante van de mDNS-socketlaag: één
// leannet-stack, één interface, dus één transport. De socket bindt :5353 en
// de stack joint de groep — daarmee zijn we een volwaardige deelnemer:
// multicast-antwoorden (naar de groep, poort 5353) én unicast-antwoorden
// komen op dezelfde socket binnen, en leannet levert onze eigen queries
// lokaal terug zoals mDNS-responders verwachten.

import (
	"net"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/app/applib/appnet"
)

// De join is éénmalig en blijft: een matter-controller is een permanente
// mDNS-deelnemer. Sweeps openen en sluiten hun socket, het lidmaatschap
// stapelt dan niet mee.
var joinOnce sync.Once
var joinErr error

func openTransports() ([]transport, error) {
	group := net.IPv4(224, 0, 0, 251)
	joinOnce.Do(func() { joinErr = appnet.JoinMulticast(group) })
	if joinErr != nil {
		return nil, joinErr
	}
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{Port: mdnsPort})
	if err != nil {
		return nil, err
	}
	return []transport{{
		connection:  connection,
		destination: &net.UDPAddr{IP: group, Port: mdnsPort},
		zone:        "appnet",
	}}, nil
}
