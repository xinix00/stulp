// Package tahoma praat met de TaHoma-cloud van Somfy.
//
// Eén weg naar binnen, en het is dezelfde als die van de bron: een
// gebruikersnaam en een wachtwoord op
// https://www.tahomalink.com/enduser-mobile-web/enduserAPI. Er is geen
// API-sleutel en geen token; wat je terugkrijgt is een sessiecookie die de
// server uitdeelt en die daarna vanzelf meegaat.
//
// Een sessie verloopt, en dat merk je pas doordat een gewoon verzoek 401
// antwoordt. Dat is hier geen fout maar een tussenstap: er wordt opnieuw
// ingelogd en het verzoek gaat over. De gebruiker hoort daar niets van te
// merken -- die heeft zijn wachtwoord al een keer ingevuld.
package tahoma

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// BaseURL is waar de enduser-API van Somfy staat. Uit lib/HttpHelper.js.
const BaseURL = "https://www.tahomalink.com/enduser-mobile-web/enduserAPI"

// maxBody begrenst wat er van de cloud gelezen wordt.
//
// Somfy is geen vijand, maar wel een dienst die wij niet draaien. Een antwoord
// dat niet ophoudt hoort te falen en niet het geheugen op te eten van een hub
// die verder niets bijzonders doet. Een setup met honderd apparaten blijft ruim
// binnen deze grens.
const maxBody = 8 << 20

// Client is één TaHoma-account.
//
// Veilig vanaf elke goroutine: de poll en een commando uit de interface lopen
// door elkaar heen, en die mogen elkaars sessie niet omvergooien.
type Client struct {
	// Base is het voorvoegsel van elk pad. Een test zet hier zijn eigen server.
	Base string
	// HTTP is te vervangen voor tests. De jar hoort erin te zitten: de sessie
	// zit in een cookie en nergens anders.
	HTTP *http.Client

	// Setup-memo (setup.go): het rauwe antwoord en zijn ontleding.
	setupMu   sync.Mutex
	setupRaw  []byte
	setupLast Setup

	mu       sync.Mutex
	username string
	password string
	// session telt hoe vaak er ingelogd is. Wie een 401 krijgt vergelijkt het
	// nummer van vóór zijn verzoek met dit nummer: is het opgeschoven, dan heeft
	// een andere goroutine intussen al ingelogd en hoeft hij alleen te herhalen.
	// Zonder die vergelijking logt elk gelijktijdig verzoek apart in, en dat is
	// precies het moment waarop Somfy je gaat weigeren.
	session uint64
	// active zegt of er ooit een geslaagde login is geweest. Zo niet, dan wordt
	// er eerst ingelogd in plaats van een 401 af te wachten.
	active bool
}

// New bouwt een client voor één account.
func New(username, password string) *Client {
	// cookiejar.New geeft met nil-opties nooit een fout; de jar is de hele
	// sessieopslag, dus zonder jar is er niets om mee verder te gaan.
	jar, _ := cookiejar.New(nil)
	return &Client{
		Base:     BaseURL,
		username: username,
		password: password,
		HTTP: &http.Client{
			Jar: jar,
			// Een cloud die niet antwoordt hoort de poll niet stil te zetten.
			Timeout: 30 * time.Second,
		},
	}
}

// Username levert de naam waarmee deze client inlogt, voor meldingen.
func (c *Client) Username() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.username
}

// Login haalt een sessie op.
//
// Nodig is dit niet -- elk verzoek doet het zelf als het moet -- maar de
// configuratiepagina wil weten of de gegevens kloppen vóórdat ze bewaard
// worden, en dat is precies deze aanroep.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.login(ctx)
}

// login doet de echte inlog. De aanroeper houdt c.mu vast: zolang er ingelogd
// wordt staat elk ander verzoek stil, en dat is de bedoeling.
func (c *Client) login(ctx context.Context) error {
	if c.username == "" || c.password == "" {
		return fmt.Errorf("er is nog geen TaHoma-account ingesteld")
	}
	form := url.Values{"userId": {c.username}, "userPassword": {c.password}}
	status, body, err := c.send(ctx, http.MethodPost, "/login", form, nil)
	if err != nil {
		return fmt.Errorf("inloggen bij TaHoma: %w", err)
	}
	if status == http.StatusUnauthorized {
		return fmt.Errorf("TaHoma weigert de gebruikersnaam of het wachtwoord van %s", c.username)
	}
	if err := checkStatus(http.MethodPost, "/login", status, body); err != nil {
		return err
	}
	// De bron kijkt naar result.success (lib/HttpHelper.js, reAuthenticate) en
	// niet naar de statuscode. Een 200 met success:false is dus een mislukte
	// inlog, en die moet hier stuklopen: anders draait de poll door op een
	// sessie die er niet is.
	var answer struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return fmt.Errorf("inloggen bij TaHoma: het antwoord is geen JSON: %w", err)
	}
	if answer.Success != nil && !*answer.Success {
		reason := answer.Error
		if reason == "" {
			reason = "TaHoma zegt niet waarom"
		}
		return fmt.Errorf("inloggen bij TaHoma mislukt: %s", reason)
	}
	c.session++
	c.active = true
	return nil
}

