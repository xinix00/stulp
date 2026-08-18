// Package myuplink praat met de myUplink-cloud van Nibe.
//
// Alleen v2, met een bearer-token: https://api.myuplink.com/v2/... Alles wat een
// warmtepomp meet en kan loopt via één lijst "points" met een parameterId per
// waarde -- er is geen apart eindpunt per sensor en geen apparaatspecifiek
// model. Welke parameterId waar over gaat staat in de plugin, niet hier: dit
// pakket weet hoe je met myUplink praat, niet wat een S735 is.
package myuplink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is waar myUplink woont. Tests zetten er hun eigen server neer.
const DefaultBaseURL = "https://api.myuplink.com"

// userAgent gaat op elke aanroep mee, ook die naar de tokenserver.
//
// De Cloudflare-voorkant van myUplink weigert de standaard-User-Agent van een
// HTTP-bibliotheek met foutcode 1010 ("browser signature banned"). De bron liep
// daar tegenaan met die van Node; "Go-http-client/1.1" is even herkenbaar, dus
// we zetten er een eigen naam neer voordat iemand hetzelfde uitzoekt.
const userAgent = "com.stulp.nibe/1.0.0"

// maxBody begrenst wat er van de cloud gelezen wordt.
//
// Een lijst points is een paar tienduizend bytes. Een antwoord dat niet ophoudt
// hoort te falen en niet het geheugen op te eten van een hub die verder niets
// bijzonders doet.
const maxBody = 4 << 20

// absentSensor is wat de pomp meldt als een voeler er niet is.
//
// Dat is geen temperatuur van 327 graden onder nul maar "niets", en het verschil
// hoort niet in een grafiek terecht te komen.
const absentSensor = -32768

// Client is één myUplink-account.
type Client struct {
	// BaseURL is te vervangen voor tests. Leeg betekent DefaultBaseURL.
	BaseURL string
	// HTTP is te vervangen voor tests. Nil betekent DefaultHTTP().
	HTTP *http.Client
	// Token levert een geldig access-token. De client vraagt het vóór elke
	// aanroep, zodat verversen op één plek gebeurt en niet in elke poll.
	Token func(context.Context) (string, error)
}

// DefaultHTTP is een client die past bij een cloud-API: één ronde duurt kort of
// hij deugt niet, en verbindingen blijven staan tussen twee polls.
func DefaultHTTP() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Product is hoe een pomp zichzelf noemt.
type Product struct {
	SerialNumber string `json:"serialNumber"`
	Name         string `json:"name"`
}

// System is één installatie: een huis met een of meer apparaten erin.
type System struct {
	SystemID string         `json:"systemId"`
	Name     string         `json:"name"`
	Devices  []SystemDevice `json:"devices"`
}

// SystemDevice is een apparaat zoals het in de systeemlijst staat.
type SystemDevice struct {
	ID              string  `json:"id"`
	ConnectionState string  `json:"connectionState"`
	Product         Product `json:"product"`
}

// Device is wat het apparaat-eindpunt over één pomp vertelt.
type Device struct {
	ID              string  `json:"id"`
	ConnectionState string  `json:"connectionState"`
	Product         Product `json:"product"`
	// AvailableFeatures zegt per bewerking of die voor dit apparaat en dit
	// abonnement mag. Zo weet de app vooraf welke bedieningen zin hebben.
	AvailableFeatures map[string]bool `json:"availableFeatures"`
}

// Point is één waarde van de pomp.
//
// value is al geschaald naar echte eenheden: 18.2 voor 18,2 °C. De rauwe
// registerwaarde en de schaalfactor staan er ook bij, maar wie ze gebruikt
// rekent iets uit wat de cloud al uitgerekend heeft.
type Point struct {
	ParameterID string   `json:"parameterId"`
	Value       *float64 `json:"value"`
}

// Number levert de waarde, of false als de pomp er geen heeft.
func (p Point) Number() (float64, bool) {
	if p.Value == nil || *p.Value == absentSensor {
		return 0, false
	}
	return *p.Value, true
}

