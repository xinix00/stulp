package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// stub is een Sigenergy-systeem met registers erin. Wat er niet in staat leest
// als nul, precies zoals een register dat wél bestaat maar niets meldt.
type stub struct{ words map[uint16]uint16 }

type recordedWrite struct {
	unit  uint8
	start uint16
	value uint16
}

type writeRecorder struct{ writes []recordedWrite }

func (w *writeRecorder) WriteSingle(unit uint8, start, value uint16) error {
	w.writes = append(w.writes, recordedWrite{unit: unit, start: start, value: value})
	return nil
}

type scanRead struct {
	unit    uint8
	start   uint16
	holding bool
}

type scanReader struct {
	present map[uint8]bool
	refuse  map[uint8]bool
	reads   []scanRead
}

func (s *scanReader) ReadInput(unit uint8, start, count uint16) ([]uint16, error) {
	return s.read(unit, start, count, false)
}

func (s *scanReader) ReadHolding(unit uint8, start, count uint16) ([]uint16, error) {
	return s.read(unit, start, count, true)
}

func (s *scanReader) read(unit uint8, start, count uint16, holding bool) ([]uint16, error) {
	s.reads = append(s.reads, scanRead{unit: unit, start: start, holding: holding})
	if s.present[unit] {
		return make([]uint16, count), nil
	}
	if s.refuse[unit] {
		function := byte(0x04)
		if holding {
			function = 0x03
		}
		return nil, modbus.Exception{Function: function, Code: 2}
	}
	return nil, errors.New("i/o timeout")
}

func (s stub) read(start, count uint16) ([]uint16, error) {
	out := make([]uint16, count)
	for i := range out {
		out[i] = s.words[start+uint16(i)]
	}
	return out, nil
}

func (s stub) ReadInput(_ uint8, start, count uint16) ([]uint16, error) {
	return s.read(start, count)
}

func (s stub) ReadHolding(_ uint8, start, count uint16) ([]uint16, error) {
	return s.read(start, count)
}

// De helpers hieronder schrijven een waarde zoals het systeem hem op de lijn zet:
// het hoogste register vooraan.
func put16(words map[uint16]uint16, addr uint16, value uint16) { words[addr] = value }

func put32(words map[uint16]uint16, addr uint16, value int32) {
	words[addr] = uint16(uint32(value) >> 16)
	words[addr+1] = uint16(uint32(value))
}

func put64(words map[uint16]uint16, addr uint16, value uint64) {
	for i := 0; i < 4; i++ {
		words[addr+uint16(i)] = uint16(value >> (48 - 16*i))
	}
}

func putString(words map[uint16]uint16, addr uint16, value string) {
	raw := []byte(value)
	for i := 0; i < len(raw); i += 2 {
		high := uint16(raw[i]) << 8
		if i+1 < len(raw) {
			high |= uint16(raw[i+1])
		}
		words[addr+uint16(i/2)] = high
	}
}

// readingOf leest een hele kaart uit een nagebouwd systeem, langs dezelfde weg
// als een echte ronde: groeperen, lezen, en de systeemregisters erbij.
func readingOf(t *testing.T, card sigen.Card, words map[uint16]uint16) sigen.Reading {
	t.Helper()
	device := stub{words: words}
	values, err := sigen.NewPoller(1, sigen.ReadingSet(card)).Read(device)
	if err != nil {
		t.Fatal(err)
	}
	if set := sigen.SystemSet(card); len(set) > 0 {
		extra, err := sigen.NewPoller(sigen.SystemUnit, set).Read(device)
		if err != nil {
			t.Fatal(err)
		}
		values = values.Merge(extra)
	}
	return values
}

func check(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for name, expected := range want {
		value, ok := got[name]
		if !ok {
			t.Errorf("%s ontbreekt in wat er op de tegels komt", name)
			continue
		}
		if !reflect.DeepEqual(value, expected) {
			t.Errorf("%s = %v (%T), wil %v (%T)", name, value, value, expected, expected)
		}
	}
	for name := range got {
		if _, expected := want[name]; !expected {
			t.Errorf("%s = %v komt erbij terwijl er niets over te melden was", name, got[name])
		}
	}
}

