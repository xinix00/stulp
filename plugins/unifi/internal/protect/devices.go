package protect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// De velden die deze plugin gebruikt, en niet meer.
//
// Een console stuurt per apparaat tientallen velden. Ze allemaal overnemen zou
// een tweede model opleveren dat bijgehouden moet worden zonder dat iemand er
// iets aan heeft. Wat hier staat is wat er ergens in een capability terechtkomt
// -- en wat er niet in staat is niet vergeten maar niet gebruikt; PORTED.md zegt
// wat er verder in het antwoord zit.

// Camera is wat de integratie-API van een camera vertelt.
//
// Dat is minder dan de oorspronkelijke app aannam. Getoetst tegen een echte
// console op 2026-08-09: er is geen isConnected, geen isRecording, geen
// ispSettings en geen recordingSettings. De opnamestand en het nachtzicht zijn
// via deze API niet te lezen, en een tegel die je wel kunt indrukken maar nooit
// terugleest is erger dan geen tegel.
type Camera struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"modelKey"`
	MAC   string `json:"mac"`

	// State is "CONNECTED" of iets anders. Een tekst en geen vlag, en het is
	// het enige wat de console over bereikbaarheid zegt.
	State string `json:"state"`

	IsMicEnabled bool `json:"isMicEnabled"`
	MicVolume    int  `json:"micVolume"`

	FeatureFlags struct {
		HasSpeaker   bool     `json:"hasSpeaker"`
		HasMic       bool     `json:"hasMic"`
		HasLedStatus bool     `json:"hasLedStatus"`
		HasHDR       bool     `json:"hasHdr"`
		SmartDetects []string `json:"smartDetectTypes"`
	} `json:"featureFlags"`
}

// Connected zegt of de console deze camera nu ziet.
func (c Camera) Connected() bool { return c.State == "CONNECTED" }

// Light is een UniFi Protect-schijnwerper.
type Light struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Model       string `json:"modelKey"`
	Host        string `json:"host"`
	IsConnected bool   `json:"isConnected"`
	IsLightOn   bool   `json:"isLightOn"`

	IsPirMotionDetected bool `json:"isPirMotionDetected"`

	LightDeviceSettings struct {
		// LedLevel loopt van 1 tot 6 op het apparaat zelf.
		LedLevel int `json:"ledLevel"`
	} `json:"lightDeviceSettings"`

	LightModeSettings struct {
		Mode     string `json:"mode"`
		EnableAt string `json:"enableAt"`
	} `json:"lightModeSettings"`
}

// Sensor is een UP-Sense: contact, beweging, temperatuur, vocht en licht in één.
type Sensor struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Model       string `json:"modelKey"`
	IsConnected bool   `json:"isConnected"`

	IsOpened            bool   `json:"isOpened"`
	IsMotionDetected    bool   `json:"isMotionDetected"`
	TamperingDetectedAt *int64 `json:"tamperingDetectedAt"`

	BatteryStatus struct {
		Percentage int  `json:"percentage"`
		IsLow      bool `json:"isLow"`
	} `json:"batteryStatus"`

	Stats struct {
		Temperature sensorReading `json:"temperature"`
		Humidity    sensorReading `json:"humidity"`
		Light       sensorReading `json:"light"`
	} `json:"stats"`
}

// sensorReading is één meetwaarde. Value is een pointer omdat de console null
// stuurt voor een meting die dit apparaat niet doet -- en nul is een geldige
// temperatuur, dus dat verschil mag niet verloren gaan.
type sensorReading struct {
	Value  *float64 `json:"value"`
	Status string   `json:"status"`
}

// Chime is een losse gong.
type Chime struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Model       string `json:"modelKey"`
	IsConnected bool   `json:"isConnected"`
	Volume      int    `json:"volume"`
}

// Relay is een schakelmodule met één of meer uitgangen.
type Relay struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Model       string        `json:"modelKey"`
	IsConnected bool          `json:"isConnected"`
	Outputs     []RelayOutput `json:"outputs"`
}

// RelayOutput is één uitgang. State is "on" of "off".
type RelayOutput struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// On leest de stand van een uitgang.
func (o RelayOutput) On() bool { return o.State == "on" }

// NVR is de console zelf.
//
// Naam en id, en verder niets bruikbaars: getoetst tegen een echte console
// draagt dit object geen versie en geen adres. De oorspronkelijke app leest die
// uit de oudere API, die deze plugin niet gebruikt.
type NVR struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) Cameras(ctx context.Context) ([]Camera, error) {
	return getJSON[[]Camera](ctx, c, "cameras", nil)
}

func (c *Client) Camera(ctx context.Context, id string) (Camera, error) {
	return getJSON[Camera](ctx, c, "cameras/"+url.PathEscape(id), nil)
}

func (c *Client) Lights(ctx context.Context) ([]Light, error) {
	return getJSON[[]Light](ctx, c, "lights", nil)
}

func (c *Client) Light(ctx context.Context, id string) (Light, error) {
	return getJSON[Light](ctx, c, "lights/"+url.PathEscape(id), nil)
}

func (c *Client) Sensors(ctx context.Context) ([]Sensor, error) {
	return getJSON[[]Sensor](ctx, c, "sensors", nil)
}

func (c *Client) Sensor(ctx context.Context, id string) (Sensor, error) {
	return getJSON[Sensor](ctx, c, "sensors/"+url.PathEscape(id), nil)
}

func (c *Client) Chimes(ctx context.Context) ([]Chime, error) {
	return getJSON[[]Chime](ctx, c, "chimes", nil)
}

func (c *Client) Chime(ctx context.Context, id string) (Chime, error) {
	return getJSON[Chime](ctx, c, "chimes/"+url.PathEscape(id), nil)
}

func (c *Client) Relays(ctx context.Context) ([]Relay, error) {
	return getJSON[[]Relay](ctx, c, "relays", nil)
}

func (c *Client) Relay(ctx context.Context, id string) (Relay, error) {
	return getJSON[Relay](ctx, c, "relays/"+url.PathEscape(id), nil)
}

// NVRs levert de console. Het eindpunt heet nvrs maar er is er altijd één.
func (c *Client) NVRs(ctx context.Context) ([]NVR, error) {
	body, err := c.do(ctx, "GET", "nvrs", nil, nil)
	if err != nil {
		return nil, err
	}
	// De console antwoordt hier soms met één object en soms met een lijst.
	// Beide accepteren is goedkoper dan uitzoeken welke firmware wat doet.
	var list []NVR
	if json.Unmarshal(body, &list) == nil {
		return list, nil
	}
	var single NVR
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("GET nvrs: the console sent something unexpected: %w", err)
	}
	return []NVR{single}, nil
}
