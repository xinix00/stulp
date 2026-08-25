// Package sigen is de registerkaart van een Sigenergy-systeem: welk adres welke
// betekenis heeft, hoe groot het is en door welke gain de ruwe waarde moet.
//
// Dit is het waardevolste deel van de app. De adressen en schalen komen
// regel voor regel uit lib/modbus/registry/ van de Homey-app; wat daar niet
// stond staat hier niet. Zie PORTED.md voor wat er is blijven liggen.
//
// Het pakket praat zelf geen Modbus. Het beschrijft wat er te lezen valt en
// vertaalt registers naar getallen; het lezen zelf gaat via de Reader die de
// aanroeper meegeeft.
package sigen

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
)

// class zegt hoe vaak een register gelezen wordt en op welke unit.
type class uint8

const (
	// info verandert niet: serienummer, aantal MPPT's, capaciteit. Eén keer
	// lezen per verbinding is genoeg.
	info class = iota
	// reading is de stand van nu, op de unit van het apparaat zelf.
	reading
	// system staat in de registerruimte van het systeem als geheel en wordt op
	// unit 247 gelezen, ook als het apparaat zelf een andere unit heeft. De
	// batterij haalt zijn vermogen daar vandaan, want de batterij-unit biedt het
	// niet aan.
	system
)

// SystemUnit is de unit waarop de systeemregisters staan. Uit lib/base.js.
const SystemUnit uint8 = 247

// kind is hoe een register gelezen wordt.
type kind uint8

const (
	kindUint16 kind = iota
	kindInt16
	kindUint32
	kindInt32
	kindUint64
	kindString
)

// space is in welke Modbus-registerruimte een veld staat. Het getal van het
// adres bepaalt dat niet voor de client: functiecode 0x04 en 0x03 kunnen allebei
// adres 0 vragen. Sigenergy zet metingen en andere read-only velden in input
// registers en zijn instelbare velden in holding registers.
type space uint8

const (
	inputRegisters space = iota
	holdingRegisters
)

// Reg is één register uit de kaart.
type Reg struct {
	// Addr is het adres zoals het de deur uit gaat. Sigenergy gebruikt de
	// 30000-reeks voor input-registers en de 40000-reeks voor holding-registers;
	// die getallen worden onveranderd verstuurd.
	Addr uint16
	// Count is hoeveel registers van 16 bits dit veld beslaat.
	Count uint16
	// What is waar het over gaat, in de eenheid die eruit komt. Staat in
	// foutmeldingen en in de logregel over een register dat ontbreekt.
	What string

	kind  kind
	class class
	space space
	// gain is waar de ruwe waarde door gedeeld wordt. Sigenergy noemt dat zo in
	// zijn eigen protocolbeschrijving, en de bron neemt die naam over.
	gain float64
}

// De bouwers hieronder laten de kaart lezen zoals de bron hem opschrijft:
// klasse, adres, gain, betekenis.

func u16(c class, addr uint16, gain float64, what string) Reg {
	return Reg{Addr: addr, Count: 1, What: what, kind: kindUint16, class: c, space: inputRegisters, gain: gain}
}

func i16(c class, addr uint16, gain float64, what string) Reg {
	return Reg{Addr: addr, Count: 1, What: what, kind: kindInt16, class: c, space: inputRegisters, gain: gain}
}

func u32(c class, addr uint16, gain float64, what string) Reg {
	return Reg{Addr: addr, Count: 2, What: what, kind: kindUint32, class: c, space: inputRegisters, gain: gain}
}

func i32(c class, addr uint16, gain float64, what string) Reg {
	return Reg{Addr: addr, Count: 2, What: what, kind: kindInt32, class: c, space: inputRegisters, gain: gain}
}

func u64(c class, addr uint16, gain float64, what string) Reg {
	return Reg{Addr: addr, Count: 4, What: what, kind: kindUint64, class: c, space: inputRegisters, gain: gain}
}

func text(c class, addr, count uint16, what string) Reg {
	return Reg{Addr: addr, Count: count, What: what, kind: kindString, class: c, space: inputRegisters, gain: 1}
}

// holdingU16 beschrijft een leesbaar holding register. De gewone bouwers
// hierboven zijn bewust input-registers: het overgrote deel van de kaart is
// read-only en hoort volgens het Sigenergy-protocol met 0x04 gelezen te worden.
func holdingU16(c class, addr uint16, gain float64, what string) Reg {
	return Reg{Addr: addr, Count: 1, What: what, kind: kindUint16, class: c, space: holdingRegisters, gain: gain}
}

