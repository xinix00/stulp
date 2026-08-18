// Package wiim praat met één WiiM-speler in huis.
//
// Er lopen twee wegen naar zo'n speler, en welke waarvoor dient komt uit de
// bron en niet uit een aanname hier:
//
//   - Opdrachten gaan over de eigen HTTP-API van de speler:
//     GET https://<speler>/httpapi.asp?command=… — `drivers/player/device.js`,
//     `getBaseURL()` plus `sendCommand()`. Https met een certificaat dat de
//     speler zichzelf uitgeeft; `_docs/dev.md` doet hetzelfde met
//     `curl --insecure`.
//   - Standen komen over UPnP: de actie GetInfoEx op AVTransport van de
//     MediaRenderer die de speler op poort 49152 aanbiedt — dezelfde bron,
//     `getDeviceData()`.
//
// Dat de standen niet via httpapi.asp komen is met opzet zo gelaten. De bron
// vraagt ze daar niet op, dus wat in dat antwoord staat is hier niet te
// herleiden — en verzinnen wat een veld heet levert een app op die het bij de
// eerste echte speler niet doet. Zie PORTED.md.
package wiim

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DescriptionPort is waar een speler zijn UPnP-beschrijving aanbiedt. De
	// bron legt die poort vast (`http://${address}:49152/description.xml`).
	// Ontdekking levert hem ook op; wat de speler zelf zei wint dan.
	DescriptionPort = 49152
	// DescriptionPath hoort bij die poort.
	DescriptionPath = "/description.xml"
)

// maxBody begrenst wat er van een speler gelezen wordt.
//
// Een speler is geen vijand, maar wel een apparaat met firmware die wij niet
// schreven. Een antwoord dat niet ophoudt hoort te falen en niet het geheugen op
// te eten van een hub die verder niets bijzonders doet. Een beschrijving is een
// paar kilobyte en een GetInfoEx-antwoord met metadata blijft daar ruim onder.
const maxBody = 1 << 20

// Client is één speler.
type Client struct {
	// Address is het IP of de hostnaam van de speler, zonder poort en zonder
	// schema. CheckAddress bewaakt dat.
	Address string
	// Port is waar de beschrijving staat. Nul betekent DescriptionPort.
	Port int
	// HTTP is te vervangen voor tests. Nul betekent de standaard hieronder.
	HTTP *http.Client

	mu sync.Mutex
	// control is het controlURL van AVTransport zoals de speler het zelf
	// opgaf. Het wordt onthouden tot een aanroep mislukt: een speler die na een
	// firmware-update zijn diensten verhangt hoort niet te blijven hangen op
	// een adres van gisteren.
	control string
}

// New maakt een client voor één speler op dit adres.
func New(address string) *Client {
	return &Client{Address: address, HTTP: defaultHTTP()}
}

// defaultHTTP accepteert het certificaat van de speler zonder het te toetsen.
//
// Dat is geen slordigheid maar de enige mogelijkheid: een WiiM geeft zichzelf
// een certificaat uit dat door niets te verifiëren is, en er is geen naam om
// tegen te toetsen — je praat tegen een IP-adres in je eigen huis. De bron doet
// hetzelfde (`rejectUnauthorized: false`), en `_docs/dev.md` gebruikt
// `curl --insecure`. Wat dit dus niet dekt is iemand die al in je netwerk zit en
// verkeer kan omleiden; die kan de speler ook rechtstreeks aansturen, want
// httpapi.asp kent geen sleutel.
func defaultHTTP() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP()
}

func (c *Client) port() int {
	if c.Port > 0 {
		return c.Port
	}
	return DescriptionPort
}

