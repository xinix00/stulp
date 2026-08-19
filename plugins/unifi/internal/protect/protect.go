// Package protect praat met de UniFi Protect Integration API.
//
// Alleen v2: https://console/proxy/protect/integration/v1/..., met een API-key
// in X-API-KEY. Dat is de API die UniFi zelf documenteert en die een sleutel
// gebruikt in plaats van een gebruikersnaam met wachtwoord -- geen sessie die
// verloopt, geen CSRF-token, geen inlogpagina die van vorm verandert.
//
// De oude API (bootstrap + cookie) kan dingen die deze niet kan, maar wat hier
// gebruikt wordt kan deze allemaal, en één weg is er één om te onderhouden.
package protect

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BasePath is het voorvoegsel van elke aanroep. Het staat hier los omdat de
// websockets hem ook nodig hebben.
const BasePath = "/proxy/protect/integration/v1"

// maxBody begrenst wat er van de console gelezen wordt.
//
// Een console is geen vijand, maar wel een apparaat waar firmware op draait die
// wij niet schreven. Een antwoord dat niet ophoudt hoort te falen en niet het
// geheugen op te eten van een hub die verder niets bijzonders doet.
const maxBody = 8 << 20

// Client is één console.
type Client struct {
	Host  string
	Port  int
	Token string

	// HTTP is te vervangen voor tests. Nul betekent de standaard hieronder.
	HTTP *http.Client
}

// New bouwt een client met een HTTP-client die past bij wat een console is.
func New(host string, port int, token string) *Client {
	if port == 0 {
		port = 443
	}
	return &Client{Host: host, Port: port, Token: token, HTTP: defaultHTTP()}
}

// defaultHTTP accepteert het zelfondertekende certificaat van de console.
//
// Dat is geen slordigheid maar de enige mogelijkheid: een UniFi-console geeft
// zichzelf een certificaat uit dat door niets te verifiëren is, en er is geen
// naam om tegen te toetsen -- je praat tegen een IP-adres in je eigen huis. De
// API-key is waar de beveiliging op leunt, en het verkeer blijft versleuteld.
//
// Wat dit dus níet dekt is iemand die al in je netwerk zit en het verkeer kan
// omleiden. Wie dat serieus wil afdekken pint het certificaat van zijn eigen
// console; dat vraagt een plek om die vingerafdruk te bewaren en staat in
// PORTED.md als openstaand.
func defaultHTTP() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Address is host:poort, zoals de websockets het ook nodig hebben.
func (c *Client) Address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// Path bouwt een pad onder de integratie-API.
func (c *Client) Path(resource string, query url.Values) string {
	path := BasePath
	if trimmed := strings.TrimLeft(resource, "/"); trimmed != "" {
		path += "/" + trimmed
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return path
}

func (c *Client) url(resource string, query url.Values) string {
	return "https://" + c.Address() + c.Path(resource, query)
}

// call voert de HTTP-aanroep uit maar leest de body niet. Dat maakt dezelfde
// geauthenticeerde weg bruikbaar voor zowel kleine JSON-antwoorden als een
// snapshot die meteen doorgestroomd moet worden.
func (c *Client) call(ctx context.Context, method, resource string, query url.Values, body any, accept string) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.url(resource, query), payload)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	request.Header.Set("X-API-KEY", c.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	client := c.HTTP
	if client == nil {
		client = defaultHTTP()
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, resource, err)
	}
	return response, nil
}

// do voert één aanroep uit en levert de rauwe body.
func (c *Client) do(ctx context.Context, method, resource string, query url.Values, body any) ([]byte, error) {
	response, err := c.call(ctx, method, resource, query, body, "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, resource, err)
	}
	if len(answer) > maxBody {
		return nil, fmt.Errorf("%s %s: the console sent more than %d bytes", method, resource, maxBody)
	}
	if err := checkStatus(method, resource, response.StatusCode, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// checkStatus vertaalt een statuscode naar iets wat de gebruiker verder helpt.
//
// 401 en 403 zijn de twee die in de praktijk gebeuren en ze betekenen iets
// verschillends: de sleutel deugt niet, of de sleutel deugt maar mag dit niet.
// Dat verschil bepaalt wat iemand moet doen, dus het hoort in de melding.
func checkStatus(method, resource string, status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("the console rejected the API key (%s %s)", method, resource)
	case status == http.StatusForbidden:
		return fmt.Errorf("this API key may not %s %s; give it the Protect role it needs", method, resource)
	case status == http.StatusNotFound:
		return fmt.Errorf("%s %s does not exist on this console", method, resource)
	case status >= 400:
		excerpt := strings.TrimSpace(string(body))
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "…"
		}
		if excerpt == "" {
			excerpt = http.StatusText(status)
		}
		return fmt.Errorf("%s %s: console answered %d: %s", method, resource, status, excerpt)
	}
	return nil
}

// getJSON haalt op en ontleedt in target.
func getJSON[T any](ctx context.Context, c *Client, resource string, query url.Values) (T, error) {
	var out T
	body, err := c.do(ctx, http.MethodGet, resource, query, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("GET %s: the console sent something that is not JSON: %w", resource, err)
	}
	return out, nil
}

// Patch wijzigt een apparaat. De console antwoordt met het bijgewerkte object,
// maar een lege body is ook goed: sommige eindpunten geven 204.
func (c *Client) Patch(ctx context.Context, resource string, body any) error {
	_, err := c.do(ctx, http.MethodPatch, resource, nil, body)
	return err
}

// Post is voor de eindpunten die iets doen in plaats van iets wijzigen.
func (c *Client) Post(ctx context.Context, resource string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, resource, nil, body)
}
