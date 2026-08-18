// Package stats houdt bij wat het huis gedaan heeft.
//
// In het geheugen en nergens anders. Stroom eruit is statistiek weg, en dat is
// de afspraak: Stulp heeft geen database en krijgt er geen. Wat hier staat is
// begrensd, kost niets aan schijf, en is er meteen weer als het draait.
//
// Drie schalen boven elkaar, elk een ring die zichzelf overschrijft:
//
//	dag    144 vakken van 10 minuten
//	week    84 vakken van 2 uur
//	maand  144 vakken van 5 uur
//
// Een fijn vak dat volloopt wordt bij het grovere opgeteld. Zo kost een reeks
// altijd evenveel, hoe lang Stulp ook draait -- er is geen groei om later spijt
// van te krijgen.
package stats

import (
	"math"
	"sort"
	"sync"
	"time"
	"unsafe"
)

// Kind zegt hoe een reeks samengevat moet worden.
//
// Dat is niet één antwoord voor alles. Het gemiddelde van een temperatuur is
// zinvol; het gemiddelde van een kilowattuurteller is onzin, want die telt
// alleen maar op. En bij een deur wil je weten hoe lang hij openstond, niet wat
// "gemiddeld open" zou betekenen.
type Kind uint8

const (
	// Gauge is een meting die op en neer gaat: temperatuur, vermogen, vocht.
	Gauge Kind = iota
	// Counter is een teller die alleen oploopt: kilowattuur, bedrijfsuren. Het
	// vak draagt wat er in die periode bij kwam.
	Counter
	// Fraction is een schakelaar of alarm: het vak draagt welk deel van de tijd
	// hij aan stond.
	Fraction
)

// Tier is één schaal.
type Tier struct {
	Every time.Duration
	Slots int
}

// Tiers zijn de drie schalen. Samen 372 vakken per reeks.
var Tiers = []Tier{
	{Every: 10 * time.Minute, Slots: 144}, // een dag
	{Every: 2 * time.Hour, Slots: 84},     // een week
	{Every: 5 * time.Hour, Slots: 144},    // dertig dagen
}

// Slot is één vak. Twintig bytes, en dat is met opzet.
//
// Geen time.Time erin: die weegt vierentwintig bytes, meer dan alle metingen
// samen. Het volgnummer van het vak is genoeg -- de begintijd volgt eruit, en
// vier bytes dragen genoeg vakken van tien minuten voor tachtig jaar.
//
// float32 en geen float64: een temperatuur op zeven cijfers is preciezer dan
// welke sensor dan ook, en het scheelt de helft.
//
// En geen aparte velden voor het begin en einde van een teller. Een teller loopt
// alleen op, dus zijn laagste waarde in een vak ís de eerste en zijn hoogste de
// laatste. Twee velden die altijd hetzelfde zeggen als twee andere zijn twee
// velden te veel.
type Slot struct {
	period uint32
	Count  uint16
	Sum    float32
	// Min en Max zijn de uitersten van een meting. Bij een teller zijn het de
	// stand aan het begin en aan het eind van het vak.
	Min float32
	Max float32
}

// Empty zegt of er in dit vak niets gemeten is.
func (s Slot) Empty() bool { return s.Count == 0 }

// Start is wanneer dit vak begon.
func (s Slot) Start(tier int) time.Time {
	return time.Unix(int64(s.period)*int64(Tiers[tier].Every/time.Second), 0).UTC()
}

// Average is de gemiddelde meting in dit vak.
func (s Slot) Average() float64 {
	if s.Count == 0 {
		return 0
	}
	return float64(s.Sum) / float64(s.Count)
}

// Delta is wat er in dit vak bij kwam, voor een teller.
//
// Een teller die terugvalt -- een apparaat dat opnieuw begint, of vervangen is
// -- levert geen negatief verbruik op maar nul. Anders trekt één herstart een
// hele maand scheef, en dat is precies het soort getal waar iemand later naar
// zit te staren.
func (s Slot) Delta() float64 {
	if s.Count == 0 {
		return 0
	}
	return float64(s.Max - s.Min)
}

// Series is één meetwaarde van één apparaat, op alle schalen.
type Series struct {
	Kind Kind

	mu    sync.Mutex
	rings [][]Slot
	// heads wijst per schaal naar het vak dat nu gevuld wordt.
	heads []int
	// last is de laatste waarde en wanneer hij binnenkwam. Nodig voor Fraction:
	// een deur die om 9:00 opengaat en om 9:30 dicht, stond een half uur open,
	// en dat weet je pas bij het tweede bericht.
	last     float64
	lastAt   time.Time
	hasValue bool
}

// NewSeries maakt de ringen aan.
func NewSeries(kind Kind) *Series {
	series := &Series{Kind: kind, rings: make([][]Slot, len(Tiers)), heads: make([]int, len(Tiers))}
	for index, tier := range Tiers {
		series.rings[index] = make([]Slot, tier.Slots)
	}
	return series
}

// Add verwerkt een meting.
func (s *Series) Add(value float64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Kind == Fraction {
		// Het vorige stuk afsluiten met de waarde die tot nu toe gold, en pas
		// daarna de nieuwe onthouden.
		if s.hasValue {
			s.spread(s.last, s.lastAt, at)
		}
		s.last, s.lastAt, s.hasValue = value, at, true
		return
	}
	for index := range Tiers {
		s.record(index, value, at)
	}
	s.last, s.lastAt, s.hasValue = value, at, true
}

