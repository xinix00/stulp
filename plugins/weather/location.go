package main

import (
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/weather/internal/openmeteo"
)

// Eén locatie als apparaat.
//
// Dat is de nette vorm en niet een adres in de app-instellingen: zo kun je er
// meer dan één hebben -- thuis, het vakantiehuis, de plek waar je zo naartoe
// rijdt -- en elke Flow-kaart wijst het aan zoals hij elke andere sensor
// aanwijst. Een locatie is verder een apparaat als alle andere: hij meldt
// waarden en neemt niets aan.

type locationDriver struct{}

type location struct {
	device    *appsdk.Device
	latitude  float64
	longitude float64

	// Wat er het laatst gemeld was, om te kunnen zien dát iets veranderde. Een
	// Flow die op "het begint te regenen" wacht heeft die overgang nodig, en die
	// bestaat alleen tegenover de vorige ronde.
	mu    sync.Mutex
	known bool
	last  openmeteo.Weather
}

func (locationDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	latitude, okLat := coordinate(device.Data()["latitude"])
	longitude, okLon := coordinate(device.Data()["longitude"])
	if !okLat || !okLon {
		return nil, fmt.Errorf("deze locatie heeft geen coördinaat; voeg hem opnieuw toe")
	}
	return &location{device: device, latitude: latitude, longitude: longitude}, nil
}

// coordinate neemt aan wat er over het protocol binnenkomt. JSON levert een
// float64; een tekst komt voor als iemand een locatie via de CLI toevoegde.
func coordinate(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case string:
		number, err := strconv.ParseFloat(typed, 64)
		return number, err == nil
	}
	return 0, false
}

func (l *location) OnInit() error {
	instance.watch(l.device.ID(), l)
	instance.refreshSoon()
	return nil
}

func (l *location) OnDeleted() { instance.forget(l.device.ID()) }

// apply zet neer wat er gemeten is, en meldt de overgangen.
func (l *location) apply(weather openmeteo.Weather) {
	if err := l.device.SetAvailable(); err != nil {
		l.device.Error(err.Error())
	}

	values := map[string]any{
		"measure_temperature":       round(weather.TemperatureC),
		"measure_temperature.feels": round(weather.FeelsLikeC),
		"measure_humidity":          round(weather.HumidityPct),
		"measure_pressure":          round(weather.PressureHpa),
		"measure_rain":              round(weather.PrecipitationMm),
	}
	// Wind in meters per seconde en niet in Beaufort: dat is wat er gemeten is.
	// Beaufort is een schaal die uit die meting volgt, en in welke eenheid iemand
	// hem wíl lezen -- Bft, km/h, mph -- is een keuze van de gebruiker. Die staat
	// in de instellingen van Stulp, en Stulp rekent hem om voor de tegel. Zou de
	// plugin hier al 3 melden, dan was de precisie weg en kon niemand er nog
	// kilometers per uur van maken.
	values["measure_wind_strength"] = round(weather.WindMs)
	values["measure_gust_strength"] = round(weather.GustMs)
	values["measure_wind_angle"] = math.Round(weather.WindDegrees)
	values["cloud_cover"] = round(weather.CloudPct)
	values["weather_state"] = string(openmeteo.StateOf(weather.Code))
	values["weather_description"] = openmeteo.Describe(weather.Code)
	values["measure_ultraviolet"] = round(weather.UVIndex)
	values["measure_temperature.dewpoint"] = round(weather.DewPointC)
	values["measure_temperature.soil"] = round(weather.SoilC)
	// Zicht in kilometers en niet in meters: 20120 op een tegel leest niemand.
	values["visibility"] = round(weather.VisibilityM / 1000)
	values["rain_chance"] = round(weather.RainChancePct)
	values["irrigation_need"] = round(weather.IrrigationNeedMm())
	if err := l.device.SetCapabilityValues(values); err != nil {
		l.device.Error(err.Error())
	}

	l.fire(weather)
}

