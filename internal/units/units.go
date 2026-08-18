// Package units rekent om tussen de eenheid waarin een app meet en de eenheid
// waarin een mens leest.
//
// De regel is: **gemeten binnen, gekozen aan de rand.** Een app declareert bij
// elke waarde in welke eenheid hij die meldt. Dat is wat er in het document
// staat, wat een Flow vergelijkt en wat de statistiek bewaart. Pas op de plek
// waar iemand iets leest of intypt wordt er omgerekend, en op de weg terug weer
// gemaakt tot wat de app meldt.
//
// Dat is met opzet zo. Zou de keuze doorwerken tot in het document, dan zou een
// gebruiker die van Celsius naar Fahrenheit gaat zijn hele geschiedenis en al
// zijn Flow-drempels laten herschrijven -- en één mislukte schrijfactie
// halverwege laat een huis achter met twee eenheden door elkaar. Nu verandert
// een andere keuze geen enkele opgeslagen waarde: alleen hoe die gelezen wordt.
//
// Een app mag elke eenheid declareren die hier bekend is, ook een die niet de
// canonieke is: Nibe meldt bijvoorbeeld kilowatt terwijl watt de canonieke
// eenheid van vermogen is. Dat hoeft die app niet te veranderen; de omrekening
// gaat gewoon via de canonieke eenheid heen en terug.
//
// Twee soorten "andere eenheid" worden hier onderscheiden, en dat verschil is de
// hele modellering:
//
//   - Een **alias** is dezelfde grootte, anders geschreven. J/s is precies een
//     watt; C is precies °C. Daar valt niets te kiezen, dus een alias staat niet
//     in de keuzelijst -- hij wordt alleen herkend.
//   - Een **optie** is een andere grootte. Kilowatt is duizend watt, Fahrenheit
//     loopt anders dan Celsius. Dat is wél een keuze.
//
// Wat hier niet in staat: procenten, volt, ampère, lux, hertz, graden van een
// windroos, kubieke meters per uur. Daar is geen tweede eenheid voor in gebruik,
// en een keuze aanbieden waar niets te kiezen valt is een instelling die alleen
// in de weg zit.
package units

import (
	"math"
	"strconv"
)

// Set is de keuze van de gebruiker. Een leeg veld betekent: lees het zoals dit
// huis het altijd al las -- zie byDefault.
type Set struct {
	Temperature string `json:"temperature,omitempty"`
	Wind        string `json:"wind,omitempty"`
	Rain        string `json:"rain,omitempty"`
	Distance    string `json:"distance,omitempty"`
	Pressure    string `json:"pressure,omitempty"`
	Power       string `json:"power,omitempty"`
	Energy      string `json:"energy,omitempty"`
}

// Quantity is één grootheid met de eenheden die ervoor te kiezen zijn.
type Quantity struct {
	Name      string           `json:"name"`
	Title     string           `json:"title"`
	Hint      string           `json:"hint"`
	Canonical string           `json:"canonical"`
	Default   string           `json:"default"`
	Options   []QuantityOption `json:"options"`
}

// QuantityOption is één keuze. Unit is wat er bewaard wordt, Title wat er in de
// keuzelijst staat -- meestal de eenheid zelf.
type QuantityOption struct {
	Unit  string `json:"unit"`
	Title string `json:"title"`
}

type option struct {
	unit string
	// title staat in de keuzelijst als de eenheid zelf niet leesbaar genoeg is.
	title string
	// aliases zijn dezelfde grootte, anders geschreven. Een app die "J/s" of "C"
	// opschrijft hoort niet stil buiten de omrekening te vallen.
	aliases []string
	// step is wat een invoerveld in déze eenheid als sprong hoort te nemen. Een
	// omgerekende stap levert 0,9 °F op, en dat typt niemand.
	step     float64
	decimals int
	// to rekent van de canonieke eenheid naar deze; from is de weg terug.
	to   func(float64) float64
	from func(float64) float64
}

