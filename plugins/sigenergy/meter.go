package main

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// meter is wat elke driver van deze app deelt: één unit, één registerkaart en
// een ronde die de tegels vult.
//
// De vijf drivers verschillen alleen in welke kaart ze lezen en wat ze met de
// waarden doen. De rest -- het unit-id uit de instellingen, de eenmalige
// registers, de systeemregisters op unit 247, bereikbaar of niet -- is voor
// allemaal hetzelfde en staat daarom één keer.
type meter struct {
	device *appsdk.Device
	card   sigen.Card
	// self is het apparaat dat deze meter gebruikt. De app houdt dát vast en
	// niet de meter, want de systeemtegel moet kunnen zien welke van zijn
	// apparaten laadpalen zijn -- en een gedeelde meter zegt daar niets over.
	self device
	// apply vertaalt een ronde naar capability-waarden.
	//
	// Dat het een tabel oplevert in plaats van meteen te schrijven is met opzet:
	// zo is de vertaling -- de schaal, het teken, welke stand welk woord wordt --
	// te toetsen zonder dat er een gekoppeld apparaat aan te pas komt. En dat is
	// precies het deel waar een fout niet opvalt: een tegel met 520 procent valt
	// op, eentje met 5,2 kWh niet.
	apply func(sigen.Reading) map[string]any
	// applyInfo krijgt de registers die niet veranderen. Nul als de kaart er
	// geen heeft.
	applyInfo func(sigen.Reading)

	mu       sync.Mutex
	unit     uint8
	reading  *sigen.Poller
	system   *sigen.Poller
	info     *sigen.Poller
	infoDone bool
	// toldMissing houdt bij dat er over ontbrekende registers al gemeld is. Dat
	// hoort één keer per verbinding en niet elke ronde opnieuw.
	toldMissing bool
}

// newMeter bouwt de leesronden voor één apparaat. self is het apparaat zelf, dat
// deze meter meteen daarna in zijn eigen veld zet.
func newMeter(self device, handle *appsdk.Device, card sigen.Card,
	apply func(sigen.Reading) map[string]any, applyInfo func(sigen.Reading)) (*meter, error) {
	unit, err := deviceUnit(handle)
	if err != nil {
		return nil, err
	}
	m := &meter{device: handle, card: card, self: self, apply: apply, applyInfo: applyInfo}
	m.buildPollers(unit)
	return m, nil
}

// buildPollers zet de drie ronden klaar. Aanroepen zonder de lock.
func (m *meter) buildPollers(unit uint8) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unit = unit
	m.reading = sigen.NewPoller(unit, sigen.ReadingSet(m.card))
	m.info, m.system, m.infoDone, m.toldMissing = nil, nil, false, false
	if set := sigen.InfoSet(m.card); len(set) > 0 {
		m.info = sigen.NewPoller(unit, set)
	}
	// De systeemregisters staan altijd op unit 247, ook voor een batterij die
	// zelf op unit 1 zit.
	if set := sigen.SystemSet(m.card); len(set) > 0 {
		m.system = sigen.NewPoller(sigen.SystemUnit, set)
	}
}

// OnInit neemt het apparaat op in de ronde. Er wordt hier niets gelezen: de
// lifecycle-callbacks lopen op één worker, en een omvormer die niet antwoordt
// zou daarmee elk ander apparaat van deze app ophouden.
func (m *meter) OnInit() error {
	instance.watch(m.device.ID(), m.self)
	return nil
}

func (m *meter) OnDeleted() { instance.forget(m.device.ID()) }

// OnSettings vangt een gewijzigd unit-id op. Zonder dit blijft de app op de oude
// unit lezen tot iemand hem herstart.
func (m *meter) OnSettings(changed map[string]any) error {
	if _, ok := changed["modbus_unitId"]; !ok {
		return nil
	}
	unit, err := deviceUnit(m.device)
	if err != nil {
		return err
	}
	m.buildPollers(unit)
	return nil
}

