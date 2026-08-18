package udp_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/appsdk/udp"
)

func TestMulticastRoundTripOverLoopback(t *testing.T) {
	// Een echte join plus een echt pakket. De zendkant kiest de interface
	// expliciet: zonder die keuze doet de kernel een route-lookup op de groep,
	// en op een machine met een VPN eindigt dat in een reject-route.
	//
	// Lukt de join of het verzenden niet, dan slaat de test over mét de fout van
	// het systeem erbij, en slaagt hij niet stilletjes.
	const group = "239.255.255.250"
	loopback := net.ParseIP("127.0.0.1")

	receiver, err := udp.Listen("udp4", "", 0, udp.Options{ReuseAddr: true})
	if err != nil {
		t.Fatalf("bind ontvanger: %v", err)
	}
	defer receiver.Close()
	port := receiver.LocalAddr().(*net.UDPAddr).Port

	if err := receiver.JoinGroup(net.ParseIP(group), loopback); err != nil {
		t.Skip("multicast is hier niet te draaien: " + err.Error())
	}

	sender, err := udp.Listen("udp4", "127.0.0.1", 0, udp.Options{})
	if err != nil {
		t.Fatalf("bind zender: %v", err)
	}
	defer sender.Close()
	if err := sender.SetMulticastInterface(loopback); err != nil {
		t.Skip("interface kiezen lukt hier niet: " + err.Error())
	}
	if err := sender.SetMulticastTTL(1); err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if err := sender.SetMulticastLoopback(true); err != nil {
		t.Fatalf("loopback: %v", err)
	}

	target := &net.UDPAddr{IP: net.ParseIP(group), Port: port}
	if _, err := sender.WriteToUDP([]byte("M-SEARCH"), target); err != nil {
		t.Skip("verzenden lukt hier niet: " + err.Error())
	}

	receiver.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 64)
	n, _, err := receiver.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("niets ontvangen: %v", err)
	}
	if got := string(buffer[:n]); got != "M-SEARCH" {
		t.Errorf("kreeg %q", got)
	}
}

func TestJoinRefusesWhatIsNotAGroup(t *testing.T) {
	// Een adres dat geen multicastgroep is hoort te falen. Stil accepteren zou
	// een plugin laten denken dat hij luistert terwijl er niets gebeurd is.
	socket, err := udp.Listen("udp4", "127.0.0.1", 0, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	if err := socket.JoinGroup(net.ParseIP("192.168.1.1"), nil); err == nil {
		t.Error("een gewoon adres werd als groep geaccepteerd")
	}
}

func TestBroadcastNeedsTheOptionSet(t *testing.T) {
	// Zonder SO_BROADCAST weigert de kernel een pakket naar 255.255.255.255.
	// Deze test bewijst dat de optie iets doet: zonder faalt het versturen, met
	// wel niet.
	socket, err := udp.Listen("udp4", "", 0, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	target := &net.UDPAddr{IP: net.IPv4bcast, Port: 9}
	_, before := socket.WriteToUDP([]byte("x"), target)
	if before == nil {
		t.Skip("deze kernel staat broadcast zonder SO_BROADCAST toe")
	}
	if err := socket.SetBroadcast(true); err != nil {
		t.Fatalf("setBroadcast: %v", err)
	}
	_, after := socket.WriteToUDP([]byte("x"), target)
	if after == nil {
		return // de optie deed wat hij moest doen
	}
	// Op een machine met een VPN kan het routeren zelf geblokkeerd zijn -- dan
	// faalt het verzenden om een andere reden dan de optie, en bewijst deze test
	// niets. Dat hoort een skip te zijn met de systeemfout erbij, geen groen.
	if isRoutingRefusal(after) {
		t.Skip("broadcast wordt hier niet gerouteerd: " + after.Error())
	}
	t.Errorf("na SetBroadcast faalde het verzenden alsnog: %v", after)
}

func isRoutingRefusal(err error) bool {
	message := err.Error()
	return strings.Contains(message, "no route to host") ||
		strings.Contains(message, "network is unreachable")
}
