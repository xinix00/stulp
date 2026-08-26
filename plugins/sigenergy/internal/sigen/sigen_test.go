package sigen

import (
	"errors"
	"reflect"
	"testing"

	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
)

// fake is een apparaat dat registers heeft en er soms een weigert.
type fake struct {
	words map[uint16]uint16
	// refuse zijn adressen waarop het apparaat een uitzondering geeft, zoals
	// firmware doet die een veld uit een nieuwere kaart niet kent.
	refuse map[uint16]bool
	// broken is een kapotte lijn in plaats van een weigering.
	broken error

	reads []read
}

type read struct {
	unit    uint8
	start   uint16
	count   uint16
	holding bool
}

func (f *fake) ReadInput(unit uint8, start, count uint16) ([]uint16, error) {
	return f.read(unit, start, count, false)
}

func (f *fake) ReadHolding(unit uint8, start, count uint16) ([]uint16, error) {
	return f.read(unit, start, count, true)
}

func (f *fake) read(unit uint8, start, count uint16, holding bool) ([]uint16, error) {
	f.reads = append(f.reads, read{unit: unit, start: start, count: count, holding: holding})
	if f.broken != nil {
		return nil, f.broken
	}
	for addr := start; addr < start+count; addr++ {
		if f.refuse[addr] {
			function := byte(0x04)
			if holding {
				function = 0x03
			}
			return nil, modbus.Exception{Function: function, Code: 2}
		}
	}
	out := make([]uint16, count)
	for i := range out {
		out[i] = f.words[start+uint16(i)]
	}
	return out, nil
}

func newFake(words map[uint16]uint16) *fake {
	return &fake{words: words, refuse: map[uint16]bool{}}
}

// De schalen komen uit de bron en zijn het verschil tussen 52 procent en 520.
func TestDecodesEveryScaleFromTheSource(t *testing.T) {
	cases := []struct {
		what  string
		reg   Reg
		words []uint16
		want  float64
	}{
		// int32 zonder gain: netvermogen in watt, ook negatief bij teruglevering.
		{"vermogen op het net", Plant.GridPower, []uint16{0x0000, 0x09C4}, 2500},
		{"teruglevering", Plant.GridPower, []uint16{0xFFFF, 0xF63C}, -2500},
		// uint16 met gain 10: laadtoestand in procenten.
		{"laadtoestand", Plant.BatterySoC, []uint16{523}, 52.3},
		// int16 met gain 10, negatief: celtemperatuur onder nul.
		{"celtemperatuur", Battery.MinCellTemp, []uint16{0xFFF1}, -1.5},
		// uint32 met gain 100: spanning.
		{"spanning L1", Inverter.PhaseAVoltage, []uint16{0x0000, 0x5AB6}, 232.22},
		// uint32 met gain 100 over twee registers: dagopbrengst in kWh.
		{"opbrengst vandaag", Inverter.DailyYield, []uint16{0x0000, 0x0BB8}, 30},
		// uint64 met gain 100 over vier registers: meterstand in kWh.
		{"totaal geladen", Battery.TotalCharged, []uint16{0, 0, 0x0001, 0x86A0}, 1000},
		// int32 met gain 1000: laadvermogen in kW.
		{"laadvermogen", EvACCharger.Power, []uint16{0x0000, 0x1B58}, 7},
	}
	for _, test := range cases {
		got, ok := test.reg.Number(test.words)
		if !ok {
			t.Errorf("%s: niet te lezen", test.what)
			continue
		}
		if got != test.want {
			t.Errorf("%s = %v, wil %v", test.what, got, test.want)
		}
	}
}

