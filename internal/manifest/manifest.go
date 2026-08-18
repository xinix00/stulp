package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Manifest is the subset of app.json that Stulp needs to
// boot an app. Unknown fields are deliberately retained in Raw so the SDK shim
// can hand the complete manifest to the app.
type Manifest struct {
	ID            string           `json:"id"`
	Version       string           `json:"version"`
	Compatibility string           `json:"compatibility"`
	SDK           int              `json:"sdk"`
	Name          LocalizedString  `json:"name"`
	Drivers       []DriverManifest `json:"drivers"`
	Permissions   []string         `json:"permissions"`
	Raw           map[string]any   `json:"-"`
}

type DriverManifest struct {
	ID                  string          `json:"id"`
	Name                LocalizedString `json:"name"`
	Class               string          `json:"class"`
	Capabilities        []string        `json:"capabilities"`
	CapabilitiesOptions map[string]any  `json:"capabilitiesOptions"`
	Settings            []any           `json:"settings"`
	Pair                []any           `json:"pair"`
}

type LocalizedString map[string]string

func (s LocalizedString) Resolve(language string) string {
	if value := s[language]; value != "" {
		return value
	}
	if value := s["en"]; value != "" {
		return value
	}
	for _, value := range s {
		return value
	}
	return ""
}

// FromRaw leest een manifest terug uit de vorm waarin het bewaard is.
//
// Een app die zich aanmeldde staat als map in het document (store.App.Manifest);
// dit maakt daar weer een Manifest van, met dezelfde validatie als een app.json
// van schijf. Via JSON heen en terug, want dat is exact de vorm waarin het
// binnenkwam -- zelf de velden overzetten zou een tweede plek zijn die bij moet
// blijven.
func FromRaw(raw map[string]any) (*Manifest, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode stored manifest: %w", err)
	}
	return Parse(data)
}

// Parse leest een app.json uit bytes in plaats van van schijf.
//
// Dat is de vorm die een app nodig heeft die zichzelf aanmeldt: een image dat HOP
// in een slot plaatst heeft geen map om app.json uit te lezen, dus draagt het zijn
// manifest mee en stuurt het bij de begroeting. Dezelfde velden, dezelfde
// validatie, dezelfde Raw -- alleen de bron verschilt.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode app.json: %w", err)
	}
	if err := json.Unmarshal(data, &m.Raw); err != nil {
		return nil, fmt.Errorf("decode raw app.json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func Load(root string) (*Manifest, string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve app path: %w", err)
	}
	absRoot, err = filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, "", fmt.Errorf("resolve app path: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, "", fmt.Errorf("open app path: %w", err)
	}
	if !info.IsDir() {
		return nil, "", errors.New("app path must be a directory")
	}

	data, err := os.ReadFile(filepath.Join(absRoot, "app.json"))
	if err != nil {
		return nil, "", fmt.Errorf("read app.json: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", fmt.Errorf("decode app.json: %w", err)
	}
	if err := json.Unmarshal(data, &m.Raw); err != nil {
		return nil, "", fmt.Errorf("decode raw app.json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, "", err
	}
	return &m, absRoot, nil
}

func (m *Manifest) Validate() error {
	if m.ID == "" {
		return errors.New("app.json: id is required")
	}
	if m.Version == "" {
		return errors.New("app.json: version is required")
	}
	if m.SDK != 3 {
		return fmt.Errorf("app.json: only manifest version 3 is supported, got %d", m.SDK)
	}
	seen := make(map[string]struct{}, len(m.Drivers))
	for _, driver := range m.Drivers {
		if driver.ID == "" {
			return errors.New("app.json: every driver needs an id")
		}
		if _, ok := seen[driver.ID]; ok {
			return fmt.Errorf("app.json: duplicate driver id %q", driver.ID)
		}
		seen[driver.ID] = struct{}{}
	}
	return nil
}

func (m *Manifest) Driver(id string) (DriverManifest, bool) {
	for _, driver := range m.Drivers {
		if driver.ID == id {
			return driver, true
		}
	}
	return DriverManifest{}, false
}
