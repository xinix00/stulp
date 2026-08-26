package appsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xinix00/stulp/internal/appproto"
)

// Host is de kant die Stulp aanraakt: de handshake, de schrijfacties en de log.
type Host struct {
	session *appproto.Session
	state   *State
	// out is waar loggen heen gaat. Standaard stderr, dat Stulp uitleest; een
	// test zet er iets anders neer.
	out io.Writer
}

func NewHost(session *appproto.Session, state *State) *Host {
	return &Host{session: session, state: state}
}

// Handshake wisselt hello/welcome uit en vult de lokale kopie.
func (h *Host) Handshake(ctx context.Context) error {
	raw, err := h.session.Call(ctx, "hello", Hello{Protocol: ProtocolVersion})
	if err != nil {
		return err
	}
	var w Welcome
	if err := jsonUnmarshal(raw, &w); err != nil {
		return err
	}
	if w.Protocol != ProtocolVersion {
		return fmt.Errorf("protocol %d, this binary speaks %d", w.Protocol, ProtocolVersion)
	}
	h.state.Load(w)
	return nil
}

func (h *Host) State() *State { return h.state }

// ---------------------------------------------------------------------------
// Lezen -- alles uit de lokale kopie
// ---------------------------------------------------------------------------

func (h *Host) AppID() string                  { return h.state.AppID() }
func (h *Host) StulpID() string                { return h.state.StulpID() }
func (h *Host) StulpVersion() string           { return h.state.StulpVersion() }
func (h *Host) Language() string               { return h.state.Language() }
func (h *Host) Timezone() string               { return h.state.Timezone() }
func (h *Host) Manifest() map[string]any       { return h.state.Manifest() }
func (h *Host) Env() map[string]any            { return h.state.Env() }
func (h *Host) LocaleStrings() map[string]any  { return h.state.LocaleStrings() }
func (h *Host) Setting(key string) (any, bool) { return h.state.Setting(key) }
func (h *Host) SettingKeys() []string          { return h.state.SettingKeys() }

func (h *Host) DriverManifest(driverID string) (map[string]any, bool) {
	return h.state.DriverManifest(driverID)
}

func (h *Host) CapabilityOptions(driverID, capabilityID string) (map[string]any, bool) {
	return h.state.CapabilityOptions(driverID, capabilityID)
}

// DeviceField levert een fout voor een onbekend device, en nil voor een veld dat
// dit device niet heeft -- een device zonder store heeft er gewoon geen.
func (h *Host) DeviceField(deviceID, field string) (any, error) {
	if _, known := h.state.DeviceField(deviceID, "name"); !known {
		if _, any := h.state.DeviceField(deviceID, field); !any {
			return nil, fmt.Errorf("device %s is not known to this app", deviceID)
		}
	}
	value, _ := h.state.DeviceField(deviceID, field)
	return value, nil
}

// Translate lost een locale-sleutel op, met {{tag}}-vervanging.
func (h *Host) Translate(key string, tags map[string]any) string {
	value := lookupPath(h.state.LocaleStrings(), key)
	if value == nil {
		return key
	}
	text := fmt.Sprint(value)
	for name, tag := range tags {
		text = strings.ReplaceAll(text, "{{"+name+"}}", fmt.Sprint(tag))
	}
	return text
}

// lookupPath loopt een sleutel met punten af: "device.error.offline".
func lookupPath(root map[string]any, key string) any {
	var current any = root
	for _, part := range strings.Split(key, ".") {
		table, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = table[part]
		if !ok {
			return nil
		}
	}
	return current
}

// AppState komt bij de handshake mee en wordt bij elke schrijfactie bijgewerkt.
func (h *Host) AppState() json.RawMessage { return h.state.AppState() }

// ---------------------------------------------------------------------------
// Schrijven -- elk een request naar Stulp
// ---------------------------------------------------------------------------

func (h *Host) SetAppState(state json.RawMessage) error {
	if err := h.write("state.set", map[string]any{"state": state}); err != nil {
		return err
	}
	h.state.setAppState(state)
	return nil
}

func (h *Host) SetDeviceField(deviceID, field string, value any) error {
	return h.write("device.set", map[string]any{"deviceId": deviceID, "field": field, "value": value})
}

func (h *Host) MergeDeviceMap(deviceID, field string, patch map[string]any) error {
	return h.write("device.merge", map[string]any{"deviceId": deviceID, "field": field, "patch": patch})
}

func (h *Host) SetSetting(key string, value any) error {
	return h.write("setting.set", map[string]any{"key": key, "value": value})
}

func (h *Host) UnsetSetting(key string) error {
	return h.write("setting.unset", map[string]any{"key": key})
}

func (h *Host) AddCapability(deviceID, capability string) error {
	return h.write("capability.add", map[string]any{"deviceId": deviceID, "capability": capability})
}

