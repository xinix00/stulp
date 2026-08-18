package myuplink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cloud is een nagebouwde myUplink: genoeg om te toetsen wat wij sturen en wat
// we met het antwoord doen.
type cloud struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	method string
	path   string
	auth   string
	agent  string
	query  string
	body   map[string]any
}

func newCloud(t *testing.T, routes map[string]string) *cloud {
	t.Helper()
	c := &cloud{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := recorded{
			method: r.Method, path: r.URL.Path,
			auth:  r.Header.Get("Authorization"),
			agent: r.Header.Get("User-Agent"),
			query: r.URL.Query().Get("parameters"),
		}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&entry.body)
		}
		c.requests = append(c.requests, entry)
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *cloud) client() *Client {
	return &Client{
		BaseURL: c.URL,
		HTTP:    c.Client(),
		Token:   func(context.Context) (string, error) { return "token123", nil },
	}
}

// Elke aanroep draagt het token en een eigen User-Agent. Zonder die tweede
// weigert de Cloudflare-voorkant van myUplink het verzoek met 1010, en dat is
// een fout waar niets in staat over wat eraan mankeert.
func TestEveryCallCarriesTheTokenAndAnAgent(t *testing.T) {
	cloud := newCloud(t, map[string]string{"GET /v2/systems/me": `{"systems":[]}`})
	if _, err := cloud.client().Systems(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := cloud.requests[0]
	if got.auth != "Bearer token123" {
		t.Fatalf("Authorization = %q", got.auth)
	}
	if got.agent != userAgent {
		t.Fatalf("User-Agent = %q", got.agent)
	}
}

// Een token dat er niet is, is geen aanroep die faalt maar een aanroep die niet
// vertrekt -- en de melding zegt wat de gebruiker moet doen.
func TestWithoutATokenNothingIsSent(t *testing.T) {
	cloud := newCloud(t, map[string]string{})
	client := cloud.client()
	client.Token = func(context.Context) (string, error) { return "", errNoToken }
	if _, err := client.Systems(context.Background()); err != errNoToken {
		t.Fatalf("zonder token = %v", err)
	}
	if len(cloud.requests) != 0 {
		t.Fatalf("er ging toch een verzoek uit: %#v", cloud.requests)
	}
}

var errNoToken = &AuthError{Code: "invalid_grant"}

const systemsJSON = `{"page":1,"numItems":1,"systems":[{
  "systemId":"sys1","name":"Thuis","devices":[
    {"id":"dev1","connectionState":"Connected","product":{"serialNumber":"0661","name":"S735-4 CU EM"}}]}]}`

// De pompen komen uit de cloud en niet uit een invulveld: hun naam, hun
// serienummer en of ze aan staan weet myUplink zelf.
func TestSystemsCarryTheDevicesTheyContain(t *testing.T) {
	cloud := newCloud(t, map[string]string{"GET /v2/systems/me": systemsJSON})
	systems, err := cloud.client().Systems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(systems) != 1 || len(systems[0].Devices) != 1 {
		t.Fatalf("systemen = %#v", systems)
	}
	device := systems[0].Devices[0]
	if device.ID != "dev1" || device.Product.Name != "S735-4 CU EM" || device.ConnectionState != "Connected" {
		t.Fatalf("apparaat = %#v", device)
	}
}

// Een voeler die er niet is meldt -32768. Dat is geen temperatuur van 327 graden
// onder nul, en een echte nul is dat wel.
func TestAbsentSensorsStayAbsentAndARealZeroSurvives(t *testing.T) {
	cloud := newCloud(t, map[string]string{
		"GET /v2/devices/dev1/points": `[
			{"parameterId":"4","value":18.2},
			{"parameterId":"44","value":-32768},
			{"parameterId":"1756","value":0},
			{"parameterId":"605","value":null}]`,
	})
	points, err := cloud.client().Points(context.Background(), "dev1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		value float64
		known bool
	}{
		"4":    {18.2, true},
		"44":   {0, false},
		"1756": {0, true},
		"605":  {0, false},
	}
	for _, point := range points {
		value, known := point.Number()
		expected := want[point.ParameterID]
		if value != expected.value || known != expected.known {
			t.Errorf("punt %s = %v %v, wil %v %v", point.ParameterID, value, known, expected.value, expected.known)
		}
	}
}

// De snelle ronde vraagt twee parameters en niet drieëntachtig. Dat is het hele
// verschil tussen elke minuut een klein verzoek en elke minuut de hele pomp.
func TestAFilteredPollAsksForOnlyWhatItNeeds(t *testing.T) {
	cloud := newCloud(t, map[string]string{"GET /v2/devices/dev1/points": `[]`})
	if _, err := cloud.client().Points(context.Background(), "dev1", "22130", "14950"); err != nil {
		t.Fatal(err)
	}
	if got := cloud.requests[0].query; got != "22130,14950" {
		t.Fatalf("parameters = %q", got)
	}
}

// Schrijven gaat met de echte waarde, niet met de registerwaarde: de bron kreeg
// 220 voor 22 graden terug als "value outside of a valid min-max range".
func TestWritingSendsTheDisplayValue(t *testing.T) {
	cloud := newCloud(t, map[string]string{"PATCH /v2/devices/dev1/points": `{}`})
	err := cloud.client().SetPoints(context.Background(), "dev1", map[string]any{"47751": 22.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := cloud.requests[0].body["47751"]; got != 22.0 {
		t.Fatalf("body = %#v", cloud.requests[0].body)
	}
}

// De cloud zegt met 403 twee verschillende dingen: dit token mag het niet, of
// dit abonnement kan het niet. Alleen het tweede los je op met een creditcard,
// dus het verschil hoort in de melding. Beide spellingen tellen -- de API is
// gezien met "premuim", en daar zoekt de bron met "premi" juist langsheen.
func TestPremiumAndPermissionAreDifferentComplaints(t *testing.T) {
	for _, spelling := range []string{"premium", "premuim"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Device does not have ` + spelling + ` subscription"}`))
		}))
		client := &Client{BaseURL: server.URL, HTTP: server.Client(),
			Token: func(context.Context) (string, error) { return "t", nil }}
		err := client.SetPoints(context.Background(), "dev1", map[string]any{"7086": 1})
		if err == nil || !strings.Contains(err.Error(), "Premium") {
			t.Errorf("403 met %q gaf %v", spelling, err)
		}
		server.Close()
	}

	// Een 403 die niets over een abonnement zegt is een token dat te weinig mag,
	// en dat is een heel ander gesprek.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer plain.Close()
	client := &Client{BaseURL: plain.URL, HTTP: plain.Client(),
		Token: func(context.Context) (string, error) { return "t", nil }}

	err := client.SetPoints(context.Background(), "dev1", map[string]any{"7086": 1})
	if err == nil || !strings.Contains(err.Error(), "WRITESYSTEM") {
		t.Fatalf("kale 403 gaf %v", err)
	}
	_, err = client.Systems(context.Background())
	if err == nil || !strings.Contains(err.Error(), "koppel deze app opnieuw") {
		t.Fatalf("401 gaf %v", err)
	}
}