type quantity struct {
	name      string
	title     string
	hint      string
	canonical string
	// preferred is wat een huis zonder keuze leest. Bij bijna alles is dat de
	// canonieke eenheid, maar bij wind niet: er wordt in meters per seconde
	// gemeten omdat dat de meting is, en in Beaufort gelezen omdat dat is wat een
	// mens zegt.
	preferred string
	// asDeclared zet de standaard op "zoals de app het meldt". Dat is nodig waar
	// apps het onderling niet eens zijn: Nibe meldt kilowatt en Sigenergy watt, en
	// dan zou elke standaardkeuze de ene tegel wél veranderen en de andere niet.
	// Zonder keuze verandert er zo niets, en wie het gelijk wil trekken kiest.
	asDeclared bool
	options    []option
}

func identity(value float64) float64 { return value }

// byDefault is de eenheid die gelezen wordt zolang niemand koos: die van het huis
// zoals het altijd al las.
func (entry quantity) byDefault() string {
	switch {
	case entry.asDeclared:
		return ""
	case entry.preferred != "":
		return entry.preferred
	}
	return entry.canonical
}

// beaufortFrom zijn de ondergrenzen van de schaal van Beaufort in meters per
// seconde: kracht 1 begint bij 0,3 en kracht 12 bij 32,7.
var beaufortFrom = []float64{0.3, 1.6, 3.4, 5.5, 8.0, 10.8, 13.9, 17.2, 20.8, 24.5, 28.5, 32.7}

// Beaufort levert de windkracht bij een snelheid in meters per seconde.
func Beaufort(metresPerSecond float64) int {
	force := 0
	for _, from := range beaufortFrom {
		if metresPerSecond < from {
			break
		}
		force++
	}
	return force
}

// BeaufortFloor is de omgekeerde weg: de laagste snelheid die nog als deze
// windkracht geldt. Dat is wat "boven 6 Bft" betekent -- vanaf 10,8 m/s -- en
// het maakt de omrekening rond: 6 wordt 10,8 wordt weer 6.
func BeaufortFloor(force int) float64 {
	switch {
	case force <= 0:
		return 0
	case force > len(beaufortFrom):
		force = len(beaufortFrom)
	}
	return beaufortFrom[force-1]
}

func factor(multiplier float64) (func(float64) float64, func(float64) float64) {
	return func(value float64) float64 { return value * multiplier },
		func(value float64) float64 { return value / multiplier }
}