func TestPlantReachesTheTiles(t *testing.T) {
	words := map[uint16]uint16{}
	put32(words, sigen.Plant.GridPower.Addr, 2500)              // 2500 W van het net
	put16(words, sigen.Plant.GridStatus.Addr, 0)                // op het net
	put16(words, sigen.Plant.BatterySoC.Addr, 800)              // gain 10 -> 80 procent
	put32(words, sigen.Plant.SolarPower.Addr, 3000)             // eigen omvormers
	put32(words, sigen.Plant.ThirdPartyInverterPower.Addr, 500) // en die van een ander merk
	put32(words, sigen.Plant.BatteryPower.Addr, -2500)          // batterij levert
	put32(words, sigen.Plant.GeneralLoadPower.Addr, 900)        // huis gebruikt

	plant := &plantDevice{}
	check(t, plant.apply(readingOf(t, sigen.Plant, words)), map[string]any{
		"measure_power.grid":      float64(2500),
		"measure_power.battery":   float64(-2500),
		"measure_power.load":      float64(900),
		"measure_power.solar":     float64(3500), // eigen plus derden
		"measure_power.evcharger": float64(0),    // geen laadpaal gekoppeld
		"measure_battery":         float64(80),
		"grid_status":             "on_grid",
	})
}

func TestBatteryReachesTheTiles(t *testing.T) {
	words := map[uint16]uint16{}
	put16(words, sigen.Battery.SoC.Addr, 512)               // gain 10 -> 51,2 procent
	put32(words, sigen.Battery.Power.Addr, -2500)           // systeemregister op unit 247
	put64(words, sigen.Battery.TotalCharged.Addr, 100000)   // gain 100 -> 1000 kWh
	put64(words, sigen.Battery.TotalDischarged.Addr, 50000) // gain 100 -> 500 kWh
	put16(words, sigen.Battery.Status.Addr, 1)              // draait
	put16(words, sigen.Battery.MaxCellTemp.Addr, 230)       // gain 10 -> 23 graden
	put16(words, sigen.Battery.MinCellTemp.Addr, 200)       // gain 10 -> 20 graden
	put16(words, sigen.Battery.PCSTemp.Addr, 340)           // gain 10 -> 34 graden
	putString(words, sigen.Battery.Firmware.Addr, "V100R001")

	battery := &batteryDevice{}
	check(t, battery.apply(readingOf(t, sigen.Battery, words)), map[string]any{
		"measure_battery":             float64(51.2),
		"measure_power":               float64(-2500),
		"meter_power.charged":         float64(1000),
		"meter_power.discharged":      float64(500),
		"measure_temperature.minCell": float64(20),
		"measure_temperature.maxCell": float64(23),
		"measure_temperature.pcs":     float64(34),
		"firmware":                    "V100R001",
		// Draaiend met vermogen eruit is ontladen; met het teken andersom zou
		// hier charging staan terwijl de batterij leegloopt.
		"battery_charging_state": "discharging",
	})
}

func TestEnergyMeterReachesTheTiles(t *testing.T) {
	words := map[uint16]uint16{}
	put32(words, sigen.Energy.Power.Addr, -1500) // teruglevering
	put16(words, sigen.Energy.GridStatus.Addr, 1)
	put32(words, sigen.Energy.PowerL1.Addr, -500)
	put32(words, sigen.Energy.PowerL2.Addr, -400)
	put32(words, sigen.Energy.PowerL3.Addr, -600)
	put64(words, sigen.Energy.TotalImport.Addr, 1234500) // gain 100 -> 12345 kWh
	put64(words, sigen.Energy.TotalExport.Addr, 678900)  // gain 100 -> 6789 kWh
	put16(words, sigen.Energy.PhaseControl.Addr, 1)

	energy := &energyDevice{}
	check(t, energy.apply(readingOf(t, sigen.Energy, words)), map[string]any{
		"measure_power":        float64(-1500),
		"measure_power.L1":     float64(-500),
		"measure_power.L2":     float64(-400),
		"measure_power.L3":     float64(-600),
		"meter_power.imported": float64(12345),
		"meter_power.exported": float64(6789),
		"grid_status":          "off_grid",
		"phase_control":        "on",
	})
}