// CheckAddress toetst wat iemand intypt op de koppelpagina.
//
// Alleen een host: geen schema, geen poort, geen pad. Wat er wél mag hangt
// vast in de twee adressen die deze app bouwt (https zonder poort voor
// httpapi.asp, http op 49152 voor UPnP), en een adres dat daar niet in past
// levert anders een URL op die nergens heen gaat met een foutmelding die dat
// niet zegt.
func CheckAddress(address string) error {
	trimmed := strings.TrimSpace(address)
	switch {
	case trimmed == "":
		return fmt.Errorf("vul het adres van de speler in, bijvoorbeeld 192.168.1.42")
	case strings.Contains(trimmed, "/"):
		return fmt.Errorf("%q is geen adres maar een URL; vul alleen het IP-adres of de naam in", address)
	case strings.Contains(trimmed, ":"):
		return fmt.Errorf("%q draagt een poort; die kiest de app zelf, vul alleen het adres in", address)
	case strings.ContainsAny(trimmed, " \t?&#"):
		return fmt.Errorf("%q kan geen adres zijn", address)
	}
	return nil
}

// ---------------------------------------------------------------------------
// httpapi.asp: de opdrachten
// ---------------------------------------------------------------------------

// Command stuurt één opdracht en levert wat de speler terugzei.
//
// De opdracht gaat ongewijzigd in de query. Dat is met opzet: een opdracht als
// `setPlayerCmd:vol:40` draagt dubbele punten, en of de speler die ook als
// %3A terugleest weet niemand hier — de bron plakt hem er onversleuteld achter
// en dat werkt. In ruil daarvoor wordt hier getoetst wat erin mag, zodat een
// getal dat uit een Flow-kaart komt er nooit een tweede parameter bij kan
// zetten.
func (c *Client) Command(ctx context.Context, command string) (string, error) {
	if err := checkCommand(command); err != nil {
		return "", err
	}
	address := "https://" + c.Address + "/httpapi.asp?command=" + command
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	response, err := c.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("%s: de speler antwoordt niet: %w", command, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", command, err)
	}
	if len(answer) > maxBody {
		return "", fmt.Errorf("%s: de speler stuurde meer dan %d bytes", command, maxBody)
	}
	if response.StatusCode >= 400 {
		excerpt := strings.TrimSpace(string(answer))
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "…"
		}
		if excerpt == "" {
			excerpt = http.StatusText(response.StatusCode)
		}
		return "", fmt.Errorf("%s: de speler antwoordde %d: %s", command, response.StatusCode, excerpt)
	}
	return strings.TrimSpace(string(answer)), nil
}

// checkCommand laat door wat de bron ook stuurt en niets meer.
func checkCommand(command string) error {
	if command == "" {
		return fmt.Errorf("een lege opdracht")
	}
	if len(command) > 200 {
		return fmt.Errorf("deze opdracht is te lang: %d tekens", len(command))
	}
	for _, letter := range command {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= 'A' && letter <= 'Z',
			letter >= '0' && letter <= '9', letter == ':', letter == '_',
			letter == '-', letter == '.':
		default:
			return fmt.Errorf("opdracht %q bevat een teken dat er niet in hoort: %q", command, letter)
		}
	}
	return nil
}

// De opdrachten die de bron stuurt, en verder geen. Elke regel hieronder staat
// letterlijk in `drivers/player/device.js`; wat daar niet staat, staat hier ook
// niet — de HTTP-API van WiiM kan meer, maar wat hij precies kan is hier niet
// na te lezen.

// Play hervat het afspelen (`onCapabilitySpeakerPlaying`, true).
func (c *Client) Play(ctx context.Context) error {
	_, err := c.Command(ctx, "setPlayerCmd:resume")
	return err
}

// Pause pauzeert (`onCapabilitySpeakerPlaying`, false).
func (c *Client) Pause(ctx context.Context) error {
	_, err := c.Command(ctx, "setPlayerCmd:pause")
	return err
}

// Next springt naar het volgende nummer (`onCapabilitySpeakerNext`).
func (c *Client) Next(ctx context.Context) error {
	_, err := c.Command(ctx, "setPlayerCmd:next")
	return err
}

// Previous springt naar het vorige nummer (`onCapabilitySpeakerPrev`).
func (c *Client) Previous(ctx context.Context) error {
	_, err := c.Command(ctx, "setPlayerCmd:prev")
	return err
}

