package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/somfy/internal/tahoma"
)

// Zonwering en rolluiken: zeven drivers, één implementatie.
//
// De bron heeft er zeven mappen voor met in elke map een klasse die niets doet
// behalve erven. Wat ze werkelijk onderscheidt zijn twee dingen: welke
// controllableName ze bij het koppelen oppikken, en welke kant hun "omhoog" op
// staat. Dat past in een tabel.

type coveringKind struct {
	// Direction zegt of omhoog opendoen is (rolluik) of dichtdoen (luifel).
	Direction tahoma.Direction
	// Controllable zijn de TaHoma-typen die deze driver oppikt. De io:-namen
	// komen uit de driver.js-bestanden van de bron; de ogp:-namen zijn erbij
	// gekomen na een toets tegen een echt account (2026-08-09), waar geen enkel
	// io:-apparaat stond en koppelen dus niets vond.
	Controllable []string
}

// coverings koppelt een driver-id uit app.json aan wat het is.
var coverings = map[string]coveringKind{
	"io_vertical_exterior_blind": {
		Direction:    tahoma.Shutter,
		Controllable: []string{"io:VerticalExteriorAwningIOComponent"},
	},
	"io_exterior_venetian_blind": {
		Direction: tahoma.Shutter,
		Controllable: []string{
			// Zelfde verhaal als bij ogp:Shutter: gezien op een echt account,
			// met closure 100 als dicht.
			"ogp:Blind",
			"io:ExteriorVenetianBlindIOComponent",
		},
	},
	"io_roller_shutter": {
		Direction: tahoma.Shutter,
		Controllable: []string{
			// ogp:Shutter is hoe een nieuwere doos een rolluik noemt. Gezien op
			// een echt account: core:ClosureState 100 met core:OpenClosedState
			// "closed", dus dezelfde richting als de io:-varianten.
			"ogp:Shutter",
			"io:RollerShutterGenericIOComponent",
			"io:RollerShutterWithLowSpeedManagementIOComponent",
			// Deze staat er zo in de bron. Het ziet eruit als een typenaam die
			// iemand van een echte doos heeft overgenomen; overslaan zou de
			// rolluiken van die gebruikers onvindbaar maken.
			"io:Re3js3W69CrGF8kKXvvmYtT4zNGqicXRjvuAnmmbvPZXnt",
		},
	},
	"io_velux_roller_shutter": {
		Direction:    tahoma.Shutter,
		Controllable: []string{"io:RollerShutterVeluxIOComponent"},
	},
	"io_velux_interior_blind": {
		Direction:    tahoma.Shutter,
		Controllable: []string{"io:VerticalInteriorBlindVeluxIOComponent"},
	},
	"io_velux_roof_window": {
		Direction:    tahoma.Shutter,
		Controllable: []string{"io:WindowOpenerVeluxIOComponent"},
	},
	// De horizontale zonneluifel is de enige die andersom loopt: opengaan is
	// naar buiten schuiven, en dat is voor de gebruiker naar beneden.
	"io_horizontal_awning": {
		Direction:    tahoma.Awning,
		Controllable: []string{"io:HorizontalAwningIOComponent"},
	},
}

type coveringDriver struct{ kind coveringKind }

// covering is één zonwering of rolluik.
type covering struct {
	device *appsdk.Device
	kind   coveringKind
	// url is het adres bij TaHoma; hiermee gaat een commando weg en hierop komt
	// een stand binnen.
	url string
	// label is de naam uit TaHoma. Die komt terug in de geschiedenis van de
	// Somfy-app zelf, zodat daar te zien is waar een beweging vandaan kwam.
	label string
}

func (d coveringDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	data := device.Data()
	url, _ := data["deviceURL"].(string)
	if url == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen TaHoma-adres; koppel het opnieuw")
	}
	label, _ := data["label"].(string)
	if label == "" {
		label = device.Name()
	}
	return &covering{device: device, kind: d.kind, url: url, label: label}, nil
}