// fire meldt wat er veranderd is sinds de vorige ronde.
//
// Alleen overgangen, want dat is wat een ALS-kaart betekent: "het begint te
// regenen" hoort één keer te vuren als het begint, en niet elke ronde zolang het
// regent. De eerste ronde na een herstart vuurt niets -- er is dan geen
// voorganger, en iets melden zou een Flow starten om een regenbui die al liep.
func (l *location) fire(now openmeteo.Weather) {
	l.mu.Lock()
	known, was := l.known, l.last
	l.known, l.last = true, now
	l.mu.Unlock()
	if !known {
		return
	}

	tokens := l.tokens(now)

	// Regen: de overgang zelf is de kaart, dus geen argument.
	switch {
	case now.Raining() && !was.Raining():
		instance.trigger("rain_started", l.device, nil, tokens)
	case !now.Raining() && was.Raining():
		instance.trigger("rain_stopped", l.device, nil, tokens)
	}

	// Elke drempelkaart krijgt "nu" en "was" mee. De kaart houdt zijn eigen
	// grens vast en kijkt of die er tussen ligt; zo vuurt hij één keer bij het
	// passeren en niet bij elke verandering erboven.
	//
	// Alles hier is de canonieke eenheid: meters per seconde, graden Celsius,
	// kilometers. De grens die de kaart meekrijgt is dat ook -- Stulp rekent wat
	// de gebruiker intypte om voordat het de plugin bereikt.
	for card, pair := range map[string][2]float64{
		"wind_changed":       {now.WindMs, was.WindMs},
		"gust_changed":       {now.GustMs, was.GustMs},
		"temperature_rose":   {now.TemperatureC, was.TemperatureC},
		"temperature_fell":   {now.TemperatureC, was.TemperatureC},
		"uv_changed":         {now.UVIndex, was.UVIndex},
		"visibility_changed": {now.VisibilityM / 1000, was.VisibilityM / 1000},
		"thunder_near":       {now.ThunderRisk(), was.ThunderRisk()},
	} {
		if pair[0] == pair[1] {
			continue
		}
		instance.trigger(card, l.device,
			map[string]any{"now": pair[0], "was": pair[1]}, tokens)
	}

	// Regen op komst: de kaart kiest de termijn, dus hier gaat de hele
	// verwachting mee in plaats van één getal.
	instance.trigger("rain_expected", l.device, map[string]any{
		"in15": now.RainWithin(15), "was15": was.RainWithin(15),
		"in30": now.RainWithin(30), "was30": was.RainWithin(30),
		"in60": now.RainWithin(60), "was60": was.RainWithin(60),
		"in120": now.RainWithin(120), "was120": was.RainWithin(120),
	}, tokens)

	if state, before := openmeteo.StateOf(now.Code), openmeteo.StateOf(was.Code); state != before {
		instance.trigger("weather_changed", l.device,
			map[string]any{"state": string(state)}, tokens)
	}
}

// weather levert de laatste meting, voor een EN-kaart die niets verse nodig
// heeft. ok is vals zolang er nog geen ronde geweest is.
func (l *location) weather() (openmeteo.Weather, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last, l.known
}

// tokens is wat elke kaart van deze locatie meekrijgt.
//
// Ook hier canoniek: app.json zegt van elk token in welke eenheid het gemeten is,
// en Stulp zet het in de eenheid van dit huis zodra het in een zin komt. Een
// pushbericht met "windkracht {{wind_speed}}" leest dus "6 Bft" of "25 mph",
// zonder dat deze app weet welke van de twee.
func (l *location) tokens(weather openmeteo.Weather) map[string]any {
	return map[string]any{
		"description": openmeteo.Describe(weather.Code),
		"temperature": round(weather.TemperatureC),
		"wind_speed":  round(weather.WindMs),
		"gust_speed":  round(weather.GustMs),
		"wind":        openmeteo.Compass(weather.WindDegrees),
		"rain":        round(weather.PrecipitationMm),
		"code":        weather.Code,
		"uv":          round(weather.UVIndex),
		"humidity":    round(weather.HumidityPct),
		"rain_soon":   round(weather.RainWithin(60)),
		"max_today":   round(weather.MaxC),
		"min_tonight": round(weather.MinTonightC),
	}
}

// unreachable: Open-Meteo antwoordde niet. Dat is iets anders dan windstil.
func (l *location) unreachable(err error) {
	l.device.SetUnavailable("Open-Meteo antwoordt niet: " + err.Error())
}

// round houdt één decimaal over. Het weer is niet nauwkeuriger dan dat, en een
// tegel met 20,700000000000003 graden erop is nauwkeuriger doen dan het is.
func round(value float64) float64 { return math.Round(value*10) / 10 }

// errNoCoordinate is wat een pagina krijgt die om weer vraagt zonder plek.
var errNoCoordinate = fmt.Errorf("er is geen coördinaat om het weer van op te vragen")

var (
	_ appsdk.Driver        = locationDriver{}
	_ appsdk.Pairer        = locationDriver{}
	_ appsdk.DeviceHandler = (*location)(nil)
	_ appsdk.Deleter       = (*location)(nil)
)