var known = []quantity{{
	name:      "temperature",
	title:     "Temperatuur",
	hint:      "Elke temperatuur: buiten, binnen, aanvoer, dauwpunt, bodem, batterijcel.",
	canonical: "°C",
	options: []option{
		{unit: "°C", aliases: []string{"C", "c", "celsius"}, step: 0.5, decimals: 1, to: identity, from: identity},
		{unit: "°F", aliases: []string{"F", "fahrenheit"}, step: 1, decimals: 1,
			to:   func(c float64) float64 { return c*9/5 + 32 },
			from: func(f float64) float64 { return (f - 32) * 5 / 9 }},
	},
}, {
	name:      "wind",
	title:     "Wind",
	hint:      "Windkracht en windstoten. Beaufort is een schaal en geen eenheid: 6 Bft betekent vanaf 10,8 m/s.",
	canonical: "m/s",
	preferred: "Bft",
	options: []option{
		{unit: "Bft", title: "Bft (Beaufort)", step: 1, decimals: 0,
			to:   func(ms float64) float64 { return float64(Beaufort(ms)) },
			from: func(force float64) float64 { return BeaufortFloor(int(math.Round(force))) }},
		{unit: "m/s", aliases: []string{"ms", "m/sec"}, step: 0.5, decimals: 1, to: identity, from: identity},
		{unit: "km/h", aliases: []string{"kmh", "kph"}, step: 1, decimals: 0,
			to: mul(3.6), from: div(3.6)},
		{unit: "mph", step: 1, decimals: 0, to: mul(2.2369362920544), from: div(2.2369362920544)},
		{unit: "kn", aliases: []string{"kt", "knots"}, step: 1, decimals: 0,
			to: mul(1.9438444924406), from: div(1.9438444924406)},
	},
}, {
	name:      "rain",
	title:     "Neerslag",
	hint:      "Regen, sneeuw en het tekort van de tuin.",
	canonical: "mm",
	options: []option{
		{unit: "mm", step: 0.5, decimals: 1, to: identity, from: identity},
		{unit: "in", aliases: []string{"inch", "\""}, step: 0.05, decimals: 2,
			to: div(25.4), from: mul(25.4)},
	},
}, {
	name:      "distance",
	title:     "Afstand",
	hint:      "Zicht.",
	canonical: "km",
	options: []option{
		{unit: "km", step: 0.1, decimals: 1, to: identity, from: identity},
		{unit: "mi", aliases: []string{"mile", "miles"}, step: 0.1, decimals: 1,
			to: div(1.609344), from: mul(1.609344)},
	},
}, {
	name:      "pressure",
	title:     "Luchtdruk",
	hint:      "Wat een barometer aanwijst.",
	canonical: "hPa",
	options: []option{
		{unit: "hPa", aliases: []string{"hpa", "mbar", "mBar"}, step: 1, decimals: 1, to: identity, from: identity},
		{unit: "inHg", aliases: []string{"inhg"}, step: 0.1, decimals: 2,
			to: div(33.863886666667), from: mul(33.863886666667)},
		{unit: "mmHg", aliases: []string{"mmhg", "torr"}, step: 1, decimals: 0,
			to: div(1.3332236842105), from: mul(1.3332236842105)},
	},
}, {
	name:      "power",
	title:     "Vermogen",
	hint:      "Watt is joule per seconde -- dat is dezelfde grootte, dus dat is geen keuze. Kilowatt en BTU per uur zijn dat wel. Apps melden het niet allemaal hetzelfde: Nibe in kW, Sigenergy in W.",
	canonical: "W",
	// Zonder keuze blijft elke tegel staan zoals zijn app hem meldt. Zie
	// asDeclared: apps zijn hier niet eenstemmig, dus elke standaard zou de ene
	// tegel veranderen en de andere niet.
	asDeclared: true,
	options: []option{
		{unit: "W", aliases: []string{"J/s", "watt", "Watt", "VA"}, step: 1, decimals: 0, to: identity, from: identity},
		{unit: "kW", aliases: []string{"kw"}, step: 0.1, decimals: 3, to: div(1000), from: mul(1000)},
		{unit: "BTU/h", aliases: []string{"btu/h", "BTU/hr"}, step: 100, decimals: 0,
			to: mul(3.412141633), from: div(3.412141633)},
	},
}, {
	name:       "energy",
	title:      "Energie",
	hint:       "Wat een meterstand telt.",
	canonical:  "kWh",
	asDeclared: true,
	options: []option{
		{unit: "kWh", aliases: []string{"kwh"}, step: 0.1, decimals: 3, to: identity, from: identity},
		{unit: "Wh", aliases: []string{"wh"}, step: 1, decimals: 0, to: mul(1000), from: div(1000)},
		{unit: "MJ", aliases: []string{"mj"}, step: 0.1, decimals: 2, to: mul(3.6), from: div(3.6)},
	},
}}

func mul(multiplier float64) func(float64) float64 {
	return func(value float64) float64 { return value * multiplier }
}

func div(divisor float64) func(float64) float64 {
	return func(value float64) float64 { return value / divisor }
}

