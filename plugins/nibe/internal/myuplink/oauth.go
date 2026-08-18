package myuplink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// De OAuth2-kant van myUplink.
//
// De oorspronkelijke app liet Homey dit doen en gebruikte diens cloud-callback
// (callback.athom.com) als redirect. Die tussenpersoon is hier niet en komt er
// niet, dus doet deze app het zelf: de gebruiker maakt bij myUplink zijn eigen
// registratie aan, de configuratiepagina bouwt de autorisatie-URL, en de code
// die daarna in de adresbalk staat plakt hij terug. Dat werkt met elke redirect
// die de gebruiker zelf registreert, want er hoeft niets op dat adres te
// luisteren -- de browser toont de code en dat is alles wat we nodig hebben.
//
// Wat niet weg te ontwerpen valt: myUplink kent geen publieke clients. De
// OIDC-metadata biedt client_secret_basic en client_secret_post en niet "none",
// dus een clientgeheim is verplicht. PKCE komt eroverheen (S256, zoals de bron)
// en niet ervoor in de plaats.

const (
	authorizePath = "/oauth/authorize"
	tokenPath     = "/oauth/token"
)

// scopes zijn de drie die deze app nodig heeft. offline_access is er de
// belangrijkste van: zonder die scope geeft myUplink geen refresh-token en zou
// de gebruiker elk uur opnieuw moeten inloggen.
const scopes = "READSYSTEM WRITESYSTEM offline_access"

// machineScopes zijn dezelfde twee zonder offline_access. Bij
// client_credentials is er geen refresh-token en het vragen erom levert bij
// myUplink een weigering op de scope op.
const machineScopes = "READSYSTEM WRITESYSTEM"

// Config is de registratie van deze installatie bij myUplink.
//
// De redirect moet exact zijn wat er op dev.myuplink.com geregistreerd staat,
// tot en met de afsluitende schuine streep: de bron liep precies daarop vast.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string

	// BaseURL is te vervangen voor tests. Leeg betekent DefaultBaseURL.
	BaseURL string
}

// Tokens is wat de tokenserver teruggaf, met de vervaltijd al uitgerekend.
//
// expires_in is een aantal seconden vanaf nu, en "nu" is voorbij zodra het
// antwoord binnen is. Een absolute tijd overleeft een herstart; een aantal
// seconden niet.
type Tokens struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	Scope        string    `json:"scope"`
	Expiry       time.Time `json:"expiry"`
}

// Stale zegt of het token binnen margin verloopt. Verversen hoort daarop te
// gebeuren en niet op een aanroep die faalt: die faalt namelijk midden in een
// poll, en dan is er al een gat in de grafiek.
func (t *Tokens) Stale(margin time.Duration) bool {
	return t == nil || t.Expiry.IsZero() || time.Now().Add(margin).After(t.Expiry)
}

// Authorization is één begonnen autorisatie: de URL waar de gebruiker naartoe
// gaat, plus wat er bij het inwisselen weer nodig is.
type Authorization struct {
	URL      string
	State    string
	Verifier string
}

// Authorize bouwt de URL waar de gebruiker moet inloggen.
func (c Config) Authorize() (Authorization, error) {
	if err := c.check(); err != nil {
		return Authorization{}, err
	}
	verifier, err := randomToken()
	if err != nil {
		return Authorization{}, err
	}
	state, err := randomToken()
	if err != nil {
		return Authorization{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.ClientID},
		"redirect_uri":          {c.RedirectURI},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(digest[:])},
		"code_challenge_method": {"S256"},
	}
	return Authorization{
		URL:      c.base() + authorizePath + "?" + query.Encode(),
		State:    state,
		Verifier: verifier,
	}, nil
}

// Exchange wisselt de code die de gebruiker terugplakte om voor een token.
func (c Config) Exchange(ctx context.Context, client *http.Client, code, verifier string) (*Tokens, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if code == "" {
		return nil, fmt.Errorf("er is geen autorisatiecode om in te wisselen")
	}
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {c.RedirectURI},
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return c.token(ctx, client, form, "", true)
}