// forgetConnection zorgt dat de eenmalige registers na een herverbinding opnieuw
// gelezen worden: er kan intussen een ander apparaat op deze unit staan.
func (m *meter) forgetConnection() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.infoDone, m.toldMissing = false, false
}

func (m *meter) refresh(client *modbus.Client) {
	m.mu.Lock()
	readingPoller, systemPoller, infoPoller := m.reading, m.system, m.info
	needInfo := infoPoller != nil && !m.infoDone
	m.mu.Unlock()

	if needInfo {
		if values, err := infoPoller.Read(client); err == nil {
			if m.applyInfo != nil {
				m.applyInfo(values)
			}
			m.mu.Lock()
			m.infoDone = true
			m.mu.Unlock()
		}
		// Een fout hier krijgt geen eigen melding: de gewone ronde hieronder
		// loopt tegen dezelfde muur en zet het apparaat op onbereikbaar met de
		// reden erbij.
	}

	values, err := readingPoller.Read(client)
	if err != nil {
		m.unreachable(err)
		return
	}
	if systemPoller != nil {
		extra, err := systemPoller.Read(client)
		if err != nil {
			m.unreachable(err)
			return
		}
		values = values.Merge(extra)
	}

	m.device.SetAvailable()
	report := m.apply(values)
	for name, value := range report {
		if number, isNumber := value.(float64); isNumber {
			report[name] = round(number)
		}
	}
	if err := m.device.SetCapabilityValues(report); err != nil {
		m.device.Error(err.Error())
	}
	m.reportMissing(values)
}

// unreachable zet het apparaat op onbereikbaar met de reden erbij. Een tegel die
// stil op zijn laatste waarde blijft staan is erger dan een tegel die zegt dat
// hij niets meer hoort: die eerste ziet eruit alsof het huis niets verbruikt.
func (m *meter) unreachable(err error) {
	m.device.SetUnavailable("Het Sigenergy-systeem antwoordt niet: " + err.Error())
}

// reportMissing meldt eenmalig welke registers dit apparaat weigerde. Dat is
// bijna altijd firmware die ouder is dan de kaart, en zonder deze regel zoekt
// iemand zich suf naar waarom één tegel leeg blijft.
func (m *meter) reportMissing(values sigen.Reading) {
	missing := values.Missing()
	if len(missing) == 0 {
		return
	}
	m.mu.Lock()
	told := m.toldMissing
	m.toldMissing = true
	m.mu.Unlock()
	if told {
		return
	}
	names := make([]string, 0, len(missing))
	for _, reg := range missing {
		names = append(names, reg.String())
	}
	m.device.Error("deze unit kent deze registers niet: " + strings.Join(names, ", "))
}

// Unit is het unit-id waarop dit apparaat gelezen wordt.
func (m *meter) Unit() uint8 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unit
}

// round houdt de waarde op drie decimalen.
//
// Niet om af te ronden maar om af te kappen wat de deling erbij verzint: de
// grofste gain in de kaart is 1000, dus verder dan drie decimalen draagt geen
// enkel register informatie. Zonder dit staat er 232.22000000000003 op een tegel
// waar 232,22 gemeten is.
func round(value float64) float64 { return math.Round(value*1000) / 1000 }

// putNumber en putText halen één register uit de ronde en zetten het in de
// tabel die apply oplevert. Een register dat dit apparaat niet aanbood wordt
// overgeslagen.
//
// Overslaan en niet nul zetten: nul watt is een echte meting, en een tegel die
// nul meldt omdat er niets te meten viel liegt harder dan een lege tegel.
func putNumber(out map[string]any, values sigen.Reading, name string, reg sigen.Reg) {
	if value, ok := values.Number(reg); ok {
		out[name] = value
	}
}

func putText(out map[string]any, values sigen.Reading, name string, reg sigen.Reg) {
	if value, ok := values.Text(reg); ok {
		out[name] = value
	}
}