func (h *Host) RemoveCapability(deviceID, capability string) error {
	return h.write("capability.remove", map[string]any{"deviceId": deviceID, "capability": capability})
}

func (h *Host) SetCapabilityOptions(deviceID, capabilityID string, options map[string]any) error {
	return h.write("capability.options", map[string]any{
		"deviceId": deviceID, "capability": capabilityID, "options": options,
	})
}

func (h *Host) TriggerFlow(kind, id string, tokens, state any) error {
	return h.write("flow.trigger", map[string]any{
		"kind": kind, "id": id, "tokens": tokens, "state": state,
	})
}

func (h *Host) ReplaceDeviceReferences(replacements map[string]DeviceReplacement) error {
	return h.write("device.replace", map[string]any{"replacements": replacements})
}

func (h *Host) Notification(excerpt string) error {
	return h.write("notification", map[string]any{"excerpt": excerpt})
}

func (h *Host) SetHomeCapability(deviceID, capabilityID string, value any) error {
	return h.write("home.capability.invoke", map[string]any{
		"deviceId": deviceID, "capability": capabilityID, "value": value,
	})
}

// write stuurt een request en wacht op de bevestiging.
//
// Dat antwoord is wat read-your-own-writes waar maakt: Stulp duwt de nieuwe
// stand door vóór hij antwoordt, dus de lokale kopie klopt zodra dit terugkeert.
func (h *Host) write(method string, params any) error {
	_, err := h.session.Call(context.Background(), method, params)
	return err
}

// ask is een vraag mét antwoord. Er zijn er weinig: bijna alles wat een app wil
// weten staat al in de lokale kopie. Wat hier langskomt is wat alleen Stulp op dit
// moment kan weten.
func (h *Host) ask(method string, params any, out any) error {
	raw, err := h.session.Call(context.Background(), method, params)
	if err != nil {
		return err
	}
	return jsonUnmarshal(raw, out)
}

// ImageRegistration is één plek waar een afbeelding te halen valt: een apparaat
// dat een stilstaand beeld heeft aangemeld, van welke app dan ook.
//
// Naast MediaRegistration, dat per apparaat zegt wat het heeft. Dit is dezelfde
// vraag over het hele huis.
type ImageRegistration struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	Slot       string `json:"slot"`
	Title      string `json:"title"`
}

// ImageSources vraagt Stulp welke afbeeldingen er in huis te halen zijn.
//
// Dit is geen weg naar een andere plugin -- die bestaat niet. Het is het register
// dat Stulp bijhoudt omdat hij media toch al doorgeeft: een camera meldt met
// SetCameraImage aan dat hij een stilstaand beeld heeft, en dat is precies wat
// hier terugkomt. Wie er iets mee wil, bijvoorbeeld een foto bij een melding,
// vraagt daarna ImageURL.
func (h *Host) ImageSources() ([]ImageRegistration, error) {
	var sources []ImageRegistration
	if err := h.ask("images.list", map[string]any{}, &sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// ImageURL laat Stulp nu een afbeelding ophalen en geeft het adres waarop hij een
// kwartier te zien is.
//
// Een leeg slot betekent het eerste stilstaande beeld van dat apparaat, want een
// camera heeft er meestal één. Het adres is relatief: waar Stulp staat weet de
// browser en een plugin niet.
func (h *Host) ImageURL(deviceID, slot string) (string, error) {
	var answer struct {
		URL string `json:"url"`
	}
	if err := h.ask("image.url", map[string]any{"deviceId": deviceID, "slot": slot}, &answer); err != nil {
		return "", err
	}
	return answer.URL, nil
}

// Log schrijft naar stderr, niet over het protocol.
//
// Stulp start dit proces, dus hij leest die stroom al. Een logregel hoeft er
// geen bericht voor te zijn, en het scheelt niet alleen een protocolbericht: wat
// een plugin buiten de SDK om uitspuugt -- een panic met stack trace, een
// bibliotheek die zelf naar stderr schrijft -- komt langs dezelfde weg en dus
// in dezelfde volgorde binnen.
// Log schrijft één regel naar stderr, die Stulp uitleest en in zijn eigen log
// zet. Het niveau staat vooraan met een tab erachter, want daar herkent Stulp
// het aan.
//
// Nieuwe regels in het bericht worden vervangen: Stulp leest regel voor regel,
// dus wat over twee regels loopt komt aan als twee meldingen waarvan de tweede
// geen niveau heeft. Een lange melding samengevouwen lezen is vervelend; een
// waarschuwing die als info binnenkomt is erger.
func (h *Host) Log(level, message string) {
	out := h.out
	if out == nil {
		out = os.Stderr
	}
	if strings.ContainsAny(message, "\r\n") {
		message = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(message)
	}
	fmt.Fprintf(out, "%s\t%s\n", level, message)
}

func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }
