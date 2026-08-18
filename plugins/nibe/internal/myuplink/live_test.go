package myuplink

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// De proef tegen de echte myUplink.
//
// Deze test slaat over zonder NIBE_CLIENT_ID en NIBE_CLIENT_SECRET, en die twee
// horen ook nergens in deze map te staan: een clientgeheim in een repository is
// een geheim dat niet meer van jou is. Draaien doe je hem zo:
//
//	NIBE_CLIENT_ID=... NIBE_CLIENT_SECRET=... go test ./plugins/nibe/... -run Live -v
//
// Alleen lezen. Er wordt niets naar een pomp geschreven, want een test die de
// verwarming van iemands huis verzet is geen test.
func liveConfig(t *testing.T) Config {
	t.Helper()
	id, secret := os.Getenv("NIBE_CLIENT_ID"), os.Getenv("NIBE_CLIENT_SECRET")
	if id == "" || secret == "" {
		t.Skip("zonder NIBE_CLIENT_ID en NIBE_CLIENT_SECRET valt er niets tegen myUplink te toetsen")
	}
	return Config{ClientID: id, ClientSecret: secret}
}

// client_credentials is de tweede koppelweg: geen browser, geen redirect, en
// geen refresh-token. Dat laatste is precies waar de gedeelde tokencode op
// afketste voordat requireRefresh bestond.
func TestLiveClientCredentialsYieldsAUsableToken(t *testing.T) {
	config := liveConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tokens, err := config.ClientCredentials(ctx, DefaultHTTP())
	if err != nil {
		t.Fatalf("client_credentials: %v", err)
	}
	if tokens.AccessToken == "" {
		t.Fatal("er kwam een token terug zonder token erin")
	}
	if tokens.RefreshToken != "" {
		t.Errorf("client_credentials leverde een refresh-token (%q); dan klopt de aanname in session.refreshLocked niet meer",
			tokens.RefreshToken)
	}
	if tokens.Stale(0) {
		t.Errorf("het token is meteen al verlopen (%v)", tokens.Expiry)
	}
	for _, scope := range []string{"READSYSTEM", "WRITESYSTEM"} {
		if !strings.Contains(tokens.Scope, scope) {
			t.Errorf("de scope %q mist %s", tokens.Scope, scope)
		}
	}

	// En het token moet ook werkelijk ergens toegang toe geven. Een tokenserver
	// die tevreden is zegt nog niets over de API erachter.
	client := &Client{Token: func(context.Context) (string, error) { return tokens.AccessToken, nil }}
	systems, err := client.Systems(ctx)
	if err != nil {
		t.Fatalf("met dit token systemen opvragen: %v", err)
	}
	if len(systems) == 0 {
		t.Fatal("deze registratie ziet geen enkel systeem; is hij aan een account gekoppeld?")
	}
	for _, system := range systems {
		for _, device := range system.Devices {
			t.Logf("%s: %s (%s), %s", system.Name, device.Product.Name,
				device.Product.SerialNumber, device.ConnectionState)
		}
	}
}

// De pomp vertelt zelf welke serie hij is. Deze test zegt welke nummers deze
// pomp meldt, zodat een ontbrekende tegel te herleiden is tot een parameter die
// er niet is in plaats van tot een fout in de tabel.
func TestLivePointsRevealTheSeries(t *testing.T) {
	config := liveConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tokens, err := config.ClientCredentials(ctx, DefaultHTTP())
	if err != nil {
		t.Fatalf("client_credentials: %v", err)
	}
	client := &Client{Token: func(context.Context) (string, error) { return tokens.AccessToken, nil }}

	systems, err := client.Systems(ctx)
	if err != nil {
		t.Fatalf("systemen opvragen: %v", err)
	}
	for _, system := range systems {
		for _, device := range system.Devices {
			points, err := client.Points(ctx, device.ID)
			if err != nil {
				t.Errorf("%s: punten opvragen: %v", device.Product.Name, err)
				continue
			}
			if len(points) == 0 {
				t.Errorf("%s meldt geen enkele parameter", device.Product.Name)
				continue
			}
			t.Logf("%s meldt %d parameters", device.Product.Name, len(points))
		}
	}
}
