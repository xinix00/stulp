package openmeteo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// De omrekening en de vertaalslag zijn waar deze app zijn geld verdient: de bron
// levert meters per seconde en een WMO-nummer, en een mens denkt in windkracht
// en in "het regent".

// De grenzen van de schaal van Beaufort, precies op de overgang.
func TestBeaufortMatchesTheScale(t *testing.T) {
	for _, test := range []struct {
		ms    float64
		force int
	}{
		{0, 0}, {0.2, 0}, {0.3, 1}, {1.5, 1}, {1.6, 2}, {3.3, 2}, {3.4, 3},
		{5.4, 3}, {5.5, 4}, {7.9, 4}, {8.0, 5}, {10.7, 5}, {10.8, 6},
		{13.8, 6}, {13.9, 7}, {17.1, 7}, {17.2, 8}, {20.7, 8}, {20.8, 9},
		{24.4, 9}, {24.5, 10}, {28.4, 10}, {28.5, 11}, {32.6, 11}, {32.7, 12},
		{50, 12},
	} {
		if got := Beaufort(test.ms); got != test.force {
			t.Errorf("%.1f m/s is %d Bft, wil %d", test.ms, got, test.force)
		}
	}
}

func TestCompass(t *testing.T) {
	for degrees, want := range map[float64]string{
		0: "N", 45: "NO", 90: "O", 135: "ZO", 180: "Z", 225: "ZW", 270: "W", 315: "NW",
		// Rond de streep, en voorbij een hele draai.
		11: "N", 12: "NNO", 348: "NNW", 349: "N", 360: "N", 370: "N",
	} {
		if got := Compass(degrees); got != want {
			t.Errorf("%g° is %q, wil %q", degrees, got, want)
		}
	}
}

// Elke WMO-code die Open-Meteo kan sturen hoort een stand én een beschrijving te
// hebben. Een gat hier is een tegel die "onbekend" zegt over gewoon weer.
func TestEveryWMOCodeIsCovered(t *testing.T) {
	codes := []int{0, 1, 2, 3, 45, 48, 51, 53, 55, 56, 57, 61, 63, 65, 66, 67,
		71, 73, 75, 77, 80, 81, 82, 85, 86, 95, 96, 99}
	for _, code := range codes {
		if state := StateOf(code); state == Unknown {
			t.Errorf("code %d heeft geen stand", code)
		}
		if description := Describe(code); description == "" || contains(description, "Onbekend") {
			t.Errorf("code %d heeft geen beschrijving: %q", code, description)
		}
	}
	// En een code die de WMO later toevoegt verdwijnt niet stil: het nummer
	// staat in de tekst, zodat hij opzoekbaar is.
	if got := Describe(123); got == "" || !contains(got, "123") {
		t.Errorf("een onbekende code levert %q, wil het nummer erin", got)
	}
	if StateOf(123) != Unknown {
		t.Error("een onbekende code krijgt een stand toegewezen")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// IJzel is geen zwaardere regen maar een ander gevaar, en hoort dus niet onder
// dezelfde stand te vallen.
func TestFreezingIsItsOwnState(t *testing.T) {
	for _, code := range []int{56, 57, 66, 67} {
		if StateOf(code) != Freezing {
			t.Errorf("code %d is %q, wil freezing", code, StateOf(code))
		}
	}
	if StateOf(65) != Rain {
		t.Error("zware regen werd ijzel")
	}
}

func TestCurrentReadsWhatTheServiceSends(t *testing.T) {
	var asked url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query()
		w.Write([]byte(`{
		  "utc_offset_seconds": 7200,
		  "current": {"time":"2026-08-10T09:15","temperature_2m":20.7,"relative_humidity_2m":78,
		    "apparent_temperature":21.9,"is_day":1,"precipitation":0.4,"rain":0.4,"weather_code":61,
		    "cloud_cover":72,"pressure_msl":1013.3,"wind_speed_10m":11.0,"wind_direction_10m":282,
		    "wind_gusts_10m":18.0},
		  "daily": {"temperature_2m_max":[24.0],"temperature_2m_min":[16.0],"precipitation_sum":[2.5],
		    "wind_speed_10m_max":[12.5],"sunrise":["2026-08-10T06:15"],"sunset":["2026-08-10T21:13"]}
		}`))
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), BaseURL: server.URL}

	weather, err := client.Current(context.Background(), 52.1, 5.18)
	if err != nil {
		t.Fatal(err)
	}

	// Meters per seconde vragen, want daar is Beaufort op gedefinieerd.
	if asked.Get("wind_speed_unit") != "ms" {
		t.Errorf("de eenheid was %q", asked.Get("wind_speed_unit"))
	}
	if weather.Beaufort() != 6 || weather.GustBeaufort() != 8 {
		t.Errorf("11 m/s met uitschieters van 18 werd %d/%d Bft, wil 6/8",
			weather.Beaufort(), weather.GustBeaufort())
	}
	if !weather.Raining() {
		t.Error("0,4 mm neerslag geldt niet als regen")
	}
	if StateOf(weather.Code) != Rain {
		t.Errorf("code 61 werd %q", StateOf(weather.Code))
	}
	if Compass(weather.WindDegrees) != "WNW" {
		t.Errorf("282° werd %q", Compass(weather.WindDegrees))
	}
	// De tijden komen zonder zone terug met de verschuiving apart; zonder die
	// verschuiving zou de zonsopkomst er twee uur naast liggen.
	if weather.Sunrise.Hour() != 6 || weather.Sunrise.Minute() != 15 {
		t.Errorf("zonsopkomst is %v", weather.Sunrise)
	}
	if _, offset := weather.At.Zone(); offset != 7200 {
		t.Errorf("de verschuiving is %d seconden, wil 7200", offset)
	}
}

