// De Sigenergy-app.
//
// Eén Modbus/TCP-adres, en daarachter units: het systeem als geheel, een of
// meer omvormers, batterijen, de netmeter en een AC-laadpaal. Alles loopt over
// één verbinding; het unit-id staat in elk bericht, dus meer sockets leveren
// niets op en een Sigenergy-systeem staat er maar een handvol toe.
//
// Wat er wel en niet geport is staat in PORTED.md, met verwijzingen naar waar
// het in de oorspronkelijke Homey-app stond.
package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

const (
	defaultPort     = 502
	defaultInterval = 10
	defaultTimeout  = 5
	// scanTimeout geldt alleen tijdens het aftasten. Zie scan().
	scanTimeout = 1500 * time.Millisecond
	// systemUnit draagt de registers van het systeem als geheel.
	systemUnit = 247

	// minInterval houdt het pollen bij een omvormer vandaan die er niet tegen
	// kan. De bron staat vijf seconden als ondergrens toe en dat is hier
	// overgenomen.
	minInterval = 5

	// defaultUnits is waar het aftasten naar apparaten kijkt. Sigenergy zet zijn
	// omvormers en batterijen op lage unit-ids en het systeem op 247; iemand met
	// een andere indeling past het aan op de configuratiepagina.
	defaultUnits = "1-32,247"
)

// app is de plugin als geheel: één verbinding en de apparaten die eraan hangen.
type app struct {
	mu      sync.RWMutex
	stulp   *appsdk.Stulp
	client  *modbus.Client
	devices map[string]device // stulp-device-id -> wie erop wacht
	order   []string          // vaste volgorde, zodat een ronde voorspelbaar loopt
	halt    chan struct{}
	lastErr string
}

// device is wat de app van een gekoppeld apparaat nodig heeft.
type device interface {
	// refresh haalt één ronde op en vult de tegels. Wat er misgaat zet het
	// apparaat zelf op zijn eigen tegel; de ronde loopt door naar de volgende.
	refresh(*modbus.Client)
	// forgetConnection vergeet wat bij de vorige verbinding hoorde, zodat de
	// eenmalige registers na een herverbinding opnieuw gelezen worden.
	forgetConnection()
}

var instance = &app{devices: map[string]device{}}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit: instance.start,
		OnStop: instance.stop,
		Drivers: map[string]appsdk.Driver{
			"plant":       plantDriver{},
			"inverter":    inverterDriver{},
			"battery":     batteryDriver{},
			"energy":      energyDriver{},
			"evaccharger": chargerDriver{},
		},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.mu.Lock()
	a.stulp = stulp
	a.mu.Unlock()

	a.registerAPI(stulp)
	stulp.OnSettingsChanged(func(map[string]any) { a.connect() })
	a.connect()
	return nil
}

func (a *app) stop() {
	a.mu.Lock()
	halt, client := a.halt, a.client
	a.halt, a.client = nil, nil
	a.mu.Unlock()

	if halt != nil {
		close(halt)
	}
	if client != nil {
		client.Close()
	}
}

// connect zet de verbinding op met wat er in de instellingen staat.
//
// Dit blokkeert niet. De eerste aanroep gebeurt tijdens OnInit, en een omvormer
// die uit staat zou daarmee elk apparaat van deze app ophouden. Wat er misgaat
// komt terug op de configuratiepagina en op de tegel van elk apparaat.
func (a *app) connect() {
	a.stop()

	stulp := a.settings()
	host := stulp.SettingText("host")
	if host == "" {
		a.setError("Vul het adres van het Sigenergy-systeem in.")
		return
	}
	port := stulp.SettingNumber("port", defaultPort)
	timeout := stulp.SettingNumber("timeout", defaultTimeout)
	interval := stulp.SettingNumber("interval", defaultInterval)
	if interval < minInterval {
		interval = minInterval
	}

	client := modbus.New(host, port, time.Duration(timeout)*time.Second)
	halt := make(chan struct{})

	a.mu.Lock()
	a.client, a.halt, a.lastErr = client, halt, ""
	for _, target := range a.devices {
		target.forgetConnection()
	}
	a.mu.Unlock()

	// De ronde loopt in een eigen goroutine met zijn eigen client. Een
	// herverbinding sluit dit kanaal, en dan stopt deze ronde -- ook als hij nog
	// midden in een trage vraag zit, want de client eronder gaat dicht.
	go a.poll(client, halt, time.Duration(interval)*time.Second)
}

