package plugin

import (
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
)

// Een kandidaat die zijn soort meebrengt (matter: "socket" uit de Descriptor)
// moet die soort houden; zonder soort geldt de driver-default. Vóór deze test
// verloor élk matter-apparaat zijn soort aan de default "other" — de
// koppelstroom kon hem simpelweg niet dragen.
func TestPairedDeviceKeepsTheCandidateClass(t *testing.T) {
	appManifest := &manifest.Manifest{
		ID: "com.acme.things",
		Drivers: []manifest.DriverManifest{{ID: "thing", Class: "other"}},
	}
	withClass, err := pairedDevice(appManifest, "com.acme.things", "thing", map[string]any{
		"name": "Stekker", "data": map[string]any{"id": "a"}, "class": "socket",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withClass.Class != "socket" {
		t.Fatalf("kandidaat-soort ging verloren: %q", withClass.Class)
	}
	withoutClass, err := pairedDevice(appManifest, "com.acme.things", "thing", map[string]any{
		"name": "Ding", "data": map[string]any{"id": "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if withoutClass.Class != "other" {
		t.Fatalf("zonder kandidaat-soort hoort de driver-default te gelden, kreeg %q", withoutClass.Class)
	}
}
