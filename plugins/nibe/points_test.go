package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// pointsJSON is een verkorte versie van wat een S735 werkelijk stuurt: de
// velden die deze app leest, plus een paar die hij negeert en een voeler die er
// niet is.
const pointsJSON = `[
  {"parameterId":"4","parameterName":"Current outdoor temperature (BT1)","value":18.2,"parameterUnit":"°C"},
  {"parameterId":"8","value":38.2},
  {"parameterId":"10","value":30.5},
  {"parameterId":"11","value":51.7},
  {"parameterId":"44","value":-32768},
  {"parameterId":"1708","value":20.4},
  {"parameterId":"1756","value":0},
  {"parameterId":"1865","value":1.7},
  {"parameterId":"1975","value":1},
  {"parameterId":"3830","value":2},
  {"parameterId":"5927","value":25},
  {"parameterId":"7086","value":0},
  {"parameterId":"8121","value":1},
  {"parameterId":"26411","value":5.8},
  {"parameterId":"26945","value":145.2},
  {"parameterId":"29972","value":13},
  {"parameterId":"32628","value":52},
  {"parameterId":"47751","value":22},
  {"parameterId":"48351","value":22.9},
  {"parameterId":"55000","value":20},
  {"parameterId":"56150","value":0}
]`

// translate doet wat pollAll doet, maar dan zonder Stulp erbij: van wat de cloud
// stuurt naar wat er op de tegels komt.
func translate(points []myuplink.Point) map[string]any {
	out := map[string]any{}
	for _, point := range points {
		entry, mapped := readable[point.ParameterID]
		if !mapped {
			continue
		}
		if value, known := capabilityValue(entry, point); known {
			out[entry.capability] = value
		}
	}
	return out
}

