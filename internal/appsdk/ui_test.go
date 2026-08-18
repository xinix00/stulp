package appsdk

import (
	"encoding/json"
	"testing"
	"testing/fstest"
)

func TestEmbeddedUIIsAnnouncedAndServedOnDemand(t *testing.T) {
	files := fstest.MapFS{
		"settings/index.html":                   {Data: []byte("<h1>Config</h1>")},
		"drivers/switch/pair/validate.html":     {Data: []byte("validate")},
		"settings/assets/ignored-directory.css": {Data: []byte("body{}")},
	}
	manifest, err := manifestWithUI([]byte(
		`{"id":"com.example.ui","version":"1.0.0","sdk":3,"ui":{"sandbox":true}}`), files)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(manifest, &raw); err != nil {
		t.Fatal(err)
	}
	ui, _ := raw["ui"].(map[string]any)
	assets, _ := ui["assets"].([]any)
	if len(assets) != 3 || assets[0] != "drivers/switch/pair/validate.html" || assets[1] != "settings/assets/ignored-directory.css" || assets[2] != "settings/index.html" {
		t.Fatalf("announced assets = %#v", assets)
	}
	if ui["sandbox"] != true {
		t.Fatalf("existing UI metadata was discarded: %#v", ui)
	}

	process := &process{plugin: Plugin{UI: files}}
	asset, err := process.uiAsset("settings/index.html")
	if err != nil || !asset.Found || string(asset.Data) != "<h1>Config</h1>" {
		t.Fatalf("served asset = %#v, err = %v", asset, err)
	}
	missing, err := process.uiAsset("settings/missing.js")
	if err != nil || missing.Found {
		t.Fatalf("missing asset = %#v, err = %v", missing, err)
	}
	if _, err := process.uiAsset("../app.json"); err == nil {
		t.Fatal("an asset path escaped the embedded UI root")
	}
}