// Logout gooit de sessie weg aan de kant van Somfy.
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return nil
	}
	c.active = false
	_, _, err := c.send(ctx, http.MethodPost, "/logout", nil, nil)
	return err
}

// do voert één aanroep uit, logt zo nodig eerst of opnieuw in, en levert de
// rauwe body.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, body any) ([]byte, error) {
	session, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	status, answer, err := c.send(ctx, method, path, form, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if status == http.StatusUnauthorized {
		// De sessie is verlopen. Dat is de normale gang van zaken bij TaHoma en
		// geen storing: opnieuw inloggen en het verzoek nog één keer doen. Eén
		// keer, want een tweede 401 betekent dat de gegevens zelf niet deugen en
		// dan hoort de gebruiker dat te horen in plaats van dat wij blijven
		// proberen.
		if err := c.refresh(ctx, session); err != nil {
			return nil, err
		}
		status, answer, err = c.send(ctx, method, path, form, body)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	if err := checkStatus(method, path, status, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// ensure zorgt dat er een sessie is en levert het nummer ervan.
func (c *Client) ensure(ctx context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		if err := c.login(ctx); err != nil {
			return 0, err
		}
	}
	return c.session, nil
}

// refresh logt opnieuw in, tenzij een ander dat net al gedaan heeft.
func (c *Client) refresh(ctx context.Context, seen uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != seen {
		return nil
	}
	return c.login(ctx)
}

// send doet één HTTP-verzoek en geeft status en body terug, zonder oordeel.
// Alleen login gebruikt hem rechtstreeks; de rest gaat via do.
func (c *Client) send(ctx context.Context, method, path string, form url.Values, body any) (int, []byte, error) {
	var payload io.Reader
	contentType := ""
	switch {
	case form != nil:
		payload = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case body != nil:
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
		contentType = "application/json; charset=utf-8"
	}

	request, err := http.NewRequestWithContext(ctx, method, c.base()+path, payload)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	response, err := c.http().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return 0, nil, err
	}
	if len(answer) > maxBody {
		return 0, nil, fmt.Errorf("TaHoma stuurde meer dan %d bytes", maxBody)
	}
	return response.StatusCode, answer, nil
}

func (c *Client) base() string {
	if c.Base == "" {
		return BaseURL
	}
	return strings.TrimRight(c.Base, "/")
}

func (c *Client) http() *http.Client {
	if c.HTTP == nil {
		jar, _ := cookiejar.New(nil)
		c.HTTP = &http.Client{Jar: jar, Timeout: 30 * time.Second}
	}
	return c.HTTP
}

// checkStatus vertaalt een statuscode naar iets waar iemand mee verder kan.
//
// Een 401 komt hier alleen nog terecht nádat er opnieuw ingelogd is; dan is het
// geen verlopen sessie meer maar een wachtwoord dat niet klopt, en dat is een
// heel ander probleem om te melden.
func checkStatus(method, path string, status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("TaHoma blijft %s %s weigeren; controleer gebruikersnaam en wachtwoord op de instellingenpagina", method, path)
	case status == http.StatusNotFound:
		return fmt.Errorf("%s %s bestaat niet bij TaHoma", method, path)
	case status >= 400:
		return fmt.Errorf("%s %s: TaHoma antwoordde %d: %s", method, path, status, excerpt(body))
	}
	return nil
}

// excerpt maakt van een foutbody iets wat in een melding past, zonder te doen
// alsof er niets stond.
func excerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "zonder uitleg"
	}
	if len(text) > 200 {
		return text[:200] + "…"
	}
	return text
}

// getJSON haalt op en ontleedt.
func getJSON[T any](ctx context.Context, c *Client, path string) (T, error) {
	var out T
	body, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("GET %s: TaHoma stuurde iets dat geen JSON is: %w", path, err)
	}
	return out, nil
}
