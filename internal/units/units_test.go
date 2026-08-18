package units

import (
	"math"
	"testing"
)

// Zonder keuze hoort er niets te gebeuren. Dit is de belangrijkste test van dit
// pakket: elke bestaande installatie leest zoals ze altijd las, en één afronding
// te veel zou elke tegel in huis stilletjes veranderen.
func TestNoChoiceLeavesEverythingAlone(t *testing.T) {
	var none Set
	for _, canonical := range []string{"°C", "mm", "km", "hPa", "%", "W", "kWh"} {
		value, label := none.Show(20.700000000000003, canonical)
		if value != 20.700000000000003 {
			t.Errorf("%s werd %v", canonical, value)
		}
		if label != canonical {
			t.Errorf("%s heet %q", canonical, label)
		}
		if none.Converts(canonical) {
			t.Errorf("%s wordt omgerekend zonder dat er iets gekozen is", canonical)
		}
	}
}

// Wind is de uitzondering, en met opzet: er wordt in meters per seconde gemeten
// omdat dat de meting is, en in Beaufort gelezen omdat dat is wat een mens zegt.
// Zonder keuze staat er dus Bft op de tegel, zoals altijd.
func TestWindReadsAsBeaufortWithoutAChoice(t *testing.T) {
	var none Set
	value, label := none.Show(4.2, "m/s")
	if value != 3 || label != "Bft" {
		t.Errorf("4,2 m/s las als %g %q, wil 3 Bft", value, label)
	}
	if !none.Converts("m/s") {
		t.Error("wind wordt niet omgerekend zonder keuze")
	}
	if got := none.Filled().Wind; got != "Bft" {
		t.Errorf("de standaard is %q", got)
	}
	// Wie het wél in de meting wil lezen, kiest dat en krijgt hem ongemoeid.
	metres := Set{Wind: "m/s"}
	if value, label := metres.Show(4.234, "m/s"); value != 4.234 || label != "m/s" {
		t.Errorf("gekozen m/s las als %g %q", value, label)
	}
}

// Een grootheid die Stulp niet kent blijft met rust gelaten, ook als er wél een
// keuze staat: procenten zijn overal procenten.
func TestUnitsWithoutAChoiceAreUntouched(t *testing.T) {
	set := Set{Temperature: "°F", Wind: "mph", Rain: "in", Distance: "mi", Pressure: "inHg"}
	for _, canonical := range []string{"%", "W", "kWh", "lx", "°", "Hz", "m³/h", ""} {
		if set.Converts(canonical) {
			t.Errorf("%q wordt omgerekend", canonical)
		}
		if value, label := set.Show(42, canonical); value != 42 || label != canonical {
			t.Errorf("%q werd %v %q", canonical, value, label)
		}
		if got := set.Canonical(42, canonical); got != 42 {
			t.Errorf("%q kwam terug als %v", canonical, got)
		}
	}
}

func TestTemperature(t *testing.T) {
	set := Set{Temperature: "°F"}
	// De twee punten die iedereen kan nakijken.
	for celsius, fahrenheit := range map[float64]float64{0: 32, 100: 212, 22.1: 71.8, -40: -40} {
		got, label := set.Show(celsius, "°C")
		if got != fahrenheit || label != "°F" {
			t.Errorf("%g °C werd %g %q, wil %g °F", celsius, got, label, fahrenheit)
		}
	}
	if got := set.Canonical(32, "°C"); math.Abs(got) > 1e-9 {
		t.Errorf("32 °F werd %g °C", got)
	}
	// En het alias: een app die "C" schrijft valt niet buiten de omrekening.
	if !set.Converts("C") {
		t.Error("de eenheid C wordt niet herkend als temperatuur")
	}
}

// Beaufort is een schaal en geen factor: 6 Bft is een gebied. "Boven 6" betekent
// vanaf de ondergrens, en dat maakt de omrekening rond.
func TestBeaufortRoundTrips(t *testing.T) {
	set := Set{Wind: "Bft"}
	for force := 0; force <= 12; force++ {
		metres := set.Canonical(float64(force), "m/s")
		if back, _ := set.Show(metres, "m/s"); back != float64(force) {
			t.Errorf("%d Bft werd %g m/s werd %g Bft", force, metres, back)
		}
	}
	if got := set.Canonical(6, "m/s"); got != 10.8 {
		t.Errorf("6 Bft werd %g m/s, wil 10,8", got)
	}
	// Een meting in het gebied leest als de kracht waar hij in valt.
	if got, label := set.Show(12.0, "m/s"); got != 6 || label != "Bft" {
		t.Errorf("12 m/s werd %g %q, wil 6 Bft", got, label)
	}
	if got, _ := set.Show(0.2, "m/s"); got != 0 {
		t.Errorf("0,2 m/s werd %g Bft, wil 0", got)
	}
}

