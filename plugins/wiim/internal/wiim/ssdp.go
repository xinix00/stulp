package wiim

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk/udp"
)

// Ontdekking met SSDP.
//
// De bron zoekt spelers met mDNS (`.homeycompose/discovery/player.json`,
// `_linkplay._tcp`) en laat Homey dat doen. Dat werk doet Stulp niet voor je,
// en de speler biedt naast mDNS ook gewoon UPnP aan — de bron praat er de rest
// van de tijd mee. Dus gaat het zoeken hier over SSDP: één M-SEARCH naar
// 239.255.255.250:1900, en wie antwoordt zegt waar zijn beschrijving staat.
//
// Wat er in huis fout kan gaan, gaat mis in de multicast en niet in het
// zoeken zelf: een VPN dat de route naar de groep wegkaapt, een gastnetwerk of
// een switch die IGMP-snooping doet. Vandaar twee dingen: de uitgaande
// interface wordt expliciet gekozen (anders doet de kernel een route-lookup op
// de groep, en dat eindigt op een machine met een VPN in een reject-route), en
// de zoekvraag vertrekt over élke interface die aan staat. Wie dan nog niets
// vindt voegt zijn speler met de hand toe op adres; dat pad is er met opzet.

const (
	// MulticastGroup en MulticastPort zijn waar SSDP woont.
	MulticastGroup = "239.255.255.250"
	MulticastPort  = 1900
	// MediaRenderer is de apparaatsoort waar een speler onder valt. Zo staat
	// hij in de beschrijving van een echte WiiM
	// (`_docs/upnpSpecs/_DeviceDescription.json`).
	MediaRenderer = "urn:schemas-upnp-org:device:MediaRenderer:1"
)

// maxAnswers begrenst wat één zoekronde oplevert. Een huis met veel UPnP komt
// hier niet aan; een apparaat dat blijft praten hoort ons niet vol te lopen.
const maxAnswers = 256

// SearchOptions zijn de keuzes van één zoekronde. De nulwaarde doet het goede
// voor een gewoon huis.
type SearchOptions struct {
	// Target is de ST van de zoekvraag. Leeg betekent MediaRenderer.
	Target string
	// Wait is hoe lang er naar antwoorden geluisterd wordt. Nul betekent
	// defaultWait.
	Wait time.Duration
	// To is waar de zoekvraag heen gaat. Nul betekent de multicastgroep. Een
	// test zet hier een eigen socket neer.
	To *net.UDPAddr
	// Interface is het adres van de interface waarover verzonden wordt. Nul
	// betekent: over allemaal.
	Interface net.IP
	// HTTP haalt de beschrijvingen op bij Discover. Nul betekent de standaard.
	HTTP *http.Client
}

const defaultWait = 3 * time.Second

// Answer is één antwoord op de zoekvraag.
type Answer struct {
	// Location is waar de beschrijving van dat apparaat staat.
	Location string
	USN      string
	ST       string
	Server   string
	From     string
}

// Search stuurt een M-SEARCH en verzamelt wat er terugkomt.
//
// Er wordt geen multicastgroep gejoind, en dat is een keuze: een antwoord op
// een M-SEARCH komt als unicast terug naar de poort waarvandaan gevraagd is.
// Joinen zou alleen de ongevraagde NOTIFY-berichten opleveren, en daarvoor moet
// je poort 1900 delen met wat er verder op de machine luistert. Dat is een
// bind die kan mislukken om het ontdekken heen te helpen dat het ook zonder kan.
func Search(ctx context.Context, options SearchOptions) ([]Answer, error) {
	target := options.To
	if target == nil {
		target = &net.UDPAddr{IP: net.ParseIP(MulticastGroup), Port: MulticastPort}
	}
	wanted := options.Target
	if wanted == "" {
		wanted = MediaRenderer
	}
	wait := options.Wait
	if wait <= 0 {
		wait = defaultWait
	}

	socket, err := udp.Listen("udp4", "", 0, udp.Options{})
	if err != nil {
		return nil, fmt.Errorf("ssdp: geen socket om mee te zoeken: %w", err)
	}
	defer socket.Close()

	if err := send(socket, target, request(target, wanted), options.Interface); err != nil {
		return nil, err
	}

	// Een geannuleerde context hoort niet tot de deadline te wachten. De
	// leesdeadline naar nu zetten breekt de lopende ReadFromUDP af.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			socket.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	deadline := time.Now().Add(wait)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if err := socket.SetReadDeadline(deadline); err != nil {
		return nil, fmt.Errorf("ssdp: %w", err)
	}

	answers := make([]Answer, 0, 8)
	buffer := make([]byte, 8192)
	for len(answers) < maxAnswers {
		read, from, err := socket.ReadFromUDP(buffer)
		if err != nil {
			break // de deadline, of de context die eronder vandaan liep
		}
		answer, ok := parseAnswer(buffer[:read])
		if !ok {
			continue // iets anders op de lijn; geen reden om te stoppen
		}
		if from != nil {
			answer.From = from.IP.String()
		}
		answers = append(answers, answer)
	}
	// De context die eronder vandaan liep is iets anders dan een ronde die
	// afliep: dan wilde de aanvrager het niet meer, en dan is een halve lijst
	// erger dan een melding.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ssdp: het zoeken werd afgebroken: %w", err)
	}
	return answers, nil
}

