// Command weather volgt het weer op een of meer locaties.
//
// Eén apparaat per plek, en de Flow-kaarten wijzen die plek aan zoals ze elke
// andere sensor aanwijzen. Zo kun je een kaart op je eigen tuin zetten en een
// tweede op de plek waar je zo naartoe rijdt.
//
// De bron is Open-Meteo en niet KNMI, waar de vraag mee begon. Waarom staat in
// internal/openmeteo en in PORTED.md: KNMI's open data vraagt een sleutel en
// levert binaire bestanden, en dat is een zware bibliotheek voor iets wat hier
// in JSON per coördinaat te krijgen is.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/weather/internal/openmeteo"
)

// pollInterval is hoe vaak er gekeken wordt.
//
// Tien minuten. Open-Meteo werkt zijn metingen per kwartier bij, dus vaker
// vragen levert hetzelfde antwoord op -- en het is een gratis dienst waar we te
// gast zijn. Een bui die tussen twee rondes valt missen we; dat is de resolutie
// van de bron en niet iets om hier te verzinnen.
const pollInterval = 10 * time.Minute

// settleTime is hoe lang er gewacht wordt na het koppelen voordat er gevraagd
// wordt. Kort: het is een cloud en er hoeft niets bij te komen.
const settleTime = 2 * time.Second

// callTimeout begrenst één aanroep.
const callTimeout = 20 * time.Second

var sky = &openmeteo.Client{}

var instance = &app{devices: map[string]*location{}}

type app struct {
	mu      sync.RWMutex
	stulp   *appsdk.Stulp
	devices map[string]*location
	order   []string
	cancel  context.CancelFunc

	// pending is de plek die de koppelpagina koos, klaar voor de volgende stap.
	pending []appsdk.PairedDevice
}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit: instance.start,
		OnStop: instance.stop,
		Drivers: map[string]appsdk.Driver{
			"location": locationDriver{},
		},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.mu.Lock()
	a.stulp = stulp
	a.mu.Unlock()
	sky.HTTP = openmeteo.DefaultHTTP()

	a.registerAPI(stulp)
	a.registerFlow(stulp)

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	go a.poll(ctx)
	return nil
}

func (a *app) stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *app) poll(ctx context.Context) {
	a.sweep(ctx)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sweep(ctx)
		}
	}
}

// sweep haalt per locatie het weer op.
//
// Eén aanroep per locatie en niet één voor alles: elke plek is een eigen
// coördinaat, en Open-Meteo antwoordt per punt. Ze staan achter elkaar en niet
// naast elkaar -- twee locaties zijn geen reden om een gratis dienst met
// gelijktijdige vragen te bestoken.
func (a *app) sweep(ctx context.Context) {
	for _, target := range a.targets() {
		call, cancel := context.WithTimeout(ctx, callTimeout)
		weather, err := sky.Current(call, target.latitude, target.longitude)
		cancel()
		if err != nil {
			target.unreachable(err)
			continue
		}
		target.apply(weather)
	}
}

func (a *app) targets() []*location {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*location, 0, len(a.order))
	for _, id := range a.order {
		if target, ok := a.devices[id]; ok {
			out = append(out, target)
		}
	}
	return out
}

func (a *app) watch(deviceID string, target *location) {
	a.mu.Lock()
	if _, known := a.devices[deviceID]; !known {
		a.order = append(a.order, deviceID)
	}
	a.devices[deviceID] = target
	a.mu.Unlock()
}

