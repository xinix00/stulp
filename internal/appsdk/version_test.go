package appsdk

import (
	"encoding/json"
	"testing"
)

// De announce draagt de buildversie: het binary is wat er draait, en de release
// stempelt hem. Zonder buildversie (kale go build) blijft de app.json-versie
// staan en verzint niemand iets.
func TestAnnounceManifestCarriesTheBuildVersion(t *testing.T) {
	raw := []byte(`{"id":"com.stulp.test","version":"1.0.0","name":{"en":"Test"}}`)

	BuildVersion = ""
	if got := string(announceManifest(raw)); got != string(raw) {
		t.Fatalf("zonder buildversie hoort het manifest onaangeroerd te blijven: %s", got)
	}

	BuildVersion = "v9.9.9"
	defer func() { BuildVersion = "" }()
	var doc map[string]any
	if err := json.Unmarshal(announceManifest(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["version"] != "v9.9.9" {
		t.Fatalf("announce zegt versie %v, wilde de buildversie", doc["version"])
	}
	if doc["id"] != "com.stulp.test" || doc["name"] == nil {
		t.Fatal("de rest van het manifest hoort intact te blijven")
	}
}
