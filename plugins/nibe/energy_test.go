package main

import (
	"math"
	"testing"
	"time"
)

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// Het vermogen gaat naar de categorie die de pomp op dat moment bedient, en de
// andere staat op nul. Zonder die nul zou een grafiek suggereren dat er twee
// dingen tegelijk verbruiken terwijl er één meter is.
func TestPowerGoesToWhateverThePumpIsDoing(t *testing.T) {
	s := &split{}
	heating, hotwater := s.power(time.Now(), 900, hotwaterPriority)
	if heating != 0 || hotwater != 900 {
		t.Fatalf("warm water gaf %v/%v", heating, hotwater)
	}
	heating, hotwater = s.power(time.Now(), 900, 1)
	if heating != 900 || hotwater != 0 {
		t.Fatalf("verwarmen gaf %v/%v", heating, hotwater)
	}
	// Uit is geen categorie op zichzelf: de ventilator en de pompen lopen dan
	// door, en die grondlast hoort bij verwarmen in plaats van nergens.
	heating, hotwater = s.power(time.Now(), 60, 0)
	if heating != 60 || hotwater != 0 {
		t.Fatalf("stand-by gaf %v/%v", heating, hotwater)
	}
}

// De meterstand van de pomp wordt verdeeld naar rato van het vermogen dat er in
// dat venster naar elke categorie ging. De som blijft precies de toename die de
// pomp zelf meldt -- daar hangt het aan.
func TestTheMeterIsSplitByTheMeasuredShare(t *testing.T) {
	s := &split{}
	start := time.Now()
	s.anchor(1000)

	// Een half uur warm water en een half uur verwarmen, met hetzelfde vermogen:
	// dat is fiftyfifty.
	s.power(start, 1000, hotwaterPriority)
	s.power(start.Add(20*time.Minute), 1000, hotwaterPriority)
	s.power(start.Add(20*time.Minute), 1000, 1)
	s.power(start.Add(40*time.Minute), 1000, 1)

	if !s.anchor(1002) {
		t.Fatal("er viel niets te verdelen")
	}
	if !closeTo(s.heatingKwh, 1) || !closeTo(s.hotwaterKwh, 1) {
		t.Fatalf("verdeling = %v verwarmen, %v warm water", s.heatingKwh, s.hotwaterKwh)
	}
	if !closeTo(s.heatingKwh+s.hotwaterKwh, 2) {
		t.Fatalf("de twee tellen niet op tot de toename: %v", s.heatingKwh+s.hotwaterKwh)
	}
}

// Een teller die terugspringt of een gat van uren is geen verbruik van vandaag.
// Die toename toch boeken zet een piek in de grafiek die er nooit geweest is.
func TestAResetOrAGapIsIgnoredAndRebaselined(t *testing.T) {
	s := &split{}
	s.anchor(1000)
	s.power(time.Now(), 1000, 1)

	if s.anchor(3) {
		t.Fatal("een teruggezette teller werd verdeeld")
	}
	if s.heatingKwh != 0 || s.hotwaterKwh != 0 {
		t.Fatalf("standen = %v/%v", s.heatingKwh, s.hotwaterKwh)
	}
	// Wel opnieuw geijkt: vanaf hier telt hij gewoon door.
	s.power(time.Now(), 1000, 1)
	if !s.anchor(4) {
		t.Fatal("na het opnieuw ijken viel er niets te verdelen")
	}
	if !closeTo(s.heatingKwh, 1) {
		t.Fatalf("verwarmen = %v", s.heatingKwh)
	}

	if s.anchor(4 + maxAnchorDelta + 1) {
		t.Fatal("een sprong van meer dan een venster werd verdeeld")
	}
}

// Een meting van een half uur geleden zegt niets over dit venster. Die uren toch
// meetellen laat de verdeling hangen op de prioriteit van één oud moment.
func TestAnOldMeasurementDoesNotWeighTheWindow(t *testing.T) {
	s := &split{}
	start := time.Now()
	s.anchor(1000)
	s.power(start, 1000, hotwaterPriority)
	s.power(start.Add(maxIntegrationGap+time.Minute), 1000, hotwaterPriority)
	s.power(start.Add(maxIntegrationGap+2*time.Minute), 1000, 1)

	s.anchor(1001)
	if !closeTo(s.heatingKwh, 1) || s.hotwaterKwh != 0 {
		t.Fatalf("verdeling = %v verwarmen, %v warm water", s.heatingKwh, s.hotwaterKwh)
	}
}

// De meter loopt op zonder dat er vermogen gemeten is: dat is elektrische
// bijverwarming, die 22130 niet meldt. De laatst bekende prioriteit is dan het
// enige wat er is, en dat is beter dan de toename laten vallen.
func TestConsumptionWithoutMeasuredPowerFollowsTheLastPriority(t *testing.T) {
	s := &split{}
	s.anchor(1000)
	s.power(time.Now(), 0, hotwaterPriority)

	if !s.anchor(1001) {
		t.Fatal("er viel niets te verdelen")
	}
	if !closeTo(s.hotwaterKwh, 1) || s.heatingKwh != 0 {
		t.Fatalf("verdeling = %v verwarmen, %v warm water", s.heatingKwh, s.hotwaterKwh)
	}
}

// En weten we ook de prioriteit niet, dan wordt er niets geraden. Het totaal
// klopt sowieso: dat komt rechtstreeks van de pomp.
func TestWithoutAnyKnowledgeNothingIsGuessed(t *testing.T) {
	s := &split{}
	s.anchor(1000)
	if s.anchor(1001) {
		t.Fatal("er werd geraden")
	}
	if s.heatingKwh != 0 || s.hotwaterKwh != 0 {
		t.Fatalf("standen = %v/%v", s.heatingKwh, s.hotwaterKwh)
	}
}

// Na een herstart telt de meter door waar hij gebleven was. Zonder dit begint
// elke grafiek na een update weer bij nul terwijl de pomp gewoon doorstookte.
func TestTheMetersSurviveARestart(t *testing.T) {
	s := &split{}
	s.restore(12.5, 3.25, 1000, true)
	s.power(time.Now(), 500, 1)
	if !s.anchor(1001) {
		t.Fatal("er viel niets te verdelen")
	}
	if !closeTo(s.heatingKwh, 13.5) || !closeTo(s.hotwaterKwh, 3.25) {
		t.Fatalf("standen = %v/%v", s.heatingKwh, s.hotwaterKwh)
	}

	// Een apparaat dat nog nooit geijkt is heeft geen vorige stand, en de eerste
	// meting is dan het nulpunt en geen toename van duizend kWh.
	fresh := &split{}
	fresh.restore(0, 0, 0, false)
	fresh.power(time.Now(), 500, 1)
	if fresh.anchor(1000) {
		t.Fatal("de eerste meting werd als verbruik geboekt")
	}
}