func (a *app) forget(deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.devices, deviceID)
	for i, id := range a.order {
		if id == deviceID {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

// refreshSoon haalt een ronde naar voren, na een tel bedenktijd.
func (a *app) refreshSoon() {
	go func() {
		time.Sleep(settleTime)
		a.sweep(context.Background())
	}()
}

// trigger vuurt een kaart voor één locatie.
func (a *app) trigger(card string, device *appsdk.Device, state, tokens map[string]any) {
	a.mu.RLock()
	stulp := a.stulp
	a.mu.RUnlock()
	if stulp == nil {
		return
	}
	stulp.TriggerDeviceFlow(card, device, tokens, state)
}

// locationFor zoekt de locatie op die een kaart aanwijst.
func (a *app) locationFor(stulpID string) (*location, error) {
	a.mu.RLock()
	target, ok := a.devices[stulpID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("die locatie hoort niet bij deze app")
	}
	return target, nil
}

// registerFlow hangt de kaarten op.
//
// Eén regel door alles heen: een ALS-kaart vuurt op de overgang, een EN-kaart
// kijkt naar hoe het nú is. En een drempelkaart vuurt bij het pásseren van zijn
// grens, niet bij elke verandering erboven -- anders gaat de zonwering elke
// meting opnieuw naar binnen.
func (a *app) registerFlow(stulp *appsdk.Stulp) {
	// --- Neerslag ---------------------------------------------------------

	// Begint en stopt: de overgang is de kaart, dus geen argument.
	stulp.OnFlowTrigger("rain_started", always)
	stulp.OnFlowTrigger("rain_stopped", always)

	// Regen op komst. De kaart kiest de termijn; de verwachting van vóór en na
	// deze ronde gaat mee, zodat hij vuurt zodra er neerslag in dat venster
	// verschijnt en niet elke ronde zolang die er staat.
	stulp.OnFlowTrigger("rain_expected", func(args, state map[string]any) (bool, error) {
		window, ok := number(args["within"])
		if !ok {
			window = 60
		}
		now, _ := number(state[fmt.Sprintf("in%d", int(window))])
		was, _ := number(state[fmt.Sprintf("was%d", int(window))])
		return now > 0 && was == 0, nil
	})

	// --- Wind -------------------------------------------------------------

	stulp.OnFlowTrigger("wind_changed", crossesUp("speed"))
	// Windstoten, en dat is niet hetzelfde: een zonwering breekt op een uitschieter
	// en niet op het gemiddelde. Vandaar een eigen kaart.
	stulp.OnFlowTrigger("gust_changed", crossesUp("speed"))

	// --- Temperatuur ------------------------------------------------------

	stulp.OnFlowTrigger("temperature_rose", crossesUp("celsius"))
	stulp.OnFlowTrigger("temperature_fell", crossesDown("celsius"))

	// --- Zon, zicht en onweer ---------------------------------------------

	stulp.OnFlowTrigger("uv_changed", crossesUp("index"))
	// Zicht loopt de andere kant op: mist is een dálende waarde.
	stulp.OnFlowTrigger("visibility_changed", crossesDown("distance"))
	stulp.OnFlowTrigger("thunder_near", crossesUp("energy"))

	stulp.OnFlowTrigger("weather_changed", func(args, state map[string]any) (bool, error) {
		wanted, _ := args["state"].(string)
		if wanted == "" || wanted == "any" {
			return true, nil
		}
		got, _ := state["state"].(string)
		return got == wanted, nil
	})

	// --- Voorwaarden ------------------------------------------------------
	//
	// Deze halen verse gegevens op: een voorwaarde wordt gewogen op het moment
	// dat een Flow loopt, en tien minuten oude regen is dan het verkeerde
	// antwoord. Het kost één aanroep, en alleen als er werkelijk een Flow draait.

	a.condition(stulp, "is_raining", func(w openmeteo.Weather, args map[string]any) (bool, error) {
		return w.Raining(), nil
	})
	// De grens komt binnen in meters per seconde, ook als de gebruiker hem in
	// Beaufort intypte: Stulp rekent hem om voordat de kaart hem ziet. Vandaar de
	// meting zelf en niet de windkracht -- 6 Bft is een gebied, en dat gebied
	// begint bij 10,8 m/s.
	a.condition(stulp, "wind_above", threshold("speed", func(w openmeteo.Weather) float64 {
		return w.WindMs
	}))
	a.condition(stulp, "gust_above", threshold("speed", func(w openmeteo.Weather) float64 {
		return w.GustMs
	}))
	a.condition(stulp, "temperature_above", threshold("celsius", func(w openmeteo.Weather) float64 {
		return w.TemperatureC
	}))
	a.condition(stulp, "uv_above", threshold("index", func(w openmeteo.Weather) float64 {
		return w.UVIndex
	}))
	a.condition(stulp, "is_sunny", func(w openmeteo.Weather, args map[string]any) (bool, error) {
		limit, ok := number(args["cloud"])
		if !ok {
			limit = 30
		}
		return w.Sunny(limit), nil
	})
	a.condition(stulp, "is_day", func(w openmeteo.Weather, args map[string]any) (bool, error) {
		return w.Day, nil
	})
	a.condition(stulp, "rain_today", threshold("chance", func(w openmeteo.Weather) float64 {
		return w.RainChancePct
	}))
	a.condition(stulp, "warmer_today", threshold("celsius", func(w openmeteo.Weather) float64 {
		return w.MaxC
	}))
	// Vorst vannacht kijkt naar het minimum van morgen: dat is de nacht die komt.
	// Het minimum van vandaag lag vannacht al achter ons.
	a.condition(stulp, "frost_tonight", func(w openmeteo.Weather, args map[string]any) (bool, error) {
		limit, ok := number(args["celsius"])
		if !ok {
			limit = 0
		}
		return w.MinTonightC <= limit, nil
	})
	// De tuin: verdamping min neerslag. Nul betekent dat de regen het werk deed.
	a.condition(stulp, "garden_dry", threshold("millimetres", func(w openmeteo.Weather) float64 {
		return w.IrrigationNeedMm()
	}))
	a.condition(stulp, "weather_is", func(w openmeteo.Weather, args map[string]any) (bool, error) {
		wanted, _ := args["state"].(string)
		if wanted == "" {
			return false, fmt.Errorf("kies eerst een weertype")
		}
		return string(openmeteo.StateOf(w.Code)) == wanted, nil
	})
}

// always is voor een kaart waarvan de overgang zelf al de voorwaarde is.
func always(map[string]any, map[string]any) (bool, error) { return true, nil }

// crossesUp maakt een handler die vuurt als de grens omhoog gepasseerd wordt.
func crossesUp(argument string) func(args, state map[string]any) (bool, error) {
	return func(args, state map[string]any) (bool, error) {
		limit, ok := number(args[argument])
		if !ok {
			return false, nil
		}
		now, _ := number(state["now"])
		was, _ := number(state["was"])
		return now >= limit && was < limit, nil
	}
}

// crossesDown is hetzelfde de andere kant op: mist en zicht dalen.
func crossesDown(argument string) func(args, state map[string]any) (bool, error) {
	return func(args, state map[string]any) (bool, error) {
		limit, ok := number(args[argument])
		if !ok {
			return false, nil
		}
		now, _ := number(state["now"])
		was, _ := number(state["was"])
		return now <= limit && was > limit, nil
	}
}

// threshold maakt een voorwaarde die een gemeten waarde tegen een grens legt.
func threshold(argument string, of func(openmeteo.Weather) float64) func(openmeteo.Weather, map[string]any) (bool, error) {
	return func(weather openmeteo.Weather, args map[string]any) (bool, error) {
		limit, ok := number(args[argument])
		if !ok {
			return false, fmt.Errorf("vul eerst een grens in")
		}
		return of(weather) >= limit, nil
	}
}

// condition hangt een EN-kaart op die verse gegevens nodig heeft.
func (a *app) condition(stulp *appsdk.Stulp, id string, weigh func(openmeteo.Weather, map[string]any) (bool, error)) {
	stulp.OnFlowCondition(id, func(args, _ map[string]any) (bool, error) {
		weather, err := a.look(args)
		if err != nil {
			return false, err
		}
		return weigh(weather, args)
	})
}

// look haalt het weer op voor de locatie die een EN-kaart aanwijst.
//
// Verse gegevens en niet de laatste ronde: een voorwaarde wordt gewogen op het
// moment dat een Flow loopt, en tien minuten oude regen is dan het verkeerde
// antwoord. Het kost één aanroep, en alleen als er werkelijk een Flow draait.
func (a *app) look(args map[string]any) (openmeteo.Weather, error) {
	target, err := a.locationFor(appsdk.DeviceArg(args, "device"))
	if err != nil {
		return openmeteo.Weather{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return sky.Current(ctx, target.latitude, target.longitude)
}

// number neemt wat een kaart bewaarde. Een keuzelijst levert tekst, een
// getalveld een float64.
func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%g", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
