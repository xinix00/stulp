package sigen

// De registerkaarten, overgenomen uit lib/modbus/registry/ van de Homey-app.
//
// Elk adres, elke lengte en elke gain staat daar; hier staat er niets bij
// verzonnen. Wat de bron uitgecommentarieerd had gelaten -- celspanningen,
// de spanningen en stromen van de energiemeter, de export- en importgrenzen --
// staat hier niet, want dat werkte daar ook niet. PORTED.md noemt ze bij naam.
//
// De gain is een deler: de waarde in de eenheid uit de omschrijving is het ruwe
// getal gedeeld door de gain. Sigenergy noemt dat zelf zo.

// Card is een registerkaart van één apparaatsoort.
type Card interface {
	// All is elk register in de kaart. De leesronden komen hieruit, dus een veld
	// dat er niet in staat wordt nooit gelezen.
	All() Set
	// Probe is het register waaraan te zien is dat er op een unit een apparaat
	// van deze soort staat.
	//
	// De bron laat de gebruiker een unit-id intypen en toetst dat met het
	// serienummer op 30515. Dat staat bij de omvormer én bij de batterij, dus
	// daarmee is niet te zien wélke van de twee er is -- goed genoeg als iemand
	// het zelf invult, maar niet om op af te tasten. Daarom wijst elke kaart hier
	// een register aan dat alleen bij die soort hoort. Alle vijf komen uit de
	// kaart van het apparaat zelf; er is niets bij verzonnen.
	Probe() Reg
}

// InfoSet zijn de registers die niet veranderen: één keer lezen per verbinding.
func InfoSet(card Card) Set { return pick(card.All(), info) }

// ReadingSet is de stand van nu, op de unit van het apparaat zelf.
func ReadingSet(card Card) Set { return pick(card.All(), reading) }

// SystemSet staat in de registerruimte van het systeem en wordt op SystemUnit
// gelezen, ook voor een apparaat dat zelf een andere unit heeft.
func SystemSet(card Card) Set { return pick(card.All(), system) }

// ---------------------------------------------------------------------------
// Systeem -- lib/modbus/registry/plant.js
// ---------------------------------------------------------------------------

// PlantMap is het systeem als geheel. Staat standaard op unit 247.
type PlantMap struct {
	GridPower               Reg
	GridStatus              Reg
	BatterySoC              Reg
	SolarPower              Reg
	BatteryPower            Reg
	GeneralLoadPower        Reg
	ThirdPartyInverterPower Reg
}

var Plant = PlantMap{
	GridPower:               i32(reading, 30005, 1, "vermogen op het net (W)"),
	GridStatus:              u16(reading, 30009, 1, "netstand"),
	BatterySoC:              u16(reading, 30014, 10, "laadtoestand batterij (%)"),
	SolarPower:              i32(reading, 30035, 1, "zonvermogen (W)"),
	BatteryPower:            i32(reading, 30037, 1, "batterijvermogen (W)"),
	ThirdPartyInverterPower: i32(reading, 30194, 1, "vermogen omvormer van derden (W)"),
	GeneralLoadPower:        i32(reading, 30282, 1, "verbruik in huis (W)"),
}

// Probe: het netvermogen staat alleen in de systeemruimte. Zo herkende de bron
// zijn systeem ook.
func (m PlantMap) Probe() Reg { return m.GridPower }

func (m PlantMap) All() Set {
	return Set{m.GridPower, m.GridStatus, m.BatterySoC, m.SolarPower, m.BatteryPower,
		m.ThirdPartyInverterPower, m.GeneralLoadPower}
}

// ---------------------------------------------------------------------------
// Omvormer -- lib/modbus/registry/inverter.js
// ---------------------------------------------------------------------------

// InverterMap is één omvormer. Hoeveel PV-ingangen hij heeft en hoeveel fasen
// hij levert vertelt hij zelf, in MPPTCount en OutputType.
type InverterMap struct {
	Serial     Reg
	OutputType Reg
	MPPTCount  Reg

	PhaseAVoltage Reg
	PhaseBVoltage Reg
	PhaseCVoltage Reg
	PhaseACurrent Reg
	PhaseBCurrent Reg
	PhaseCCurrent Reg

	PV1Voltage Reg
	PV2Voltage Reg
	PV3Voltage Reg
	PV4Voltage Reg
	Power      Reg

	DailyYield Reg
	TotalYield Reg
}