// ListDevices haalt op wat er bij TaHoma hangt en houdt over wat bij deze driver
// hoort.
func (d coveringDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	setup, err := client.Setup(ctx)
	if err != nil {
		return nil, err
	}

	found := make([]appsdk.PairedDevice, 0)
	for _, device := range setup.Devices {
		if !d.kind.handles(device.ControllableName) {
			continue
		}
		found = append(found, appsdk.PairedDevice{
			Name: device.Label,
			// deviceURL is de identiteit: daar gaan de commando's heen. oid en
			// label staan erbij omdat de bron ermee werkt -- oid als sleutel,
			// label als naam in de TaHoma-geschiedenis -- en een melding tegen
			// de oorspronkelijke app zo hier terug te vinden is.
			Data: map[string]any{
				"deviceURL": device.DeviceURL,
				"oid":       device.OID,
				"label":     device.Label,
			},
			Store: map[string]any{"controllableName": device.ControllableName},
		})
	}
	return found, nil
}

func (k coveringKind) handles(controllableName string) bool {
	for _, known := range k.Controllable {
		if known == controllableName {
			return true
		}
	}
	return false
}

func (c *covering) OnInit() error {
	instance.watch(c.url, c)
	return nil
}

func (c *covering) OnDeleted() { instance.forget(c.url) }

// reachable en gone volgen wat de poll over dit apparaat zegt. Alleen schrijven
// als er iets verandert: anders gaat er elke ronde een schrijfactie naar Stulp
// voor een apparaat dat gewoon stilstaat.
func (c *covering) reachable() {
	if !c.device.Available() {
		c.device.SetAvailable()
	}
}

func (c *covering) gone() {
	if c.device.Available() {
		c.device.SetUnavailable("TaHoma kent dit apparaat niet meer. Staat het nog in de Somfy-app?")
	}
}

// apply zet de standen uit TaHoma om in capabilities.
func (c *covering) apply(device tahoma.Device) {
	values := map[string]any{}
	closure, hasClosure := device.Number(tahoma.StateClosure)
	if hasClosure {
		values["windowcoverings_set"] = tahoma.Position(closure)
	}

	openClosed, hasOpenClosed := device.Text(tahoma.StateOpenClosed)
	if !hasOpenClosed && !hasClosure {
		// Zonder een van beide standen is er niets te melden. Dat stil laten
		// passeren betekent een tegel die leeg blijft zonder dat iemand weet
		// waarom, dus het gaat in de log. Dit gebeurt hooguit één keer per
		// apparaat: de poll meldt alleen wat veranderde.
		c.device.Error("TaHoma meldt voor dit apparaat geen " + tahoma.StateClosure +
			" en geen " + tahoma.StateOpenClosed + "; de stand blijft leeg")
		return
	}

	state, ok := c.kind.Direction.Motion(openClosed, closure, hasClosure)
	if ok {
		values["windowcoverings_state"] = state
		c.report(values)
		return
	}
	c.report(values)
	// Niet kunnen vertalen is geen reden om te zwijgen. Er zijn twee manieren
	// waarop het misgaat en ze vragen om iets anders: een stand die ontbreekt is
	// een apparaat dat anders in elkaar zit dan verwacht, een stand met een
	// onbekende tekst is een woord dat we niet mogen raden.
	if !hasOpenClosed {
		c.device.Error("TaHoma meldt geen " + tahoma.StateOpenClosed +
			"; omhoog of omlaag blijft daardoor onbekend")
		return
	}
	c.device.Error("TaHoma meldt " + tahoma.StateOpenClosed + " als " + openClosed +
		"; dat is niet open of closed en wordt niet geraden")
}