// poll draait de leesronden tot de verbinding vervangen wordt.
func (a *app) poll(client *modbus.Client, halt chan struct{}, interval time.Duration) {
	// Meteen een eerste ronde: wachten op de eerste tik zou elke tegel na een
	// herstart een interval lang leeg laten.
	a.sweep(client, halt)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-halt:
			return
		case <-ticker.C:
			a.sweep(client, halt)
		}
	}
}

// sweep loopt alle apparaten één keer langs.
//
// Eén voor één en niet parallel: ze delen één verbinding, en die neemt toch maar
// één vraag tegelijk aan. Zo blijft een trage unit ook een trage unit en niet een
// wachtrij waar de rest achter komt te staan.
func (a *app) sweep(client *modbus.Client, halt chan struct{}) {
	for _, target := range a.targets() {
		select {
		case <-halt:
			return
		default:
		}
		target.refresh(client)
	}
}

func (a *app) targets() []device {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]device, 0, len(a.order))
	for _, id := range a.order {
		if target, ok := a.devices[id]; ok {
			out = append(out, target)
		}
	}
	return out
}

// watch neemt een apparaat op in de ronde en haalt meteen zijn eerste waarden
// op, zodat een net gekoppeld apparaat geen interval lang leeg blijft.
func (a *app) watch(deviceID string, target device) {
	a.mu.Lock()
	if _, known := a.devices[deviceID]; !known {
		a.order = append(a.order, deviceID)
	}
	a.devices[deviceID] = target
	client := a.client
	a.mu.Unlock()

	if client != nil {
		go target.refresh(client)
	}
}

