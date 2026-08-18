package tahoma

import "math"

// De standen die zonwering en rolluiken melden.
const (
	// StateClosure is een sluitingspercentage van 0 tot en met 100.
	StateClosure = "core:ClosureState"
	// StateOpenClosed is "open" of "closed".
	StateOpenClosed = "core:OpenClosedState"
)

// Het commando waarmee je een stand zet. De bron gebruikt alleen deze drie.
const (
	CommandSetClosure = "setClosure"
	CommandOpen       = "open"
	CommandClose      = "close"
)

// Position vertaalt een Somfy-sluiting naar windowcoverings_set.
//
// De assen lopen tegen elkaar in, en dat is geen slordigheid maar een verschil
// in wat er geteld wordt. Somfy meet hoevéél er dicht zit: core:ClosureState 0
// is helemaal open, 100 is helemaal dicht. windowcoverings_set meet hoe ver het
// open staat: 0 is dicht, 1 is open. De één telt de weg naar dicht, de ander de
// weg naar open, dus de omrekening is een spiegeling.
//
// Beide kanten staan zo in de bron, com.somfy.tahoma 1.5.4,
// drivers/WindowCoveringsDevice.js:
//
//	regel 70:  parameters: [Math.round((1-value)*100)]   // Stulp -> Somfy
//	regel 112: 1-(closureState.value/100)                // Somfy -> Stulp
func Position(closure float64) float64 {
	return clamp01(1 - closure/100)
}

// Closure vertaalt windowcoverings_set terug naar een Somfy-sluiting.
//
// Afronden op hele procenten omdat setClosure een geheel getal wil; de bron doet
// dat met Math.round en dat is hier math.Round, inclusief het afronden van
// precies 0,5 naar boven.
func Closure(position float64) int {
	return int(math.Round((1 - clamp01(position)) * 100))
}

// clamp01 houdt een stand binnen 0..1.
//
// Een waarde daarbuiten komt niet van de interface maar wel van een Flow met een
// rekenkaart ervoor, en die hoort geen setClosure van 140 op te leveren: TaHoma
// zou dat weigeren en de gebruiker zou een fout zien op een plek waar de fout
// niet zit.
func clamp01(value float64) float64 {
	switch {
	case math.IsNaN(value):
		return 0
	case value < 0:
		return 0
	case value > 1:
		return 1
	}
	return value
}

// Direction is welke Somfy-opdracht bij "omhoog" hoort.
//
// Voor een rolluik is omhoog opendoen. Voor een zonneluifel is het precies
// andersom: die schuift naar buiten als hij "opengaat", en dat ziet de gebruiker
// als naar beneden. De bron regelt dat door in
// drivers/io_horizontal_awning/device.js beide tabellen om te draaien; hier is
// het één waarde per driver.
//
// Let op dat dit niets zegt over de sluiting: ook een luifel rekent zijn
// core:ClosureState op dezelfde spiegeling om. De bron draait alleen de
// commando's en de open/dicht-tekst om, en versie 1.5.1 van de bron bestond
// juist uit die correctie.
type Direction struct {
	// Up en Down zijn de commando's bij windowcoverings_state.
	Up   string
	Down string
	// openClosed vertaalt core:OpenClosedState terug naar windowcoverings_state.
	openClosed map[string]string
}

var (
	// Shutter geldt voor rolluiken, jaloezieën, verticale schermen en dakramen:
	// omhoog is opendoen.
	Shutter = Direction{
		Up:         CommandOpen,
		Down:       CommandClose,
		openClosed: map[string]string{"open": StateUp, "closed": StateDown},
	}
	// Awning geldt voor de horizontale zonneluifel: omhoog is dichtdoen, want
	// een luifel die opengaat komt naar beneden.
	Awning = Direction{
		Up:         CommandClose,
		Down:       CommandOpen,
		openClosed: map[string]string{"open": StateDown, "closed": StateUp},
	}
)

// De drie waarden van windowcoverings_state.
const (
	StateUp   = "up"
	StateDown = "down"
	StateIdle = "idle"
)

// Command levert het Somfy-commando bij een windowcoverings_state.
//
// idle staat er niet bij: stoppen is bij TaHoma geen commando maar het intrekken
// van een lopende opdracht. Zie Cancel in exec.go.
func (d Direction) Command(state string) (string, bool) {
	switch state {
	case StateUp:
		return d.Up, true
	case StateDown:
		return d.Down, true
	}
	return "", false
}

// Motion vertaalt wat TaHoma meldt naar windowcoverings_state.
//
// TaHoma kent geen idle. Een rolluik dat halverwege hangt meldt gewoon "open",
// want het zit niet dicht. De bron maakt daar idle van zodra de sluiting tussen
// 0 en 100 in ligt (WindowCoveringsDevice.js:107) en dat is overgenomen -- maar
// het betekent "staat ergens tussenin", niet "staat stil". Iets beters biedt
// deze API niet: er is geen stand die zegt of de motor loopt.
func (d Direction) Motion(openClosed string, closure float64, hasClosure bool) (string, bool) {
	if hasClosure && closure != 0 && closure != 100 {
		return StateIdle, true
	}
	state, ok := d.openClosed[openClosed]
	return state, ok
}