// deviceUnit leest het unit-id uit de instellingen van dit apparaat.
func deviceUnit(device *appsdk.Device) (uint8, error) {
	value, ok := device.Setting("modbus_unitId")
	if !ok {
		return 0, fmt.Errorf("dit apparaat heeft geen Modbus unit-id in zijn instellingen")
	}
	var number int
	switch typed := value.(type) {
	case float64:
		number = int(typed)
	case int:
		number = typed
	default:
		return 0, fmt.Errorf("het Modbus unit-id is %v en dat is geen getal", value)
	}
	// Nul is in Modbus het omroepadres: daar antwoordt niemand op, dus een
	// apparaat met unit 0 zou eindeloos op onbereikbaar blijven staan.
	if number < 1 || number > 255 {
		return 0, fmt.Errorf("Modbus unit-id %d bestaat niet; het loopt van 1 tot en met 255", number)
	}
	return uint8(number), nil
}

// ensureCapability en dropCapability laten de tegels overeenkomen met wat een
// apparaat werkelijk heeft. De omvormer is de enige die dat nodig heeft: hoeveel
// PV-ingangen en fasen hij heeft weet hij zelf, en dat verschilt per model.
func (m *meter) ensureCapability(name string) {
	if m.device.HasCapability(name) {
		return
	}
	if err := m.device.AddCapability(name); err != nil {
		m.device.Error("capability " + name + " toevoegen mislukte: " + err.Error())
	}
}

func (m *meter) dropCapability(name string) {
	if !m.device.HasCapability(name) {
		return
	}
	if err := m.device.RemoveCapability(name); err != nil {
		m.device.Error("capability " + name + " verwijderen mislukte: " + err.Error())
	}
}

// listByUnit levert de gevonden units als koppelbare apparaten, herkenbaar aan
// hun unit-id. Voor apparaten die geen serienummer in hun registers hebben.
func listByUnit(card sigen.Card, label string) ([]appsdk.PairedDevice, error) {
	found, err := instance.scan(card)
	if err != nil {
		return nil, err
	}
	devices := make([]appsdk.PairedDevice, 0, len(found))
	for _, unit := range found {
		devices = append(devices, paired(label, unitID(unit), unit))
	}
	return devices, nil
}

// listBySerial doet hetzelfde, maar noemt het apparaat bij zijn serienummer. Dat
// blijft kloppen als iemand de unit-ids in zijn systeem hernummert.
func listBySerial(card sigen.Card, serial sigen.Reg, label string) ([]appsdk.PairedDevice, error) {
	found, err := instance.scan(card)
	if err != nil {
		return nil, err
	}
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	devices := make([]appsdk.PairedDevice, 0, len(found))
	for _, unit := range found {
		// Het serienummer staat op een ander adres dan het register waarmee de
		// unit herkend werd, dus het wordt er apart bij gehaald -- alleen voor de
		// units die al geantwoord hebben. Een unit zonder leesbaar serienummer is
		// er nog steeds; dan is het unit-id waaraan hij te herkennen is.
		id := unitID(unit)
		if words, err := serial.Read(client, unit); err == nil {
			if text, ok := serial.Text(words); ok && text != "" {
				id = text
			}
		}
		devices = append(devices, paired(label, id, unit))
	}
	return devices, nil
}

// paired maakt één regel voor het koppelscherm.
//
// Het unit-id gaat naar de instellingen en niet naar Data: Data is de identiteit
// en ligt vast, en een unit-id is iets wat iemand kan hernummeren. Dan hoort het
// aan te passen te zijn zonder opnieuw te koppelen.
func paired(label, id string, unit uint8) appsdk.PairedDevice {
	return appsdk.PairedDevice{
		Name:     fmt.Sprintf("%s %s", label, id),
		Data:     map[string]any{"id": id},
		Settings: map[string]any{"modbus_unitId": int(unit)},
	}
}

// unitID is hoe een apparaat zonder serienummer heet: het unit-id is dan het
// enige wat het van zijn buren onderscheidt.
func unitID(unit uint8) string { return fmt.Sprintf("unit %d", unit) }