func (a *app) forget(deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.devices, deviceID)
	for i, id := range a.order {
		if id == deviceID {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

// api levert de verbinding, of een fout die zegt wat eraan mankeert.
func (a *app) api() (*modbus.Client, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.client == nil {
		return nil, fmt.Errorf("er is nog geen Sigenergy-adres ingesteld")
	}
	return a.client, nil
}

// settings levert de greep op de instellingen van de app. Via de lock, want
// connect draait ook vanuit de configuratiepagina.
func (a *app) settings() *appsdk.Stulp {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.stulp
}

func (a *app) setError(message string) {
	a.mu.Lock()
	a.lastErr = message
	a.mu.Unlock()
}

// chargerPower telt op wat de laadpalen nu trekken. Het systeem meldt dat niet
// als eigen register; de bron telt het net zo bij elkaar op uit de laders die
// gekoppeld zijn.
func (a *app) chargerPower() float64 {
	total := 0.0
	for _, target := range a.targets() {
		if charger, ok := target.(*chargerDevice); ok {
			total += charger.lastPower()
		}
	}
	return total
}

// ---------------------------------------------------------------------------
// Aftasten naar units
// ---------------------------------------------------------------------------

// scan zoekt de units die deze soort apparaat aanbieden.
//
// Modbus/TCP kent geen ontdekking: er is geen lijst om op te vragen en geen
// omroepbericht. Wat er wel is, is het antwoord op een vraag -- een unit die
// bestaat levert het register, en een unit die er niet is weigert. Dat is
// genoeg om de koppelpagina te vullen zonder iemand een unit-id te laten
// opzoeken.
func (a *app) scan(card sigen.Card) ([]uint8, error) {
	probe := card.Probe()
	units, err := parseUnits(a.settings().SettingText("units"))
	if err != nil {
		return nil, err
	}
	// Een eigen verbinding met een korte wachttijd.
	//
	// Getoetst tegen een echt systeem: een unit die bestaat antwoordt binnen
	// enkele milliseconden, en een unit die er niet is zwíjgt -- hij weigert
	// niet. Met de gewone wachttijd van vijf seconden kost het aftasten van
	// 1-32 dus bijna drie minuten per soort apparaat, en dan lijkt de
	// koppelpagina te hangen. Op een LAN is anderhalve seconde ruim.
	host := a.settings().SettingText("host")
	if host == "" {
		return nil, fmt.Errorf("er is nog geen adres ingesteld")
	}
	client := modbus.New(host, a.settings().SettingNumber("port", defaultPort), scanTimeout)
	defer client.Close()

	// Een weigering betekent: op deze unit staat dit apparaat niet. Dat is het
	// normale antwoord voor bijna elk afgetast unit-id en dus geen fout.
	//
	// Iets anders -- de lijn ligt eruit, of het systeem antwoordt helemaal niet --
	// laat de zoektocht wél doorlopen, want een systeem dat onbekende units
	// negeert in plaats van weigert zou anders bij de eerste misser afbreken. Het
	// wordt onthouden: als er niets gevonden wordt is dít de reden.
	var found []uint8
	var firstFailure error
	silent := 0
	// Sigenergy nummert zijn apparaten aaneengesloten vanaf 1. Zwijgen er drie
	// achter elkaar, dan zijn we voorbij het laatste en kost verder zoeken
	// alleen wachttijd -- op een echt systeem drie minuten voor niets. Het
	// systeem-id (247) staat los van die reeks en wordt daarom altijd gevraagd.
	const enoughSilence = 3
	for _, unit := range units {
		if silent >= enoughSilence && unit < systemUnit {
			continue
		}
		_, err := client.ReadHolding(unit, probe.Addr, probe.Count)
		switch {
		case err == nil:
			found = append(found, unit)
			silent = 0
		case isRefusal(err):
			// Een weigering is een antwoord: dit apparaat staat niet op deze
			// unit, maar er luistert wel iets. Dat is geen stilte.
			silent = 0
		default:
			if firstFailure == nil {
				firstFailure = err
			}
			silent++
		}
	}
	if len(found) == 0 && firstFailure != nil {
		return nil, firstFailure
	}
	return found, nil
}

func isRefusal(err error) bool {
	var exception modbus.Exception
	return errors.As(err, &exception)
}

// parseUnits leest "1-32,247" als de lijst unit-ids om af te tasten.
func parseUnits(text string) ([]uint8, error) {
	if strings.TrimSpace(text) == "" {
		text = defaultUnits
	}
	seen := map[uint8]bool{}
	var units []uint8
	add := func(value int) error {
		// Unit 0 is in Modbus het omroepadres en levert geen antwoord; 255 is de
		// bovengrens van het veld.
		if value < 1 || value > 255 {
			return fmt.Errorf("unit-id %d bestaat niet; het loopt van 1 tot en met 255", value)
		}
		if !seen[uint8(value)] {
			seen[uint8(value)] = true
			units = append(units, uint8(value))
		}
		return nil
	}
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, isRange := strings.Cut(part, "-")
		start, err := strconv.Atoi(strings.TrimSpace(from))
		if err != nil {
			return nil, fmt.Errorf("%q is geen unit-id of bereik", part)
		}
		if !isRange {
			if err := add(start); err != nil {
				return nil, err
			}
			continue
		}
		end, err := strconv.Atoi(strings.TrimSpace(to))
		if err != nil {
			return nil, fmt.Errorf("%q is geen bereik van unit-ids", part)
		}
		if end < start {
			return nil, fmt.Errorf("het bereik %q loopt achteruit", part)
		}
		for value := start; value <= end; value++ {
			if err := add(value); err != nil {
				return nil, err
			}
		}
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("er zijn geen unit-ids om af te tasten")
	}
	sort.Slice(units, func(a, b int) bool { return units[a] < units[b] })
	return units, nil
}

// ---------------------------------------------------------------------------
// Instellingen
// ---------------------------------------------------------------------------

// De koppelschermen en de opdrachten gaan via een type-assertie, dus zonder deze
// regels merk je een verkeerde handtekening pas als iemand op koppelen drukt.
var (
	_ appsdk.Pairer = plantDriver{}
	_ appsdk.Pairer = inverterDriver{}
	_ appsdk.Pairer = batteryDriver{}
	_ appsdk.Pairer = energyDriver{}
	_ appsdk.Pairer = chargerDriver{}

	_ appsdk.CapabilityHandler = (*chargerDevice)(nil)
	_ appsdk.SettingsChanger   = (*meter)(nil)
	_ appsdk.Deleter           = (*meter)(nil)
)