// request bouwt de M-SEARCH. MX is de tijd waarbinnen een apparaat mag
// antwoorden; die staat lager dan Wait, anders sluiten we de socket terwijl er
// nog iets onderweg is.
func request(target *net.UDPAddr, wanted string) []byte {
	return []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + target.String() + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: " + wanted + "\r\n" +
		"\r\n")
}

// send verstuurt de zoekvraag, en bij multicast over elke interface apart.
func send(socket *udp.Socket, target *net.UDPAddr, message []byte, only net.IP) error {
	if !target.IP.IsMulticast() {
		// Een vast adres: één keer sturen en klaar. Dit is het pad van "voeg
		// hem met de hand toe" en van de test.
		if _, err := socket.WriteToUDP(message, target); err != nil {
			return fmt.Errorf("ssdp: de zoekvraag vertrok niet naar %s: %w", target, err)
		}
		return nil
	}

	// Eén hop: ontdekking hoort in huis te blijven, en een router die dit
	// doorlaat helpt niemand.
	if err := socket.SetMulticastTTL(1); err != nil {
		return fmt.Errorf("ssdp: ttl: %w", err)
	}
	// Zodat een speler op deze machine (en de test) zichzelf ook ziet.
	if err := socket.SetMulticastLoopback(true); err != nil {
		return fmt.Errorf("ssdp: loopback: %w", err)
	}

	addresses, err := outgoing(only)
	if err != nil {
		return err
	}
	sent := 0
	var last error
	for _, address := range addresses {
		if err := socket.SetMulticastInterface(address); err != nil {
			last = err
			continue
		}
		// Twee keer, met een tel ertussen. UDP mag pakketten laten vallen en
		// een M-SEARCH die verdwijnt levert een leeg koppelscherm op; twee
		// pakketten kosten niets en dubbele antwoorden vallen verderop weg.
		for round := 0; round < 2; round++ {
			if _, err := socket.WriteToUDP(message, target); err != nil {
				last = err
				continue
			}
			sent++
			if round == 0 {
				time.Sleep(150 * time.Millisecond)
			}
		}
	}
	if sent == 0 {
		if last != nil {
			return fmt.Errorf("ssdp: de zoekvraag vertrok op geen enkele interface: %w", last)
		}
		return fmt.Errorf("ssdp: er is geen netwerkverbinding die multicast aankan")
	}
	return nil
}

// outgoing levert de adressen waarover gezocht wordt.
//
// Loopback valt af: daar hangt geen speler aan, en hem meenemen zou elke ronde
// een zoekvraag naar onszelf sturen. Een test die juist wél op loopback wil
// zoeken geeft het adres expliciet mee.
func outgoing(only net.IP) ([]net.IP, error) {
	if only != nil {
		return []net.IP{only}, nil
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("ssdp: de netwerkverbindingen zijn niet op te vragen: %w", err)
	}
	var addresses []net.IP
	for index := range interfaces {
		candidate := &interfaces[index]
		if candidate.Flags&net.FlagUp == 0 || candidate.Flags&net.FlagMulticast == 0 {
			continue
		}
		if candidate.Flags&net.FlagLoopback != 0 {
			continue
		}
		found, err := candidate.Addrs()
		if err != nil {
			continue
		}
		for _, address := range found {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.To4() == nil {
				continue
			}
			addresses = append(addresses, network.IP)
			break
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("ssdp: er is geen netwerkverbinding die multicast aankan")
	}
	return addresses, nil
}

// parseAnswer leest het antwoord op een M-SEARCH: een HTTP-antwoord zonder
// body, in één datagram.
func parseAnswer(datagram []byte) (Answer, bool) {
	reader := bufio.NewReader(bytes.NewReader(datagram))
	status, err := reader.ReadString('\n')
	if err != nil {
		return Answer{}, false
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(status)), "HTTP/1.1 200") {
		// Een NOTIFY of iets anders dat toevallig langskomt. Dit is geen fout:
		// op deze poort mag van alles binnenwaaien.
		return Answer{}, false
	}
	// De fout van ReadMIMEHeader wordt bewust genegeerd zolang er een LOCATION
	// uit komt: een datagram dat niet op een lege regel eindigt is een
	// slordigheid van de firmware, geen reden om het apparaat over te slaan.
	header, _ := textproto.NewReader(reader).ReadMIMEHeader()
	location := strings.TrimSpace(header.Get("Location"))
	if location == "" {
		return Answer{}, false
	}
	return Answer{
		Location: location,
		USN:      strings.TrimSpace(header.Get("Usn")),
		ST:       strings.TrimSpace(header.Get("St")),
		Server:   strings.TrimSpace(header.Get("Server")),
	}, true
}