// De laadpaal is de enige waar de bron een eenheid verkeerd doorzet: het
// register staat in kW en measure_power gaat in watt.
func TestChargerReachesTheTilesInWatts(t *testing.T) {
	words := map[uint16]uint16{}
	put16(words, sigen.EvACCharger.Status.Addr, 4)            // laden (C1)
	put32(words, sigen.EvACCharger.TotalCharged.Addr, 123456) // gain 100 -> 1234,56 kWh
	put32(words, sigen.EvACCharger.Power.Addr, 7000)          // gain 1000 -> 7 kW

	charger := &chargerDevice{}
	check(t, charger.apply(readingOf(t, sigen.EvACCharger, words)), map[string]any{
		"measure_power":            float64(7000),
		"meter_power.charged":      float64(1234.56),
		"evcharger_charging":       true,
		"evcharger_charging_state": "plugged_in_charging",
	})
	if charger.lastPower() != 7000 {
		t.Fatalf("de systeemtegel krijgt %v W van deze lader", charger.lastPower())
	}
}

func TestChargerOnCapabilityUsesSingleWriteAndProtocolPolarity(t *testing.T) {
	writer := &writeRecorder{}
	charger := &chargerDevice{meter: &meter{unit: 2}, writer: writer}

	if err := charger.OnCapability("evcharger_charging", true); err != nil {
		t.Fatal(err)
	}
	if err := charger.OnCapability("evcharger_charging", false); err != nil {
		t.Fatal(err)
	}
	want := []recordedWrite{
		{unit: 2, start: sigen.EvACChargerControl, value: 0},
		{unit: 2, start: sigen.EvACChargerControl, value: 1},
	}
	if !reflect.DeepEqual(writer.writes, want) {
		t.Fatalf("laadpaalopdrachten = %#v, wil %#v", writer.writes, want)
	}
	if err := charger.OnCapability("evcharger_charging", "aan"); err == nil {
		t.Fatal("een niet-booleaanse laadopdracht werd als stoppen behandeld")
	}
	if !reflect.DeepEqual(writer.writes, want) {
		t.Fatalf("ongeldige opdracht schreef toch: %#v", writer.writes)
	}
}

// Een omvormer met twee PV-ingangen op één fase meldt twee spanningen en één
// fase, en niet vier lege ingangen en drie fasen.
func TestInverterOnlyReportsWhatItHas(t *testing.T) {
	words := map[uint16]uint16{}
	put32(words, sigen.Inverter.Power.Addr, 5000)
	put32(words, sigen.Inverter.DailyYield.Addr, 1550)     // gain 100 -> 15,5 kWh
	put32(words, sigen.Inverter.TotalYield.Addr, 1234500)  // gain 100 -> 12345 kWh
	put16(words, sigen.Inverter.PV1Voltage.Addr, 4500)     // gain 10 -> 450 V
	put16(words, sigen.Inverter.PV2Voltage.Addr, 4200)     // gain 10 -> 420 V
	put16(words, sigen.Inverter.PV3Voltage.Addr, 9999)     // deze ingang bestaat niet
	put16(words, sigen.Inverter.PV4Voltage.Addr, 9999)     // en deze ook niet
	put32(words, sigen.Inverter.PhaseAVoltage.Addr, 23022) // gain 100 -> 230,22 V
	put32(words, sigen.Inverter.PhaseACurrent.Addr, 1234)  // gain 100 -> 12,34 A
	put32(words, sigen.Inverter.PhaseBVoltage.Addr, 23000)
	put32(words, sigen.Inverter.PhaseCVoltage.Addr, 23000)

	inverter := &inverterDevice{mppt: 2, threePhase: false, configured: true}
	check(t, inverter.apply(readingOf(t, sigen.Inverter, words)), map[string]any{
		"measure_power":          float64(5000),
		"meter_power.daily":      float64(15.5),
		"meter_power":            float64(12345),
		"measure_voltage.pv1":    float64(450),
		"measure_voltage.pv2":    float64(420),
		"measure_voltage.phaseA": float64(230.22),
		"measure_current.phaseA": float64(12.34),
	})
}

