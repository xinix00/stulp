package stats

import (
	"math"
	"runtime"
	"testing"
	"time"
)

var start = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// Een meting die op en neer gaat: het gemiddelde is zinvol, maar de uitschieters
// mogen er niet in verdwijnen. Wie alleen het gemiddelde bewaart ziet nooit dat
// het tien minuten dertig graden was.
func TestGaugeKeepsItsExtremes(t *testing.T) {
	series := NewSeries(Gauge)
	for index, value := range []float64{20, 25, 30, 21} {
		series.Add(value, start.Add(time.Duration(index)*time.Minute))
	}
	day := series.Window(0)
	if len(day) != 1 {
		t.Fatalf("%d vakken, wil 1 -- vier metingen binnen tien minuten horen in hetzelfde vak", len(day))
	}
	slot := day[0]
	if slot.Min != 20 || slot.Max != 30 {
		t.Fatalf("uitersten = %v..%v", slot.Min, slot.Max)
	}
	if got := slot.Average(); math.Abs(got-24) > 0.001 {
		t.Fatalf("gemiddelde = %v, wil 24", got)
	}
}

// Een teller draagt wat er bij kwam, niet wat hij aanwijst. Het gemiddelde van
// een kilowattuurstand is een getal zonder betekenis.
func TestCounterCarriesWhatWasAdded(t *testing.T) {
	series := NewSeries(Counter)
	series.Add(1000, start)
	series.Add(1004, start.Add(3*time.Minute))
	series.Add(1010, start.Add(6*time.Minute))
	day := series.Window(0)
	if len(day) != 1 || day[0].Delta() != 10 {
		t.Fatalf("verbruik = %v, wil 10", day[0].Delta())
	}
}

// Een teller die terugvalt -- apparaat vervangen, firmware opnieuw begonnen --
// mag geen negatief verbruik opleveren.
func TestCounterThatRestartsDoesNotGoNegative(t *testing.T) {
	series := NewSeries(Counter)
	series.Add(5000, start)
	series.Add(3, start.Add(time.Minute))
	if got := series.Window(0)[0].Delta(); got != 0 {
		t.Fatalf("na een herstart = %v, wil 0", got)
	}
}

// Een deur die drie uur openstaat raakt achttien vakken van tien minuten, niet
// één. Zonder dat zeggen alle andere vakken dat hij dicht was.
func TestOpenDoorFillsEveryTenMinutesItSpans(t *testing.T) {
	series := NewSeries(Fraction)
	series.Add(1, start)                  // open
	series.Add(0, start.Add(3*time.Hour)) // weer dicht
	day := series.Window(0)
	if len(day) != 18 {
		t.Fatalf("%d vakken van tien minuten, wil 18 voor drie uur", len(day))
	}
	for _, slot := range day {
		if got := slot.Average(); math.Abs(got-1) > 0.05 {
			t.Fatalf("vak op %v = %v, wil 1 (helemaal open)", slot.Start(0), got)
		}
	}
}

// Een halve periode open telt als een half vak.
// Een breuk is pas bekend als de tijd verantwoord is. Vijf minuten open zegt
// niets zolang de andere vijf nog niet meegeteld zijn -- daarom sluit deze test
// de periode af, net als de verzamelaar dat op zijn klok doet.
func TestHalfAnHourOpenCountsAsHalf(t *testing.T) {
	series := NewSeries(Fraction)
	series.Add(1, start)
	series.Add(0, start.Add(5*time.Minute))
	series.Close(start.Add(10 * time.Minute))
	slot := series.Window(0)[0]
	if got := slot.Average(); math.Abs(got-0.5) > 0.05 {
		t.Fatalf("vijf van de tien minuten open = %v, wil 0,5", got)
	}
}

// Een vak waarin niets gemeten is blijft leeg. "Niets gemeten" en "nul gemeten"
// zijn verschillende dingen, en dat verschil mag niet verdwijnen.
func TestNothingMeasuredIsNotZeroMeasured(t *testing.T) {
	series := NewSeries(Gauge)
	series.Add(20, start)
	series.Add(22, start.Add(3*time.Hour))
	day := series.Window(0)
	if len(day) != 2 {
		t.Fatalf("%d vakken, wil 2 -- de uren ertussen horen leeg te blijven", len(day))
	}
}

