package openmeteo

// De WMO-weercode, teruggebracht tot iets waar een Flow-kaart op kan staan.
//
// Open-Meteo levert code 4677 van de WMO: 28 waarden waarvan er drie over
// motregen gaan en drie over sneeuwbuien. Die allemaal op een keuzelijst zetten
// levert een lijst waar niemand uit kiest, dus ze gaan in tien standen die een
// mens gebruikt. De ruwe code blijft als Flow-token beschikbaar, zodat wie het
// verschil tussen lichte en zware motregen wél wil, er nog bij kan.

// State is het weer in één woord. De id's staan in app.json als keuzelijst.
type State string

const (
	Clear        State = "clear"        // onbewolkt
	PartlyCloudy State = "partly"       // licht bewolkt
	Cloudy       State = "cloudy"       // zwaar bewolkt
	Fog          State = "fog"          // nevel of mist
	Drizzle      State = "drizzle"      // motregen
	Rain         State = "rain"         // regen
	Freezing     State = "freezing"     // ijzel
	Snow         State = "snow"         // sneeuw
	Showers      State = "showers"      // buien
	Thunderstorm State = "thunderstorm" // onweer
	Unknown      State = "unknown"
)

// StateOf vertaalt een WMO-code naar een stand.
func StateOf(code int) State {
	switch code {
	case 0:
		return Clear
	case 1, 2:
		return PartlyCloudy
	case 3:
		return Cloudy
	case 45, 48:
		return Fog
	case 51, 53, 55:
		return Drizzle
	case 56, 57, 66, 67:
		// IJzel: motregen of regen die op de grond bevriest. Dat is geen zwaardere
		// regen maar een ander gevaar, en het hoort dus een eigen stand te zijn.
		return Freezing
	case 61, 63, 65:
		return Rain
	case 71, 73, 75, 77, 85, 86:
		return Snow
	case 80, 81, 82:
		return Showers
	case 95, 96, 99:
		return Thunderstorm
	}
	return Unknown
}

// Describe is de volledige beschrijving, voor een tegel en een Flow-token.
//
// Dit is de tekst van de WMO-tabel en niet een samenvatting: wie op een tegel
// kijkt wil weten of het lichte of zware regen is.
func Describe(code int) string {
	switch code {
	case 0:
		return "Onbewolkt"
	case 1:
		return "Vrijwel onbewolkt"
	case 2:
		return "Half bewolkt"
	case 3:
		return "Zwaar bewolkt"
	case 45:
		return "Nevel"
	case 48:
		return "Mist met rijpaanslag"
	case 51:
		return "Lichte motregen"
	case 53:
		return "Motregen"
	case 55:
		return "Zware motregen"
	case 56:
		return "Lichte ijzel"
	case 57:
		return "Zware ijzel"
	case 61:
		return "Lichte regen"
	case 63:
		return "Regen"
	case 65:
		return "Zware regen"
	case 66:
		return "Lichte ijzelregen"
	case 67:
		return "Zware ijzelregen"
	case 71:
		return "Lichte sneeuwval"
	case 73:
		return "Sneeuwval"
	case 75:
		return "Zware sneeuwval"
	case 77:
		return "Sneeuwkorrels"
	case 80:
		return "Lichte buien"
	case 81:
		return "Buien"
	case 82:
		return "Zware buien"
	case 85:
		return "Lichte sneeuwbuien"
	case 86:
		return "Zware sneeuwbuien"
	case 95:
		return "Onweer"
	case 96:
		return "Onweer met lichte hagel"
	case 99:
		return "Onweer met zware hagel"
	}
	// Een code die de WMO later toevoegt hoort niet als "onbekend" te
	// verdwijnen: het nummer erbij maakt hem opzoekbaar.
	return "Onbekend weertype (WMO " + itoa(code) + ")"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
