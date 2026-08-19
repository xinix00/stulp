package protect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// console is een nagebouwde UniFi-console: genoeg om te toetsen wat wij sturen
// en wat we met het antwoord doen.
type console struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	method string
	path   string
	key    string
	body   map[string]any
}

func newConsole(t *testing.T, routes map[string]string) *console {
	t.Helper()
	c := &console{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := recorded{method: r.Method, path: r.URL.Path, key: r.Header.Get("X-API-KEY")}
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

// client wijst naar de nagebouwde console. httptest praat plain http, dus de
// URL wordt hier gezet in plaats van via New.
func (c *console) client() *Client {
	address := strings.TrimPrefix(c.URL, "http://")
	host, port, _ := strings.Cut(address, ":")
	client := New(host, atoi(port), "sleutel")
	client.HTTP = c.Client()
	client.HTTP.Transport = plainTransport{c.Client().Transport}
	return client
}

// plainTransport zet https terug naar http, want httptest doet geen TLS.
type plainTransport struct{ inner http.RoundTripper }

func (p plainTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.URL.Scheme = "http"
	inner := p.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(request)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type trackedBody struct {
	io.Reader
	reads int
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (*trackedBody) Close() error { return nil }

func atoi(text string) int {
	value := 0
	for _, digit := range text {
		value = value*10 + int(digit-'0')
	}
	return value
}

// Zoals een echte console het stuurt, overgenomen van 192.168.1.1 op
// 2026-08-09: state als tekst, geen isConnected, geen opnamestand.
const camerasJSON = `[
  {"id":"cam1","name":"Oprit","modelKey":"camera","state":"CONNECTED","mac":"1C0B8B5C0001",
   "isMicEnabled":true,"micVolume":100,
   "featureFlags":{"hasMic":true,"hasSpeaker":false,"hasHdr":true,
                   "smartDetectTypes":["person","vehicle"]}},
  {"id":"bel1","name":"Voordeur","modelKey":"camera","state":"DISCONNECTED","mac":"1C0B8B5C0002",
   "featureFlags":{"hasMic":true,"hasSpeaker":true}}
]`

// Elke aanroep draagt de sleutel en gaat naar het integratiepad. Zonder een van
// beide antwoordt een echte console met 401 of 404, en dat is precies het soort
// fout dat je pas ontdekt als er niets werkt.
func TestEveryCallCarriesTheKeyAndThePath(t *testing.T) {
	console := newConsole(t, map[string]string{
		"GET " + BasePath + "/cameras": camerasJSON,
	})
	if _, err := console.client().Cameras(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(console.requests) != 1 {
		t.Fatalf("aantal verzoeken = %d", len(console.requests))
	}
	got := console.requests[0]
	if got.key != "sleutel" {
		t.Fatalf("X-API-KEY = %q", got.key)
	}
	if got.path != "/proxy/protect/integration/v1/cameras" {
		t.Fatalf("pad = %q", got.path)
	}
}

func TestSnapshotStaysAStream(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader("jpeg-bytes")}
	client := New("console", 443, "sleutel")
	client.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-API-KEY") != "sleutel" || request.Header.Get("Accept") != "image/*" {
			t.Fatalf("snapshot headers = %#v", request.Header)
		}
		if request.URL.EscapedPath() != BasePath+"/cameras/front%20door/snapshot" ||
			request.URL.Query().Get("highQuality") != "true" {
			t.Fatalf("snapshot URL = %s", request.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
			Body:       body,
		}, nil
	})}

	response, err := client.OpenSnapshot(context.Background(), "front door", true)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if body.reads != 0 {
		t.Fatalf("OpenSnapshot read the body %d times", body.reads)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "jpeg-bytes" {
		t.Fatalf("snapshot body = %q, err = %v", data, err)
	}
}

// Bereikbaarheid is een tekst en geen vlag. Wie op een ontbrekende isConnected
// vertrouwt zet elke camera als onbereikbaar in huis -- of juist als bereikbaar,
// wat erger is.
func TestConnectionComesFromTheStateText(t *testing.T) {
	console := newConsole(t, map[string]string{"GET " + BasePath + "/cameras": camerasJSON})
	cameras, err := console.client().Cameras(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cameras) != 2 {
		t.Fatalf("%d camera's", len(cameras))
	}
	if !cameras[0].Connected() || cameras[1].Connected() {
		t.Fatalf("bereikbaarheid = %v en %v", cameras[0].State, cameras[1].State)
	}
}

// De console meldt een sleutel die niet mag met 403, en dat betekent iets
// anders dan een sleutel die niet deugt. Het verschil bepaalt wat iemand moet
// doen, dus het hoort in de melding te staan.
func TestPermissionAndAuthenticationAreDifferentComplaints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lights") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	host, port, _ := strings.Cut(address, ":")
	client := New(host, atoi(port), "sleutel")
	client.HTTP = &http.Client{Transport: plainTransport{}}

	_, err := client.Lights(context.Background())
	if err == nil || !strings.Contains(err.Error(), "may not") {
		t.Fatalf("403 gaf %v", err)
	}
	_, err = client.Cameras(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("401 gaf %v", err)
	}
}

// Het stream-adres komt van de console. Dat is het hele punt: niemand hoeft een
// RTSP-URL over te typen die morgen anders is.
func TestStreamAddressComesFromTheConsole(t *testing.T) {
	console := newConsole(t, map[string]string{
		"POST " + BasePath + "/cameras/cam1/rtsps-stream": `{"high":"rtsps://console:7441/abc?enableSrtp","medium":null}`,
	})
	streams, err := console.client().EnableStream(context.Background(), "cam1")
	if err != nil {
		t.Fatal(err)
	}
	address, ok := streams.Best()
	if !ok || address != "rtsps://console:7441/abc?enableSrtp" {
		t.Fatalf("stream = %q %v", address, ok)
	}
	// medium stond op null: een lege stream doorgeven is erger dan zeggen dat
	// hij er niet is.
	if _, exists := streams["medium"]; exists {
		t.Fatalf("een lege kwaliteit werd toch aangeboden: %#v", streams)
	}
	if body := console.requests[0].body; body["qualities"] == nil {
		t.Fatalf("de aanvraag droeg geen kwaliteiten: %#v", body)
	}
}

// Een uitgang schakelen mag de andere uitgangen niet meenemen.
func TestSwitchingOneRelayOutputLeavesTheOthers(t *testing.T) {
	console := newConsole(t, map[string]string{
		"GET " + BasePath + "/relays/r1": `{"id":"r1","name":"Garage","outputs":[
			{"id":"1","name":"Deur","state":"off"},{"id":"2","name":"Licht","state":"on"}]}`,
		"PATCH " + BasePath + "/relays/r1": `{}`,
	})
	if err := console.client().SetRelayOutput(context.Background(), "r1", "1", true); err != nil {
		t.Fatal(err)
	}
	patch := console.requests[len(console.requests)-1]
	outputs, _ := patch.body["outputs"].([]any)
	if len(outputs) != 2 {
		t.Fatalf("PATCH stuurde %d uitgangen", len(outputs))
	}
	first, _ := outputs[0].(map[string]any)
	second, _ := outputs[1].(map[string]any)
	if first["state"] != "on" {
		t.Fatalf("de geschakelde uitgang = %#v", first)
	}
	if second["state"] != "on" {
		t.Fatalf("de andere uitgang is meeveranderd: %#v", second)
	}
}

// Een uitgang die niet bestaat is een fout, geen stille PATCH die niets doet.
func TestUnknownRelayOutputFailsLoudly(t *testing.T) {
	console := newConsole(t, map[string]string{
		"GET " + BasePath + "/relays/r1": `{"id":"r1","outputs":[{"id":"1","state":"off"}]}`,
	})
	err := console.client().SetRelayOutput(context.Background(), "r1", "9", true)
	if err == nil || !strings.Contains(err.Error(), "no output 9") {
		t.Fatalf("onbekende uitgang gaf %v", err)
	}
}

// De zes standen van de schijnwerper. Nul mag geen lamp uitzetten die op tien
// procent staat: dat is de bedoeling van de schuif niet.
func TestLightLevelsCoverTheSixSteps(t *testing.T) {
	for _, test := range []struct {
		level float64
		want  int
	}{{0, 1}, {0.1, 1}, {0.5, 4}, {0.9, 6}, {1, 6}, {-1, 1}, {2, 6}} {
		if got := LedLevel(test.level); got != test.want {
			t.Errorf("LedLevel(%v) = %d, wil %d", test.level, got, test.want)
		}
	}
	if got := LightBrightness(6); got != 1 {
		t.Errorf("LightBrightness(6) = %v", got)
	}
}

// Een sensor die geen temperatuur meet stuurt null. Dat is niet nul graden.
func TestMissingReadingsStayMissing(t *testing.T) {
	console := newConsole(t, map[string]string{
		"GET " + BasePath + "/sensors/s1": `{"id":"s1","name":"Schuur","isConnected":true,
			"stats":{"temperature":{"value":null},"humidity":{"value":0},"light":{"value":12.5}}}`,
	})
	sensor, err := console.client().Sensor(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if sensor.Stats.Temperature.Value != nil {
		t.Fatalf("een ontbrekende temperatuur werd %v", *sensor.Stats.Temperature.Value)
	}
	if sensor.Stats.Humidity.Value == nil || *sensor.Stats.Humidity.Value != 0 {
		t.Fatalf("een echte nul ging verloren: %v", sensor.Stats.Humidity.Value)
	}
}

// De console antwoordt op nvrs soms met een object en soms met een lijst.
func TestNVRAcceptsBothShapes(t *testing.T) {
	for _, body := range []string{`{"id":"n1","name":"Thuis"}`, `[{"id":"n1","name":"Thuis"}]`} {
		console := newConsole(t, map[string]string{"GET " + BasePath + "/nvrs": body})
		consoles, err := console.client().NVRs(context.Background())
		if err != nil || len(consoles) != 1 || consoles[0].Name != "Thuis" {
			t.Fatalf("nvrs %s gaf %#v %v", body, consoles, err)
		}
	}
}