// De ring loopt rond en groeit niet. Dat is de hele reden dat dit in het
// geheugen kan.
func TestRingWrapsInsteadOfGrowing(t *testing.T) {
	series := NewSeries(Gauge)
	before := series.Bytes()
	at := start
	// Twee dagen aan metingen, elke tien minuten.
	for index := 0; index < 288; index++ {
		series.Add(float64(index), at)
		at = at.Add(10 * time.Minute)
	}
	if series.Bytes() != before {
		t.Fatalf("de reeks groeide van %d naar %d bytes", before, series.Bytes())
	}
	if got := len(series.Window(0)); got > Tiers[0].Slots {
		t.Fatalf("%d vakken op de dagschaal, meer dan de %d die er zijn", got, Tiers[0].Slots)
	}
}

// Wat een reeks kost, gemeten. Dit getal draagt het hele ontwerp: is het klein,
// dan is comprimeren een oplossing voor een probleem dat er niet is.
func TestSeriesCostsWhatWeThinkItCosts(t *testing.T) {
	series := NewSeries(Gauge)
	slots := 0
	for _, tier := range Tiers {
		slots += tier.Slots
	}
	t.Logf("  één vak:    %d bytes", slotSize)
	t.Logf("  vakken:     %d", slots)
	t.Logf("  één reeks:  %d bytes (%.1f KB)", series.Bytes(), float64(series.Bytes())/1024)
	t.Logf("  19 reeksen: %.0f KB   (dit huis vandaag)", float64(series.Bytes()*19)/1024)
	t.Logf("  100 reeksen:%.0f KB", float64(series.Bytes()*100)/1024)
	t.Logf("  1000 reeksen: %.1f MB", float64(series.Bytes()*1000)/1024/1024)
	// Een huis als dit heeft er straks honderd tot tweehonderd. Duizend is een
	// huis met tweehonderd apparaten -- die grens is er zodat dit ontwerp niet
	// stil duur wordt als iemand daar ooit komt.
	if series.Bytes() > 8*1024 {
		t.Fatalf("een reeks kost %d bytes; bij duizend reeksen is dat te veel", series.Bytes())
	}
}

// En het echte getal: wat het proces er werkelijk bij krijgt.
func TestRealMemoryForAHouseFullOfSeries(t *testing.T) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const count = 1000
	all := make([]*Series, 0, count)
	for index := 0; index < count; index++ {
		series := NewSeries(Gauge)
		at := start
		for step := 0; step < 300; step++ {
			series.Add(float64(step%40), at)
			at = at.Add(10 * time.Minute)
		}
		all = append(all, series)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	grown := float64(after.HeapAlloc-before.HeapAlloc) / 1024 / 1024
	t.Logf("  %d reeksen, elk twee dagen vol: %.2f MB werkelijk geheugen", count, grown)
	if grown > 10 {
		t.Fatalf("%d reeksen kosten %.1f MB; dat is meer dan een hub over heeft", count, grown)
	}
	runtime.KeepAlive(all)
}

// De verzamelaar kiest per capability de juiste samenvatting. Op naam, want de
// naam zegt wat een getal betekent -- een teller van 21,5 en een temperatuur van
// 21,5 zijn aan de waarde niet te onderscheiden.
func TestCapabilitiesGetTheRightKind(t *testing.T) {
	for capability, want := range map[string]Kind{
		"measure_temperature":  Gauge,
		"measure_power":        Gauge,
		"measure_power.solar":  Gauge,
		"dim":                  Gauge,
		"target_temperature":   Gauge,
		"meter_power":          Counter,
		"meter_power.imported": Counter,
		"alarm_contact":        Fraction,
		"alarm_motion":         Fraction,
		"onoff":                Fraction,
		"locked":               Fraction,
	} {
		got, ok := KindOf(capability)
		if !ok || got != want {
			t.Errorf("%s werd %v (bekend=%v), wil %v", capability, got, ok, want)
		}
	}
	// Wat geen getal is hoort niet bijgehouden te worden.
	for _, capability := range []string{"speaker_track", "windowcoverings_state", "grid_status"} {
		if _, ok := KindOf(capability); ok {
			t.Errorf("%s wordt bijgehouden terwijl het geen meting is", capability)
		}
	}
}

// Een aan/uit-stand komt als bool binnen en moet als één en nul tellen.
func TestBooleansCountAsOneAndZero(t *testing.T) {
	if value, ok := number(true); !ok || value != 1 {
		t.Fatalf("true werd %v", value)
	}
	if value, ok := number(false); !ok || value != 0 {
		t.Fatalf("false werd %v", value)
	}
	if _, ok := number("open"); ok {
		t.Fatal("een tekst werd als getal geteld")
	}
}
