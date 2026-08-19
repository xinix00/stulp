package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/units"
)

// documentVersion is stamped into the file so a future change can migrate it
// rather than guess at what it is reading.
const documentVersion = 1

// maxNotifications bounds the only list that would otherwise grow forever. The
// UI shows the most recent fifty; keeping four times that is generous and keeps
// the document small.
const maxNotifications = 200

// document is everything Stulp persists.
//
// Two things are deliberately absent. An app's manifest is not here: it already
// exists as app.json inside the app's own bundle, and a second copy would drift
// the moment an app is updated. Device capability values are not here either:
// they are what the house is doing right now, not what it is configured to be,
// and writing every temperature reading to disk would rewrite this whole file
// several times a minute for nothing.
type document struct {
	Version       int                       `json:"version"`
	Apps          []appRecord               `json:"apps"`
	Settings      map[string]map[string]any `json:"appSettings,omitempty"`
	Devices       []deviceRecord            `json:"devices"`
	DeviceGroups  []DeviceGroup             `json:"deviceGroups"`
	Flows         []Flow                    `json:"flows"`
	Notifications []Notification            `json:"notifications,omitempty"`
	// AppState is per app een ondoorzichtige blob. Zie appstate.go.
	AppState map[string]json.RawMessage `json:"appState,omitempty"`
	// System is wat Stulp zelf aan of uit heeft staan. Geen losse map maar een
	// struct: hier hoort alleen in wat de kern echt kent, en een map wordt op den
	// duur een la met vergeten sleutels.
	System System `json:"system,omitempty"`
}

// System zijn de schakelaars van Stulp zelf.
type System struct {
	// Statistics laat de verzamelaar meelezen. Standaard uit: hij kost geheugen
	// zolang Stulp draait, en een installatie die er niet naar kijkt hoort daar
	// niet voor te betalen.
	Statistics bool `json:"statistics"`
	// Units is in welke eenheden dit huis leest. Alleen de weergave: de waarden
	// in dit document blijven canoniek, want anders zou een andere keuze de hele
	// geschiedenis en elke Flow-drempel moeten herschrijven. Leeg betekent
	// canoniek, dus een document van voor deze sleutel leest precies zoals het
	// altijd deed.
	Units units.Set `json:"units,omitempty"`
	// AttachSecret is de sleutel waaruit het token van elke app volgt. Leeg tot
	// iemand er voor het eerst om vraagt: een Stulp zonder apps op afstand hoort
	// geen geheim te hebben dat kan uitlekken.
	//
	// Eén sleutel en niet één per app, want dan valt elk token te herleiden
	// (HMAC over de app-id) en hoeft er niets per app bewaard te worden. Roteren is
	// dit veld leegmaken: alle tokens verlopen tegelijk, en dat is precies wat je
	// wilt als je niet weet welke er weg is.
	//
	// Hij staat in het document, dus hij gaat mee in een backup. Wie zijn backup
	// deelt, deelt zijn attach-tokens.
	AttachSecret string `json:"attachSecret,omitempty"`
}

// appRecord is an installed app minus its manifest.
type appRecord struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Root    string `json:"root"`
	Enabled bool   `json:"enabled"`

	// Offered betekent: deze app heeft zich aangemeld, met zijn manifest, en
	// niemand heeft hem nog geaccepteerd. Hij draait niet.
	//
	// Het is een eigen veld en niet "Enabled = false", want dat zijn twee
	// verschillende dingen: uitgezet is een app die je kende en stopzette,
	// aangeboden is een app die je nog nooit gezien hebt. Op de Apps-pagina
	// horen die niet naast elkaar te staan als hetzelfde.
	Offered bool `json:"offered,omitempty"`

	// Wat hier bewust NIET staat: het manifest. Dat is applicatie-kennis — de
	// app draagt het zelf en vertelt het bij élke aanmelding opnieuw (en het
	// verandert bij elke app-update). Het leeft in de manifest-cache van de
	// store; na een herstart is een aangemelde app even naamloos tot zijn
	// eerstvolgende announce, seconden later. Het document onthoudt alleen wat
	// van Stulp zelf is: dát de app er is, en hoe de gebruiker hem zette.

	Source          string `json:"source,omitempty"`
	UpdateVersion   string `json:"updateVersion,omitempty"`
	UpdateCheckedAt string `json:"updateCheckedAt,omitempty"`
	InstalledAt     string `json:"installedAt,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
}

// deviceRecord is a paired device minus its capability values. Data, settings
// and store are configuration and survive a restart; state does not.
type deviceRecord struct {
	ID           string         `json:"id"`
	AppID        string         `json:"appId"`
	DriverID     string         `json:"driverId"`
	GroupID      string         `json:"groupId,omitempty"`
	SortOrder    int            `json:"sortOrder,omitempty"`
	Name         string         `json:"name"`
	Class        string         `json:"class"`
	Data         map[string]any `json:"data,omitempty"`
	Settings     map[string]any `json:"settings,omitempty"`
	Store        map[string]any `json:"store,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Available    bool           `json:"available"`
	Message      string         `json:"unavailableMessage,omitempty"`
	CreatedAt    string         `json:"createdAt,omitempty"`
	UpdatedAt    string         `json:"updatedAt,omitempty"`
}

