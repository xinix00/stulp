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
	// scanTimeout geldt alleen tijdens het aftasten. Zie scan(). Een bestaande
	// unit antwoordt op het lokale netwerk binnen milliseconden; een halve
	// seconde per stille unit houdt een volledige, ook sparse scan hanteerbaar.
	scanTimeout = 500 * time.Millisecond
	// Sigenergy schrijft voor dat een Modbus-slave maximaal 1000 ms over een
	// antwoord mag doen. De waarschijnlijke EVAC-units krijgen die hele tijd plus
	// een kleine netwerkmarge. Alleen de resterende 1-246-fallback blijft kort;
	// anders kan automatisch toevoegen ruim vier minuten duren.
	chargerScanTimeout     = 1100 * time.Millisecond
	chargerFallbackTimeout = 100 * time.Millisecond
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

	// De Gateway loopt via de optionele mySigen-koppeling, los van de lokale
	// Modbus-verbinding. Een wijziging aan het lokale IP-adres mag deze
	// apparaten daarom niet stoppen of opnieuw aanmelden.
	cloud           cloudClient
	cloudIdentity   storedCloud
	cloudGeneration uint64
	gateways        map[string]*gatewayDevice
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

var instance = &app{devices: map[string]device{}, gateways: map[string]*gatewayDevice{}}

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
			"gateway":     gatewayDriver{},
		},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.mu.Lock()
	a.stulp = stulp
	a.mu.Unlock()

	a.registerAPI(stulp)
	a.registerCloudAPI(stulp)
	stulp.OnSettingsChanged(func(map[string]any) { a.connect() })
	if err := a.restoreCloud(stulp.State()); err != nil {
		stulp.Error(err.Error())
	}
	a.connect()
	return nil
}

func (a *app) stop() {
	a.stopModbus()
	a.stopCloudRuntime()
}

// stopModbus sluit alleen de lokale meetverbinding. connect gebruikt hem ook;
// de cloud-Gateway is een onafhankelijke verbinding en moet daarbij blijven
// draaien.
func (a *app) stopModbus() {
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
	a.stopModbus()

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
	units, err := parseUnits(a.settings().SettingText("units"))
	if err != nil {
		return nil, err
	}
	return a.scanAt(card, units, scanTimeout)
}

// scanCharger gebruikt een expliciet EVAC-unit-id als dat is ingevuld. Zonder
// zo'n aanwijzing doorzoekt alleen deze driver het volledige officiële bereik:
// een AC-lader kan in mySigen ieder uniek adres van 1 tot en met 246 krijgen en
// hoeft dus niet binnen de historische algemene scan 1-32 te vallen.
func (a *app) scanCharger() ([]uint8, error) {
	plan, err := planChargerScan(a.settings().SettingText("units"), a.settings().SettingText("chargerUnit"))
	if err != nil {
		return nil, err
	}
	found, err := a.scanFirstAt(sigen.EvACCharger, plan.reliable, chargerScanTimeout)
	if err != nil {
		return nil, err
	}
	if len(found) > 0 {
		return found, nil
	}
	if plan.exact {
		return nil, fmt.Errorf("op unit %d zijn geen Sigenergy AC-laadpaalregisters gevonden; controleer het EVAC-unit-id in mySigen", plan.reliable[0])
	}

	found, err = a.scanFirstAt(sigen.EvACCharger, plan.fallback, chargerFallbackTimeout)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("geen Sigenergy AC-laadpaal gevonden; vul het Modbus unit-id van de EVAC in de app-instellingen in voor een gerichte controle van een volle seconde")
	}
	return found, nil
}

func (a *app) scanAt(card sigen.Card, units []uint8, timeout time.Duration) ([]uint8, error) {
	return a.scanAtUsing(card, units, timeout, scanUnits)
}

// scanFirstAt stopt zodra één unit het herkenningsregister aanbiedt. Dat is bij
// toevoegen precies wat nodig is en voorkomt dat een gevonden EVAC alsnog moet
// wachten op honderden stille adressen voordat de tegel verschijnt.
func (a *app) scanFirstAt(card sigen.Card, units []uint8, timeout time.Duration) ([]uint8, error) {
	return a.scanAtUsing(card, units, timeout, scanFirstUnit)
}