// Number vertaalt de ruwe registers naar de waarde in de eenheid van What.
//
// De woordvolgorde is die van de bron: het hoogste register staat vooraan. De
// Homey-app leest de rauwe registerbytes met readUInt32BE en readBigUInt64BE, en
// dat is precies dit. Een omgekeerde volgorde zou hier een vermogen van
// honderden megawatt opleveren, dus dit is niet iets om te gokken.
func (r Reg) Number(words []uint16) (float64, bool) {
	if len(words) < int(r.Count) {
		return 0, false
	}
	var raw float64
	switch r.kind {
	case kindUint16:
		raw = float64(words[0])
	case kindInt16:
		raw = float64(int16(words[0]))
	case kindUint32:
		raw = float64(uint32(words[0])<<16 | uint32(words[1]))
	case kindInt32:
		raw = float64(int32(uint32(words[0])<<16 | uint32(words[1])))
	case kindUint64:
		combined := uint64(words[0])<<48 | uint64(words[1])<<32 | uint64(words[2])<<16 | uint64(words[3])
		raw = float64(combined)
	default:
		return 0, false
	}
	if r.gain == 0 {
		return raw, true
	}
	return raw / r.gain, true
}

// Text vertaalt de registers naar tekst. Sigenergy vult de rest van het veld met
// nulbytes; die horen niet in een serienummer.
func (r Reg) Text(words []uint16) (string, bool) {
	if r.kind != kindString || len(words) < int(r.Count) {
		return "", false
	}
	raw := make([]byte, 0, len(words)*2)
	for _, word := range words[:r.Count] {
		raw = append(raw, byte(word>>8), byte(word))
	}
	// Firmware van een ander merk stuurt hier wel eens rommel; een serienummer
	// dat geen tekst is hoort geen halve tekens op te leveren.
	if !utf8.Valid(raw) {
		return "", false
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", "")), true
}

// Set is wat er in één ronde gelezen wordt.
type Set []Reg

// pick levert de registers van één klasse, in de volgorde van de kaart.
func pick(all Set, want class) Set {
	out := make(Set, 0, len(all))
	for _, reg := range all {
		if reg.class == want {
			out = append(out, reg)
		}
	}
	return out
}

// Reader is wat dit pakket van een Modbus-verbinding nodig heeft.
type Reader interface {
	ReadInput(unit uint8, start, count uint16) ([]uint16, error)
	ReadHolding(unit uint8, start, count uint16) ([]uint16, error)
}

// Read haalt precies dit register uit zijn eigen registerruimte. Ook probes
// lopen hierdoor, zodat pairing niet per ongeluk een read-only register met
// functiecode 0x03 probeert te herkennen.
func (r Reg) Read(reader Reader, unit uint8) ([]uint16, error) {
	return readRegisters(reader, r.space, unit, r.Addr, r.Count)
}

func readRegisters(reader Reader, space space, unit uint8, start, count uint16) ([]uint16, error) {
	if space == holdingRegisters {
		return reader.ReadHolding(unit, start, count)
	}
	return reader.ReadInput(unit, start, count)
}

// refused zegt of het apparaat de vraag weigerde in plaats van dat de lijn het
// begaf. Dat verschil bepaalt of de rest van de ronde nog zin heeft: een
// geweigerd adres is firmware die dit veld niet kent, een gevallen verbinding is
// het einde van de ronde.
func refused(err error) bool {
	var exception modbus.Exception
	return errors.As(err, &exception)
}

// Reading is wat één ronde opleverde.
type Reading struct {
	words   map[uint16][]uint16
	missing Set
}

// Number levert de waarde van een register, of false als dit apparaat het niet
// aanbood.
func (r Reading) Number(reg Reg) (float64, bool) {
	words, ok := r.words[reg.Addr]
	if !ok {
		return 0, false
	}
	return reg.Number(words)
}

// Text levert een tekstregister.
func (r Reading) Text(reg Reg) (string, bool) {
	words, ok := r.words[reg.Addr]
	if !ok {
		return "", false
	}
	return reg.Text(words)
}

// Merge legt de uitkomst van een tweede ronde erbij. Een apparaat leest zijn
// eigen registers en die van het systeem apart -- andere unit -- en de driver
// die de tegels vult hoort dat verschil niet te hoeven kennen.
func (r Reading) Merge(other Reading) Reading {
	if r.words == nil {
		r.words = map[uint16][]uint16{}
	}
	for addr, words := range other.words {
		r.words[addr] = words
	}
	r.missing = append(r.missing, other.missing...)
	return r
}

// Missing zijn de registers die dit apparaat weigerde. Dat is bijna altijd
// firmware die ouder is dan de kaart, en het hoort één keer in de log te staan
// in plaats van een tegel stil leeg te laten.
func (r Reading) Missing() Set { return r.missing }

// Poller leest één registerset van één unit.
//
// Hij onthoudt tussen rondes door welke bereiken het apparaat niet in één keer
// wil leveren, zodat die niet elke ronde opnieuw geprobeerd worden.
type Poller struct {
	unit uint8
	runs []Run
	solo map[uint16]bool
}

// NewPoller bouwt de leesronde: de registers worden gegroepeerd tot bereiken die
// in één vraag passen.
func NewPoller(unit uint8, set Set) *Poller {
	return &Poller{unit: unit, runs: group(set), solo: map[uint16]bool{}}
}

// Unit is de unit waarop deze poller leest.
func (p *Poller) Unit() uint8 { return p.unit }

// Read haalt de hele set op.
//
// Een weigering op één register laat de rest staan: dat is meestal firmware die
// de kaart nog niet kent. Een kapotte verbinding stopt de ronde wel, want dan
// klopt niets van wat er nog zou komen.
func (p *Poller) Read(reader Reader) (Reading, error) {
	result := Reading{words: map[uint16][]uint16{}}
	for _, run := range p.runs {
		if len(run.Regs) == 1 || p.solo[run.Start] {
			if err := p.readOneByOne(reader, run, &result); err != nil {
				return Reading{}, err
			}
			continue
		}
		words, err := readRegisters(reader, run.space, p.unit, run.Start, run.Count)
		if err != nil {
			if !refused(err) {
				return Reading{}, err
			}
			// Het apparaat wil dit bereik niet als geheel -- er zit een adres in
			// dat het niet kent. Vanaf nu per register, anders blijft één gat de
			// hele groep leeghouden.
			p.solo[run.Start] = true
			if err := p.readOneByOne(reader, run, &result); err != nil {
				return Reading{}, err
			}
			continue
		}
		for _, reg := range run.Regs {
			offset := reg.Addr - run.Start
			result.words[reg.Addr] = words[offset : offset+reg.Count]
		}
	}
	return result, nil
}

func (p *Poller) readOneByOne(reader Reader, run Run, into *Reading) error {
	for _, reg := range run.Regs {
		words, err := reg.Read(reader, p.unit)
		if err != nil {
			if !refused(err) {
				return err
			}
			into.missing = append(into.missing, reg)
			continue
		}
		into.words[reg.Addr] = words
	}
	return nil
}

// Run is een aaneengesloten bereik dat in één vraag past.
type Run struct {
	Start uint16
	Count uint16
	Regs  Set
	space space
}

const (
	// maxGap is hoeveel ongebruikte registers er overbrugd mogen worden om twee
	// velden in één vraag te krijgen. Een paar gereserveerde adressen meelezen is
	// goedkoper dan een tweede rondje over het netwerk.
	maxGap = 8
	// maxRun is hoe lang zo'n bereik mag worden. Modbus zelf staat 125 registers
	// per leesactie toe; de bron houdt 120 aan en dat is hier overgenomen.
	maxRun = 120
)

// group sorteert de registers op adres en plakt ze aan elkaar tot bereiken.
func group(set Set) []Run {
	sorted := make(Set, len(set))
	copy(sorted, set)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Addr < sorted[b].Addr })

	var runs []Run
	for _, reg := range sorted {
		end := reg.Addr + reg.Count
		if len(runs) > 0 {
			current := &runs[len(runs)-1]
			currentEnd := current.Start + current.Count
			if current.space == reg.space && reg.Addr >= currentEnd && reg.Addr-currentEnd <= maxGap && end-current.Start <= maxRun {
				current.Regs = append(current.Regs, reg)
				if end > currentEnd {
					current.Count = end - current.Start
				}
				continue
			}
		}
		runs = append(runs, Run{Start: reg.Addr, Count: reg.Count, Regs: Set{reg}, space: reg.space})
	}
	return runs
}

// String maakt een register herkenbaar in een melding.
func (r Reg) String() string { return fmt.Sprintf("%s (%d)", r.What, r.Addr) }