func TestWindInOtherUnits(t *testing.T) {
	for unit, want := range map[string]float64{"km/h": 36, "mph": 22, "kn": 19} {
		set := Set{Wind: unit}
		if got, label := set.Show(10, "m/s"); got != want || label != unit {
			t.Errorf("10 m/s werd %g %q, wil %g %s", got, label, want, unit)
		}
	}
	// Terug moet exact zijn, want dit is een factor en geen schaal.
	set := Set{Wind: "km/h"}
	if got := set.Canonical(36, "m/s"); math.Abs(got-10) > 1e-9 {
		t.Errorf("36 km/h werd %g m/s", got)
	}
}

func TestRainDistanceAndPressure(t *testing.T) {
	for _, test := range []struct {
		set       Set
		canonical string
		value     float64
		want      float64
		label     string
	}{
		{Set{Rain: "in"}, "mm", 25.4, 1, "in"},
		{Set{Rain: "in"}, "mm", 3.6, 0.14, "in"},
		{Set{Distance: "mi"}, "km", 1.609344, 1, "mi"},
		{Set{Pressure: "inHg"}, "hPa", 1013.25, 29.92, "inHg"},
		{Set{Pressure: "mmHg"}, "hPa", 1013.25, 760, "mmHg"},
	} {
		got, label := test.set.Show(test.value, test.canonical)
		if got != test.want || label != test.label {
			t.Errorf("%g %s werd %g %q, wil %g %s",
				test.value, test.canonical, got, label, test.want, test.label)
		}
		if back := test.set.Canonical(test.want, test.canonical); math.Abs(back-test.value) > 0.05*math.Abs(test.value)+0.01 {
			t.Errorf("%g %s kwam terug als %g, wil rond %g", test.want, test.label, back, test.value)
		}
	}
}

// Elke aangeboden eenheid moet werken. Een keuzelijst met een optie die niets
// doet is erger dan geen keuze.
func TestEveryOfferedUnitConvertsBothWays(t *testing.T) {
	for _, entry := range Quantities() {
		for _, choice := range entry.Options {
			unit := choice.Unit
			// De keuze "zoals de app meldt" heeft geen eenheid en rekent per
			// definitie niets om; die staat hieronder apart.
			if unit == "" {
				continue
			}
			set, ok := Set{}.Choose(entry.Name, unit)
			if !ok {
				t.Fatalf("%s kan niet op %s gezet worden terwijl het aangeboden wordt", entry.Name, unit)
			}
			if label := set.Label(entry.Canonical); label != unit {
				t.Errorf("%s in %s leest als %q", entry.Name, unit, label)
			}
			if unit == entry.Canonical {
				continue
			}
			if !set.Converts(entry.Canonical) {
				t.Errorf("%s in %s rekent niet om", entry.Name, unit)
			}
			if _, ok := set.Step(entry.Canonical); !ok {
				t.Errorf("%s in %s heeft geen sprong voor een invoerveld", entry.Name, unit)
			}
			// Heen en terug binnen de nauwkeurigheid van de weergave zelf.
			// Een tegel in km/h toont hele getallen, dus 1 m/s leest als 4 km/h
			// en komt terug als 1,11 -- dat is de afronding en niet een fout in de
			// omrekening. De marge is dus een halve weergavestap, teruggerekend
			// naar de canonieke eenheid.
			//
			// Beaufort is de uitzondering: dat is een schaal, en die is hierboven
			// apart nagegaan.
			if unit == "Bft" {
				continue
			}
			perUnit := math.Abs(set.Canonical(1, entry.Canonical) - set.Canonical(0, entry.Canonical))
			// Twee afrondingen liggen op de weg: de weergave rondt af op wat een
			// tegel laat zien, en de weg terug rondt af op wat het document
			// bewaart. Beide horen in de marge.
			margin := 0.5*displayPrecision(entry.Name, unit)*perUnit +
				0.5*displayPrecision(entry.Name, entry.Canonical)/10 + 1e-9
			for _, value := range []float64{0, 1, 7.5, 20, 1013.25} {
				shown, _ := set.Show(value, entry.Canonical)
				if back := set.Canonical(shown, entry.Canonical); math.Abs(back-value) > margin {
					t.Errorf("%s: %g werd %g %s werd %g (marge %g)", entry.Name, value, shown, unit, back, margin)
				}
			}
		}
	}
}

