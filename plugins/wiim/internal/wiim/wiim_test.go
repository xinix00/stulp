package wiim

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// speler is een nagebouwde WiiM: hij neemt httpapi.asp aan, geeft zijn
// beschrijving af en beantwoordt GetInfoEx. Genoeg om te toetsen wat wij sturen
// en wat we met een antwoord doen.
type speler struct {
	*httptest.Server

	mu       sync.Mutex
	commands []string
	action   string
	// fields zijn de velden die GetInfoEx teruggeeft. Nul betekent: een fout.
	fields map[string]string
	fault  bool
}

const descriptionXML = `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
    <friendlyName>Büro Audio</friendlyName>
    <manufacturer>Linkplay Technology Inc.</manufacturer>
    <manufacturerURL>https://wiimhome.com</manufacturerURL>
    <modelName>WiiM Pro Receiver</modelName>
    <modelNumber>V01-Apr 16 2024</modelNumber>
    <UDN>uuid:FF98F09C-74E9-46A5-07FF-155EFF98F09C</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:AVTransport</serviceId>
        <SCPDURL>/upnp/rendertransportSCPD.xml</SCPDURL>
        <controlURL>/upnp/control/rendertransport1</controlURL>
        <eventSubURL>/upnp/event/rendertransport1</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-wiimu-com:service:PlayQueue:1</serviceType>
        <serviceId>urn:wiimu-com:serviceId:PlayQueue</serviceId>
        <SCPDURL>/upnp/PlayQueueSCPD.xml</SCPDURL>
        <controlURL>/upnp/control/PlayQueue1</controlURL>
        <eventSubURL>/upnp/event/PlayQueue1</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`

// Het DIDL-Lite van een radiozender, letterlijk uit
// _docs/upnpSpecs/AVTransport/GetMediaInfo.json van de bron. Ingekort tot de
// velden die deze app leest.
const radioDIDL = `<?xml version="1.0" encoding="UTF-8"?>
<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/">
<item id="">
<dc:subtitle>David Guetta &amp; Sia - Floating through space</dc:subtitle>
<dc:title>SWR3</dc:title>
<upnp:artist></upnp:artist>
<upnp:album></upnp:album>
<upnp:albumArtURI>http://cdn-albums.tunein.com/gn/RLVRT1LV8Ng.jpg</upnp:albumArtURI>
</item>
</DIDL-Lite>`

// En hetzelfde voor een gewoon nummer uit een bibliotheek: geen ondertitel,
// wel artiest en album.
const trackDIDL = `<?xml version="1.0" encoding="UTF-8"?>
<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/">
<item id="">
<dc:title>Teardrop</dc:title>
<upnp:artist>Massive Attack</upnp:artist>
<upnp:album>Mezzanine</upnp:album>
<upnp:albumArtURI>http://192.168.50.56/art.jpg</upnp:albumArtURI>
</item>
</DIDL-Lite>`

func playingFields(metadata string) map[string]string {
	return map[string]string{
		"CurrentTransportState": "PLAYING",
		"LoopMode":              "2",
		"TrackDuration":         "00:03:21",
		"RelTime":               "00:01:05",
		"CurrentVolume":         "40",
		"CurrentMute":           "0",
		"TrackMetaData":         metadata,
		// Velden die deze app niet leest. Of een echte speler ze in dít antwoord
		// meestuurt is uit de bron niet te zien; ze staan erbij omdat het
		// ontleden er niet op mag struikelen als hij dat wel doet.
		"CurrentTransportStatus": "OK",
		"CurrentSpeed":           "1",
		"AbsTime":                "NOT_IMPLEMENTED",
		"TrackSource":            "newTuneIn",
	}
}

