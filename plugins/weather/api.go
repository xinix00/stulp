package main

import (
	"context"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/weather/internal/openmeteo"
)

// De koppelpagina en de instellingenpagina.
//
// Er is niets in te stellen -- Open-Meteo vraagt geen sleutel en geen account --
// dus de instellingenpagina laat zien welke locaties gevolgd worden en of ze
// antwoorden. De koppelpagina zoekt een plaats op naam; dat is het enige wat de
// gebruiker weet en de dienst niet.

func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("status", func(map[string]any, map[string]any) (any, error) {
		found := []map[string]any{}
		for _, target := range a.targets() {
			last, known := target.weather()
			entry := map[string]any{
				"name":      target.device.Name(),
				"latitude":  target.latitude,
				"longitude": target.longitude,
				"answered":  known,
			}
			if known {
				entry["state"] = string(openmeteo.StateOf(last.Code))
				entry["description"] = openmeteo.Describe(last.Code)
				// Measure zegt in welke eenheid dit gemeten is; Stulp maakt er de
				// eenheid van dit huis van, met de tekst erbij. Zo leest deze pagina
				// hetzelfde als de tegel ernaast.
				entry["temperature"] = appsdk.Measure(round(last.TemperatureC), "°C")
			}
			found = append(found, entry)
		}
		return map[string]any{"locations": found}, nil
	})

	// search is voor de koppelpagina: van een plaatsnaam naar een coördinaat.
	stulp.OnRequest("search", func(_, body map[string]any) (any, error) {
		name, _ := body["name"].(string)
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		places, err := sky.Search(ctx, name, 10)
		if err != nil {
			return nil, err
		}
		found := []map[string]any{}
		for _, place := range places {
			found = append(found, map[string]any{
				"name": place.Name, "where": place.Where(),
				"latitude": place.Latitude, "longitude": place.Longitude,
				"people": place.People,
			})
		}
		return map[string]any{"places": found, "found": len(found)}, nil
	})

	// peek laat de koppelpagina zien wat het daar nú doet, zodat je weet dat je
	// de goede plek koos voordat je hem toevoegt.
	stulp.OnRequest("peek", func(_, body map[string]any) (any, error) {
		latitude, okLat := coordinate(body["latitude"])
		longitude, okLon := coordinate(body["longitude"])
		if !okLat || !okLon {
			return nil, errNoCoordinate
		}
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		weather, err := sky.Current(ctx, latitude, longitude)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"description": openmeteo.Describe(weather.Code),
			"temperature": appsdk.Measure(round(weather.TemperatureC), "°C"),
			"wind":        appsdk.Measure(round(weather.WindMs), "m/s"),
			"direction":   openmeteo.Compass(weather.WindDegrees),
			"raining":     weather.Raining(),
		}, nil
	})
}