var Inverter = InverterMap{
	Serial:     text(info, 30515, 10, "serienummer"),
	OutputType: u16(info, 31004, 1, "uitgangstype"),
	MPPTCount:  u16(info, 31026, 1, "aantal MPPT-ingangen"),

	PhaseAVoltage: u32(reading, 31011, 100, "spanning L1 (V)"),
	PhaseBVoltage: u32(reading, 31013, 100, "spanning L2 (V)"),
	PhaseCVoltage: u32(reading, 31015, 100, "spanning L3 (V)"),
	PhaseACurrent: i32(reading, 31017, 100, "stroom L1 (A)"),
	PhaseBCurrent: i32(reading, 31019, 100, "stroom L2 (A)"),
	PhaseCCurrent: i32(reading, 31021, 100, "stroom L3 (A)"),

	PV1Voltage: i16(reading, 31027, 10, "spanning PV1 (V)"),
	PV2Voltage: i16(reading, 31029, 10, "spanning PV2 (V)"),
	PV3Voltage: i16(reading, 31031, 10, "spanning PV3 (V)"),
	PV4Voltage: i16(reading, 31033, 10, "spanning PV4 (V)"),
	Power:      i32(reading, 31035, 1, "vermogen (W)"),

	DailyYield: u32(reading, 31509, 100, "opbrengst vandaag (kWh)"),
	TotalYield: u32(reading, 31511, 100, "opbrengst totaal (kWh)"),
}

// Probe: het aantal MPPT-ingangen staat alleen bij een omvormer. Het serienummer
// waarmee de bron toetste deelt de omvormer met de batterij.
func (m InverterMap) Probe() Reg { return m.MPPTCount }

func (m InverterMap) All() Set {
	return Set{m.Serial, m.OutputType, m.MPPTCount,
		m.PhaseAVoltage, m.PhaseBVoltage, m.PhaseCVoltage,
		m.PhaseACurrent, m.PhaseBCurrent, m.PhaseCCurrent,
		m.PV1Voltage, m.PV2Voltage, m.PV3Voltage, m.PV4Voltage, m.Power,
		m.DailyYield, m.TotalYield}
}

// PVVoltages zijn de vier PV-spanningen op volgorde, zodat MPPTCount ze kan
// afsnijden zonder dat er ergens een lijstje opnieuw wordt opgeschreven.
func (m InverterMap) PVVoltages() []Reg {
	return []Reg{m.PV1Voltage, m.PV2Voltage, m.PV3Voltage, m.PV4Voltage}
}

// ---------------------------------------------------------------------------
// Batterij -- lib/modbus/registry/battery.js
// ---------------------------------------------------------------------------

// BatteryMap is één batterij.
type BatteryMap struct {
	Serial   Reg
	Capacity Reg

	// Power staat met opzet in de systeemruimte. De batterij-unit biedt geen
	// vermogen aan; de bron haalt het van het systeem en dat is hier hetzelfde.
	Power Reg

	Firmware        Reg
	TotalCharged    Reg
	TotalDischarged Reg
	Status          Reg
	SoC             Reg
	MaxCellTemp     Reg
	MinCellTemp     Reg
	PCSTemp         Reg
}

var Battery = BatteryMap{
	Serial:   text(info, 30515, 10, "serienummer"),
	Capacity: u32(info, 30548, 100, "capaciteit (kWh)"),

	Power: i32(system, 30037, 1, "batterijvermogen (W)"),

	Firmware:        text(reading, 30525, 15, "firmware"),
	TotalCharged:    u64(reading, 30568, 100, "totaal geladen (kWh)"),
	TotalDischarged: u64(reading, 30574, 100, "totaal ontladen (kWh)"),
	Status:          u16(reading, 30578, 1, "bedrijfsstand"),
	SoC:             u16(reading, 30601, 10, "laadtoestand (%)"),
	MaxCellTemp:     i16(reading, 30620, 10, "hoogste celtemperatuur (°C)"),
	MinCellTemp:     i16(reading, 30621, 10, "laagste celtemperatuur (°C)"),
	PCSTemp:         i16(reading, 31003, 10, "temperatuur omvormerdeel (°C)"),
}