// Quantities is wat de instellingenpagina moet weten. De pagina bouwt zijn
// keuzelijsten hieruit en niet uit een eigen lijstje: twee lijsten die uit elkaar
// lopen levert een keuze op die niets doet.
func Quantities() []Quantity {
	out := make([]Quantity, 0, len(known))
	for _, entry := range known {
		options := make([]QuantityOption, 0, len(entry.options)+1)
		if entry.asDeclared {
			options = append(options, QuantityOption{Unit: "", Title: "zoals de app meldt"})
		}
		for _, choice := range entry.options {
			title := choice.title
			if title == "" {
				title = choice.unit
			}
			options = append(options, QuantityOption{Unit: choice.unit, Title: title})
		}
		out = append(out, Quantity{
			Name: entry.name, Title: entry.title, Hint: entry.hint,
			Canonical: entry.canonical, Default: entry.byDefault(), Options: options,
		})
	}
	return out
}

// Valid zegt of deze eenheid bij deze grootheid hoort. Wat er niet bij hoort
// wordt geweigerd in plaats van bewaard: een onbekende eenheid in het document
// zou stil terugvallen en dan lijkt de instelling stuk.
//
// Een lege eenheid is geldig bij een grootheid die "zoals de app meldt" aanbiedt:
// dat is daar een echte keuze en niet de afwezigheid van een keuze.
func Valid(quantityName, unit string) bool {
	entry := byName(quantityName)
	if entry == nil {
		return false
	}
	if unit == "" {
		return entry.asDeclared
	}
	for _, choice := range entry.options {
		if choice.unit == unit {
			return true
		}
	}
	return false
}

// Choose zet één grootheid, en zegt of dat kon.
func (s Set) Choose(quantityName, unit string) (Set, bool) {
	if !Valid(quantityName, unit) {
		return s, false
	}
	switch quantityName {
	case "temperature":
		s.Temperature = unit
	case "wind":
		s.Wind = unit
	case "rain":
		s.Rain = unit
	case "distance":
		s.Distance = unit
	case "pressure":
		s.Pressure = unit
	case "power":
		s.Power = unit
	case "energy":
		s.Energy = unit
	default:
		return s, false
	}
	return s, true
}

// Filled vult de lege velden met de eenheid die toch al gelezen wordt, zodat een
// pagina altijd een keuze kan aanwijzen.
func (s Set) Filled() Set {
	filled := s
	for _, entry := range known {
		if filled.chosen(entry) == "" {
			filled, _ = filled.Choose(entry.name, entry.byDefault())
		}
	}
	return filled
}

func (s Set) chosen(entry quantity) string {
	switch entry.name {
	case "temperature":
		return s.Temperature
	case "wind":
		return s.Wind
	case "rain":
		return s.Rain
	case "distance":
		return s.Distance
	case "pressure":
		return s.Pressure
	case "power":
		return s.Power
	case "energy":
		return s.Energy
	}
	return ""
}

// Label is de eenheid waarin de gebruiker deze waarde leest.
func (s Set) Label(declared string) string {
	entry, _, chosen := s.resolve(declared)
	if entry == nil || chosen == nil {
		return declared
	}
	return chosen.unit
}

// Show rekent een gemelde waarde om naar de eenheid van de gebruiker, en levert
// het label erbij.
//
// Leest het huis in dezelfde eenheid als de app meldt, dan komt de waarde er
// ongemoeid uit -- niet afgerond, niet aangeraakt. Dat is wat een installatie
// zonder keuze precies hetzelfde laat lezen als voorheen.
func (s Set) Show(value float64, declared string) (float64, string) {
	entry, from, chosen := s.resolve(declared)
	if entry == nil || chosen == nil || chosen.unit == from.unit {
		if entry == nil {
			return value, declared
		}
		return value, from.unit
	}
	return roundTo(chosen.to(from.from(value)), chosen.decimals), chosen.unit
}