func newSpeler(t *testing.T, fields map[string]string) *speler {
	t.Helper()
	s := &speler{fields: fields}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/httpapi.asp":
			s.mu.Lock()
			// Bewust de rauwe query en niet r.URL.Query(): de test wil weten
			// wat er letterlijk op de lijn stond.
			s.commands = append(s.commands, strings.TrimPrefix(r.URL.RawQuery, "command="))
			s.mu.Unlock()
			w.Write([]byte("OK"))
		case "/description.xml":
			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte(descriptionXML))
		case "/upnp/control/rendertransport1":
			s.mu.Lock()
			s.action = r.Header.Get("SOAPACTION")
			fields, fault := s.fields, s.fault
			s.mu.Unlock()
			w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
			if fault {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(faultXML))
				return
			}
			w.Write([]byte(responseXML(fields)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func responseXML(fields map[string]string) string {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`)
	body.WriteString(`<u:GetInfoExResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`)
	for name, value := range fields {
		body.WriteString("<" + name + ">")
		xml.EscapeText(&body, []byte(value))
		body.WriteString("</" + name + ">")
	}
	body.WriteString(`</u:GetInfoExResponse></s:Body></s:Envelope>`)
	return body.String()
}

const faultXML = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>
<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>
<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
<errorCode>701</errorCode><errorDescription>Transition not available</errorDescription>
</UPnPError></detail></s:Fault></s:Body></s:Envelope>`

// gezien is wat de client écht wilde bereiken, voordat het naar de testserver
// werd omgeleid: schema, host en pad. Dat is waar de test op let, want daar
// zitten de twee wegen in die deze app gebruikt.
type gezien struct{ scheme, host, path, query string }

type omleiding struct {
	target *url.URL
	inner  http.RoundTripper

	mu  *sync.Mutex
	log *[]gezien
}

func (o omleiding) RoundTrip(request *http.Request) (*http.Response, error) {
	o.mu.Lock()
	*o.log = append(*o.log, gezien{request.URL.Scheme, request.URL.Host, request.URL.Path, request.URL.RawQuery})
	o.mu.Unlock()

	clone := request.Clone(request.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = o.target.Host
	return o.inner.RoundTrip(clone)
}

// client wijst naar de nagebouwde speler. httptest praat plain http op een
// eigen poort, dus alles wordt daarheen omgeleid.
func (s *speler) client(t *testing.T) (*Client, *[]gezien) {
	t.Helper()
	target, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	log := &[]gezien{}
	client := New("192.168.50.56")
	client.HTTP = &http.Client{Transport: omleiding{
		target: target, inner: http.DefaultTransport, mu: &sync.Mutex{}, log: log,
	}}
	return client, log
}

func (s *speler) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Opdrachten gaan over https naar httpapi.asp, met de dubbele punten er
// onversleuteld in. De bron plakt de opdracht net zo achter de query
// (`sendCommand`), en of een speler %3A ook terugleest weet niemand hier.
func TestCommandsGoToHttpapiWithTheirColonsIntact(t *testing.T) {
	speler := newSpeler(t, playingFields(trackDIDL))
	client, log := speler.client(t)

	if err := client.SetVolume(context.Background(), 0.4); err != nil {
		t.Fatal(err)
	}
	sent := speler.sent()
	if len(sent) != 1 || sent[0] != "setPlayerCmd:vol:40" {
		t.Fatalf("verstuurd: %q", sent)
	}
	if got := (*log)[0]; got.scheme != "https" || got.host != "192.168.50.56" || got.path != "/httpapi.asp" {
		t.Fatalf("de opdracht ging naar %+v", got)
	}
}

// Volume is bij Stulp 0..1 en bij WiiM 0..100. Beide kanten op, en zonder
// nakomma's op de lijn: de bron stuurt `value * 100` en levert daarmee
// geregeld setPlayerCmd:vol:33.000000000000004 af.
func TestVolumeTravelsBothWays(t *testing.T) {
	for _, test := range []struct {
		level float64
		want  int
	}{{0, 0}, {0.005, 1}, {0.33, 33}, {0.4, 40}, {0.999, 100}, {1, 100}, {-1, 0}, {2, 100}} {
		if got := VolumePercent(test.level); got != test.want {
			t.Errorf("VolumePercent(%v) = %d, wil %d", test.level, got, test.want)
		}
	}
	for _, test := range []struct {
		percent float64
		want    float64
	}{{0, 0}, {40, 0.4}, {100, 1}, {-5, 0}, {120, 1}} {
		if got := Volume(test.percent); got != test.want {
			t.Errorf("Volume(%v) = %v, wil %v", test.percent, got, test.want)
		}
	}
	// En heen en terug moet hetzelfde procent opleveren, anders springt de
	// schuif terug zodra de volgende ronde binnenkomt.
	for percent := 0; percent <= 100; percent++ {
		if got := VolumePercent(Volume(float64(percent))); got != percent {
			t.Fatalf("%d procent werd %d na heen en terug", percent, got)
		}
	}
}

// Het hele antwoord van GetInfoEx, van de SOAP-envelop tot de metadata van het
// nummer. Dit is de ronde die elke vijf seconden draait, dus alles wat op de
// tegel komt hangt hieraan.
func TestStatusIsReadFromGetInfoEx(t *testing.T) {
	speler := newSpeler(t, playingFields(trackDIDL))
	client, log := speler.client(t)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Playing() || status.State != "PLAYING" {
		t.Errorf("stand = %q", status.State)
	}
	if status.Volume != 0.4 || status.Muted {
		t.Errorf("volume = %v, gedempt = %v", status.Volume, status.Muted)
	}
	if !status.Loop.Known || !status.Loop.Shuffle || status.Loop.Repeat != "playlist" {
		t.Errorf("loopmode 2 werd %+v", status.Loop)
	}
	if !status.Duration.Known || status.Duration.Value != 201 {
		t.Errorf("duur = %+v, wil 201 seconden", status.Duration)
	}
	if !status.Position.Known || status.Position.Value != 65 {
		t.Errorf("positie = %+v, wil 65 seconden", status.Position)
	}
	if status.Track.Artist != "Massive Attack, Mezzanine" ||
		status.Track.Album != "Mezzanine" || status.Track.Title != "Teardrop" {
		t.Errorf("nummer = %+v", status.Track)
	}

	// De beschrijving eerst, en daarna het controladres dat de speler zelf
	// opgaf -- niet een pad dat wij verzonnen hebben. Beide over http op 49152,
	// want UPnP loopt niet over de https-poort.
	if len(*log) != 2 {
		t.Fatalf("verzoeken: %+v", *log)
	}
	if got := (*log)[0]; got.scheme != "http" || got.host != "192.168.50.56:49152" || got.path != "/description.xml" {
		t.Errorf("de beschrijving werd gehaald bij %+v", got)
	}
	if got := (*log)[1]; got.scheme != "http" || got.path != "/upnp/control/rendertransport1" {
		t.Errorf("de actie ging naar %+v", got)
	}
	speler.mu.Lock()
	action := speler.action
	speler.mu.Unlock()
	if action != `"urn:schemas-upnp-org:service:AVTransport:1#GetInfoEx"` {
		t.Errorf("SOAPACTION = %q", action)
	}

	// De tweede ronde vraagt de beschrijving niet opnieuw op.
	if _, err := client.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 3 {
		t.Errorf("de tweede ronde deed %d verzoeken in plaats van 1", len(*log)-2)
	}
}

// Radio is de vorm waar de indeling van de bron vandaan komt: dc:title is de
// zender en dc:subtitle wat er klinkt. Zonder die omkering staat er "SWR3" als
// nummer en niets als artiest.
func TestRadioPutsTheStationInFrontOfTheSong(t *testing.T) {
	speler := newSpeler(t, playingFields(radioDIDL))
	client, _ := speler.client(t)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Track.Artist != "SWR3" || status.Track.Album != "SWR3" {
		t.Errorf("zender = %+v", status.Track)
	}
	if status.Track.Title != "David Guetta & Sia - Floating through space" {
		t.Errorf("titel = %q", status.Track.Title)
	}
	if status.Track.ArtURI != "http://cdn-albums.tunein.com/gn/RLVRT1LV8Ng.jpg" {
		t.Errorf("hoes = %q", status.Track.ArtURI)
	}
}

// Zonder medium hoort er niets te staan in plaats van wat er tien minuten
// geleden speelde.
func TestWithoutMediaTheTrackIsEmpty(t *testing.T) {
	fields := playingFields("")
	fields["CurrentTransportState"] = "NO_MEDIA_PRESENT"
	fields["TrackDuration"] = "00:00:00"
	fields["RelTime"] = "00:00:00"
	speler := newSpeler(t, fields)
	client, _ := speler.client(t)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Playing() {
		t.Error("NO_MEDIA_PRESENT telt als spelen")
	}
	if status.Track.Present || status.Track.Title != "" || status.Track.Artist != "" {
		t.Errorf("nummer = %+v", status.Track)
	}
}

// Een speler die niet antwoordt hoort een fout op te leveren waar het adres en
// de opdracht in staan -- niet een lege stand die eruitziet als "er speelt
// niets".
func TestAPlayerThatDoesNotAnswer(t *testing.T) {
	speler := newSpeler(t, playingFields(trackDIDL))
	client, _ := speler.client(t)
	speler.Close() // de speler staat uit

	if _, err := client.Status(context.Background()); err == nil {
		t.Fatal("een dode speler gaf een stand")
	} else if !strings.Contains(err.Error(), "description.xml") {
		t.Errorf("de melding zegt niet waar het misging: %v", err)
	}
	err := client.Play(context.Background())
	if err == nil {
		t.Fatal("een dode speler nam een opdracht aan")
	}
	if !strings.Contains(err.Error(), "setPlayerCmd:resume") ||
		!strings.Contains(err.Error(), "antwoordt niet") {
		t.Errorf("de melding helpt niemand verder: %v", err)
	}
}

// En hetzelfde voor een speler die de verbinding aanneemt en dan blijft hangen:
// de ronde hoort af te lopen op zijn eigen tijd en niet te blijven staan.
func TestAPlayerThatKeepsQuietRunsOut(t *testing.T) {
	stall := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-stall
	}))
	defer func() { close(stall); server.Close() }()

	target, _ := url.Parse(server.URL)
	client := New("192.168.50.56")
	client.HTTP = &http.Client{Transport: omleiding{
		target: target, inner: http.DefaultTransport, mu: &sync.Mutex{}, log: &[]gezien{},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := client.Status(ctx); err == nil {
		t.Fatal("een speler die niets zegt gaf een stand")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("de ronde bleef %v hangen", time.Since(start))
	}
}

// Een veld dat ontbreekt is een antwoord waar deze app niet op gebouwd is, en
// dat hoort te vallen met de naam erbij. Een tegel die stil op nul blijft staan
// kost meer tijd dan een melding.
func TestAMissingFieldFailsLoudly(t *testing.T) {
	fields := playingFields(trackDIDL)
	delete(fields, "CurrentVolume")
	speler := newSpeler(t, fields)
	client, _ := speler.client(t)

	_, err := client.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CurrentVolume") {
		t.Fatalf("een ontbrekend veld gaf %v", err)
	}
}

// Een loopmode die deze app niet kent laat shuffle en herhalen ongemoeid, maar
// de rest van de ronde gaat gewoon door: één raar veld hoort niet het volume en
// de tijd mee te nemen.
func TestAnUnknownLoopModeDoesNotSinkTheRound(t *testing.T) {
	fields := playingFields(trackDIDL)
	fields["LoopMode"] = "9"
	speler := newSpeler(t, fields)
	client, _ := speler.client(t)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Loop.Known {
		t.Errorf("loopmode 9 werd %+v", status.Loop)
	}
	if status.Loop.Raw != "9" {
		t.Errorf("de rauwe waarde ontbreekt in de melding: %+v", status.Loop)
	}
	if status.Volume != 0.4 {
		t.Errorf("het volume ging verloren: %v", status.Volume)
	}
}

// De zes standen heen en terug. Shuffle en herhalen zijn bij WiiM één getal,
// dus wie er één van verzet moet de andere meesturen -- en een combinatie die
// er niet in staat is een fout en geen gok. De bron stuurt in dat geval
// `loopmode:??`.
func TestLoopModesGoBothWays(t *testing.T) {
	for mode, want := range loopModes {
		shuffle, repeat, ok := ParseLoopMode(mode)
		if !ok || shuffle != want.shuffle || repeat != want.repeat {
			t.Fatalf("ParseLoopMode(%q) = %v %q %v", mode, shuffle, repeat, ok)
		}
		got, err := LoopMode(shuffle, repeat)
		if err != nil || got != mode {
			t.Fatalf("LoopMode(%v, %q) = %q, %v", shuffle, repeat, got, err)
		}
	}
	if _, _, ok := ParseLoopMode("9"); ok {
		t.Error("loopmode 9 werd geaccepteerd")
	}
	if _, err := LoopMode(true, "somtijds"); err == nil {
		t.Error("een verzonnen herhaalstand leverde een loopmode op")
	}
}

// Tijden zijn seconden. De bron rekent hier mis (`#convertTimeToNumber` doet
// uren*216000 + minuten*3600 + seconden*60, elke term zestig keer te groot) en
// die fout is niet meegeport.
func TestTimesAreSeconds(t *testing.T) {
	for _, test := range []struct {
		raw   string
		want  float64
		known bool
	}{
		{"00:00:00", 0, true},
		{"00:03:21", 201, true},
		{"01:00:00", 3600, true},
		{"00:03:21.500", 201.5, true},
		{"NOT_IMPLEMENTED", 0, false},
		{"", 0, false},
	} {
		got, err := ParseTime(test.raw)
		if err != nil {
			t.Fatalf("ParseTime(%q): %v", test.raw, err)
		}
		if got.Known != test.known || got.Value != test.want {
			t.Errorf("ParseTime(%q) = %+v, wil %v (bekend: %v)", test.raw, got, test.want, test.known)
		}
	}
	for _, raw := range []string{"drie minuten", "00:03", "-1:00:00"} {
		if _, err := ParseTime(raw); err == nil {
			t.Errorf("ParseTime(%q) kwam er zonder klacht doorheen", raw)
		}
	}
}

// Een UPnP-fout komt met een code en een beschrijving; die horen in de melding
// en niet weggevouwen te worden tot "er ging iets mis".
func TestASoapFaultBecomesAMessage(t *testing.T) {
	speler := newSpeler(t, playingFields(trackDIDL))
	speler.mu.Lock()
	speler.fault = true
	speler.mu.Unlock()
	client, _ := speler.client(t)

	_, err := client.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Transition not available") ||
		!strings.Contains(err.Error(), "701") {
		t.Fatalf("een SOAP-fout gaf %v", err)
	}
}

// Een getal dat uit een Flow-kaart komt mag er geen tweede parameter bij
// zetten. De opdracht gaat onversleuteld in de query, dus dit is waar dat
// bewaakt wordt.
func TestNothingSneaksIntoTheQuery(t *testing.T) {
	speler := newSpeler(t, playingFields(trackDIDL))
	client, _ := speler.client(t)

	for _, command := range []string{
		"setPlayerCmd:vol:40&command=reboot",
		"setPlayerCmd:vol:40 en nog wat",
		"getStatus?x=1",
		"",
	} {
		if _, err := client.Command(context.Background(), command); err == nil {
			t.Errorf("opdracht %q werd verstuurd", command)
		}
	}
	if len(speler.sent()) != 0 {
		t.Errorf("er ging toch iets naar de speler: %q", speler.sent())
	}
}

// De bron kent twaalf voorkeurtoetsen op de Flow-kaart. Wat daarbuiten valt is
// een fout en geen commando dat de speler maar moet uitzoeken.
func TestPresetsStayInsideWhatTheSourceKnows(t *testing.T) {
	speler := newSpeler(t, playingFields(trackDIDL))
	client, _ := speler.client(t)

	if err := client.Preset(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if sent := speler.sent(); len(sent) != 1 || sent[0] != "MCUKeyShortClick:3" {
		t.Fatalf("verstuurd: %q", sent)
	}
	for _, number := range []int{0, -1, 13, 99} {
		if err := client.Preset(context.Background(), number); err == nil {
			t.Errorf("voorkeurtoets %d werd verstuurd", number)
		}
	}
}

// Een adres met een schema, een poort of een pad hoort geweigerd te worden op
// het moment dat iemand het intypt -- niet pas als er nooit iets antwoordt.
func TestAddressesAreCheckedWhereTheyAreTyped(t *testing.T) {
	for _, address := range []string{"192.168.1.42", "wiim-pro.local"} {
		if err := CheckAddress(address); err != nil {
			t.Errorf("CheckAddress(%q) = %v", address, err)
		}
	}
	for _, address := range []string{"", "http://192.168.1.42", "192.168.1.42:8080", "192.168.1.42/httpapi.asp"} {
		if err := CheckAddress(address); err == nil {
			t.Errorf("CheckAddress(%q) liet het door", address)
		}
	}
}
