package main

import (
	"fmt"
	"math"
	"strconv"

	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// Welke parameterId waar over gaat.
//
// De cloud stuurt per pomp ruim tachtig waarden en noemt ze alleen bij nummer.
// Deze tabel is dus de hele vertaalslag, en tegelijk de lijst van wat deze app
// gebruikt: wat er niet in staat komt binnen en gaat weer weg. Welke nummers dat
// zijn en wat ze betekenen staat in PORTED.md.
//
// De waarden zijn al geschaald door de cloud -- 18.2 is 18,2 °C -- dus er wordt
// hier niets omgerekend. Alleen bij schrijven telt een grens, en die staat bij
// het punt zelf.

// point is één parameter en de capability waar hij naartoe gaat.
type point struct {
	capability string
	// kind bepaalt wat er van het getal gemaakt wordt: een getal blijft een
	// getal, een enum wordt de tekst die de capability als id gebruikt, en een
	// bool is de aan/uit-variant daarvan.
	kind kind
	// min en max begrenzen wat er geschreven mag worden, in echte eenheden.
	// Alleen zinvol bij een schrijfbaar punt.
	min, max float64
	writable bool
}

type kind int

const (
	number kind = iota
	boolean
	enum
)

// Nibe nummert zijn parameters per serie, en de twee series die myUplink bedient
// nummeren volstrekt anders: de S-serie telt vanaf 4, de F-serie gebruikt de
// klassieke 4xxxx-nummering. Er is geen enkel nummer dat in beide voorkomt, dus
// lezen kan uit één tabel -- wat een pomp niet heeft, stuurt hij ook niet mee.
//
// Schrijven kan dat niet: target_temperature bestaat in beide series onder een
// ander nummer. Daarom wordt de schrijftabel per pomp gemaakt, uit de nummers
// die die pomp werkelijk meldt. Zie writeTable.

// sPoints is de S-serie (S1155, S1255, S2125 en verwanten).
var sPoints = map[string]point{
	"4":     {capability: "measure_temperature.outdoor"},
	"8":     {capability: "measure_temperature.supply"},
	"10":    {capability: "measure_temperature.return"},
	"1708":  {capability: "measure_temperature.calculated_supply"},
	"32628": {capability: "measure_temperature.hotwater"},
	"48351": {capability: "measure_temperature"},
	"29972": {capability: "hotwater_amount"},
	"55000": {capability: "operating_priority", kind: enum},
	"1756":  {capability: "additional_heat_power"},
	"5927":  {capability: "compressor_frequency"},
	"1975":  {capability: "pump_speed"},
	"26945": {capability: "airflow"},
	"26411": {capability: "add_heat_time_heating"},
	"1865":  {capability: "add_heat_time_hotwater"},

	// De schrijfbare punten worden ook gelezen: een tegel hoort te tonen wat de
	// pomp werkelijk doet, ook als iemand het aan het paneel zelf omzette.
	"47751": {capability: "target_temperature", writable: true, min: 5, max: 35},
	"7086":  {capability: "hot_water_boost", kind: boolean, writable: true},
	"8121":  {capability: "ventilation_boost", kind: boolean, writable: true},
	"3830":  {capability: "ventilation_mode", kind: enum, writable: true, min: 0, max: 4},
}

// fPoints is de F-serie (F1155, F1255, F750 en verwanten), afgelezen van een
// echte F1255-6 R PC op 2026-08-09. Wat daar niet in de lijst van 113 stond
// staat hier ook niet: die pomp is grondgebonden, dus er is geen ventilatie en
// geen luchtdebiet, en -- belangrijker -- er is geen totaalvermogen en geen
// kWh-meter. PORTED.md zegt wat dat betekent voor de energie-tegels.
var fPoints = map[string]point{
	"40004": {capability: "measure_temperature.outdoor"},
	"40008": {capability: "measure_temperature.supply"},
	"40012": {capability: "measure_temperature.return"},
	"43009": {capability: "measure_temperature.calculated_supply"},
	"40013": {capability: "measure_temperature.hotwater"},
	"40033": {capability: "measure_temperature"},
	"50345": {capability: "hotwater_amount"},
	"49994": {capability: "operating_priority", kind: enum},
	"43084": {capability: "additional_heat_power"},
	"41778": {capability: "compressor_frequency"},
	"43437": {capability: "pump_speed"},

	// 43239 is de bijverwarmingstijd voor warm water. De tegenhanger voor
	// verwarmen bestaat niet: 43081 is het totaal en niet het deel, en een
	// totaal onder een tegel hangen die "verwarming" heet is een leugen die
	// niemand meer terugvindt.
	"43239": {capability: "add_heat_time_hotwater"},

	"47398": {capability: "target_temperature", writable: true, min: 5, max: 35},

	// Niet 48132: dat is de duur-enum (uit, eenmalig, 3, 6, 12 uur) en hoort bij
	// de Flow-kaart hieronder. 50004 is de aan/uit-schakelaar die bij deze
	// tegel past.
	"50004": {capability: "hot_water_boost", kind: boolean, writable: true},
}

// readable is de twee series bij elkaar. pollAll leest hieruit; wat een pomp
// niet meldt, komt niet binnen.
var readable = mergePoints(sPoints, fPoints)

func mergePoints(tables ...map[string]point) map[string]point {
	out := map[string]point{}
	for _, table := range tables {
		for parameter, entry := range table {
			if _, clash := out[parameter]; clash {
				// De hele opzet leunt erop dat de nummers niet botsen. Botst er
				// ooit toch een, dan hoort dat hier te knallen en niet stilletjes
				// de verkeerde serie te winnen.
				panic("nibe: parameter " + parameter + " staat in twee series")
			}
			out[parameter] = entry
		}
	}
	return out
}

// energyPoints zijn de nummers waar de vermogensverdeling op draait, per serie.
// De F-serie heeft ze niet: die pomp meldt geen opgenomen vermogen en geen
// meterstand, alleen drie fasestromen in ampère.
type energyPoints struct {
	power    string // momentaan opgenomen vermogen van de hele pomp (W)
	priority string // waar de pomp nu mee bezig is
	meter    string // cumulatieve meterstand (kWh)
}

// sEnergy hoort bij de S-serie. Let op: priority is een andere nummering dan de
// 55000 hierboven, die voor de tegel bedoeld is.
var sEnergy = energyPoints{power: "22130", priority: "14950", meter: "28393"}

// boost is de kaart "extra warm water", per serie.
//
// De kaart biedt uren aan en 2 betekent "eenmalig". Wat de pomp daarvan wil zien
// verschilt: de S-serie neemt het aantal uur zelf aan, de F-serie een code.
type boost struct {
	parameter string
	raw       map[int]int
}

var sBoost = boost{parameter: "4564", raw: map[int]int{0: 0, 2: 2, 3: 3, 6: 6, 12: 12, 24: 24, 48: 48}}

// fBoost is 48132: {0=Off, 4=One-time incr., 1=3 hr, 2=6 hr, 3=12 hr}, afgelezen
// van de echte pomp. Langer dan twaalf uur kent deze serie niet.
var fBoost = boost{parameter: "48132", raw: map[int]int{0: 0, 2: 4, 3: 1, 6: 2, 12: 3}}

// writablePoint is de tabel andersom: van capability naar de parameter waar een
// bediening naartoe gaat.
type writablePoint struct {
	parameter string
	point     point
}

// writeTable maakt de schrijftabel voor één pomp, uit de nummers die die pomp
// meldt. Dat lost meteen op wat een gedeelde tabel niet kan: welke van de twee
// target_temperature-nummers deze pomp bedoelt.
func writeTable(present map[string]bool) map[string]writablePoint {
	out := map[string]writablePoint{}
	for parameter, entry := range readable {
		if entry.writable && present[parameter] {
			out[entry.capability] = writablePoint{parameter: parameter, point: entry}
		}
	}
	return out
}

// capabilityValue maakt van een punt de waarde die de capability verwacht, of
// false als de pomp niets te melden had.
func capabilityValue(entry point, p myuplink.Point) (any, bool) {
	value, ok := p.Number()
	if !ok {
		return nil, false
	}
	switch entry.kind {
	case boolean:
		return value != 0, true
	case enum:
		return strconv.Itoa(int(math.Trunc(value))), true
	default:
		return value, true
	}
}

// apiValue maakt van een capability-waarde het getal dat myUplink verwacht.
//
// Buiten bereik is een fout en geen stille correctie: wie 40 graden vraagt hoort
// te horen dat de pomp op 35 stopt, in plaats van te denken dat het gelukt is.
func apiValue(capability string, entry point, value any) (float64, error) {
	switch entry.kind {
	case boolean:
		on, ok := value.(bool)
		if !ok {
			return 0, fmt.Errorf("%s verwacht aan of uit, kreeg %v", capability, value)
		}
		if on {
			return 1, nil
		}
		return 0, nil

	case enum:
		text, ok := value.(string)
		if !ok {
			return 0, fmt.Errorf("%s verwacht een keuze, kreeg %v", capability, value)
		}
		choice, err := strconv.Atoi(text)
		if err != nil {
			return 0, fmt.Errorf("%s kent de keuze %q niet", capability, text)
		}
		if choice < int(entry.min) || choice > int(entry.max) {
			return 0, fmt.Errorf("%s kent de keuze %q niet", capability, text)
		}
		return float64(choice), nil

	default:
		amount, ok := asNumber(value)
		if !ok {
			return 0, fmt.Errorf("%s verwacht een getal, kreeg %v", capability, value)
		}
		// Een punt zonder grenzen heeft min en max allebei nul; alleen
		// target_temperature heeft ze echt, en daar is 5 tot 35 de grens die de
		// pomp zelf noemt (rauw 50-350 maal schaal 0,1).
		if entry.min != entry.max && (amount < entry.min || amount > entry.max) {
			return 0, fmt.Errorf("%s ligt tussen %g en %g; %g kan de pomp niet",
				capability, entry.min, entry.max, amount)
		}
		return amount, nil
	}
}

// asNumber neemt wat er over het protocol binnenkomt. JSON levert een float64,
// een Flow-kaart of een test soms een int.
func asNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	}
	return 0, false
}