// Text is een waarde als leesbare tekst, met de eenheid erbij: "71.8 °F",
// "3 Bft", "78%". Voor een pushbericht, een Flow-token, een pagina van een app --
// overal waar een mens een getal in een zin leest.
//
// Procenten en graden schuiven tegen het getal aan omdat dat is hoe ze
// geschreven worden; de rest krijgt een spatie.
func (s Set) Text(value float64, declared string) string {
	shown, label := s.Show(value, declared)
	number := strconv.FormatFloat(shown, 'f', -1, 64)
	switch label {
	case "":
		return number
	case "%", "°":
		return number + label
	}
	return number + " " + label
}

// Canonical is de weg terug: wat iemand intypte in zijn eigen eenheid, in de
// eenheid waarin de app meldt en het document bewaart.
func (s Set) Canonical(value float64, declared string) float64 {
	entry, to, chosen := s.resolve(declared)
	if entry == nil || chosen == nil || chosen.unit == to.unit {
		return value
	}
	// Afgerond op één cijfer meer dan de eenheid van de app zelf laat zien. 70 °F
	// is 21,111111111111114 °C, en dat getal is nauwkeuriger dan de invoer waar
	// het uit komt: wie 70 typt bedoelt niet 21,111111111111114. Het staat
	// bovendien in het document en in een tegel, en zo'n staart maakt beide
	// onleesbaar zonder ergens iets toe te voegen.
	return roundTo(to.to(chosen.from(value)), to.decimals+1)
}

// Step is de sprong die een invoerveld in de gekozen eenheid hoort te nemen. ok
// is vals als er niets omgerekend wordt; dan blijft staan wat de app declareerde.
func (s Set) Step(declared string) (float64, bool) {
	entry, from, chosen := s.resolve(declared)
	if entry == nil || chosen == nil || chosen.unit == from.unit {
		return 0, false
	}
	return chosen.step, true
}

// Converts zegt of deze eenheid omgerekend wordt. Dat scheelt de aanroeper het
// werk voor de honderden waarden waar niets aan te rekenen valt.
func (s Set) Converts(declared string) bool {
	entry, from, chosen := s.resolve(declared)
	return entry != nil && chosen != nil && chosen.unit != from.unit
}

// resolve zoekt bij een gedeclareerde eenheid de grootheid, de optie die de app
// gebruikt, en de optie die de gebruiker wil lezen.
//
// chosen is nil als er niets omgerekend hoort te worden: dat is het geval bij
// "zoals de app meldt".
func (s Set) resolve(declared string) (*quantity, *option, *option) {
	entry, from := byUnit(declared)
	if entry == nil {
		return nil, nil, nil
	}
	wanted := s.chosen(*entry)
	if wanted == "" {
		wanted = entry.byDefault()
	}
	if wanted == "" {
		// "Zoals de app meldt": lees hem in de eenheid waarin hij gemeld is.
		return entry, from, from
	}
	for index := range entry.options {
		if entry.options[index].unit == wanted {
			return entry, from, &entry.options[index]
		}
	}
	// Een keuze die Stulp niet kent -- met de hand in het document gezet, of van
	// een nieuwere versie -- leest zoals een huis zonder keuze leest. Dat is de
	// veilige kant: liever de gewone weergave dan een omrekening die niemand
	// vroeg.
	return entry, from, from
}

func byName(name string) *quantity {
	for index := range known {
		if known[index].name == name {
			return &known[index]
		}
	}
	return nil
}

// byUnit zoekt de grootheid en de optie waar deze eenheid bij hoort. Een alias
// wijst naar dezelfde optie: J/s is een watt.
func byUnit(unit string) (*quantity, *option) {
	if unit == "" {
		return nil, nil
	}
	for index := range known {
		for choice := range known[index].options {
			if known[index].options[choice].unit == unit {
				return &known[index], &known[index].options[choice]
			}
			for _, alias := range known[index].options[choice].aliases {
				if alias == unit {
					return &known[index], &known[index].options[choice]
				}
			}
		}
	}
	return nil, nil
}

func roundTo(value float64, decimals int) float64 {
	factor := math.Pow(10, float64(decimals))
	return math.Round(value*factor) / factor
}