// record legt een waarde in het vak waar hij hoort.
func (s *Series) record(tier int, value float64, at time.Time) {
	slot := s.slotFor(tier, at)
	if slot == nil {
		return
	}
	number := float32(value)
	// Een teller die terugvalt is opnieuw begonnen: apparaat vervangen, firmware
	// die zijn tellers wist. Wat er vóór die val bij kwam is niet meer te
	// achterhalen, dus het vak begint hier opnieuw. Doen alsof de val een
	// verbruik was zou het verschil tussen de oude hoge stand en de nieuwe lage
	// als verbruik opschrijven, en dat is een getal waar iemand later naar zit
	// te staren zonder te begrijpen waar het vandaan komt.
	if s.Kind == Counter && slot.Count > 0 && number < slot.Max {
		slot.Min, slot.Max, slot.Sum, slot.Count = number, number, number, 1
		return
	}
	if slot.Count == 0 {
		slot.Min, slot.Max = number, number
	} else {
		if number < slot.Min {
			slot.Min = number
		}
		if number > slot.Max {
			slot.Max = number
		}
	}
	slot.Sum += number
	if slot.Count < math.MaxUint16 {
		slot.Count++
	}
}

// slotFor levert het vak voor dit moment, en schuift de ring op als de tijd
// verder is. Een waarde die ouder is dan het huidige vak hoort nergens meer:
// die zou een vak vullen dat al voorbij is.
func (s *Series) slotFor(tier int, at time.Time) *Slot {
	every := Tiers[tier].Every
	period := uint32(at.Unix() / int64(every/time.Second))
	head := &s.rings[tier][s.heads[tier]]
	switch {
	case head.Count > 0 && head.period == period:
		return head
	case head.Count > 0 && head.period > period:
		return nil
	case head.Count == 0 && head.period == period:
		return head
	}
	// Vooruit. Vakken die overgeslagen worden blijven leeg -- dat is het
	// verschil tussen "niets gemeten" en "nul gemeten", en dat verschil hoort
	// zichtbaar te blijven.
	s.heads[tier] = (s.heads[tier] + 1) % len(s.rings[tier])
	next := &s.rings[tier][s.heads[tier]]
	*next = Slot{period: period}
	return next
}

// spread verdeelt een stand over de vakken waar hij overheen liep.
//
// Zonder dit zou een deur die drie uur openstaat maar één vak raken, en zeggen
// alle andere vakken dat hij dicht was.
func (s *Series) spread(value float64, from, until time.Time) {
	if !until.After(from) {
		return
	}
	for tier := range Tiers {
		every := Tiers[tier].Every
		for cursor := from.Truncate(every); cursor.Before(until); cursor = cursor.Add(every) {
			slot := s.slotFor(tier, cursor)
			if slot == nil {
				continue
			}
			// Hoeveel van dit vak viel binnen de periode.
			begin, end := maxTime(cursor, from), minTime(cursor.Add(every), until)
			part := float32(end.Sub(begin).Seconds() / every.Seconds())
			// Count telt hier honderdsten van een vak in plaats van berichten,
			// en Sum telt op dezelfde schaal mee. Zo levert Average het deel
			// van de tijd dat de stand gold: een deur die de helft van een vak
			// openstond geeft 0,5, en niet 1 omdat er één bericht was.
			hundredths := float32(math.Round(float64(part) * 100))
			slot.Sum += float32(value) * hundredths
			if slot.Count == 0 {
				slot.Min, slot.Max = float32(value), float32(value)
			} else {
				if float32(value) < slot.Min {
					slot.Min = float32(value)
				}
				if float32(value) > slot.Max {
					slot.Max = float32(value)
				}
			}
			slot.Count += uint16(hundredths)
		}
	}
}

// Window levert de vakken van één schaal, oud naar nieuw, zonder de lege.
func (s *Series) Window(tier int) []Slot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tier < 0 || tier >= len(s.rings) {
		return nil
	}
	out := make([]Slot, 0, len(s.rings[tier]))
	for _, slot := range s.rings[tier] {
		if !slot.Empty() {
			out = append(out, slot)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].period < out[b].period })
	return out
}

// Close sluit een lopende stand af, zodat wat er nu geldt ook meetelt.
//
// Nodig voor Fraction: zonder dit telt de tijd sinds het laatste bericht niet
// mee, en dat kan uren zijn -- juist bij een deur die dicht blijft.
func (s *Series) Close(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Kind != Fraction || !s.hasValue {
		return
	}
	s.spread(s.last, s.lastAt, at)
	s.lastAt = at
}

// Bytes is wat deze reeks aan geheugen kost. Gemeten en niet geschat: dit is het
// getal waar de vraag "kan dit uit?" op moet rusten.
func (s *Series) Bytes() int {
	total := 0
	for _, ring := range s.rings {
		total += len(ring) * int(slotSize)
	}
	return total
}

// slotSize is wat één vak weegt.
var slotSize = unsafe.Sizeof(Slot{})

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