// Stop zet de speler uit (`onCapabilityPlayerOff`). Dat is wat de bron onder
// "uit" verstaat: er is geen aan/uit in deze API.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.Command(ctx, "setPlayerCmd:stop")
	return err
}

// SetVolume zet het volume. level is 0..1 zoals Stulp het kent.
func (c *Client) SetVolume(ctx context.Context, level float64) error {
	_, err := c.Command(ctx, fmt.Sprintf("setPlayerCmd:vol:%d", VolumePercent(level)))
	return err
}

// SetMute dempt of hervat (`onCapabilityVolumeMute`).
func (c *Client) SetMute(ctx context.Context, on bool) error {
	value := "0"
	if on {
		value = "1"
	}
	_, err := c.Command(ctx, "setPlayerCmd:mute:"+value)
	return err
}

// SetLoopMode zet shuffle en herhalen in één keer; die twee zitten bij WiiM in
// hetzelfde getal. Zie LoopMode.
func (c *Client) SetLoopMode(ctx context.Context, mode string) error {
	_, err := c.Command(ctx, "setPlayerCmd:loopmode:"+mode)
	return err
}

// Preset drukt op een van de voorkeurtoetsen (`onCapabilityPreset`).
//
// De bron biedt er twaalf aan op de Flow-kaart en vier als knop op het
// apparaat. Twaalf is dus de grens die uit de bron komt; wat een echte speler
// aankan hangt van het model af.
func (c *Client) Preset(ctx context.Context, number int) error {
	if number < 1 || number > 12 {
		return fmt.Errorf("voorkeurtoets %d bestaat niet; de bron kent er twaalf", number)
	}
	_, err := c.Command(ctx, fmt.Sprintf("MCUKeyShortClick:%d", number))
	return err
}

// ---------------------------------------------------------------------------
// Omrekenen
// ---------------------------------------------------------------------------

// Volume rekent het volume van de speler (0..100) om naar dat van Stulp (0..1).
func Volume(percent float64) float64 {
	switch {
	case percent <= 0:
		return 0
	case percent >= 100:
		return 1
	}
	return percent / 100
}

// VolumePercent rekent de andere kant op en rondt af.
//
// De bron stuurt `value * 100` zoals het is, en dat levert in JavaScript
// geregeld een adres als `setPlayerCmd:vol:33.000000000000004` op. Een speler
// wil een geheel getal, dus hier wordt afgerond en begrensd: buiten 0..100
// bestaat er geen stand om heen te gaan.
func VolumePercent(level float64) int {
	switch {
	case math.IsNaN(level), level <= 0:
		return 0
	case level >= 1:
		return 100
	}
	return int(math.Round(level * 100))
}

// De zes loopmodes van WiiM. Shuffle en herhalen zijn bij deze speler één
// getal, dus wie er één van verzet moet de andere meesturen. De tabel staat
// letterlijk in de bron: `#convertToLoopMode` schrijft hem heen en
// `onDeviceGetInfoEx` leest hem terug.
var loopModes = map[string]struct {
	shuffle bool
	repeat  string
}{
	"0": {false, "playlist"},
	"1": {false, "track"},
	"2": {true, "playlist"},
	"3": {true, "none"},
	"4": {false, "none"},
	"5": {true, "track"},
}

// LoopMode levert het getal dat bij deze combinatie hoort.
//
// Een combinatie die er niet in staat is een fout en geen gok. De bron stuurt
// in dat geval `setPlayerCmd:loopmode:??` — een opdracht waarvan niemand weet
// wat de speler ermee doet.
func LoopMode(shuffle bool, repeat string) (string, error) {
	for mode, combination := range loopModes {
		if combination.shuffle == shuffle && combination.repeat == repeat {
			return mode, nil
		}
	}
	return "", fmt.Errorf("shuffle %v met herhalen %q kent deze speler niet", shuffle, repeat)
}

// ParseLoopMode leest het getal terug.
func ParseLoopMode(raw string) (shuffle bool, repeat string, ok bool) {
	combination, ok := loopModes[strings.TrimSpace(raw)]
	if !ok {
		return false, "", false
	}
	return combination.shuffle, combination.repeat, true
}
