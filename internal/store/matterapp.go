package store

import (
	"context"

	"github.com/xinix00/stulp/internal/manifest"
)

// InstallMatterApp registreert de Matter-plugin als gewone app.
//
// Matter werd vroeger door de store zelf aangemaakt, als "native" app zonder
// binary. Die uitzondering bestaat niet meer: hij is een plugin als elke andere
// en hoort dus geïnstalleerd te worden. Dit is de kortste weg daarheen, voor
// tests en voor een installatie die van vóór de overstap komt.
func (s *Store) InstallMatterApp(ctx context.Context, root string) error {
	return s.InstallApp(ctx, &manifest.Manifest{
		ID: NativeMatterAppID, Version: "1.0.0", SDK: 3,
		Raw: map[string]any{
			"id": NativeMatterAppID, "version": "1.0.0", "sdk": 3,
			"name": map[string]any{"en": "Matter", "nl": "Matter"},
			"drivers": []any{map[string]any{
				"id": "matter", "class": "other",
				"name": map[string]any{"en": "Matter device", "nl": "Matter-apparaat"},
			}},
		},
	}, root, "")
}
