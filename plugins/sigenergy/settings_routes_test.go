package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCloudSettingsUseManifestRoutes(t *testing.T) {
	raw, err := os.ReadFile("app.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		API map[string]struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"api"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("settings/page.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := string(page)
	for _, handler := range []string{"cloud_connect", "cloud_check", "cloud_disconnect"} {
		route, exists := manifest.API[handler]
		if !exists {
			t.Fatalf("manifest has no %q route", handler)
		}
		path := strings.TrimPrefix(route.Path, "/")
		call := fmt.Sprintf("Stulp.api('%s', '%s'", route.Method, path)
		if !strings.Contains(javascript, call) {
			t.Errorf("settings page does not call manifest route %q for handler %q", path, handler)
		}
	}
}
