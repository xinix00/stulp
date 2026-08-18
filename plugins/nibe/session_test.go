package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// tokenServer is een nagebouwde tokenserver van myUplink.
type tokenServer struct {
	*httptest.Server
	calls  int
	status int
	body   string
}

func newTokenServer(t *testing.T, status int, body string) *tokenServer {
	t.Helper()
	s := &tokenServer{status: status, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		w.Write([]byte(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

// testSession bouwt een sessie die zijn token in het geheugen bewaart, zoals
// Stulp het in de app-state zou bewaren.
func testSession(t *testing.T, server *tokenServer, tokens *myuplink.Tokens) (*session, *[]json.RawMessage, *[]string) {
	t.Helper()
	saved := &[]json.RawMessage{}
	notified := &[]string{}
	s := newSession(
		func(tokens *myuplink.Tokens) error {
			raw, err := writeStored(tokens)
			if err != nil {
				return err
			}
			*saved = append(*saved, raw)
			return nil
		},
		func(message string) { *notified = append(*notified, message) },
	)
	s.http = server.Client()
	s.setConfig(myuplink.Config{ClientID: "id", ClientSecret: "geheim",
		RedirectURI: "https://localhost/nibe/", BaseURL: server.URL})
	s.tokens = tokens
	return s, saved, notified
}

// Verversen hoort te gebeuren vóórdat het token verloopt. Gebeurt het pas als
// een aanroep faalt, dan valt die aanroep midden in een poll en staat er een gat
// in de grafiek waar niemand iets aan heeft.
func TestATokenIsRefreshedBeforeItExpires(t *testing.T) {
	server := newTokenServer(t, http.StatusOK, `{"access_token":"nieuw","refresh_token":"r2","expires_in":3600}`)
	session, saved, _ := testSession(t, server, &myuplink.Tokens{
		AccessToken: "oud", RefreshToken: "r1",
		// Nog geldig, maar binnen de marge: dit is precies het moment waarop het
		// vervangen hoort te worden.
		Expiry: time.Now().Add(refreshMargin / 2),
	})

	token, err := session.token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "nieuw" {
		t.Fatalf("token = %q, wil het ververste", token)
	}
	if server.calls != 1 {
		t.Fatalf("aantal verversingen = %d", server.calls)
	}
	// Wat ververst is hoort bewaard te worden, anders begint de app na een
	// herstart met een token dat al verlopen is.
	if len(*saved) != 1 {
		t.Fatalf("aantal keer bewaard = %d", len(*saved))
	}
	stored, err := readStored((*saved)[0])
	if err != nil || stored.AccessToken != "nieuw" || stored.RefreshToken != "r2" {
		t.Fatalf("bewaard = %#v %v", stored, err)
	}
}

// Een token dat nog uren mee kan hoeft niet ververst te worden. Elke aanroep
// laten verversen zou de tokenserver zonder reden bezighouden.
func TestAFreshTokenIsHandedOutAsIs(t *testing.T) {
	server := newTokenServer(t, http.StatusOK, `{"access_token":"nieuw","refresh_token":"r2","expires_in":3600}`)
	session, saved, _ := testSession(t, server, &myuplink.Tokens{
		AccessToken: "oud", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour),
	})

	token, err := session.token(context.Background())
	if err != nil || token != "oud" {
		t.Fatalf("token = %q %v", token, err)
	}
	if server.calls != 0 || len(*saved) != 0 {
		t.Fatalf("er werd toch ververst: %d aanroepen, %d keer bewaard", server.calls, len(*saved))
	}
}

// Een verlopen token wordt alsnog vervangen voordat het de deur uit gaat. Dat is
// de gewone gang van zaken na een hub die een nacht uit heeft gestaan.
func TestAnExpiredTokenIsReplacedBeforeUse(t *testing.T) {
	server := newTokenServer(t, http.StatusOK, `{"access_token":"nieuw","refresh_token":"r2","expires_in":3600}`)
	session, _, _ := testSession(t, server, &myuplink.Tokens{
		AccessToken: "oud", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour),
	})

	token, err := session.token(context.Background())
	if err != nil || token != "nieuw" {
		t.Fatalf("token = %q %v", token, err)
	}
}

// Een refresh-token dat myUplink definitief weigert komt nooit meer terug. Het
// bewaren zou de app elke minuut hetzelfde dode papiertje laten aanbieden; het
// weggooien maakt duidelijk dat er iets van de gebruiker nodig is.
func TestARefusedRefreshTokenIsThrownAwayAndReported(t *testing.T) {
	server := newTokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	session, saved, notified := testSession(t, server, &myuplink.Tokens{
		AccessToken: "oud", RefreshToken: "dood", Expiry: time.Now().Add(-time.Minute),
	})

	if _, err := session.token(context.Background()); err == nil {
		t.Fatal("een dood token gaf geen fout")
	}
	if linked, _, _ := session.linked(); linked {
		t.Fatal("de koppeling bleef staan")
	}
	if len(*saved) != 1 || (*saved)[0] != nil {
		t.Fatalf("het dode token werd niet opgeruimd: %#v", *saved)
	}
	if len(*notified) != 1 || !strings.Contains((*notified)[0], "opnieuw") {
		t.Fatalf("de gebruiker kreeg %#v te horen", *notified)
	}

	// En daarna is de melding die elke aanroep krijgt de melding die zegt wat er
	// moet gebeuren, niet de oude fout van de tokenserver.
	if _, err := session.token(context.Background()); !errors.Is(err, errNotLinked) {
		t.Fatalf("na het opruimen = %v", err)
	}
}

// Een storing bij myUplink is geen reden om de koppeling te verbreken: die gaat
// vanzelf over, en opnieuw koppelen kost de gebruiker een avond.
func TestAServerFaultKeepsTheLink(t *testing.T) {
	server := newTokenServer(t, http.StatusInternalServerError, `{"error":"server_error"}`)
	session, saved, notified := testSession(t, server, &myuplink.Tokens{
		AccessToken: "oud", RefreshToken: "levend", Expiry: time.Now().Add(-time.Minute),
	})

	if _, err := session.token(context.Background()); err == nil {
		t.Fatal("een storing gaf geen fout")
	}
	if linked, _, lastErr := session.linked(); !linked || lastErr == "" {
		t.Fatalf("koppeling = %v, laatste fout = %q", linked, lastErr)
	}
	if len(*saved) != 0 || len(*notified) != 0 {
		t.Fatalf("er werd opgeruimd bij een storing: %#v %#v", *saved, *notified)
	}
}

// Zonder koppeling is elke aanroep een fout die zegt waar de gebruiker naartoe
// moet, en niet een aanroep die met een leeg token vertrekt.
func TestWithoutALinkEveryCallSaysWhereToGo(t *testing.T) {
	server := newTokenServer(t, http.StatusOK, `{}`)
	session, _, _ := testSession(t, server, nil)

	_, err := session.token(context.Background())
	if !errors.Is(err, errNotLinked) || !strings.Contains(err.Error(), "instellingen") {
		t.Fatalf("zonder koppeling = %v", err)
	}
}

// Wat bewaard wordt moet ook weer terug te lezen zijn: een token dat een
// herstart niet overleeft is geen token maar een sessie.
func TestTheStoredLinkSurvivesARoundTrip(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Round(time.Second)
	raw, err := writeStored(&myuplink.Tokens{AccessToken: "a", RefreshToken: "r", Expiry: expiry})
	if err != nil {
		t.Fatal(err)
	}
	back, err := readStored(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.AccessToken != "a" || back.RefreshToken != "r" || !back.Expiry.Equal(expiry) {
		t.Fatalf("teruggelezen = %#v", back)
	}

	// Een lege state is een app die nog nooit gekoppeld is, geen fout.
	if back, err := readStored(nil); err != nil || back != nil {
		t.Fatalf("lege state = %#v %v", back, err)
	}
	if _, err := readStored(json.RawMessage("{niet")); err == nil {
		t.Fatal("een kapotte state werd stil geslikt")
	}
}
