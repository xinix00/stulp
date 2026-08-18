package wiim

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/appsdk/udp"
)

// Een nagebouwde SSDP-antwoorder op een eigen socket. Hij neemt de M-SEARCH
// aan, schrijft op wat erin stond en stuurt de antwoorden terug die de test hem
// meegeeft -- unicast, want zo hoort een antwoord op een M-SEARCH terug te
// komen.
type antwoorder struct {
	socket *udp.Socket

	mu       sync.Mutex
	requests []string
}

func newAntwoorder(t *testing.T, replies func() []string) *antwoorder {
	t.Helper()
	socket, err := udp.Listen("udp4", "127.0.0.1", 0, udp.Options{})
	if err != nil {
		t.Fatalf("de antwoorder kan geen socket krijgen: %v", err)
	}
	a := &antwoorder{socket: socket}
	t.Cleanup(func() { socket.Close() })

	go func() {
		buffer := make([]byte, 4096)
		for {
			read, from, err := socket.ReadFromUDP(buffer)
			if err != nil {
				return // de test is klaar en de socket is dicht
			}
			message := string(buffer[:read])
			if !strings.HasPrefix(message, "M-SEARCH") {
				continue
			}
			a.mu.Lock()
			a.requests = append(a.requests, message)
			a.mu.Unlock()
			for _, reply := range replies() {
				socket.WriteToUDP([]byte(reply), from)
			}
		}
	}()
	return a
}

func (a *antwoorder) address() *net.UDPAddr { return a.socket.LocalAddr().(*net.UDPAddr) }

func (a *antwoorder) asked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func ok(location, usn, st, server string) string {
	return "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"EXT:\r\n" +
		"LOCATION: " + location + "\r\n" +
		"SERVER: " + server + "\r\n" +
		"ST: " + st + "\r\n" +
		"USN: " + usn + "::" + st + "\r\n\r\n"
}

// Een tv die ook MediaRenderer is: hetzelfde apparaatsoort, ander merk. Dit is
// waarom herkennen niet aan het SSDP-antwoord kan.
const tvXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
<deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
<friendlyName>Woonkamer TV</friendlyName>
<manufacturer>Samsung Electronics</manufacturer>
<modelName>UE55</modelName>
<UDN>uuid:11111111-2222-3333-4444-555555555555</UDN>
<serviceList><service>
<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
<serviceId>urn:upnp-org:serviceId:AVTransport</serviceId>
<controlURL>/upnp/control/AVTransport1</controlURL>
</service></serviceList>
</device></root>`

// En de router, die op een M-SEARCH net zo goed antwoordt.
const routerXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
<friendlyName>Router</friendlyName>
<manufacturer>AVM</manufacturer>
<modelName>FRITZ!Box</modelName>
<UDN>uuid:99999999-8888-7777-6666-555555555555</UDN>
<serviceList><service>
<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
<serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
<controlURL>/igd/control</controlURL>
</service></serviceList>
</device></root>`