// Refresh haalt een nieuw access-token op met het refresh-token.
//
// Het antwoord draagt vaak een nieuw refresh-token en soms niet. Ontbreekt het,
// dan blijft het oude gelden -- het weggooien zou de koppeling verbreken bij de
// eerstvolgende verversing.
func (c Config) Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Tokens, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("er is geen refresh-token; koppel deze app opnieuw met myUplink")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return c.token(ctx, client, form, refreshToken, true)
}

// ClientCredentials haalt een token op de andere manier: op naam van de
// registratie zelf, zonder browser en zonder gebruiker.
//
// myUplink biedt beide wegen aan en deze is voor een eigen huis de eenvoudigste
// -- er is geen redirect nodig, niets om terug te plakken, en niets wat verloopt
// zolang het geheim klopt. Wat je ervoor inlevert is dat het token bij de
// registratie hoort en niet bij een persoon: je ziet de systemen waar deze
// registratie bij mag, en dat is bij een zelfgemaakte registratie precies de
// eigen pomp.
//
// Er komt hier geen refresh-token terug, en dat hoeft ook niet: een nieuw token
// halen kost dezelfde ene aanroep als het verversen ervan.
func (c Config) ClientCredentials(ctx context.Context, client *http.Client) (*Tokens, error) {
	if err := c.checkClient(); err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {machineScopes},
	}
	return c.token(ctx, client, form, "", false)
}

// token voert één aanroep naar het token-eindpunt uit.
//
// Het geheim gaat in de body (client_secret_post) en niet in een Basic-header.
// Beide staan in de OIDC-metadata; dit is de vorm die de bron tegen de echte
// server gebruikte, en dat weegt zwaarder dan de metadata.
// requireRefresh is vals bij client_credentials: die weg levert er geen en zou
// anders op de laatste regel alsnog afketsen.
func (c Config) token(ctx context.Context, client *http.Client, form url.Values, keepRefresh string, requireRefresh bool) (*Tokens, error) {
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)

	if client == nil {
		client = DefaultHTTP()
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("myUplink-tokenserver: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("myUplink-tokenserver: %w", err)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("myUplink-tokenserver stuurde meer dan %d bytes", maxBody)
	}
	if response.StatusCode >= 400 {
		return nil, tokenError(response.StatusCode, body)
	}

	var answer struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("myUplink-tokenserver stuurde geen JSON: %w", err)
	}
	if answer.AccessToken == "" {
		return nil, fmt.Errorf("myUplink-tokenserver stuurde een antwoord zonder access_token")
	}
	if answer.ExpiresIn <= 0 {
		return nil, fmt.Errorf("myUplink-tokenserver noemde geen geldigheidsduur (expires_in)")
	}
	tokens := &Tokens{
		AccessToken:  answer.AccessToken,
		RefreshToken: answer.RefreshToken,
		Scope:        answer.Scope,
		Expiry:       time.Now().Add(time.Duration(answer.ExpiresIn) * time.Second),
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = keepRefresh
	}
	if requireRefresh && tokens.RefreshToken == "" {
		return nil, fmt.Errorf("myUplink gaf geen refresh-token terug; is de scope offline_access toegestaan voor deze registratie?")
	}
	return tokens, nil
}

func (c Config) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// checkClient vangt de twee velden af die beide wegen nodig hebben.
func (c Config) checkClient() error {
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "client-id")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "clientgeheim")
	}
	return missingFields(missing)
}

// check vangt de drie lege velden af waar iemand anders pas achter komt als de
// browser een foutpagina van myUplink toont. De redirect hoort alleen bij de
// weg langs de browser.
func (c Config) check() error {
	if err := c.checkClient(); err != nil {
		return err
	}
	var missing []string
	if c.RedirectURI == "" {
		missing = append(missing, "redirect-URI")
	}
	return missingFields(missing)
}