// Droog is droog: geen neerslag mag niet als regen gelden, anders vuurt "het
// begint te regenen" bij elke ronde.
func TestNoPrecipitationIsNotRain(t *testing.T) {
	if (Weather{PrecipitationMm: 0}).Raining() {
		t.Error("nul millimeter geldt als regen")
	}
	if !(Weather{PrecipitationMm: 0.1}).Raining() {
		t.Error("een tiende millimeter geldt niet als regen")
	}
}

func TestSearchAndPlaceLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") == "" {
			t.Error("er ging een zoekvraag zonder naam uit")
		}
		w.Write([]byte(`{"results":[
		  {"name":"Nijmegen","admin1":"Gelderland","country_code":"NL","latitude":51.84,"longitude":5.85,"population":177359},
		  {"name":"Nijmegen","admin1":"North Carolina","country_code":"US","latitude":35.13,"longitude":-79.02}]}`))
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), GeocodingURL: server.URL}

	places, err := client.Search(context.Background(), "Nijmegen", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(places) != 2 {
		t.Fatalf("treffers: %+v", places)
	}
	// De regel moet de twee Nijmegens uit elkaar houden, anders is kiezen gokken.
	if places[0].Where() != "Nijmegen, Gelderland NL" {
		t.Errorf("eerste regel is %q", places[0].Where())
	}
	if places[1].Where() == places[0].Where() {
		t.Error("twee plaatsen met dezelfde naam lezen hetzelfde")
	}
}

// Zoeken zonder naam hoort geen aanroep te doen: een leeg veld is geen vraag.
func TestAnEmptySearchAsksNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("er ging een aanroep uit voor een lege zoekopdracht")
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), GeocodingURL: server.URL}

	places, err := client.Search(context.Background(), "  ", 10)
	if err != nil || len(places) != 0 {
		t.Errorf("kreeg %v %v", places, err)
	}
}

// Een klacht van de dienst hoort doorgegeven te worden: die zegt wélke parameter
// niet kan, en dat is meer dan "http 400".
func TestTheServiceReasonReachesTheUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":true,"reason":"Latitude must be in range of -90 to 90°."}`))
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), BaseURL: server.URL}

	_, err := client.Current(context.Background(), 999, 5)
	if err == nil {
		t.Fatal("een 400 gaf geen fout")
	}
	if !contains(err.Error(), "Latitude must be in range") {
		t.Errorf("de melding is %q", err.Error())
	}
	// En het verzoek erbij, zodat je ziet wat er gevraagd werd.
	if !contains(err.Error(), "latitude=999") {
		t.Errorf("de melding noemt het verzoek niet: %q", err.Error())
	}
}