// Probe: de laadtoestand staat alleen bij een batterij.
func (m BatteryMap) Probe() Reg { return m.SoC }

func (m BatteryMap) All() Set {
	return Set{m.Serial, m.Capacity, m.Power, m.Firmware, m.TotalCharged, m.TotalDischarged,
		m.Status, m.SoC, m.MaxCellTemp, m.MinCellTemp, m.PCSTemp}
}

// ---------------------------------------------------------------------------
// Energiemeter -- lib/modbus/registry/energy.js
// ---------------------------------------------------------------------------

// EnergyMap is de meter op de netaansluiting. Staat op unit 247, net als het
// systeem zelf.
type EnergyMap struct {
	Power        Reg
	GridStatus   Reg
	PowerL1      Reg
	PowerL2      Reg
	PowerL3      Reg
	TotalImport  Reg
	TotalExport  Reg
	PhaseControl Reg
}

var Energy = EnergyMap{
	Power:        i32(reading, 30005, 1, "vermogen (W)"),
	GridStatus:   u16(system, 30009, 1, "netstand"),
	PowerL1:      i32(reading, 30052, 1, "vermogen L1 (W)"),
	PowerL2:      i32(reading, 30054, 1, "vermogen L2 (W)"),
	PowerL3:      i32(reading, 30056, 1, "vermogen L3 (W)"),
	TotalImport:  u64(reading, 30260, 100, "afgenomen energie (kWh)"),
	TotalExport:  u64(reading, 30264, 100, "teruggeleverde energie (kWh)"),
	PhaseControl: holdingU16(reading, 40030, 1, "individuele fasesturing"),
}

// Probe: hetzelfde register als het systeem, want de netmeter staat in dezelfde
// registerruimte. Zo herkende de bron hem ook.
func (m EnergyMap) Probe() Reg { return m.Power }

func (m EnergyMap) All() Set {
	return Set{m.Power, m.GridStatus, m.PowerL1, m.PowerL2, m.PowerL3,
		m.TotalImport, m.TotalExport, m.PhaseControl}
}

// ---------------------------------------------------------------------------
// AC-laadpaal -- lib/modbus/registry/evACCharger.js
// ---------------------------------------------------------------------------

// EvACChargerMap is de wisselstroomlader.
type EvACChargerMap struct {
	Status Reg
	// TotalCharged telt door over alle laadbeurten heen.
	TotalCharged Reg
	// Power komt in kW binnen -- gain 1000 -- terwijl measure_power in watt
	// gaat. Wie deze waarde op measure_power zet moet hem dus eerst
	// vermenigvuldigen; zie WattFromKilowatt en PORTED.md.
	Power Reg
}

var EvACCharger = EvACChargerMap{
	Status:       u16(reading, 32000, 1, "laadstand"),
	TotalCharged: u32(reading, 32001, 100, "totaal geladen (kWh)"),
	Power:        i32(reading, 32003, 1000, "laadvermogen (kW)"),
}

// Probe: de laadstand staat alleen bij een laadpaal. Zo herkende de bron hem ook.
func (m EvACChargerMap) Probe() Reg { return m.Status }

func (m EvACChargerMap) All() Set { return Set{m.Status, m.TotalCharged, m.Power} }

// EvACChargerControl is het write-only register waarmee een laadbeurt begint en
// eindigt. Sigenergy schrijft voor dit ene register functiecode 0x06 voor.
const EvACChargerControl uint16 = 42000

const (
	EvACChargerStart uint16 = 0
	EvACChargerStop  uint16 = 1
)

// WattFromKilowatt rekent een vermogen in kW om naar watt.
//
// De bron zet het laadvermogen van de AC-lader ongewijzigd op measure_power,
// terwijl het register in kW staat en measure_power in watt: een lader die 7 kW
// levert komt daar als 7 W op de tegel. Dat is hier rechtgezet, en het staat als
// afwijking in PORTED.md.
func WattFromKilowatt(kilowatt float64) float64 { return kilowatt * 1000 }