func missingFields(missing []string) error {
	if len(missing) > 0 {
		return fmt.Errorf("de myUplink-registratie is niet compleet: %s ontbreekt", strings.Join(missing, ", "))
	}
	return nil
}

// AuthError is een weigering van de tokenserver zelf, met de foutcode die OAuth2
// voorschrijft.
type AuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *AuthError) Error() string {
	switch {
	case e.Description != "" && e.Code != "":
		return fmt.Sprintf("myUplink weigerde het token (%s): %s", e.Code, e.Description)
	case e.Code != "":
		return fmt.Sprintf("myUplink weigerde het token (%s)", e.Code)
	default:
		return fmt.Sprintf("myUplink weigerde het token (HTTP %d)", e.Status)
	}
}

// Final zegt of opnieuw proberen zin heeft.
//
// Een netwerkstoring of een 500 gaat vanzelf over; een refresh-token dat
// geweigerd wordt komt nooit meer terug. Dat verschil bepaalt of de app blijft
// proberen of de gebruiker om een nieuwe koppeling vraagt, en dus of iemand elke
// minuut dezelfde fout in de log krijgt.
func (e *AuthError) Final() bool {
	switch e.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_scope", "unsupported_grant_type":
		return true
	}
	return false
}

func tokenError(status int, body []byte) error {
	var answer struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Error == "" {
		return &AuthError{Status: status, Description: excerpt(body)}
	}
	return &AuthError{Status: status, Code: answer.Error, Description: answer.Description}
}

// randomToken levert 48 willekeurige bytes als URL-veilige tekst. Dat is de
// lengte die de bron voor de PKCE-verifier gebruikte en ruim binnen wat RFC 7636
// toestaat (43-128 tekens).
func randomToken() (string, error) {
	buffer := make([]byte, 48)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("geen willekeur beschikbaar: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// CodeFromRedirect haalt de autorisatiecode uit wat de gebruiker terugplakte.
//
// Dat is meestal de hele adresbalk na het inloggen, soms alleen de code. Beide
// mogen: overtypen van een lange code is precies waar tikfouten ontstaan.
// Draagt de tekst een state, dan moet die kloppen -- anders wisselt de app een
// code in die bij een andere autorisatie hoort.
func CodeFromRedirect(pasted, expectState string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", fmt.Errorf("plak het adres waar myUplink je na het inloggen naartoe stuurde")
	}
	query, err := redirectQuery(pasted)
	if err != nil {
		return "", err
	}
	if query == nil {
		return pasted, nil
	}
	if failure := query.Get("error"); failure != "" {
		description := query.Get("error_description")
		if description == "" {
			return "", fmt.Errorf("myUplink wees de autorisatie af (%s)", failure)
		}
		return "", fmt.Errorf("myUplink wees de autorisatie af (%s): %s", failure, description)
	}
	if state := query.Get("state"); state != "" && expectState != "" && state != expectState {
		return "", fmt.Errorf("dit adres hoort bij een andere autorisatie; begin opnieuw")
	}
	code := query.Get("code")
	if code == "" {
		return "", fmt.Errorf("in dit adres staat geen code=; kopieer de hele adresbalk na het inloggen")
	}
	return code, nil
}

// redirectQuery levert de queryparameters van geplakte tekst, of nil als het
// geen adres is maar een kale code.
func redirectQuery(pasted string) (url.Values, error) {
	if mark := strings.IndexByte(pasted, '?'); mark >= 0 {
		values, err := url.ParseQuery(pasted[mark+1:])
		if err != nil {
			return nil, fmt.Errorf("dit adres is niet te lezen: %w", err)
		}
		return values, nil
	}
	if strings.Contains(pasted, "://") {
		return nil, fmt.Errorf("in dit adres staat geen code=; kopieer de hele adresbalk na het inloggen")
	}
	return nil, nil
}