// De hele weg van een antwoord van myUplink naar de waarden op de tegels. Dit is
// de vertaalslag waar alles op leunt: de cloud noemt alleen nummers.
func TestTheCloudAnswerBecomesCapabilityValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/devices/dev1/points" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(pointsJSON))
	}))
	defer server.Close()

	client := &myuplink.Client{BaseURL: server.URL, HTTP: server.Client(),
		Token: func(context.Context) (string, error) { return "t", nil }}
	points, err := client.Points(context.Background(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	got := translate(points)

	want := map[string]any{
		"measure_temperature.outdoor":           18.2,
		"measure_temperature.supply":            38.2,
		"measure_temperature.return":            30.5,
		"measure_temperature.calculated_supply": 20.4,
		"measure_temperature.hotwater":          52.0,
		"measure_temperature":                   22.9,
		"target_temperature":                    22.0,
		"hotwater_amount":                       13.0,
		"additional_heat_power":                 0.0,
		"add_heat_time_hotwater":                1.7,
		"add_heat_time_heating":                 5.8,
		"pump_speed":                            1.0,
		"compressor_frequency":                  25.0,
		"airflow":                               145.2,
		// Een enum wordt de tekst die de capability als id gebruikt, geen getal:
		// 20 is "warm water" en niet twintig van iets.
		"operating_priority": "20",
		"ventilation_mode":   "2",
		// En een aan/uit-punt wordt echt aan of uit.
		"hot_water_boost":   false,
		"ventilation_boost": true,
	}
	for capability, expected := range want {
		if got[capability] != expected {
			t.Errorf("%s = %#v, wil %#v", capability, got[capability], expected)
		}
	}
	for capability := range got {
		if _, expected := want[capability]; !expected {
			t.Errorf("%s = %#v kwam er onverwacht bij", capability, got[capability])
		}
	}
}

// Wat de app niet leest, komt er ook niet in. De pomp stuurt ruim tachtig
// waarden; een tegel voor elke daarvan is geen integratie maar een dump.
func TestUnmappedParametersAreLeftAlone(t *testing.T) {
	points := []myuplink.Point{{ParameterID: "56150", Value: float64Ptr(0)}}
	if got := translate(points); len(got) != 0 {
		t.Fatalf("ongebruikte parameter gaf %#v", got)
	}
}

// Een voeler die niets meldt laat de tegel staan zoals hij stond. Nul invullen
// zou een grafiek maken met een lijn naar beneden die er nooit geweest is.
func TestAnAbsentSensorWritesNothing(t *testing.T) {
	points := []myuplink.Point{
		{ParameterID: "4", Value: float64Ptr(-32768)},
		{ParameterID: "8", Value: nil},
	}
	if got := translate(points); len(got) != 0 {
		t.Fatalf("ontbrekende waarden gaven %#v", got)
	}
}

// Schrijven gaat de andere kant op, en met de echte waarde: 22 voor 22 graden,
// 1 voor aan, en het getal achter een keuze.
func TestCapabilityValuesBecomeApiValues(t *testing.T) {
	for _, test := range []struct {
		capability string
		value      any
		want       float64
		parameter  string
	}{
		{"target_temperature", 21.5, 21.5, "47751"},
		{"hot_water_boost", true, 1, "7086"},
		{"hot_water_boost", false, 0, "7086"},
		{"ventilation_boost", true, 1, "8121"},
		{"ventilation_mode", "3", 3, "3830"},
	} {
		entry, ok := sWritable[test.capability]
		if !ok {
			t.Fatalf("%s is niet schrijfbaar", test.capability)
		}
		if entry.parameter != test.parameter {
			t.Errorf("%s gaat naar parameter %s, wil %s", test.capability, entry.parameter, test.parameter)
		}
		got, err := apiValue(test.capability, entry.point, test.value)
		if err != nil || got != test.want {
			t.Errorf("%s(%v) = %v %v, wil %v", test.capability, test.value, got, err, test.want)
		}
	}
}

// Buiten bereik is een fout en geen stille correctie. Wie 40 graden vraagt hoort
// te horen dat de pomp op 35 stopt, in plaats van te denken dat het gelukt is.
func TestAnImpossibleSetpointFailsLoudly(t *testing.T) {
	entry := sWritable["target_temperature"]
	if _, err := apiValue("target_temperature", entry.point, 40.0); err == nil ||
		!strings.Contains(err.Error(), "tussen 5 en 35") {
		t.Fatalf("40 graden gaf %v", err)
	}
	mode := sWritable["ventilation_mode"]
	if _, err := apiValue("ventilation_mode", mode.point, "9"); err == nil {
		t.Fatal("een stand die niet bestaat werd geaccepteerd")
	}
}

func float64Ptr(value float64) *float64 { return &value }

// sWritable en fWritable zijn de schrijftabellen zoals een pomp van die serie ze
// oplevert: alsof die pomp precies zijn eigen nummers meldde.
var (
	sWritable = writeTable(reporting(sPoints))
	fWritable = writeTable(reporting(fPoints))
)

func reporting(table map[string]point) map[string]bool {
	out := map[string]bool{}
	for parameter := range table {
		out[parameter] = true
	}
	return out
}

// De twee series delen geen enkel nummer. Daar leunt de hele opzet op: één
// leestabel voor allebei, en de serie volgt uit wat er binnenkomt.
func TestTheTwoSeriesShareNoParameterNumber(t *testing.T) {
	for parameter := range sPoints {
		if _, both := fPoints[parameter]; both {
			t.Errorf("parameter %s staat in beide series", parameter)
		}
	}
	if len(readable) != len(sPoints)+len(fPoints) {
		t.Errorf("de leestabel telt %d punten, wil %d", len(readable), len(sPoints)+len(fPoints))
	}
}

// Dezelfde tegel, een ander nummer. Dit is waarom de schrijftabel per pomp
// gemaakt wordt en niet één keer voor de hele app: een gedeelde tabel zou hier
// willekeurig een van de twee winnen.
func TestTheSameTileWritesToADifferentNumberPerSeries(t *testing.T) {
	if got := sWritable["target_temperature"].parameter; got != "47751" {
		t.Errorf("S-serie schrijft target_temperature naar %s, wil 47751", got)
	}
	if got := fWritable["target_temperature"].parameter; got != "47398" {
		t.Errorf("F-serie schrijft target_temperature naar %s, wil 47398", got)
	}
	// Een F1255 heeft geen ventilatie. Die schuif hoort er dus niet te zijn, in
	// plaats van naar het S-nummer te schrijven dat deze pomp niet kent.
	if _, has := fWritable["ventilation_mode"]; has {
		t.Error("de F-serie biedt ventilation_mode aan, terwijl die pomp geen ventilatie heeft")
	}
}

// Een pomp die zijn nummers nog niet gemeld heeft, schrijft nergens naartoe. Dat
// is beter dan gokken welke serie het is.
func TestAPumpThatHasNotReportedYetWritesNowhere(t *testing.T) {
	if len(writeTable(nil)) != 0 {
		t.Error("een onbekende pomp levert toch een schrijftabel op")
	}
}

// De kaart voor extra warm water biedt uren aan; wat de pomp wil zien verschilt.
// De F-serie kent bovendien geen 24 en 48 uur, en dat hoort een fout te zijn en
// geen stil afronden naar twaalf.
func TestTheHotWaterCardTranslatesHoursPerSeries(t *testing.T) {
	for hours, want := range map[int]int{0: 0, 2: 2, 3: 3, 12: 12, 48: 48} {
		if got, ok := sBoost.raw[hours]; !ok || got != want {
			t.Errorf("S-serie: %d uur wordt %v (%v), wil %d", hours, got, ok, want)
		}
	}
	for hours, want := range map[int]int{0: 0, 2: 4, 3: 1, 6: 2, 12: 3} {
		if got, ok := fBoost.raw[hours]; !ok || got != want {
			t.Errorf("F-serie: %d uur wordt %v (%v), wil %d", hours, got, ok, want)
		}
	}
	for _, hours := range []int{24, 48} {
		if _, ok := fBoost.raw[hours]; ok {
			t.Errorf("F-serie neemt %d uur aan, terwijl die serie dat niet kent", hours)
		}
	}
}

// Een F-serie meldt geen vermogen en geen meterstand. De zes energie-tegels
// horen dan weg te blijven in plaats van voor altijd leeg te staan.
func TestAPumpWithoutAMeterGetsNoEnergyTiles(t *testing.T) {
	f := backedCapabilities(reporting(fPoints), energyPoints{})
	for _, capability := range energyCapabilities {
		if f[capability] {
			t.Errorf("%s staat aan terwijl deze pomp geen meter heeft", capability)
		}
	}
	if !f["measure_temperature.outdoor"] || !f["target_temperature"] {
		t.Error("een F-serie krijgt zijn eigen tegels niet")
	}
	if f["airflow"] || f["ventilation_mode"] {
		t.Error("een F-serie krijgt tegels voor ventilatie die hij niet heeft")
	}

	s := backedCapabilities(reporting(sPoints), sEnergy)
	for _, capability := range energyCapabilities {
		if !s[capability] {
			t.Errorf("%s blijft weg terwijl deze pomp wel een meter heeft", capability)
		}
	}
}