// displayPrecision is het kleinste verschil dat een tegel in deze eenheid nog
// laat zien: 1 voor km/h, 0,01 voor inches.
func displayPrecision(quantityName, unit string) float64 {
	for _, choice := range byName(quantityName).options {
		if choice.unit == unit {
			return math.Pow(10, -float64(choice.decimals))
		}
	}
	return 1
}

func TestAnUnknownChoiceIsRefused(t *testing.T) {
	if _, ok := (Set{}).Choose("temperature", "kelvin"); ok {
		t.Error("kelvin werd aangenomen")
	}
	if _, ok := (Set{}).Choose("brightness", "lm"); ok {
		t.Error("een onbekende grootheid werd aangenomen")
	}
	if Valid("wind", "furlongs per fortnight") {
		t.Error("een onzin-eenheid geldt als geldig")
	}
	// En een keuze die tóch in het document staat -- met de hand erin gezet, of
	// van een nieuwere Stulp -- leest canoniek in plaats van te raden.
	set := Set{Temperature: "kelvin"}
	if value, label := set.Show(20, "°C"); value != 20 || label != "°C" {
		t.Errorf("een onbekende keuze leverde %g %q", value, label)
	}
}

func TestFilledNamesEveryQuantity(t *testing.T) {
	filled := Set{Wind: "mph"}.Filled()
	if filled.Wind != "mph" {
		t.Error("een gemaakte keuze werd overschreven")
	}
	for _, entry := range Quantities() {
		got := filled.chosen(*byName(entry.Name))
		if got == "" && !byName(entry.Name).asDeclared {
			t.Errorf("%s bleef leeg", entry.Name)
		}
		// Waar apps het onderling niet eens zijn is "zoals de app meldt" de
		// standaard, en dat is een lege keuze -- geen ontbrekende. Wind is hier al
		// gekozen, dus die blijft staan.
		if entry.Name != "wind" && got != entry.Default {
			t.Errorf("%s werd %q, wil de standaard %q", entry.Name, got, entry.Default)
		}
	}
}

// Een app mag melden in een eenheid die niet de canonieke is: Nibe meldt kilowatt
// terwijl watt de canonieke eenheid is. Dat hoort die app niet te hoeven
// veranderen -- de omrekening gaat via de canonieke eenheid heen en terug.
func TestAnAppMayDeclareANonCanonicalUnit(t *testing.T) {
	// Zonder keuze verandert er niets: 6,5 kW blijft 6,5 kW, en de watt van een
	// andere app blijft watt. Dát is waarom vermogen standaard "zoals de app
	// meldt" leest.
	var none Set
	if value, label := none.Show(6.5, "kW"); value != 6.5 || label != "kW" {
		t.Errorf("zonder keuze werd 6,5 kW %g %q", value, label)
	}
	if value, label := none.Show(2400, "W"); value != 2400 || label != "W" {
		t.Errorf("zonder keuze werd 2400 W %g %q", value, label)
	}

	// Wie het gelijk wil trekken kiest, en dan leest álles in die eenheid.
	watts := Set{Power: "W"}
	if value, label := watts.Show(6.5, "kW"); value != 6500 || label != "W" {
		t.Errorf("6,5 kW werd %g %q, wil 6500 W", value, label)
	}
	if value, label := watts.Show(2400, "W"); value != 2400 || label != "W" {
		t.Errorf("2400 W werd %g %q", value, label)
	}
	kilowatts := Set{Power: "kW"}
	if value, label := kilowatts.Show(2400, "W"); value != 2.4 || label != "kW" {
		t.Errorf("2400 W werd %g %q, wil 2,4 kW", value, label)
	}
	// En de weg terug gaat naar de eenheid van de app, niet naar de canonieke:
	// wie 6500 W intypt bij een Nibe-veld levert 6,5 kW.
	if got := watts.Canonical(6500, "kW"); got != 6.5 {
		t.Errorf("6500 W werd %g, wil 6,5 kW", got)
	}
	if got := kilowatts.Canonical(2.4, "W"); got != 2400 {
		t.Errorf("2,4 kW werd %g, wil 2400 W", got)
	}
}

