package protect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Wat een gebruiker kan veranderen.
//
// Minder dan de oorspronkelijke app: de integratie-API meldt de opnamestand en
// het nachtzicht niet terug, dus die staan hier niet. Zie PORTED.md.
//
// Elke schrijfactie gaat als PATCH met precies het veld dat verandert. De
// console voegt samen; een heel object terugsturen zou instellingen overschrijven
// die iemand anders net heeft aangepast.

// SetCameraMicVolume zet het microfoonvolume, 0 tot 100.
func (c *Client) SetCameraMicVolume(ctx context.Context, id string, volume int) error {
	return c.Patch(ctx, "cameras/"+url.PathEscape(id), map[string]any{"micVolume": clamp(volume, 0, 100)})
}

// SetLightOn schakelt de schijnwerper met de hand aan of uit.
func (c *Client) SetLightOn(ctx context.Context, id string, on bool) error {
	return c.Patch(ctx, "lights/"+url.PathEscape(id), map[string]any{"isLightForceEnabled": on})
}

// SetLightLevel zet de helderheid. Het apparaat kent zes standen, dus een
// schuif van 0 tot 1 wordt daarheen vertaald in plaats van afgerond naar nul --
// een lamp die op 10% staat hoort te branden, niet uit te zijn.
func (c *Client) SetLightLevel(ctx context.Context, id string, level float64) error {
	return c.Patch(ctx, "lights/"+url.PathEscape(id), map[string]any{
		"lightDeviceSettings": map[string]any{"ledLevel": LedLevel(level)},
	})
}

// LedLevel vertaalt 0..1 naar de zes standen van het apparaat.
func LedLevel(level float64) int {
	switch {
	case level <= 0:
		return 1
	case level >= 1:
		return 6
	}
	return clamp(int(level*6)+1, 1, 6)
}

// LightBrightness is de omgekeerde weg, voor wat er van het apparaat terugkomt.
func LightBrightness(ledLevel int) float64 {
	return float64(clamp(ledLevel, 1, 6)) / 6
}

// SetLightMode zet wanneer de schijnwerper vanzelf aangaat: off, motion of
// always, met enableAt dark of fulltime.
func (c *Client) SetLightMode(ctx context.Context, id, mode, enableAt string) error {
	settings := map[string]any{"mode": mode}
	if enableAt != "" {
		settings["enableAt"] = enableAt
	}
	return c.Patch(ctx, "lights/"+url.PathEscape(id), map[string]any{"lightModeSettings": settings})
}

// SetChimeVolume zet het volume van een gong, 0 tot 100.
func (c *Client) SetChimeVolume(ctx context.Context, id string, volume int) error {
	return c.Patch(ctx, "chimes/"+url.PathEscape(id), map[string]any{"volume": clamp(volume, 0, 100)})
}

// SetRelayOutput schakelt één uitgang.
//
// De console wil de hele lijst uitgangen terug, dus die wordt eerst gelezen. Zo
// blijft de stand van de andere uitgangen staan; alleen deze verandert.
func (c *Client) SetRelayOutput(ctx context.Context, relayID, outputID string, on bool) error {
	relay, err := c.Relay(ctx, relayID)
	if err != nil {
		return err
	}
	state := "off"
	if on {
		state = "on"
	}
	outputs := make([]map[string]any, 0, len(relay.Outputs))
	found := false
	for _, output := range relay.Outputs {
		entry := map[string]any{"id": output.ID, "state": output.State}
		if output.ID == outputID {
			entry["state"] = state
			found = true
		}
		outputs = append(outputs, entry)
	}
	if !found {
		return fmt.Errorf("relay %s has no output %s", relayID, outputID)
	}
	return c.Patch(ctx, "relays/"+url.PathEscape(relayID), map[string]any{"outputs": outputs})
}

// PulseRelayOutput schakelt een uitgang kort aan. Dat is wat een garagedeur of
// een deuropener nodig heeft: een tik, geen schakelaar die aan blijft staan.
func (c *Client) PulseRelayOutput(ctx context.Context, relayID, outputID string, milliseconds int) error {
	_, err := c.Post(ctx,
		"relays/"+url.PathEscape(relayID)+"/outputs/"+url.PathEscape(outputID)+"/activate",
		map[string]any{"pulseDuration": clamp(milliseconds, 100, 10_000)})
	return err
}

// Snapshot haalt een stilstaand beeld op. De console levert JPEG.
func (c *Client) Snapshot(ctx context.Context, id string, highQuality bool) ([]byte, error) {
	query := url.Values{}
	query.Set("highQuality", boolText(highQuality))
	return c.do(ctx, "GET", "cameras/"+url.PathEscape(id)+"/snapshot", query, nil)
}

// SnapshotURL is waar dat beeld staat, voor wie het zelf ophaalt.
func (c *Client) SnapshotURL(id string, highQuality bool) string {
	query := url.Values{}
	query.Set("highQuality", boolText(highQuality))
	return c.url("cameras/"+url.PathEscape(id)+"/snapshot", query)
}

// Streams zijn de RTSPS-adressen per kwaliteit.
type Streams map[string]string

// EnableStream vraagt de console een RTSPS-stream klaar te zetten en geeft de
// adressen terug.
//
// Dit is met opzet geen instelling die iemand met de hand invult. De console
// weet zelf waar zijn stream staat, en dat adres verandert als de camera
// opnieuw ingericht wordt. Een URL die de gebruiker een keer heeft overgetypt
// is een URL die op een dag niet meer klopt.
func (c *Client) EnableStream(ctx context.Context, id string, qualities ...string) (Streams, error) {
	if len(qualities) == 0 {
		qualities = []string{"high"}
	}
	body, err := c.Post(ctx, "cameras/"+url.PathEscape(id)+"/rtsps-stream",
		map[string]any{"qualities": qualities})
	if err != nil {
		return nil, err
	}
	return decodeStreams(body)
}

// Stream leest de stream die al klaarstaat, zonder er een aan te zetten.
func (c *Client) Stream(ctx context.Context, id string) (Streams, error) {
	body, err := c.do(ctx, "GET", "cameras/"+url.PathEscape(id)+"/rtsps-stream", nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeStreams(body)
}

// decodeStreams houdt alleen de kwaliteiten over waar echt een adres bij staat.
// De console zet de andere op null, en een lege string doorgeven als stream is
// erger dan zeggen dat hij er niet is.
func decodeStreams(body []byte) (Streams, error) {
	var raw map[string]*string
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("rtsps-stream: the console sent something unexpected: %w", err)
	}
	streams := Streams{}
	for quality, address := range raw {
		if address != nil && *address != "" {
			streams[quality] = *address
		}
	}
	return streams, nil
}

// Best kiest de mooiste stream die er is.
func (s Streams) Best() (string, bool) {
	for _, quality := range []string{"high", "medium", "low", "package"} {
		if address, ok := s[quality]; ok {
			return address, true
		}
	}
	for _, address := range s {
		return address, true
	}
	return "", false
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