func (record deviceRecord) device(state map[string]any) Device {
	if state == nil {
		state = make(map[string]any)
	}
	return Device{
		ID: record.ID, AppID: record.AppID, DriverID: record.DriverID, GroupID: record.GroupID, SortOrder: record.SortOrder,
		Name: record.Name, Class: record.Class,
		Data: cloneMap(record.Data), Settings: cloneMap(record.Settings), Store: cloneMap(record.Store),
		Capabilities: append([]string(nil), record.Capabilities...),
		State:        state, Available: record.Available, Message: record.Message,
	}
}

func newDeviceRecord(device Device) deviceRecord {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return deviceRecord{
		ID: device.ID, AppID: device.AppID, DriverID: device.DriverID, GroupID: device.GroupID, SortOrder: device.SortOrder,
		Name: device.Name, Class: device.Class,
		Data: cloneMap(device.Data), Settings: cloneMap(device.Settings), Store: cloneMap(device.Store),
		Capabilities: append([]string(nil), device.Capabilities...),
		Available:    device.Available, Message: device.Message,
		CreatedAt: now, UpdatedAt: now,
	}
}

// MarshalJSON schrijft het record zonder de ~-sleutels uit Store: een sleutel
// die met '~' begint is een cache van de app — kennis die de app uit de wereld
// zelf kan terughalen (zoals matter's endpoint-inventaris) en die met elke
// app-update kan veranderen. In het geheugen doet zo'n sleutel volledig mee
// (apps lezen hem gewoon terug zolang Stulp draait); het document blijft er
// vrij van, en na een herstart leidt de app hem opnieuw af.
func (record deviceRecord) MarshalJSON() ([]byte, error) {
	for key := range record.Store {
		if strings.HasPrefix(key, "~") {
			trimmed := make(map[string]any, len(record.Store))
			for name, value := range record.Store {
				if !strings.HasPrefix(name, "~") {
					trimmed[name] = value
				}
			}
			record.Store = trimmed
			break
		}
	}
	type persisted deviceRecord // alias zonder methoden: geen recursie
	return json.Marshal(persisted(record))
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// load reads the document, tolerating a file that is not there yet.
func loadDocument(path string) (*document, error) {
	raw, err := files.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &document{Version: documentVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return &document{Version: documentVersion}, nil
	}
	return decodeDocument(path, raw)
}

func decodeDocument(name string, raw []byte) (*document, error) {
	var loaded document
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if loaded.Version > documentVersion {
		return nil, fmt.Errorf("%s was written by a newer Stulp (format %d, this build understands %d)",
			name, loaded.Version, documentVersion)
	}
	loaded.Version = documentVersion
	if loaded.Settings == nil {
		loaded.Settings = make(map[string]map[string]any)
	}
	return &loaded, nil
}

// save encodes the document and hands the bytes to the platform's file store.
// How those bytes land without a torn file is that store's problem; see
// document_os.go for what it costs on a filesystem.
func saveDocument(path string, value *document) error {
	value.Version = documentVersion
	sort.Slice(value.Notifications, func(left, right int) bool {
		return value.Notifications[left].CreatedAt > value.Notifications[right].CreatedAt
	})
	if len(value.Notifications) > maxNotifications {
		value.Notifications = value.Notifications[:maxNotifications]
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	encoded = append(encoded, '\n')

	return files.WriteFile(path, encoded)
}