// Systems levert de installaties van deze gebruiker, met hun apparaten.
func (c *Client) Systems(ctx context.Context) ([]System, error) {
	var answer struct {
		Systems []System `json:"systems"`
	}
	if err := c.getJSON(ctx, "/v2/systems/me", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Systems, nil
}

// Device haalt de gegevens van één apparaat op, inclusief availableFeatures.
func (c *Client) Device(ctx context.Context, deviceID string) (Device, error) {
	var device Device
	err := c.getJSON(ctx, "/v2/devices/"+url.PathEscape(deviceID), nil, &device)
	return device, err
}

// Points haalt de waarden van een apparaat op. Zonder parameters komt alles;
// met parameters alleen die, en dat scheelt bij een poll die elke minuut loopt.
func (c *Client) Points(ctx context.Context, deviceID string, parameters ...string) ([]Point, error) {
	var query url.Values
	if len(parameters) > 0 {
		query = url.Values{"parameters": {strings.Join(parameters, ",")}}
	}
	var points []Point
	err := c.getJSON(ctx, "/v2/devices/"+url.PathEscape(deviceID)+"/points", query, &points)
	return points, err
}

// SetPoints schrijft waarden naar een apparaat.
//
// De waarden zijn dezelfde als bij het lezen: de echte, geschaalde waarde. De
// bron ontdekte dat de rauwe registerwaarde (220 voor 22 °C) geweigerd wordt met
// "value outside of a valid min-max range".
func (c *Client) SetPoints(ctx context.Context, deviceID string, values map[string]any) error {
	if len(values) == 0 {
		return fmt.Errorf("SetPoints zonder waarden")
	}
	_, err := c.do(ctx, http.MethodPatch, "/v2/devices/"+url.PathEscape(deviceID)+"/points", nil, values)
	return err
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	body, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("GET %s: myUplink stuurde geen bruikbare JSON: %w", path, err)
	}
	return nil
}

// do voert één aanroep uit en levert de rauwe body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	if c.Token == nil {
		return nil, fmt.Errorf("deze client heeft geen tokenbron")
	}
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	address := c.base() + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, address, payload)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", userAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	client := c.HTTP
	if client == nil {
		client = DefaultHTTP()
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if len(answer) > maxBody {
		return nil, fmt.Errorf("%s %s: myUplink stuurde meer dan %d bytes", method, path, maxBody)
	}
	if err := checkStatus(method, path, response.StatusCode, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// checkStatus vertaalt een statuscode naar iets waar iemand mee verder kan.
func checkStatus(method, path string, status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s %s: myUplink accepteerde het token niet; koppel deze app opnieuw", method, path)
	case status == http.StatusForbidden:
		// myUplink zet een deel van de bedieningen achter een
		// Premium-abonnement en zegt dat in de body. De bron schrijft erbij dat
		// de API het woord soms als "premuim" spelt, dus we matchen op het stuk
		// dat beide spellingen delen -- de bron zelf zocht naar "premi" en zou
		// de tikfout waar hij voor waarschuwt dus juist missen.
		if strings.Contains(strings.ToLower(string(body)), "prem") {
			return fmt.Errorf("%s %s: myUplink staat dit alleen toe met een Premium-abonnement", method, path)
		}
		return fmt.Errorf("%s %s: dit token mag dit niet; koppel opnieuw en sta ook WRITESYSTEM toe", method, path)
	case status == http.StatusNotFound:
		return fmt.Errorf("%s %s: bestaat niet bij myUplink", method, path)
	case status >= 400:
		return fmt.Errorf("%s %s: myUplink antwoordde %d: %s", method, path, status, excerpt(body))
	}
	return nil
}

// excerpt houdt een foutmelding leesbaar zonder hem stil af te kappen: wat er
// weg is, is te zien.
func excerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(leeg antwoord)"
	}
	if len(text) > 300 {
		return text[:300] + "… (afgekapt)"
	}
	return text
}
