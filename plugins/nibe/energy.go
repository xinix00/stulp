package main

import "time"

// De verdeling van het vermogen over verwarmen en warm water.
//
// De pomp meet één keer stroom (22130) en houdt één meterstand bij (28393). Een
// uitsplitsing per doel geeft de API niet -- de vlakke tellers per categorie
// (25137/25138) ververst hij maar eens per twintig minuten en die zijn te grof.
// Wat de pomp wél elke minuut vertelt is waar hij op dat moment mee bezig is
// (14950), en dat is genoeg.
//
// Warm water is prioriteit 3; verwarmen is met opzet al het andere, ook Uit. Dat
// laatste is geen slordigheid: bij een afvoerluchtpomp lopen de ventilator, de
// circulatiepompen en de elektronica dag en nacht door, en die grondlast hoort
// ergens. Doordat de twee elkaars complement zijn tellen ze altijd op tot het
// geheel en valt er niets buiten de boot.
//
// De meterstanden zelf komen niet uit de integratie maar uit 28393: elke keer
// dat die teller oploopt wordt de toename verdeeld naar rato van het vermogen
// dat sinds de vorige keer aan elke categorie toegerekend werd. Zo blijft het
// totaal precies de stand die de pomp zelf noemt, inclusief elektrische
// bijverwarming die in het momentane vermogen niet volledig zichtbaar is.

// hotwaterPriority is de enige stand van 14950 die als warm water telt.
// De nummering is 0=uit, 1=verwarmen, 2=koelen, 3=warm water, 4=zwembad,
// 5=zwembad 2, 6=voorverwarmen.
const hotwaterPriority = 3

// maxAnchorDelta begrenst wat er in één keer verdeeld wordt.
//
// Een echte toename over vijf minuten blijft ver onder een kWh. Wat daarboven
// komt is een teller die teruggezet of omgeslagen is, of een pomp die uren uit
// heeft gestaan -- dat als verbruik van deze middag boeken zou een piek in de
// grafiek zetten die er nooit geweest is.
const maxAnchorDelta = 5.0

// maxIntegrationGap begrenst hoe lang één meting mag meetellen. Na een herstart
// of een storing is de laatste meting oud, en die uren toeschrijven aan de
// prioriteit van dat ene moment maakt de verdeling onbruikbaar.
const maxIntegrationGap = 30 * time.Minute

// split houdt de verdeling bij van één pomp.
type split struct {
	// heatingWh en hotwaterWh zijn het geïntegreerde vermogen sinds de laatste
	// ijking. Alleen hun verhouding telt; de absolute waarde is een tussenstand.
	heatingWh, hotwaterWh float64
	measured              time.Time

	// lastPriority is waar de pomp het laatst mee bezig was. Nodig als het
	// venster geen enkele meting opleverde en er toch verbruik bijkwam.
	lastPriority int
	knowPriority bool

	lastTotal float64
	knowTotal bool

	heatingKwh, hotwaterKwh float64
}

// power verwerkt één meting en levert wat de twee vermogenscapabilities moeten
// tonen: het hele vermogen bij de categorie die de pomp op dit moment bedient,
// en nul bij de andere.
func (s *split) power(now time.Time, watt float64, priority int) (heating, hotwater float64) {
	if priority == hotwaterPriority {
		hotwater = watt
	} else {
		heating = watt
	}
	if !s.measured.IsZero() {
		if gap := now.Sub(s.measured); gap > 0 && gap <= maxIntegrationGap {
			hours := gap.Hours()
			s.heatingWh += heating * hours
			s.hotwaterWh += hotwater * hours
		}
	}
	s.measured = now
	s.lastPriority, s.knowPriority = priority, true
	return heating, hotwater
}

// anchor verdeelt de toename van de echte meterstand over de twee categorieën
// en begint een nieuw venster. Hij levert false als er niets te verdelen viel.
func (s *split) anchor(total float64) bool {
	defer func() { s.heatingWh, s.hotwaterWh = 0, 0 }()

	previous, known := s.lastTotal, s.knowTotal
	s.lastTotal, s.knowTotal = total, true
	if !known {
		return false
	}
	delta := total - previous
	if delta <= 0 || delta > maxAnchorDelta {
		return false
	}

	window := s.heatingWh + s.hotwaterWh
	if window > 0 {
		s.heatingKwh += delta * s.heatingWh / window
		s.hotwaterKwh += delta * s.hotwaterWh / window
		return true
	}
	// Geen vermogen gemeten in dit venster, maar de meter liep wel op -- dat is
	// bijverwarming die 22130 niet meldt. De laatst bekende prioriteit is dan
	// het enige wat we hebben. Is die er ook niet, dan blijft de toename bij de
	// twee categorieën weg; het totaal klopt sowieso, want dat komt rechtstreeks
	// van de pomp.
	if !s.knowPriority {
		return false
	}
	if s.lastPriority == hotwaterPriority {
		s.hotwaterKwh += delta
	} else {
		s.heatingKwh += delta
	}
	return true
}

// restore zet de standen terug die van vóór een herstart bewaard waren. Zonder
// dit begint elke meter na een update weer bij nul.
func (s *split) restore(heatingKwh, hotwaterKwh, lastTotal float64, knowTotal bool) {
	s.heatingKwh, s.hotwaterKwh = heatingKwh, hotwaterKwh
	s.lastTotal, s.knowTotal = lastTotal, knowTotal
}
