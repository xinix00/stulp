package spotify

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

// De OAuth2-kant van Spotify.
//
// Anders dan bij myUplink is hier géén clientgeheim nodig: Spotify accepteert
// PKCE voor publieke clients, en dat is precies wat een plugin is. Er staat dus
// nergens iets dat je account opent -- alleen een client-id, dat net zo openbaar
// is als de naam van de app.
//
// Wat wél nodig is, is een eigen registratie op developer.spotify.com. Stulp
// heeft geen cloud-callback, dus de gebruiker registreert zelf een redirect en
// plakt terug waar de browser uitkwam. Op dat adres hoeft niets te luisteren:
// de code staat in de adresbalk.

const (
	authorizeURL = "https://accounts.spotify.com/authorize"
	tokenURL     = "https://accounts.spotify.com/api/token"
)

// scopes zijn de twee die het doel vragen: zien welke apparaten er zijn, en
// zeggen wat er moet spelen. Meer niet -- geen bibliotheek, geen playlists,
// geen luistergeschiedenis.
const scopes = "user-read-playback-state user-modify-playback-state"

// Config is de registratie van deze installatie bij Spotify.
type Config struct {
	ClientID    string
	RedirectURI string

	// BaseURL vervangt accounts.spotify.com in een test. Leeg is de echte.
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

// Stale zegt of het token binnen margin verloopt.
func (t *Tokens) Stale(margin time.Duration) bool {
	return t == nil || t.Expiry.IsZero() || time.Now().Add(margin).After(t.Expiry)
}

// Authorization is één begonnen autorisatie: waar de gebruiker naartoe gaat,
// plus wat er bij het inwisselen weer nodig is.
type Authorization struct {
	URL      string
	State    string
	Verifier string
}

// Authorize bouwt de URL waar de gebruiker toestemming geeft.
func (c Config) Authorize() (Authorization, error) {
	if err := c.check(); err != nil {
		return Authorization{}, err
	}
	state, err := randomToken()
	if err != nil {
		return Authorization{}, err
	}
	verifier, err := randomToken()
	if err != nil {
		return Authorization{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id":             {c.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {c.RedirectURI},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	return Authorization{
		URL:      c.base() + "/authorize?" + query.Encode(),
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
	return c.token(ctx, client, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.RedirectURI},
		"code_verifier": {verifier},
	}, "")
}

// Refresh haalt een nieuw access-token op met het refresh-token.
//
// Spotify wisselt het refresh-token niet altijd om. Ontbreekt het in het
// antwoord, dan blijft het oude gelden -- het weggooien zou de koppeling bij de
// eerstvolgende verversing verbreken.
func (c Config) Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Tokens, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("er is geen refresh-token; koppel deze app opnieuw met Spotify")
	}
	return c.token(ctx, client, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}, refreshToken)
}

func (c Config) token(ctx context.Context, client *http.Client, form url.Values, keepRefresh string) (*Tokens, error) {
	form.Set("client_id", c.ClientID)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	if client == nil {
		client = DefaultHTTP()
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Spotify-tokenserver: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("Spotify-tokenserver: %w", err)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("Spotify-tokenserver stuurde meer dan %d bytes", maxBody)
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
		return nil, fmt.Errorf("Spotify-tokenserver stuurde geen JSON: %w", err)
	}
	if answer.AccessToken == "" {
		return nil, fmt.Errorf("Spotify-tokenserver stuurde een antwoord zonder access_token")
	}
	if answer.ExpiresIn <= 0 {
		return nil, fmt.Errorf("Spotify-tokenserver noemde geen geldigheidsduur")
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
	if tokens.RefreshToken == "" {
		return nil, fmt.Errorf("Spotify gaf geen refresh-token terug")
	}
	return tokens, nil
}

// CodeFromRedirect haalt de code uit het adres dat de gebruiker terugplakte, en
// controleert de state.
//
// De state is geen formaliteit: hij bewijst dat dit antwoord bij de autorisatie
// hoort die hier begon, en niet bij een adres dat iemand anders aanreikte.
func CodeFromRedirect(pasted, state string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", fmt.Errorf("plak het hele adres terug waar de browser uitkwam")
	}
	// Ook een los stuk vanaf het vraagteken accepteren: dat is wat iemand
	// selecteert als hij alleen het einde van de adresbalk pakt.
	if i := strings.IndexAny(pasted, "?#"); i >= 0 {
		pasted = pasted[i+1:]
	}
	values, err := url.ParseQuery(pasted)
	if err != nil {
		return "", fmt.Errorf("dat adres is niet te lezen: %w", err)
	}
	if reason := values.Get("error"); reason != "" {
		return "", fmt.Errorf("Spotify weigerde de autorisatie: %s", reason)
	}
	if got := values.Get("state"); got != state {
		return "", fmt.Errorf("dit adres hoort niet bij deze autorisatie; begin opnieuw")
	}
	code := values.Get("code")
	if code == "" {
		return "", fmt.Errorf("er staat geen code in dat adres")
	}
	return code, nil
}

func (c Config) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://accounts.spotify.com"
}

func (c Config) check() error {
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "client-id")
	}
	if c.RedirectURI == "" {
		missing = append(missing, "redirect-URI")
	}
	if len(missing) > 0 {
		return fmt.Errorf("de Spotify-registratie is niet compleet: %s ontbreekt", strings.Join(missing, ", "))
	}
	return nil
}

// AuthError is een weigering van de tokenserver zelf.
type AuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *AuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("Spotify weigerde de koppeling: %s (%s)", e.Description, e.Code)
	}
	return fmt.Sprintf("Spotify weigerde de koppeling met %s", e.Code)
}

// Final zegt of het zin heeft dit nog eens te proberen. Een verlopen of
// ingetrokken koppeling komt niet terug; een storing wel.
func (e *AuthError) Final() bool {
	switch e.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_scope", "unsupported_grant_type":
		return true
	}
	return false
}

func tokenError(status int, body []byte) error {
	var answer struct {
		Code        string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &answer)
	if answer.Code == "" {
		answer.Code = fmt.Sprintf("http %d", status)
	}
	return &AuthError{Status: status, Code: answer.Code, Description: answer.Description}
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("kan geen willekeur trekken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