// Zolang de omvormer nog niet verteld heeft wat hij is, blijft het bij wat hoe
// dan ook klopt. Anders zou een enkelfasige omvormer even drie fasen melden.
func TestInverterSaysNothingAboutPhasesItHasNotConfirmed(t *testing.T) {
	words := map[uint16]uint16{}
	put32(words, sigen.Inverter.Power.Addr, 5000)
	put32(words, sigen.Inverter.PhaseAVoltage.Addr, 23022)

	inverter := &inverterDevice{}
	check(t, inverter.apply(readingOf(t, sigen.Inverter, words)), map[string]any{
		"measure_power":     float64(5000),
		"meter_power.daily": float64(0),
		"meter_power":       float64(0),
	})
}

func TestParseUnitsReadsRangesAndSingles(t *testing.T) {
	units, err := parseUnits("3, 1-4 ,247")
	if err != nil {
		t.Fatal(err)
	}
	want := []uint8{1, 2, 3, 4, 247}
	if !reflect.DeepEqual(units, want) {
		t.Fatalf("units = %v, wil %v", units, want)
	}
}

func TestParseUnitsFallsBackToTheDefault(t *testing.T) {
	units, err := parseUnits("  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 33 || units[0] != 1 || units[len(units)-1] != 247 {
		t.Fatalf("de standaard leverde %v", units)
	}
}

func TestChargerScanPlanGivesConfiguredRangeReliableTimeThenCoversOfficialDeviceRange(t *testing.T) {
	plan, err := planChargerScan("1-4,100,247", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.exact {
		t.Fatal("een lege laadpaalunit werd als expliciet behandeld")
	}
	if want := []uint8{1, 2, 3, 4, 100}; !reflect.DeepEqual(plan.reliable, want) {
		t.Fatalf("betrouwbare units = %v, wil %v", plan.reliable, want)
	}
	units := append(append([]uint8(nil), plan.reliable...), plan.fallback...)
	if len(units) != 246 {
		t.Fatalf("%d laadpaalunits, wil 246", len(units))
	}
	if want := []uint8{1, 2, 3, 4, 100, 5}; !reflect.DeepEqual(units[:len(want)], want) {
		t.Fatalf("voorkeursvolgorde = %v, wil %v", units[:len(want)], want)
	}
	seen := map[uint8]bool{}
	for _, unit := range units {
		if unit == 247 || seen[unit] {
			t.Fatalf("ongeldige of dubbele laadpaalunit %d in %v", unit, units)
		}
		seen[unit] = true
	}
}

func TestExplicitChargerUnitAvoidsTheFullScan(t *testing.T) {
	plan, err := planChargerScan("1-32,247", "203")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.exact || !reflect.DeepEqual(plan.reliable, []uint8{203}) || len(plan.fallback) != 0 {
		t.Fatalf("laadpaalplan = %+v", plan)
	}
	for _, value := range []string{"0", "247", "abc"} {
		if _, err := planChargerScan("1-32,247", value); err == nil {
			t.Errorf("expliciete laadpaalunit %q werd geaccepteerd", value)
		}
	}
}

func TestAutomaticChargerScanCanFindAUnitOutsideTheLegacyRange(t *testing.T) {
	plan, err := planChargerScan("1-32,247", "")
	if err != nil {
		t.Fatal(err)
	}
	units := append(append([]uint8(nil), plan.reliable...), plan.fallback...)
	reader := &scanReader{present: map[uint8]bool{203: true}}
	found, err := scanUnits(reader, sigen.EvACCharger, units)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found, []uint8{203}) {
		t.Fatalf("gevonden laadpaalunits = %v, wil [203]", found)
	}
	if len(reader.reads) != 246 {
		t.Fatalf("%d van 246 officiële device-units zijn afgetast", len(reader.reads))
	}
}

