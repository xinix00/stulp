package appsdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
)

// uiAssetAnswer houdt "bestaat niet" uit de foutstroom. Een ontbrekende asset
// is een gewone 404 in de browser; een kapotte embedded FS of verbinding is wel
// een fout die zichtbaar moet blijven.
type uiAssetAnswer struct {
	Found bool   `json:"found"`
	Data  []byte `json:"data,omitempty"`
}

func (p *process) uiAsset(name string) (uiAssetAnswer, error) {
	if name == "" || name == "." || !fs.ValidPath(name) {
		return uiAssetAnswer{}, fmt.Errorf("invalid app UI asset path %q", name)
	}
	if p.plugin.UI == nil {
		return uiAssetAnswer{}, nil
	}
	data, err := fs.ReadFile(p.plugin.UI, name)
	if errors.Is(err, fs.ErrNotExist) {
		return uiAssetAnswer{}, nil
	}
	if err != nil {
		return uiAssetAnswer{}, fmt.Errorf("read app UI asset %q: %w", name, err)
	}
	return uiAssetAnswer{Found: true, Data: data}, nil
}

// manifestWithUI beschrijft welke ingebedde assets de app kan uitleveren. De
// bytes zelf horen nadrukkelijk niet in de begroeting: Matter heeft onder meer
// een flinke graph-library, terwijl de attach-greeting klein en begrensd hoort
// te blijven. Stulp bewaart alleen deze paden en vraagt een bestand pas als een
// browser het werkelijk opent.
func manifestWithUI(data []byte, ui fs.FS) ([]byte, error) {
	if len(data) == 0 || ui == nil {
		return data, nil
	}
	assets := make([]string, 0, 16)
	if err := fs.WalkDir(ui, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && fs.ValidPath(name) {
			assets = append(assets, name)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list embedded app UI: %w", err)
	}
	sort.Strings(assets)

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("add embedded UI to app manifest: %w", err)
	}
	uiDescription, _ := raw["ui"].(map[string]any)
	if uiDescription == nil {
		uiDescription = make(map[string]any)
	}
	uiDescription["assets"] = assets
	raw["ui"] = uiDescription
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode app manifest with embedded UI: %w", err)
	}
	return encoded, nil
}