// Watt is joule per seconde: dezelfde grootte, anders geschreven. Dat is een
// alias en geen keuze, dus het hoort herkend te worden en niet in het menu te
// staan.
func TestAnAliasIsRecognisedButNotOffered(t *testing.T) {
	kilowatts := Set{Power: "kW"}
	if value, label := kilowatts.Show(9000, "J/s"); value != 9 || label != "kW" {
		t.Errorf("9000 J/s werd %g %q, wil 9 kW", value, label)
	}
	for _, entry := range Quantities() {
		for _, choice := range entry.Options {
			if choice.Unit == "J/s" || choice.Unit == "C" || choice.Unit == "mbar" {
				t.Errorf("%s biedt het synoniem %q aan als keuze", entry.Name, choice.Unit)
			}
		}
	}
	// Een paar schrijfwijzen die in het wild voorkomen.
	for unit, quantityName := range map[string]string{
		"C": "temperature", "fahrenheit": "temperature", "ms": "wind", "kmh": "wind",
		"mbar": "pressure", "torr": "pressure", "watt": "power", "kwh": "energy", "inch": "rain",
	} {
		entry, _ := byUnit(unit)
		if entry == nil || entry.name != quantityName {
			t.Errorf("%q werd niet herkend als %s", unit, quantityName)
		}
	}
}

func TestEnergy(t *testing.T) {
	if value, label := (Set{Energy: "MJ"}).Show(10, "kWh"); value != 36 || label != "MJ" {
		t.Errorf("10 kWh werd %g %q, wil 36 MJ", value, label)
	}
	if value, label := (Set{Energy: "Wh"}).Show(1.5, "kWh"); value != 1500 || label != "Wh" {
		t.Errorf("1,5 kWh werd %g %q", value, label)
	}
	if value, label := (Set{Power: "BTU/h"}).Show(1000, "W"); value != 3412 || label != "BTU/h" {
		t.Errorf("1000 W werd %g %q, wil 3412 BTU/h", value, label)
	}
}

// Een omgerekende waarde gaat het document in, dus die hoort geen staart van
// zeventien cijfers te hebben: 70 °F is 21,111111111111114 °C, en wie 70 typt
// bedoelt niet die precisie.
func TestAConvertedValueIsNotAbsurdlyPrecise(t *testing.T) {
	set := Set{Temperature: "°F", Wind: "km/h", Rain: "in", Pressure: "inHg"}
	for _, test := range []struct {
		canonical string
		typed     float64
		want      float64
	}{
		{"°C", 70, 21.11},
		{"m/s", 36, 10},
		{"mm", 1, 25.4},
		{"hPa", 29.92, 1013.21},
	} {
		if got := set.Canonical(test.typed, test.canonical); got != test.want {
			t.Errorf("%g in %s werd %v, wil %v", test.typed, test.canonical, got, test.want)
		}
	}
	// En wat niet omgerekend wordt blijft precies zoals het gemeten is: een
	// warmtepomp die 21,357 meldt houdt zijn cijfers.
	if got := (Set{}).Canonical(21.357, "°C"); got != 21.357 {
		t.Errorf("een canonieke waarde werd afgerond op %v", got)
	}
}

func TestTextReadsAsASentence(t *testing.T) {
	celsius := Set{}
	if got := celsius.Text(21.5, "°C"); got != "21.5 °C" {
		t.Errorf("kreeg %q", got)
	}
	fahrenheit := Set{Temperature: "°F"}
	if got := fahrenheit.Text(21.5, "°C"); got != "70.7 °F" {
		t.Errorf("kreeg %q", got)
	}
	// Procenten en graden schuiven aan tegen het getal.
	if got := celsius.Text(78, "%"); got != "78%" {
		t.Errorf("kreeg %q", got)
	}
	if got := celsius.Text(324, "°"); got != "324°" {
		t.Errorf("kreeg %q", got)
	}
	// Beaufort is de standaardweergave van wind, ook in tekst.
	if got := celsius.Text(4.2, "m/s"); got != "3 Bft" {
		t.Errorf("kreeg %q", got)
	}
}
