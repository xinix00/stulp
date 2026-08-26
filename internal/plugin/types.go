package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/store"
)

// Options zijn de vaste gegevens die elke app bij de start meekrijgt.
type Options struct {
	// OnExit draait als het proces uit zichzelf stopt: een crash, een panic,
	// of iemand die het van buitenaf omlegt. Niet bij een stop die Stulp zelf
	// gevraagd heeft -- die weet hij al.
	OnExit func(appID string, err error)

	Language     string
	Timezone     string
	StulpID      string
	StulpVersion string
	CallTimeout  time.Duration
	Logger       *slog.Logger

	// ImageSources en ImageURL zijn het afbeeldingsregister van Stulp: welke
	// apparaten een stilstaand beeld hebben aangemeld, en waar dat beeld nu te
	// halen valt.
	//
	// Ze staan hier als functie omdat een proces alleen zijn eigen app kent,
	// terwijl het register over alle apps loopt -- dat overzicht heeft alleen de
	// supervisor. En het is met opzet géén plugin die een andere plugin aanroept:
	// die naad bestaat niet. Een plugin vraagt het aan Stulp, en Stulp geeft media
	// al door voor de interface; hij doet hier hetzelfde voor een plugin.
	ImageSources func(ctx context.Context) ([]ImageRegistration, error)
	ImageURL     func(ctx context.Context, deviceID, slot string) (string, error)

	// HomeCapability is the deliberately narrow cross-app control seam used by
	// trusted bridge apps. Process still enforces the manifest permission and
	// refuses self-invocation before this callback is reached.
	HomeCapability func(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) error

	// NewRuntime kiest welke implementatie NewRuntime teruggeeft. Nil betekent
	// het plugin-proces, en dat is de enige die er is.
	NewRuntime func(ctx context.Context, database *store.Store, appID string, options Options) (Runtime, error)
}

// ImageRegistration is één plek waar een afbeelding te halen valt: een apparaat
// dat een stilstaand beeld heeft aangemeld. Slot is leeg als er niets te kiezen
// valt.
type ImageRegistration struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	Slot       string `json:"slot"`
	Title      string `json:"title"`
}

// RegistrationSnapshot is wat een app aan Stulp meldt dat hij aankan.
type RegistrationSnapshot struct {
	Drivers []string            `json:"drivers"`
	Flows   []FlowRegistration  `json:"flows"`
	Tokens  []map[string]any    `json:"tokens"`
	Widgets []map[string]any    `json:"widgets"`
	Media   []MediaRegistration `json:"media"`
}

// UIAsset is één bestand uit de ingebedde instellingen- of koppelinterface van
// een aangemelde app. Found maakt een gewone ontbrekende asset onderscheidbaar
// van een verbroken appverbinding.
type UIAsset struct {
	Found bool   `json:"found"`
	Data  []byte `json:"data,omitempty"`
}

type FlowRegistration struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	RunListener  bool     `json:"runListener"`
	Autocomplete []string `json:"autocomplete"`
}

// MediaRegistration is één slot dat een device gevuld heeft.
type MediaRegistration struct {
	DeviceID   string         `json:"deviceId"`
	Slot       string         `json:"slot"`
	Title      string         `json:"title"`
	Kind       string         `json:"kind"`
	ResourceID string         `json:"resourceId"`
	Options    map[string]any `json:"options,omitempty"`
}

// VideoStream is waar Stulp de stream ophaalt om hem door te geven.
type VideoStream struct {
	DeviceID    string `json:"deviceId"`
	Slot        string `json:"slot"`
	Title       string `json:"title"`
	ResourceID  string `json:"resourceId"`
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
}

// driverSettingDefaults leest de standaardwaarden die het manifest per driver
// opgeeft. Een gekoppeld apparaat begint daarmee, zodat de instellingenpagina
// niet leeg is voordat de gebruiker er iets aan gedaan heeft.
func driverSettingDefaults(driver manifest.DriverManifest) map[string]any {
	settings := make(map[string]any)
	for _, raw := range driver.Settings {
		setting, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := setting["id"].(string)
		if id != "" && setting["value"] != nil {
			settings[id] = setting["value"]
		}
	}
	return settings
}

// remarshal zet een losse waarde om naar een getypeerd doel, via JSON. Dat is
// de vorm waarin alles over het protocol binnenkomt.
func remarshal(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