// Een getal van 32 bits staat met het hoogste register vooraan. Andersom levert
// een zonvermogen van tientallen megawatt op, dus dit is geen detail.
func TestWordOrderPutsTheHighRegisterFirst(t *testing.T) {
	got, _ := Plant.SolarPower.Number([]uint16{0x0001, 0x0000})
	if got != 65536 {
		t.Fatalf("32-bits waarde = %v, wil 65536", got)
	}
	got64, _ := Battery.TotalDischarged.Number([]uint16{0x0001, 0, 0, 0})
	if got64 != float64(uint64(1)<<48)/100 {
		t.Fatalf("64-bits waarde = %v", got64)
	}
}

// Sigenergy vult tekstvelden aan met nulbytes. Die horen niet in een serienummer.
func TestReadsTextWithoutThePadding(t *testing.T) {
	words := []uint16{0x5349, 0x4745, 0x3132, 0x3300, 0x0000, 0, 0, 0, 0, 0}
	got, ok := Inverter.Serial.Text(words)
	if !ok {
		t.Fatal("serienummer niet te lezen")
	}
	if got != "SIGE123" {
		t.Fatalf("serienummer = %q", got)
	}
}

// Aaneengesloten registers gaan in één vraag de deur uit; een klein gat wordt
// overbrugd omdat een tweede rondje duurder is dan een paar bytes meelezen.
func TestGroupsRegistersIntoRuns(t *testing.T) {
	runs := group(ReadingSet(Plant))
	want := []Run{
		{Start: 30005, Count: 10}, // 30005, 30009 en 30014 in één vraag
		{Start: 30035, Count: 4},  // 30035 en 30037
		{Start: 30194, Count: 2},
		{Start: 30282, Count: 2},
	}
	if len(runs) != len(want) {
		t.Fatalf("aantal bereiken = %d, wil %d (%v)", len(runs), len(want), runs)
	}
	for i, run := range runs {
		if run.Start != want[i].Start || run.Count != want[i].Count {
			t.Errorf("bereik %d = %d+%d, wil %d+%d", i, run.Start, run.Count, want[i].Start, want[i].Count)
		}
	}
}

// Een bereik dat langer zou worden dan Modbus draagt wordt gesplitst.
func TestNeverGroupsBeyondWhatModbusCarries(t *testing.T) {
	var set Set
	for addr := uint16(30000); addr < 30200; addr += 2 {
		set = append(set, u16(reading, addr, 1, "vulling"))
	}
	for _, run := range group(set) {
		if run.Count > maxRun {
			t.Fatalf("bereik van %d registers, meer dan de %d die zijn toegestaan", run.Count, maxRun)
		}
		if run.Count > modbus.MaxReadRegisters {
			t.Fatalf("bereik van %d registers past niet in één Modbus-vraag", run.Count)
		}
	}
}

func TestPollerReadsAWholeSet(t *testing.T) {
	device := newFake(map[uint16]uint16{
		30005: 0x0000, 30006: 0x09C4, // 2500 W van het net
		30014: 523,                   // 52,3 procent
		30037: 0xFFFF, 30038: 0xF63C, // -2500 W uit de batterij
	})
	poller := NewPoller(SystemUnit, ReadingSet(Plant))
	values, err := poller.Read(device)
	if err != nil {
		t.Fatal(err)
	}
	if power, _ := values.Number(Plant.GridPower); power != 2500 {
		t.Errorf("netvermogen = %v", power)
	}
	if soc, _ := values.Number(Plant.BatterySoC); soc != 52.3 {
		t.Errorf("laadtoestand = %v", soc)
	}
	if battery, _ := values.Number(Plant.BatteryPower); battery != -2500 {
		t.Errorf("batterijvermogen = %v", battery)
	}
	if len(device.reads) != 4 {
		t.Fatalf("%d vragen voor zeven registers; het groeperen doet niets", len(device.reads))
	}
	for _, call := range device.reads {
		if call.unit != SystemUnit {
			t.Fatalf("gelezen op unit %d in plaats van %d", call.unit, SystemUnit)
		}
	}
}

func TestReadOnlyRegistersUseInputAndRWRegistersUseHolding(t *testing.T) {
	charger := newFake(map[uint16]uint16{})
	if _, err := NewPoller(2, ReadingSet(EvACCharger)).Read(charger); err != nil {
		t.Fatal(err)
	}
	if len(charger.reads) == 0 {
		t.Fatal("de laadpaal leverde geen leesvragen op")
	}
	for _, call := range charger.reads {
		if call.holding {
			t.Fatalf("read-only laadpaalregister %d ging als holding-register de deur uit", call.start)
		}
	}

	energy := newFake(map[uint16]uint16{})
	if _, err := NewPoller(SystemUnit, ReadingSet(Energy)).Read(energy); err != nil {
		t.Fatal(err)
	}
	foundPhaseControl := false
	for _, call := range energy.reads {
		coversPhaseControl := call.start <= Energy.PhaseControl.Addr &&
			call.start+call.count > Energy.PhaseControl.Addr
		if coversPhaseControl {
			foundPhaseControl = true
			if !call.holding {
				t.Fatal("RW-register 40030 ging als input-register de deur uit")
			}
		} else if call.holding {
			t.Fatalf("read-only registerbereik %d+%d ging als holding-register de deur uit", call.start, call.count)
		}
	}
	if !foundPhaseControl {
		t.Fatal("RW-register 40030 werd niet gelezen")
	}
}

// Sigenergy laat gaten in zijn kaart. Wie een bereik leest dat over zo'n gat
// loopt krijgt soms een weigering voor het hele bereik -- en dan zou één
// onbekend adres zes tegels leeg houden.
func TestFallsBackToSingleRegistersWhenARangeIsRefused(t *testing.T) {
	device := newFake(map[uint16]uint16{30005: 0, 30006: 100, 30014: 500})
	device.refuse[30009] = true

	poller := NewPoller(SystemUnit, ReadingSet(Plant))
	values, err := poller.Read(device)
	if err != nil {
		t.Fatal(err)
	}
	if power, ok := values.Number(Plant.GridPower); !ok || power != 100 {
		t.Fatalf("netvermogen = %v (%v); de terugval liet de buren vallen", power, ok)
	}
	if soc, ok := values.Number(Plant.BatterySoC); !ok || soc != 50 {
		t.Fatalf("laadtoestand = %v (%v)", soc, ok)
	}
	if _, ok := values.Number(Plant.GridStatus); ok {
		t.Fatal("het geweigerde register leverde toch een waarde")
	}
	missing := values.Missing()
	if len(missing) != 1 || missing[0].Addr != 30009 {
		t.Fatalf("ontbrekende registers = %v; die horen te worden gemeld", missing)
	}

	// De volgende ronde weet het al: geen tweede poging op het hele bereik.
	device.reads = nil
	if _, err := poller.Read(device); err != nil {
		t.Fatal(err)
	}
	for _, call := range device.reads {
		if call.start == 30005 && call.count > 2 {
			t.Fatal("het geweigerde bereik werd opnieuw als geheel gevraagd")
		}
	}
}

// Een kapotte lijn is iets anders dan een geweigerd adres: dan klopt niets van
// wat er nog zou komen, en de ronde hoort te stoppen.
func TestStopsWhenTheConnectionBreaks(t *testing.T) {
	device := newFake(nil)
	device.broken = errors.New("connection reset by peer")
	poller := NewPoller(SystemUnit, ReadingSet(Plant))
	if _, err := poller.Read(device); err == nil {
		t.Fatal("een kapotte lijn leverde geen fout")
	}
}

// De systeemregisters gaan naar unit 247, ook als het apparaat zelf op unit 1
// staat. De batterij haalt zijn vermogen daar vandaan.
func TestSystemRegistersLiveOnTheSystemUnit(t *testing.T) {
	set := SystemSet(Battery)
	if len(set) != 1 || set[0].Addr != 30037 {
		t.Fatalf("systeemregisters van de batterij = %v", set)
	}
	if len(ReadingSet(Battery)) == 0 || len(InfoSet(Battery)) != 2 {
		t.Fatal("de batterij mist zijn gewone of zijn eenmalige registers")
	}
}

// All() is de lijst waaruit de leesronden komen. Een veld dat er niet in staat
// wordt nooit gelezen, en dat is precies het soort fout dat pas opvalt als
// iemand zich afvraagt waarom een tegel leeg blijft.
func TestEveryRegisterOfACardIsListedInAll(t *testing.T) {
	cards := map[string]Card{
		"systeem": Plant, "omvormer": Inverter, "batterij": Battery,
		"energiemeter": Energy, "AC-lader": EvACCharger,
	}
	for name, card := range cards {
		listed := map[uint16]bool{}
		for _, reg := range card.All() {
			if listed[reg.Addr] {
				t.Errorf("%s: adres %d staat twee keer in All()", name, reg.Addr)
			}
			listed[reg.Addr] = true
		}
		value := reflect.ValueOf(card)
		for i := 0; i < value.NumField(); i++ {
			reg, ok := value.Field(i).Interface().(Reg)
			if !ok {
				continue
			}
			if !listed[reg.Addr] {
				t.Errorf("%s: %s staat niet in All() en wordt dus nooit gelezen",
					name, value.Type().Field(i).Name)
			}
		}
	}
}

func TestTranslatesStatesTheWayTheSourceDoes(t *testing.T) {
	if got := BatteryChargingState(1, 2500); got != "charging" {
		t.Errorf("draaiend met vermogen erin = %q", got)
	}
	if got := BatteryChargingState(1, -2500); got != "discharging" {
		t.Errorf("draaiend met vermogen eruit = %q", got)
	}
	for _, status := range []float64{0, 2, 3, 7, 99} {
		if got := BatteryChargingState(status, 2500); got != "idle" {
			t.Errorf("stand %v = %q, wil idle", status, got)
		}
	}
	if got := GridStatus(1); got != "off_grid" {
		t.Errorf("netstand 1 = %q", got)
	}
	if got := GridStatus(9); got != "unknown_9" {
		t.Errorf("een onbekende netstand hoort het getal te dragen, kreeg %q", got)
	}
	if got := ACChargerChargingState(4, 7000); got != "plugged_in_charging" {
		t.Errorf("ladend = %q", got)
	}
	if got := ACChargerChargingState(4, 0); got != "plugged_in" {
		t.Errorf("aangesloten zonder vermogen = %q", got)
	}
	if got := ACChargerChargingState(1, 0); got != "plugged_out" {
		t.Errorf("niets aangesloten = %q", got)
	}
	if !ThreePhase(InverterOutputType(2)) || ThreePhase(InverterOutputType(3)) {
		t.Error("het uitgangstype bepaalt of er drie fasen te melden zijn")
	}
}

// Het laadvermogen van de AC-lader staat in kW en measure_power gaat in watt.
func TestChargerPowerReachesTheCapabilityInWatts(t *testing.T) {
	device := newFake(map[uint16]uint16{32003: 0x0000, 32004: 0x1B58})
	values, err := NewPoller(1, ReadingSet(EvACCharger)).Read(device)
	if err != nil {
		t.Fatal(err)
	}
	kilowatt, ok := values.Number(EvACCharger.Power)
	if !ok || kilowatt != 7 {
		t.Fatalf("laadvermogen = %v kW", kilowatt)
	}
	if watt := WattFromKilowatt(kilowatt); watt != 7000 {
		t.Fatalf("op measure_power = %v W, wil 7000", watt)
	}
}

func TestChargerControlUsesTheProtocolPolarity(t *testing.T) {
	if EvACChargerStart != 0 {
		t.Fatalf("startopdracht = %d, wil 0", EvACChargerStart)
	}
	if EvACChargerStop != 1 {
		t.Fatalf("stopopdracht = %d, wil 1", EvACChargerStop)
	}
}
