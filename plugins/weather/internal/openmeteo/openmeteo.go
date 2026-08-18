// Package openmeteo haalt het weer op een coördinaat op bij Open-Meteo.
//
// Waarom niet KNMI, waar dit om begon: KNMI's open data vraagt een sleutel en
// levert bestanden -- NetCDF, HDF5, GRIB. Dat zijn binaire formaten waar een
// plugin een zware bibliotheek voor nodig heeft, en een installatie zonder
// weerstation zou daarvoor meebetalen. Open-Meteo vraagt geen sleutel, antwoordt
// in JSON, en neemt een coördinaat aan in plaats van een meetstation -- precies
// wat een plugin met een apparaat per locatie nodig heeft. PORTED.md zegt wat er
// gemeten is en wat KNMI wél zou toevoegen.
package openmeteo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/units"
)

// maxBody begrenst een antwoord. Een weerbericht voor één punt is een paar
// kilobyte; wat daar ver boven zit is geen antwoord dat deze app verwacht.
const maxBody = 1 << 20

const (
	forecastURL  = "https://api.open-meteo.com/v1/forecast"
	geocodingURL = "https://geocoding-api.open-meteo.com/v1/search"
)

// DefaultHTTP is de client die deze app gebruikt.
func DefaultHTTP() *http.Client { return &http.Client{Timeout: 20 * time.Second} }

// Client is de kant van Open-Meteo. Er is niets te authenticeren.
type Client struct {
	HTTP *http.Client
	// BaseURL en GeocodingURL zijn te vervangen voor een test. Leeg is de echte.
	BaseURL      string
	GeocodingURL string
}

// Place is een plaats zoals het zoeken hem teruggeeft.
type Place struct {
	Name      string  `json:"name"`
	Country   string  `json:"country"`
	Code      string  `json:"country_code"`
	Region    string  `json:"admin1"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Elevation float64 `json:"elevation"`
	Timezone  string  `json:"timezone"`
	People    int     `json:"population"`
}

// Where is de plaats in één regel: "Nijmegen, Gelderland (NL)".
func (p Place) Where() string {
	parts := make([]string, 0, 2)
	if p.Region != "" && p.Region != p.Name {
		parts = append(parts, p.Region)
	}
	if p.Code != "" {
		parts = append(parts, p.Code)
	}
	if len(parts) == 0 {
		return p.Name
	}
	return p.Name + ", " + strings.Join(parts, " ")
}

// Search zoekt een plaats op naam. Zonder treffers is de lijst leeg en dat is
// geen fout: niet elke tikfout is een storing.
func (c *Client) Search(ctx context.Context, name string, limit int) ([]Place, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	var answer struct {
		Results []Place `json:"results"`
	}
	err := c.getJSON(ctx, c.geocoding(), url.Values{
		"name": {name}, "count": {strconv.Itoa(limit)},
		"language": {"nl"}, "format": {"json"},
	}, &answer)
	if err != nil {
		return nil, err
	}
	return answer.Results, nil
}

// Weather is het weer op één punt: nu, en het vooruitzicht voor vandaag.
type Weather struct {
	// At is het moment waarop deze waarden gelden, in de tijdzone van de plaats.
	At time.Time

	TemperatureC float64
	FeelsLikeC   float64
	HumidityPct  float64
	PressureHpa  float64
	CloudPct     float64
	// PrecipitationMm is wat er in het laatste kwartier viel: regen, sneeuw of
	// hagel bij elkaar. RainMm is daarvan alleen de regen.
	PrecipitationMm float64
	RainMm          float64
	WindMs          float64
	GustMs          float64
	WindDegrees     float64
	Code            int
	Day             bool

	DewPointC   float64
	VisibilityM float64
	UVIndex     float64
	SoilC       float64
	SnowCm      float64

	// Vandaag, uit het dagbericht.
	MaxC          float64
	MinC          float64
	RainTodayMm   float64
	MaxWindMs     float64
	MaxGustMs     float64
	RainChancePct float64
	RainHours     float64
	MaxUVIndex    float64
	// EvaporationMm is de referentieverdamping van vandaag (ET0 volgens de FAO):
	// hoeveel water een gewas vandaag kwijtraakt. Min de neerslag is dat wat een
	// tuin tekortkomt.
	EvaporationMm float64
	Sunrise       time.Time
	Sunset        time.Time

	// MinTonightC is het minimum van morgen: dat is de nacht die komt. Voor "het
	// vriest vannacht" is het minimum van vandaag het verkeerde getal -- dat lag
	// vannacht al achter ons.
	MinTonightC float64

	// Soon is de neerslag per kwartier voor de komende twee uur, en de kans op
	// onweer daarbij. Hiermee kan een kaart vóór de bui reageren in plaats van
	// erna.
	Soon []Quarter
}

// Quarter is één kwartier vooruit.
type Quarter struct {
	At        time.Time
	RainMm    float64
	Cape      float64
	Lightning float64
}

// RainWithin is hoeveel neerslag er in de komende minuten verwacht wordt.
//
// Op de kwartierverwachting en niet op de huidige meting: dat is het verschil
// tussen "de zonwering staat nog uit als het begint" en "hij is al binnen".
func (w Weather) RainWithin(minutes int) float64 {
	total := 0.0
	limit := w.At.Add(time.Duration(minutes) * time.Minute)
	for _, quarter := range w.Soon {
		if quarter.At.After(limit) {
			break
		}
		total += quarter.RainMm
	}
	return total
}

// ThunderRisk is de hoogste onweersaanleg in de komende twee uur, in J/kg.
//
// CAPE is de energie die in de lucht zit; lightning_potential is de kans dat er
// werkelijk ontlading uit komt. De hoogste van de twee schalen is genomen omdat
// een model soms het een en soms het ander vult.
func (w Weather) ThunderRisk() float64 {
	highest := 0.0
	for _, quarter := range w.Soon {
		if quarter.Cape > highest {
			highest = quarter.Cape
		}
		if quarter.Lightning*100 > highest {
			highest = quarter.Lightning * 100
		}
	}
	return highest
}

// IrrigationNeedMm is wat een tuin vandaag tekortkomt: de verdamping min wat er
// gevallen is. Nul of minder betekent dat de regen het werk deed.
func (w Weather) IrrigationNeedMm() float64 {
	need := w.EvaporationMm - w.RainTodayMm
	if need < 0 {
		return 0
	}
	return need
}

// Sunny zegt of de zon werkelijk schijnt: bewolking onder de grens én dag.
func (w Weather) Sunny(cloudBelowPct float64) bool {
	return w.Day && w.CloudPct < cloudBelowPct
}

// Beaufort is de windkracht. Dat is wat een mens bedoelt met "windkracht boven
// zes"; meters per seconde zegt niemand.
func (w Weather) Beaufort() int { return Beaufort(w.WindMs) }

// GustBeaufort is hetzelfde voor de uitschieters.
func (w Weather) GustBeaufort() int { return Beaufort(w.GustMs) }

// Raining zegt of er nu neerslag valt.
//
// Op de laatste meting en niet op een voorspelling: "het regent nu" hoort te
// gaan over buiten, niet over straks. Open-Meteo meet per kwartier, dus een bui
// van twee minuten kan er tussendoor vallen -- dat is de resolutie van de bron
// en niet iets om hier te verzinnen.
func (w Weather) Raining() bool { return w.PrecipitationMm > 0 }

// Beaufort zet meters per seconde om in de schaal van Beaufort.
//
// De grenzen zijn die van de schaal zelf: 0,3 / 1,6 / 3,4 / 5,5 / 8,0 / 10,8 /
// 13,9 / 17,2 / 20,8 / 24,5 / 28,5 / 32,7 m/s.
// De schaal zelf staat in internal/units, want Stulp gebruikt hem ook: daar is
// Beaufort een eenheid waarin je een tegel kunt lezen. Twee kopieën van dezelfde
// twaalf grenzen is één kopie te veel -- die lopen uit elkaar en dan waait het in
// een Flow harder dan op de tegel.
func Beaufort(ms float64) int { return units.Beaufort(ms) }

// Compass is de windrichting in woorden, in twaalf en een halve graad per punt.
func Compass(degrees float64) string {
	points := []string{"N", "NNO", "NO", "ONO", "O", "OZO", "ZO", "ZZO",
		"Z", "ZZW", "ZW", "WZW", "W", "WNW", "NW", "NNW"}
	if math.IsNaN(degrees) {
		return ""
	}
	index := int(math.Mod(math.Round(degrees/22.5), 16))
	if index < 0 {
		index += 16
	}
	return points[index]
}

// Current haalt het weer op één punt op.
func (c *Client) Current(ctx context.Context, latitude, longitude float64) (Weather, error) {
	var answer struct {
		UTCOffset int `json:"utc_offset_seconds"`
		Current   struct {
			Time        string  `json:"time"`
			Temperature float64 `json:"temperature_2m"`
			Humidity    float64 `json:"relative_humidity_2m"`
			Apparent    float64 `json:"apparent_temperature"`
			IsDay       int     `json:"is_day"`
			Precip      float64 `json:"precipitation"`
			Rain        float64 `json:"rain"`
			Code        int     `json:"weather_code"`
			Cloud       float64 `json:"cloud_cover"`
			Pressure    float64 `json:"pressure_msl"`
			Wind        float64 `json:"wind_speed_10m"`
			Direction   float64 `json:"wind_direction_10m"`
			Gusts       float64 `json:"wind_gusts_10m"`
			DewPoint    float64 `json:"dew_point_2m"`
			Visibility  float64 `json:"visibility"`
			UV          float64 `json:"uv_index"`
			Soil        float64 `json:"soil_temperature_0cm"`
			Snow        float64 `json:"snowfall"`
		} `json:"current"`
		Daily struct {
			Max         []float64 `json:"temperature_2m_max"`
			Min         []float64 `json:"temperature_2m_min"`
			Rain        []float64 `json:"precipitation_sum"`
			RainHours   []float64 `json:"precipitation_hours"`
			RainChance  []float64 `json:"precipitation_probability_max"`
			MaxWind     []float64 `json:"wind_speed_10m_max"`
			MaxGust     []float64 `json:"wind_gusts_10m_max"`
			MaxUV       []float64 `json:"uv_index_max"`
			Evaporation []float64 `json:"et0_fao_evapotranspiration"`
			Sunrise     []string  `json:"sunrise"`
			Sunset      []string  `json:"sunset"`
		} `json:"daily"`
		Minutely struct {
			Time      []string  `json:"time"`
			Rain      []float64 `json:"precipitation"`
			Cape      []float64 `json:"cape"`
			Lightning []float64 `json:"lightning_potential"`
		} `json:"minutely_15"`
	}
	err := c.getJSON(ctx, c.forecast(), url.Values{
		"latitude":  {strconv.FormatFloat(latitude, 'f', -1, 64)},
		"longitude": {strconv.FormatFloat(longitude, 'f', -1, 64)},
		"current": {"temperature_2m,relative_humidity_2m,apparent_temperature,is_day," +
			"precipitation,rain,snowfall,weather_code,cloud_cover,pressure_msl," +
			"wind_speed_10m,wind_direction_10m,wind_gusts_10m," +
			"dew_point_2m,visibility,uv_index,soil_temperature_0cm"},
		"daily": {"temperature_2m_max,temperature_2m_min,precipitation_sum,precipitation_hours," +
			"precipitation_probability_max,wind_speed_10m_max,wind_gusts_10m_max," +
			"uv_index_max,et0_fao_evapotranspiration,sunrise,sunset"},
		// Twee uur vooruit per kwartier: hiermee kan een kaart vóór de bui
		// reageren. Acht vakken is precies die twee uur.
		"minutely_15":          {"precipitation,cape,lightning_potential"},
		"forecast_minutely_15": {"8"},
		// Meters per seconde, want daar is de schaal van Beaufort op gedefinieerd;
		// km/h zou een omrekening toevoegen die niets oplost.
		"wind_speed_unit": {"ms"},
		"timezone":        {"auto"},
		// Twee dagen: het minimum van morgen is de nacht die komt, en dat is het
		// getal waar "het vriest vannacht" over gaat.
		"forecast_days": {"2"},
	}, &answer)
	if err != nil {
		return Weather{}, err
	}

	// De tijden komen zonder zone terug, met de verschuiving apart. Zonder die
	// verschuiving erop zou een zonsopkomst er twee uur naast liggen.
	zone := time.FixedZone("", answer.UTCOffset)
	weather := Weather{
		At:              parseLocal(answer.Current.Time, zone),
		TemperatureC:    answer.Current.Temperature,
		FeelsLikeC:      answer.Current.Apparent,
		HumidityPct:     answer.Current.Humidity,
		PressureHpa:     answer.Current.Pressure,
		CloudPct:        answer.Current.Cloud,
		PrecipitationMm: answer.Current.Precip,
		RainMm:          answer.Current.Rain,
		WindMs:          answer.Current.Wind,
		GustMs:          answer.Current.Gusts,
		WindDegrees:     answer.Current.Direction,
		Code:            answer.Current.Code,
		Day:             answer.Current.IsDay == 1,
		DewPointC:       answer.Current.DewPoint,
		VisibilityM:     answer.Current.Visibility,
		UVIndex:         answer.Current.UV,
		SoilC:           answer.Current.Soil,
		SnowCm:          answer.Current.Snow,
	}
	weather.MaxC = first(answer.Daily.Max)
	weather.MinC = first(answer.Daily.Min)
	weather.RainTodayMm = first(answer.Daily.Rain)
	weather.MaxWindMs = first(answer.Daily.MaxWind)
	weather.MaxGustMs = first(answer.Daily.MaxGust)
	weather.RainChancePct = first(answer.Daily.RainChance)
	weather.RainHours = first(answer.Daily.RainHours)
	weather.MaxUVIndex = first(answer.Daily.MaxUV)
	weather.EvaporationMm = first(answer.Daily.Evaporation)
	// Het minimum van morgen is de nacht die komt. Is er geen morgen in het
	// antwoord, dan valt het terug op vandaag -- beter iets dan niets.
	weather.MinTonightC = weather.MinC
	if len(answer.Daily.Min) > 1 {
		weather.MinTonightC = answer.Daily.Min[1]
	}
	for i, moment := range answer.Minutely.Time {
		quarter := Quarter{At: parseLocal(moment, zone)}
		if i < len(answer.Minutely.Rain) {
			quarter.RainMm = answer.Minutely.Rain[i]
		}
		if i < len(answer.Minutely.Cape) {
			quarter.Cape = answer.Minutely.Cape[i]
		}
		if i < len(answer.Minutely.Lightning) {
			quarter.Lightning = answer.Minutely.Lightning[i]
		}
		weather.Soon = append(weather.Soon, quarter)
	}
	if len(answer.Daily.Sunrise) > 0 {
		weather.Sunrise = parseLocal(answer.Daily.Sunrise[0], zone)
	}
	if len(answer.Daily.Sunset) > 0 {
		weather.Sunset = parseLocal(answer.Daily.Sunset[0], zone)
	}
	return weather, nil
}

func first(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return values[0]
}

// parseLocal leest "2026-08-10T09:15" in de zone van de plaats. Een tijd die
// niet te lezen is levert de nulwaarde op; daar hangt geen beslissing aan.
func parseLocal(value string, zone *time.Location) time.Time {
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, zone); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func (c *Client) forecast() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return forecastURL
}

func (c *Client) geocoding() string {
	if c.GeocodingURL != "" {
		return strings.TrimRight(c.GeocodingURL, "/")
	}
	return geocodingURL
}

func (c *Client) getJSON(ctx context.Context, address string, query url.Values, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = DefaultHTTP()
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Open-Meteo: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("Open-Meteo: %w", err)
	}
	if len(body) > maxBody {
		return fmt.Errorf("Open-Meteo stuurde meer dan %d bytes", maxBody)
	}
	if response.StatusCode >= 400 {
		// Open-Meteo zet zijn klacht in "reason". Die doorgeven is beter dan
		// "http 400": hij zegt bijvoorbeeld welke parameter niet kan.
		var refused struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(body, &refused)
		if refused.Reason != "" {
			return fmt.Errorf("Open-Meteo: %s (http %d op %s)", refused.Reason, response.StatusCode, query.Encode())
		}
		return fmt.Errorf("Open-Meteo antwoordde met http %d op %s", response.StatusCode, query.Encode())
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("Open-Meteo stuurde geen bruikbare JSON: %w", err)
	}
	return nil
}