func TestChargerScanStopsAsSoonAsItFindsTheFirstUnit(t *testing.T) {
	reader := &scanReader{present: map[uint8]bool{3: true, 8: true}}
	found, err := scanFirstUnit(reader, sigen.EvACCharger, []uint8{1, 2, 3, 4, 8})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found, []uint8{3}) {
		t.Fatalf("gevonden laadpaalunits = %v, wil [3]", found)
	}
	if len(reader.reads) != 3 {
		t.Fatalf("na de treffer werden nog units gelezen: %v", reader.reads)
	}
}

func TestChargerScanUsesSigenergyFunction03AndDoesNotSkipSparseUnits(t *testing.T) {
	reader := &scanReader{
		present: map[uint8]bool{8: true},
		refuse:  map[uint8]bool{247: true},
	}
	units := []uint8{1, 2, 3, 4, 5, 6, 7, 8, 247}
	found, err := scanUnits(reader, sigen.EvACCharger, units)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found, []uint8{8}) {
		t.Fatalf("gevonden units = %v, wil [8]", found)
	}
	if len(reader.reads) != len(units) {
		t.Fatalf("%d van %d opgegeven units zijn afgetast", len(reader.reads), len(units))
	}
	for _, call := range reader.reads {
		if !call.holding {
			t.Fatalf("probe op unit %d gebruikte niet de door Sigenergy voorgeschreven functie 0x03", call.unit)
		}
		if call.start != sigen.EvACCharger.Status.Addr {
			t.Fatalf("probe-adres = %d", call.start)
		}
	}
}

func TestScanTreatsARefusalAsAReachableSystem(t *testing.T) {
	reader := &scanReader{
		present: map[uint8]bool{},
		refuse:  map[uint8]bool{247: true},
	}
	found, err := scanUnits(reader, sigen.EvACCharger, []uint8{1, 2, 247})
	if err != nil {
		t.Fatalf("een antwoordend systeem werd als timeout gemeld: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("gevonden units = %v", found)
	}
}

// De systeemtegel telt op wat de laadpalen trekken. Daarvoor moet de app het
// apparaat zelf vasthouden en niet de gedeelde meter: die zegt niet wat voor
// soort apparaat eraan hangt, en dan blijft de tegel eeuwig op nul staan.
func TestTheSystemTileFindsTheChargers(t *testing.T) {
	charger := &chargerDevice{power: 3600}
	instance.watch("lader-1", charger)
	t.Cleanup(func() { instance.forget("lader-1") })

	if got := instance.chargerPower(); got != 3600 {
		t.Fatalf("laadpaalvermogen = %v W, wil 3600", got)
	}
	plant := &plantDevice{}
	values := plant.apply(readingOf(t, sigen.Plant, map[uint16]uint16{}))
	if values["measure_power.evcharger"] != float64(3600) {
		t.Fatalf("op de systeemtegel = %v", values["measure_power.evcharger"])
	}
}

// Een unit-id dat niet bestaat hoort te struikelen en niet stil te worden
// weggelaten: dan tast iemand af naar een apparaat dat nooit gevonden wordt.
func TestParseUnitsRefusesUnitIDsModbusDoesNotHave(t *testing.T) {
	for _, text := range []string{"0", "256", "1-256", "abc", "5-1", "1-"} {
		if _, err := parseUnits(text); err == nil {
			t.Errorf("%q werd geaccepteerd", text)
		}
	}
}
