package appsdk

import (
	"encoding/json"
	"sort"
	"sync"
)

// State is wat de app mag lezen zonder het te vragen.
//
// Elke synchrone lezing komt hiervandaan. Zonder deze kopie zou elke getName()
// een round-trip zijn, en app-code doet die voortdurend. Stulp vult hem bij de
// handshake en houdt hem bij met events, dus hij loopt nooit achter op een
// schrijfactie die de app zelf gedaan heeft: het antwoord op die schrijfactie
// komt pas nadat de nieuwe stand is doorgeduwd.
type State struct {
	mu sync.RWMutex

	appID        string
	stulpID      string
	stulpVersion string
	language     string
	timezone     string
	manifest     map[string]any
	env          map[string]any
	locale       map[string]any

	settings map[string]any
	devices  map[string]map[string]any
	appState json.RawMessage
}

func NewState() *State {
	return &State{
		manifest: map[string]any{},
		env:      map[string]any{},
		locale:   map[string]any{},
		settings: map[string]any{},
		devices:  map[string]map[string]any{},
	}
}

// Hello is wat de app als eerste stuurt.
type Hello struct {
	Protocol int    `json:"protocol"`
	AppID    string `json:"appId"`
}

// Welcome is het antwoord van Stulp: alles wat vastligt plus de eerste stand.
type Welcome struct {
	Protocol     int                       `json:"protocol"`
	AppID        string                    `json:"appId"`
	StulpID      string                    `json:"stulpId"`
	StulpVersion string                    `json:"stulpVersion"`
	Language     string                    `json:"language"`
	Timezone     string                    `json:"timezone"`
	Manifest     map[string]any            `json:"manifest"`
	Env          map[string]any            `json:"env"`
	Locale       map[string]any            `json:"locale"`
	Settings     map[string]any            `json:"settings"`
	Devices      map[string]map[string]any `json:"devices"`
	AppState     json.RawMessage           `json:"appState,omitempty"`
}

// ProtocolVersion beschermt tegen een binary en een Stulp die uit elkaar zijn
// gelopen. Bij verschil faalt de start met een duidelijke melding in plaats van
// zich later vreemd te gedragen.
const ProtocolVersion = 1

func (s *State) Load(w Welcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appID, s.stulpID, s.stulpVersion = w.AppID, w.StulpID, w.StulpVersion
	s.language, s.timezone = w.Language, w.Timezone
	s.manifest, s.env, s.locale = orEmpty(w.Manifest), orEmpty(w.Env), orEmpty(w.Locale)
	s.settings = orEmpty(w.Settings)
	s.appState = w.AppState
	s.devices = map[string]map[string]any{}
	for id, device := range w.Devices {
		s.devices[id] = device
	}
}

// Apply verwerkt een statebericht van Stulp.
func (s *State) Apply(method string, params json.RawMessage) {
	switch method {
	case "state.settings":
		var settings map[string]any
		if json.Unmarshal(params, &settings) == nil {
			s.mu.Lock()
			s.settings = orEmpty(settings)
			s.mu.Unlock()
		}
	case "state.device":
		var p struct {
			DeviceID string         `json:"deviceId"`
			Device   map[string]any `json:"device"`
		}
		if json.Unmarshal(params, &p) == nil {
			s.mu.Lock()
			if p.Device == nil {
				delete(s.devices, p.DeviceID)
			} else {
				s.devices[p.DeviceID] = p.Device
			}
			s.mu.Unlock()
		}
	}
}

func (s *State) AppID() string        { s.mu.RLock(); defer s.mu.RUnlock(); return s.appID }
func (s *State) StulpID() string      { s.mu.RLock(); defer s.mu.RUnlock(); return s.stulpID }
func (s *State) StulpVersion() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.stulpVersion }
func (s *State) Language() string     { s.mu.RLock(); defer s.mu.RUnlock(); return s.language }
func (s *State) Timezone() string     { s.mu.RLock(); defer s.mu.RUnlock(); return s.timezone }

func (s *State) Manifest() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manifest
}

func (s *State) Env() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.env
}

func (s *State) LocaleStrings() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.locale
}

// DriverManifest levert de entry van één driver uit app.json.
func (s *State) DriverManifest(driverID string) (map[string]any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	drivers, _ := s.manifest["drivers"].([]any)
	for _, entry := range drivers {
		driver, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := driver["id"].(string); id == driverID {
			return driver, true
		}
	}
	return nil, false
}

// CapabilityOptions staan per driver in het manifest.
func (s *State) CapabilityOptions(driverID, capabilityID string) (map[string]any, bool) {
	driver, ok := s.DriverManifest(driverID)
	if !ok {
		return nil, false
	}
	options, _ := driver["capabilitiesOptions"].(map[string]any)
	if options == nil {
		return nil, false
	}
	value, ok := options[capabilityID].(map[string]any)
	return value, ok
}

// DeviceField leest één veld van een device: state, data, name, class,
// available, settings, store, capabilities.
func (s *State) DeviceField(deviceID, field string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	device, ok := s.devices[deviceID]
	if !ok {
		return nil, false
	}
	value, ok := device[field]
	return value, ok
}

// DeviceMap leest een veld dat een map is, als kopie. Een kopie omdat de
// aanroeper hem anders onder de lock vandaan zou lezen terwijl een event hem
// vervangt.
func (s *State) DeviceMap(deviceID, field string) (map[string]any, bool) {
	value, ok := s.DeviceField(deviceID, field)
	if !ok {
		return nil, false
	}
	source, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(source))
	for key, item := range source {
		out[key] = item
	}
	return out, true
}

func (s *State) AppState() json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.appState == nil {
		return nil
	}
	out := make(json.RawMessage, len(s.appState))
	copy(out, s.appState)
	return out
}

func (s *State) setAppState(state json.RawMessage) {
	s.mu.Lock()
	s.appState = state
	s.mu.Unlock()
}

// DeviceIDs levert de apparaten die deze app heeft, gesorteerd.
func (s *State) DeviceIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.devices))
	for id := range s.devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *State) Setting(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.settings[key]
	return value, ok
}

func (s *State) SettingKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.settings))
	for key := range s.settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