// ---------------------------------------------------------------------------
// Van antwoord naar speler
// ---------------------------------------------------------------------------

// Player is een gevonden speler: genoeg om hem te koppelen.
type Player struct {
	// UUID is de identiteit en verandert niet als het adres verandert.
	UUID string
	Name string
	// Model is wat de speler zichzelf noemt, bijvoorbeeld "WiiM Pro Receiver".
	Model   string
	Address string
	// Port is waar de beschrijving stond. Meestal DescriptionPort, maar wat de
	// speler zei wint.
	Port     int
	Location string
}

// Discover zoekt spelers en vraagt elk antwoord wie het is.
//
// Het herkennen gebeurt dus niet aan het SSDP-antwoord: daar staat een adres en
// een apparaatsoort in, en een MediaRenderer is in een gemiddeld huis ook een
// tv, een AV-ontvanger of een NAS. Het merk staat in de beschrijving, en die
// wordt daarom opgehaald — zie Description.IsPlayer.
func Discover(ctx context.Context, options SearchOptions) ([]Player, error) {
	answers, err := Search(ctx, options)
	if err != nil {
		return nil, err
	}

	client := options.HTTP
	if client == nil {
		client = defaultHTTP()
	}

	// Eén beschrijving per adres. Een apparaat antwoordt geregeld meerdere
	// keren (per dienst, en op elk van onze twee pakketten).
	locations := make([]string, 0, len(answers))
	seen := map[string]bool{}
	for _, answer := range answers {
		if seen[answer.Location] {
			continue
		}
		seen[answer.Location] = true
		locations = append(locations, answer.Location)
	}

	// Vier tegelijk: een apparaat dat niet antwoordt houdt de rest dan niet op,
	// en een huis vol UPnP krijgt geen twintig gelijktijdige verbindingen.
	var (
		mutex   sync.Mutex
		players []Player
		work    = make(chan string)
		group   sync.WaitGroup
	)
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for location := range work {
				description, err := describe(ctx, client, location)
				if err != nil || !description.IsPlayer() {
					// Een apparaat dat geen speler is, is geen fout: dat is de
					// helft van wat er op een M-SEARCH antwoordt.
					continue
				}
				player, err := playerFrom(description)
				if err != nil {
					continue
				}
				mutex.Lock()
				players = append(players, player)
				mutex.Unlock()
			}
		}()
	}
	for _, location := range locations {
		work <- location
	}
	close(work)
	group.Wait()

	// Op uuid ontdubbelen: een speler met twee netwerkkaarten (kabel en wifi)
	// antwoordt op twee adressen met dezelfde identiteit.
	unique := make([]Player, 0, len(players))
	known := map[string]bool{}
	sort.Slice(players, func(i, j int) bool { return players[i].Address < players[j].Address })
	for _, player := range players {
		if known[player.UUID] {
			continue
		}
		known[player.UUID] = true
		unique = append(unique, player)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].Name < unique[j].Name })
	return unique, nil
}

func playerFrom(description Description) (Player, error) {
	address, port, err := hostOf(description.Location)
	if err != nil {
		return Player{}, err
	}
	name := description.FriendlyName
	if name == "" {
		name = description.ModelName
	}
	if name == "" {
		name = address
	}
	return Player{
		UUID:     description.UUID(),
		Name:     name,
		Model:    description.ModelName,
		Address:  address,
		Port:     port,
		Location: description.Location,
	}, nil
}

func hostOf(location string) (string, int, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return "", 0, err
	}
	host := parsed.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("%q draagt geen adres", location)
	}
	port := DescriptionPort
	if text := parsed.Port(); text != "" {
		number, err := strconv.Atoi(text)
		if err != nil {
			return "", 0, fmt.Errorf("%q draagt geen poort: %w", location, err)
		}
		port = number
	}
	return host, port, nil
}

// Identify vraagt één adres wie het is. Dit is het pad voor een speler die niet
// gevonden wordt — een huis met een VPN of een apart gastnetwerk krijgt geen
// multicast rond, en dan is het adres wat iemand nog heeft.
func Identify(ctx context.Context, address string) (Player, error) {
	if err := CheckAddress(address); err != nil {
		return Player{}, err
	}
	return identify(ctx, New(address))
}

func identify(ctx context.Context, client *Client) (Player, error) {
	address := client.Address
	description, err := client.Describe(ctx)
	if err != nil {
		return Player{}, fmt.Errorf("op %s antwoordt geen speler: %w", address, err)
	}
	if !description.IsPlayer() {
		what := strings.TrimSpace(description.Manufacturer + " " + description.ModelName)
		if what == "" {
			what = description.DeviceType
		}
		return Player{}, fmt.Errorf("op %s staat wel een UPnP-apparaat maar geen WiiM: %s", address, what)
	}
	return playerFrom(description)
}