// OnCapability voert een opdracht uit.
//
// Wat hier niet gebeurt is net zo belangrijk als wat er wel gebeurt: een
// opdracht aan TaHoma is asynchroon. Je krijgt een uitvoer-id terug zodra de
// doos hem heeft aangenomen, en pas twintig seconden later hangt het rolluik
// ergens anders. Zie de opmerkingen per geval.
func (c *covering) OnCapability(name string, value any) error {
	client, err := instance.api()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch name {
	case "windowcoverings_set":
		position, ok := value.(float64)
		if !ok {
			return fmt.Errorf("een stand hoort een getal tussen 0 en 1 te zijn, niet %T", value)
		}
		execution, err := client.Execute(ctx, c.label, c.url, tahoma.Command{
			Name:       tahoma.CommandSetClosure,
			Parameters: []any{tahoma.Closure(position)},
		})
		if err != nil {
			return err
		}
		c.remember(execution)
		// windowcoverings_set wordt hier met opzet níet gezet. Het is een
		// positie, en die is nog niet bereikt -- hem nu op de gevraagde waarde
		// zetten is beweren dat het rolluik er al hangt. De echte stand komt van
		// de volgende ronde van de poll; de nudge haalt die naar voren.
		instance.nudge()
		return nil

	case "windowcoverings_state":
		state, ok := value.(string)
		if !ok {
			return fmt.Errorf("een richting hoort tekst te zijn, niet %T", value)
		}
		if state == tahoma.StateIdle {
			return c.stop(ctx, client)
		}
		command, ok := c.kind.Direction.Command(state)
		if !ok {
			return fmt.Errorf("%q is geen richting die dit apparaat kent", state)
		}
		execution, err := client.Execute(ctx, c.label, c.url, tahoma.Command{Name: command})
		if err != nil {
			return err
		}
		c.remember(execution)
		instance.nudge()
		// Ook de richting wordt hier niet vooruit gezet. Het leek te
		// verdedigen -- up zegt welke kant hij opgaat -- maar TaHoma heeft de
		// opdracht alleen aangenomen, en de poll zet hem een tel later alsnog
		// om. Dat is precies de sprong die je niet wilt zien.
		return nil
	}
	return fmt.Errorf("dit apparaat kent %q niet", name)
}

// stop trekt de lopende opdracht in.
//
// Dat is het enige wat de bron als stoppen kent: er is geen los stopcommando in
// deze API, alleen het intrekken van een uitvoering die je zelf gestart bent
// (lib/Tahoma.js, cancelExecution). Een beweging die met de wandschakelaar of
// vanuit de Somfy-app begon heeft hier geen uitvoer-id, en die valt dus niet te
// stoppen -- dat hoort de gebruiker te horen in plaats van een knop die niets
// doet.
func (c *covering) stop(ctx context.Context, client *tahoma.Client) error {
	execution, _ := c.device.StoreValue("executionId")
	id, _ := execution.(string)
	if id == "" {
		return fmt.Errorf("er loopt geen beweging die deze app gestart is; TaHoma biedt geen manier om een andere beweging te stoppen")
	}
	if err := client.Cancel(ctx, id); err != nil {
		return err
	}
	c.remember("")
	instance.nudge()
	return c.device.SetCapabilityValue("windowcoverings_state", tahoma.StateIdle)
}

// report meldt een waarde en laat het niet lopen als dat niet lukt.
//
// Stulp weigert een capability die dit apparaat niet heeft. Dat is een tikfout
// in app.json of hier, en die hoort op te vallen zodra hij gebeurt in plaats van
// wanneer iemand zich afvraagt waarom een tegel leeg blijft. Dit draait alleen
// als er iets veranderd is, dus het kan de log niet vollopen.
func (c *covering) report(values map[string]any) {
	if err := c.device.SetCapabilityValues(values); err != nil {
		c.device.Error(err.Error())
	}
}

// remember bewaart het uitvoer-id, want de stopknop heeft het later nodig en de
// app kan tussendoor herstart zijn.
func (c *covering) remember(execution string) {
	if err := c.device.SetStore(map[string]any{"executionId": execution}); err != nil {
		c.device.Error("het uitvoer-id kon niet bewaard worden: " + err.Error())
	}
}