func description(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/description.xml" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// Dit is de kern van het ontdekken: op een M-SEARCH antwoordt in een gemiddeld
// huis van alles, en alleen de WiiM hoort in de lijst te komen. Het SSDP-
// antwoord zelf zegt dat niet -- daar staat een adres en een apparaatsoort in,
// en die deelt een WiiM met de tv. Het merk staat in de beschrijving.
func TestAWiiMIsPickedOutFromBetweenTheOtherDevices(t *testing.T) {
	wiimServer := description(t, descriptionXML)
	tv := description(t, tvXML)
	router := description(t, routerXML)
	// En een apparaat dat wel antwoordt maar zijn beschrijving niet meer
	// afgeeft: dat mag de ronde niet laten vallen.
	dood := httptest.NewServer(http.NotFoundHandler())
	dood.Close()

	responder := newAntwoorder(t, func() []string {
		return []string{
			ok(router.URL+"/description.xml", "uuid:99999999-8888-7777-6666-555555555555",
				"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "AVM UPnP/1.0"),
			ok(tv.URL+"/description.xml", "uuid:11111111-2222-3333-4444-555555555555",
				MediaRenderer, "Linux/4.1 UPnP/1.0"),
			ok(wiimServer.URL+"/description.xml", "uuid:FF98F09C-74E9-46A5-07FF-155EFF98F09C",
				MediaRenderer, "Linux/4.1 UPnP/1.0 WiiMu/1.0"),
			ok(dood.URL+"/description.xml", "uuid:00000000-0000-0000-0000-000000000000",
				MediaRenderer, "Onbekend"),
			// Twee keer dezelfde speler: een apparaat antwoordt geregeld per
			// dienst, en dat mag geen twee tegels opleveren.
			ok(wiimServer.URL+"/description.xml", "uuid:FF98F09C-74E9-46A5-07FF-155EFF98F09C",
				"urn:schemas-upnp-org:service:AVTransport:1", "Linux/4.1 UPnP/1.0 WiiMu/1.0"),
			// En rommel: een ongevraagde NOTIFY en iets wat geen HTTP is.
			"NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nNTS: ssdp:alive\r\n\r\n",
			"\x00\x01 dit is geen SSDP",
		}
	})

	players, err := Discover(context.Background(), SearchOptions{
		To:   responder.address(),
		Wait: 700 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(players) != 1 {
		t.Fatalf("gevonden: %+v", players)
	}
	player := players[0]
	if player.UUID != "FF98F09C-74E9-46A5-07FF-155EFF98F09C" {
		t.Errorf("uuid = %q", player.UUID)
	}
	if player.Name != "Büro Audio" || player.Model != "WiiM Pro Receiver" {
		t.Errorf("naam en model = %q / %q", player.Name, player.Model)
	}
	// Adres en poort komen uit de LOCATION en niet uit een aanname hier.
	wanted, _ := url.Parse(wiimServer.URL)
	if player.Address != wanted.Hostname() || player.Port != atoi(wanted.Port()) {
		t.Errorf("adres = %s:%d, wil %s", player.Address, player.Port, wanted.Host)
	}

	// En wat er gevraagd is: een M-SEARCH die zich aan SSDP houdt. Zonder MAN
	// en ST antwoordt een echt apparaat niet.
	asked := responder.asked()
	if len(asked) == 0 {
		t.Fatal("de antwoorder kreeg geen zoekvraag")
	}
	for _, header := range []string{"M-SEARCH * HTTP/1.1", `MAN: "ssdp:discover"`, "ST: " + MediaRenderer, "MX:"} {
		if !strings.Contains(asked[0], header) {
			t.Errorf("de zoekvraag mist %q:\n%s", header, asked[0])
		}
	}
}

func atoi(text string) int {
	value := 0
	for _, digit := range text {
		value = value*10 + int(digit-'0')
	}
	return value
}

// Niemand die antwoordt is geen fout: dat is een huis waar multicast niet
// rondkomt, en daarvoor is er het adresveld op de koppelpagina.
func TestNobodyAnsweringIsNotAFailure(t *testing.T) {
	stil := newAntwoorder(t, func() []string { return nil })

	players, err := Discover(context.Background(), SearchOptions{
		To:   stil.address(),
		Wait: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("een lege ronde gaf een fout: %v", err)
	}
	if len(players) != 0 {
		t.Fatalf("uit het niets kwamen %d spelers", len(players))
	}
}

// De echte weg: een M-SEARCH over multicast, met de uitgaande interface
// expliciet gekozen. Zonder die keuze doet de kernel een route-lookup op de
// groep, en op een machine met een VPN eindigt dat in een reject-route.
//
// Lukt multicast hier niet, dan slaat de test over mét de fout van het systeem
// erbij, en slaagt hij niet stilletjes.
func TestTheSearchReallyGoesOutOverMulticast(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	group := net.ParseIP(MulticastGroup)

	socket, err := udp.Listen("udp4", "", 0, udp.Options{ReuseAddr: true})
	if err != nil {
		t.Fatalf("bind antwoorder: %v", err)
	}
	defer socket.Close()
	if err := socket.JoinGroup(group, loopback); err != nil {
		t.Skip("multicast is hier niet te draaien: " + err.Error())
	}
	port := socket.LocalAddr().(*net.UDPAddr).Port

	wiimServer := description(t, descriptionXML)
	// Gebufferd: de melding komt binnen terwijl de test nog in Discover zit, en
	// een ongebufferd kanaal zou hem laten vallen -- dan slaat de test over
	// terwijl multicast het gewoon deed.
	heard := make(chan struct{}, 1)
	go func() {
		buffer := make([]byte, 4096)
		for {
			read, from, err := socket.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if !strings.HasPrefix(string(buffer[:read]), "M-SEARCH") {
				continue
			}
			select {
			case heard <- struct{}{}:
			default:
			}
			socket.WriteToUDP([]byte(ok(wiimServer.URL+"/description.xml",
				"uuid:FF98F09C-74E9-46A5-07FF-155EFF98F09C", MediaRenderer, "WiiMu/1.0")), from)
		}
	}()

	players, err := Discover(context.Background(), SearchOptions{
		// Naar de echte groep, maar op de poort van deze test: poort 1900 is
		// van het huis en niet van een test.
		To:        &net.UDPAddr{IP: group, Port: port},
		Interface: loopback,
		Wait:      time.Second,
	})
	if err != nil {
		t.Skip("multicast verzenden lukt hier niet: " + err.Error())
	}
	select {
	case <-heard:
	default:
		t.Skip("de zoekvraag kwam niet op de loopback aan; multicast is hier niet te toetsen")
	}
	if len(players) != 1 || players[0].Name != "Büro Audio" {
		t.Fatalf("over multicast gevonden: %+v", players)
	}
}

// Met de hand toevoegen vraagt het apparaat zelf wie het is. Een naam of een
// uuid overtypen hoeft niemand -- en staat er iets anders op dat adres, dan
// zegt de melding wát er staat.
func TestAddingByAddressAsksTheDevice(t *testing.T) {
	server := description(t, descriptionXML)
	target, _ := url.Parse(server.URL)

	client := New("192.168.50.56")
	client.HTTP = &http.Client{Transport: omleiding{
		target: target, inner: http.DefaultTransport, mu: &sync.Mutex{}, log: &[]gezien{},
	}}
	player, err := identify(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if player.UUID != "FF98F09C-74E9-46A5-07FF-155EFF98F09C" || player.Name != "Büro Audio" {
		t.Fatalf("gevonden: %+v", player)
	}

	// En nu de tv op hetzelfde adres.
	tv := description(t, tvXML)
	tvTarget, _ := url.Parse(tv.URL)
	client.HTTP = &http.Client{Transport: omleiding{
		target: tvTarget, inner: http.DefaultTransport, mu: &sync.Mutex{}, log: &[]gezien{},
	}}
	_, err = identify(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "Samsung") {
		t.Fatalf("een tv op dat adres gaf %v", err)
	}
}

// Een beschrijving zonder AVTransport is geen speler, hoe hij zich verder ook
// noemt: er valt niets mee te bedienen.
func TestRecognitionNeedsBothTheBrandAndTheService(t *testing.T) {
	withService, err := ParseDescription("http://192.168.50.56:49152/description.xml", []byte(descriptionXML))
	if err != nil {
		t.Fatal(err)
	}
	if !withService.IsPlayer() {
		t.Error("een echte WiiM werd niet herkend")
	}
	if got := withService.UUID(); got != "FF98F09C-74E9-46A5-07FF-155EFF98F09C" {
		t.Errorf("uuid = %q", got)
	}
	// Het controladres komt uit de beschrijving en wordt opgelost tegen de
	// plek waar die stond; zelf een pad verzinnen werkt tot de eerste speler
	// die het anders doet.
	control, ok := withService.Control(AVTransport)
	if !ok || control != "http://192.168.50.56:49152/upnp/control/rendertransport1" {
		t.Errorf("controladres = %q", control)
	}

	tv, err := ParseDescription("http://192.168.1.9:9197/description.xml", []byte(tvXML))
	if err != nil {
		t.Fatal(err)
	}
	if tv.IsPlayer() {
		t.Error("een Samsung-tv werd als WiiM herkend")
	}
	router, err := ParseDescription("http://192.168.1.1:49000/description.xml", []byte(routerXML))
	if err != nil {
		t.Fatal(err)
	}
	if router.IsPlayer() {
		t.Error("een router werd als WiiM herkend")
	}
}