func (a *app) scanAtUsing(card sigen.Card, units []uint8, timeout time.Duration, scan func(sigen.Reader, sigen.Card, []uint8) ([]uint8, error)) ([]uint8, error) {
	if len(units) == 0 {
		return nil, nil
	}
	// Een eigen verbinding met een korte wachttijd.
	//
	// Getoetst tegen een echt systeem: een unit die bestaat antwoordt binnen
	// enkele milliseconden, en een unit die er niet is zwíjgt -- hij weigert
	// niet. Met de gewone wachttijd van vijf seconden kost het aftasten van
	// 1-32 dus bijna drie minuten per soort apparaat, en dan lijkt de
	// koppelpagina te hangen.
	host := a.settings().SettingText("host")
	if host == "" {
		return nil, fmt.Errorf("er is nog geen adres ingesteld")
	}
	port := a.settings().SettingNumber("port", defaultPort)
	// Een stille of onbereikbare TCP-server hoeft niet voor ieder mogelijk
	// slave-adres opnieuw zijn timeout te krijgen. Unit 247 is het verplichte
	// plantadres; één vraag daar maakt onderscheid tussen een kapotte verbinding
	// en een bereikbare omvormer waar alleen deze apparaatsoort ontbreekt.
	checkTimeout := scanTimeout
	if timeout > checkTimeout {
		checkTimeout = timeout
	}
	check := modbus.New(host, port, checkTimeout)
	_, checkErr := sigen.Plant.Probe().Read(check, sigen.SystemUnit)
	_ = check.Close()
	if checkErr != nil {
		return nil, fmt.Errorf("Sigenergy op %s:%d antwoordt niet op systeem-unit 247: %w; controleer het adres en of Modbus TCP in mySigen aanstaat", host, port, checkErr)
	}

	client := modbus.New(host, port, timeout)
	defer client.Close()
	found, err := scan(client, card, units)
	if err == nil {
		return found, nil
	}

	// Als geen geprobeerde slave antwoordde, controleert scanUnits terecht de
	// verbinding. De plantcheck hierboven bewees dat die bij aanvang goed was;
	// toets hem na de ronde nogmaals. Antwoordt hij nog, dan is "geen apparaat"
	// de juiste uitkomst. Is hij intussen weggevallen, behoud dan de echte
	// transportfout uit de scan.
	recheck := modbus.New(host, port, checkTimeout)
	_, stillReachable := sigen.Plant.Probe().Read(recheck, sigen.SystemUnit)
	_ = recheck.Close()
	if stillReachable == nil {
		return found, nil
	}
	return nil, err
}

type chargerScanPlan struct {
	reliable []uint8
	fallback []uint8
	exact    bool
}

// planChargerScan geeft de al gekozen units de officiële antwoordtijd. Daarna
// volgen alle nog niet genoemde device-adressen als snelle vangnetronde. Unit
// 247 is het plantadres en kan nooit een EVAC zijn.
func planChargerScan(configured, exactText string) (chargerScanPlan, error) {
	exactText = strings.TrimSpace(exactText)
	if exactText != "" {
		exact, err := strconv.Atoi(exactText)
		if err != nil || exact < 1 || exact > 246 {
			return chargerScanPlan{}, fmt.Errorf("het Modbus unit-id van de AC-laadpaal moet een getal van 1 tot en met 246 zijn")
		}
		return chargerScanPlan{reliable: []uint8{uint8(exact)}, exact: true}, nil
	}

	preferred, err := parseUnits(configured)
	if err != nil {
		return chargerScanPlan{}, err
	}
	seen := map[uint8]bool{}
	plan := chargerScanPlan{reliable: make([]uint8, 0, len(preferred))}
	for _, unit := range preferred {
		if unit <= 246 && !seen[unit] {
			seen[unit] = true
			plan.reliable = append(plan.reliable, unit)
		}
	}
	for unit := 1; unit <= 246; unit++ {
		value := uint8(unit)
		if !seen[value] {
			plan.fallback = append(plan.fallback, value)
		}
	}
	return plan, nil
}

// scanUnits tast iedere opgegeven unit af. De lijst kan bewust gaten bevatten:
// een laadpaal op unit 8 mag niet verdwijnen omdat units 4 tot en met 7 stil
// waren. Daarom wordt niets op grond van aaneengesloten nummering overgeslagen.
//
// Een Modbus-uitzondering bewijst bovendien dat het adres bereikbaar is. Als
// ten minste één unit netjes antwoordde maar deze apparaatsoort nergens stond,
// is dat een lege vondst en niet de eerste timeout op een niet-bestaande unit.
func scanUnits(reader sigen.Reader, card sigen.Card, units []uint8) ([]uint8, error) {
	probe := card.Probe()
	var found []uint8
	var firstFailure error
	answered := false
	for _, unit := range units {
		_, err := probe.Read(reader, unit)
		switch {
		case err == nil:
			found = append(found, unit)
			answered = true
		case isRefusal(err):
			// Een weigering is een antwoord: dit apparaat staat niet op deze
			// unit, maar er luistert wel iets.
			answered = true
		default:
			if firstFailure == nil {
				firstFailure = err
			}
		}
	}
	if len(found) == 0 && !answered && firstFailure != nil {
		return nil, firstFailure
	}
	return found, nil
}

// scanFirstUnit heeft dezelfde foutbetekenis als scanUnits, maar levert de
// eerste echte treffer meteen op. Weigeringen bewijzen nog steeds dat het
// systeem antwoordt; alleen stilte zonder enig antwoord wordt een fout.
func scanFirstUnit(reader sigen.Reader, card sigen.Card, units []uint8) ([]uint8, error) {
	probe := card.Probe()
	var firstFailure error
	answered := false
	for _, unit := range units {
		_, err := probe.Read(reader, unit)
		switch {
		case err == nil:
			return []uint8{unit}, nil
		case isRefusal(err):
			answered = true
		default:
			if firstFailure == nil {
				firstFailure = err
			}
		}
	}
	if !answered && firstFailure != nil {
		return nil, firstFailure
	}
	return nil, nil
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
