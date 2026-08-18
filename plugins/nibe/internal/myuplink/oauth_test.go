package myuplink

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// tokenServer is een nagebouwde tokenserver: hij bewaart wat wij sturen en
// antwoordt met wat we hem meegeven.
type tokenServer struct {
	*httptest.Server
	forms  []url.Values
	status int
	body   string
}

func newTokenServer(t *testing.T, status int, body string) *tokenServer {
	t.Helper()
	s := &tokenServer{status: status, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		s.forms = append(s.forms, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		w.Write([]byte(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func testConfig(base string) Config {
	return Config{ClientID: "id", ClientSecret: "geheim", RedirectURI: "https://localhost/nibe/", BaseURL: base}
}

// De autorisatie-URL is wat de gebruiker in zijn browser opent. Wat daar niet in
// staat kan hij niet toestaan: zonder offline_access krijgt de app geen
// refresh-token en zou hij elk uur opnieuw moeten inloggen.
func TestAuthorizationURLAsksForWhatTheAppNeeds(t *testing.T) {
	authorization, err := testConfig("https://api.example").Authorize()
	if err != nil {
		t.Fatal(err)
	}
	address, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	if address.Path != authorizePath {
		t.Fatalf("pad = %q", address.Path)
	}
	query := address.Query()
	if query.Get("response_type") != "code" || query.Get("client_id") != "id" {
		t.Fatalf("grant = %v", query)
	}
	if query.Get("redirect_uri") != "https://localhost/nibe/" {
		t.Fatalf("redirect = %q", query.Get("redirect_uri"))
	}
	for _, scope := range []string{"READSYSTEM", "WRITESYSTEM", "offline_access"} {
		if !strings.Contains(query.Get("scope"), scope) {
			t.Fatalf("scope %q ontbreekt in %q", scope, query.Get("scope"))
		}
	}

	// PKCE: de challenge in de URL hoort de SHA-256 van de verifier te zijn die
	// we straks meesturen. Klopt dat niet, dan weigert de tokenserver de code en
	// merk je het pas als iemand koppelt.
	digest := sha256.Sum256([]byte(authorization.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(digest[:]); query.Get("code_challenge") != want {
		t.Fatalf("code_challenge = %q, wil %q", query.Get("code_challenge"), want)
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("methode = %q", query.Get("code_challenge_method"))
	}
	if query.Get("state") == "" || query.Get("state") == authorization.Verifier {
		t.Fatalf("state = %q", query.Get("state"))
	}
}

// Een onvolledige registratie hoort te stranden vóórdat er een browser
// opengaat: een foutpagina van myUplink zegt niet welk veld je vergat.
func TestIncompleteRegistrationFailsBeforeTheBrowser(t *testing.T) {
	_, err := Config{ClientID: "id"}.Authorize()
	if err == nil || !strings.Contains(err.Error(), "clientgeheim") {
		t.Fatalf("lege registratie gaf %v", err)
	}
}

// Het inwisselen draagt de code, de verifier en het geheim. myUplink kent geen
// publieke clients, dus zonder dat geheim komt er niets terug.
func TestExchangeCarriesTheSecretAndTheVerifier(t *testing.T) {
	server := newTokenServer(t, http.StatusOK,
		`{"access_token":"aaa","refresh_token":"rrr","expires_in":3600,"token_type":"Bearer","scope":"READSYSTEM"}`)
	tokens, err := testConfig(server.URL).Exchange(context.Background(), server.Client(), "code123", "verifier123")
	if err != nil {
		t.Fatal(err)
	}
	form := server.forms[0]
	for key, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "code123",
		"code_verifier": "verifier123",
		"redirect_uri":  "https://localhost/nibe/",
		"client_id":     "id",
		"client_secret": "geheim",
	} {
		if form.Get(key) != want {
			t.Errorf("%s = %q, wil %q", key, form.Get(key), want)
		}
	}
	if tokens.AccessToken != "aaa" || tokens.RefreshToken != "rrr" {
		t.Fatalf("token = %#v", tokens)
	}
	// expires_in is een aantal seconden vanaf nu; wat bewaard wordt is een
	// tijdstip, want alleen dat overleeft een herstart.
	if until := time.Until(tokens.Expiry); until < 55*time.Minute || until > time.Hour {
		t.Fatalf("verloopt over %v", until)
	}
}

// myUplink wisselt bij het verversen meestal ook het refresh-token om. Doet hij
// dat een keer niet, dan blijft het oude gelden -- het weggooien zou de
// koppeling verbreken bij de eerstvolgende ronde.
func TestRefreshKeepsTheOldRefreshTokenWhenNoNewOneComes(t *testing.T) {
	server := newTokenServer(t, http.StatusOK, `{"access_token":"bbb","expires_in":3600}`)
	tokens, err := testConfig(server.URL).Refresh(context.Background(), server.Client(), "oud")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "bbb" {
		t.Fatalf("access = %q", tokens.AccessToken)
	}
	if tokens.RefreshToken != "oud" {
		t.Fatalf("refresh = %q, wil het oude", tokens.RefreshToken)
	}
	if server.forms[0].Get("grant_type") != "refresh_token" || server.forms[0].Get("refresh_token") != "oud" {
		t.Fatalf("verzoek = %v", server.forms[0])
	}
}

// Een geweigerd refresh-token komt nooit meer terug. Dat verschil met een storing
// bepaalt of de app blijft proberen of de gebruiker om een nieuwe koppeling
// vraagt, dus het moet uit de fout af te lezen zijn.
func TestRefusedGrantIsFinalAndAServerFaultIsNot(t *testing.T) {
	refused := newTokenServer(t, http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"Token is niet meer geldig"}`)
	_, err := testConfig(refused.URL).Refresh(context.Background(), refused.Client(), "dood")
	var authErr *AuthError
	if !errors.As(err, &authErr) || !authErr.Final() {
		t.Fatalf("invalid_grant gaf %v", err)
	}
	if !strings.Contains(err.Error(), "Token is niet meer geldig") {
		t.Fatalf("de uitleg van myUplink ging verloren: %v", err)
	}

	broken := newTokenServer(t, http.StatusInternalServerError, `{"error":"server_error"}`)
	_, err = testConfig(broken.URL).Refresh(context.Background(), broken.Client(), "levend")
	if !errors.As(err, &authErr) || authErr.Final() {
		t.Fatalf("500 gaf %v", err)
	}
}

// Een antwoord zonder refresh-token is geen halve koppeling maar een fout: zonder
// dat token is de app over een uur weer los, en dat hoort meteen te blijken.
func TestATokenWithoutRefreshIsRefused(t *testing.T) {
	server := newTokenServer(t, http.StatusOK, `{"access_token":"aaa","expires_in":3600}`)
	_, err := testConfig(server.URL).Exchange(context.Background(), server.Client(), "code", "verifier")
	if err == nil || !strings.Contains(err.Error(), "offline_access") {
		t.Fatalf("antwoord zonder refresh-token gaf %v", err)
	}
}

// De gebruiker plakt terug waar zijn browser uitkwam. Dat mag het hele adres
// zijn of alleen de code -- een lange code overtypen is precies waar tikfouten
// ontstaan.
func TestTheCodeIsReadFromWhateverWasPasted(t *testing.T) {
	code, err := CodeFromRedirect("https://localhost/nibe/?code=abc&state=xyz", "xyz")
	if err != nil || code != "abc" {
		t.Fatalf("adres gaf %q %v", code, err)
	}
	if code, err := CodeFromRedirect("  abc  ", "xyz"); err != nil || code != "abc" {
		t.Fatalf("kale code gaf %q %v", code, err)
	}
}

// Een state die niet klopt hoort het inwisselen te stoppen: die code is van een
// andere autorisatie, en waar die vandaan komt weten we niet.
func TestAMismatchedStateStopsTheExchange(t *testing.T) {
	_, err := CodeFromRedirect("https://localhost/nibe/?code=abc&state=anders", "xyz")
	if err == nil || !strings.Contains(err.Error(), "andere autorisatie") {
		t.Fatalf("verkeerde state gaf %v", err)
	}
}

// Zegt myUplink nee, dan staat dat in het adres. Dat doorgeven is nuttiger dan
// klagen dat er geen code in staat.
func TestADeniedAuthorizationSaysWhy(t *testing.T) {
	_, err := CodeFromRedirect("https://localhost/nibe/?error=access_denied&error_description=Geweigerd", "xyz")
	if err == nil || !strings.Contains(err.Error(), "Geweigerd") {
		t.Fatalf("geweigerde autorisatie gaf %v", err)
	}
}
