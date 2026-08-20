package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/store"
)

func TestBackupRestoresDatabaseAndRelocatesAppBundles(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.backup","version":"1.2.3","sdk":3,"name":{"en":"Backup"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte(`module.exports = class {};`), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := store.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice(ctx, store.Device{
		AppID: appManifest.ID, DriverID: "switch", Name: "Backuplamp", Class: "light",
		Data: map[string]any{"id": "backup-light"}, Capabilities: []string{"onoff"}, State: map[string]any{"onoff": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.CreateNotification(ctx, appManifest.ID, "Blijft bewaard"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "stulp.zip")
	if err := WriteFile(ctx, source, archive); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "restored.db")
	result, err := Restore(ctx, archive, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.Document != destination || result.AppsRoot != destination+".apps" {
		t.Fatalf("unexpected restore result: %#v", result)
	}
	restored, err := store.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredDevice, err := restored.Device(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Configuration is restored; capability values are not stored at all, so a
	// restored house is configured but silent until its apps report again.
	if restoredDevice.Name != device.Name || restoredDevice.DriverID != device.DriverID ||
		!reflect.DeepEqual(restoredDevice.Capabilities, device.Capabilities) {
		t.Fatalf("device configuration was not restored: %#v", restoredDevice)
	}
	if len(restoredDevice.State) != 0 {
		t.Fatalf("capability values were persisted after all: %#v", restoredDevice.State)
	}
	restoredApp, err := restored.App(ctx, appManifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(restoredApp.Root, destination+".apps"+string(filepath.Separator)) {
		t.Fatalf("app root was not relocated: %q", restoredApp.Root)
	}
	if _, _, err := manifest.Load(restoredApp.Root); err != nil {
		t.Fatalf("restored app bundle is invalid: %v", err)
	}
	notifications, err := restored.Notifications(ctx, 10)
	if err != nil || len(notifications) != 1 || notifications[0].Excerpt != "Blijft bewaard" {
		t.Fatalf("notification history was not restored: %#v err=%v", notifications, err)
	}
}

func TestRestoreRejectsArchiveTraversal(t *testing.T) {
	var value bytes.Buffer
	archive := zip.NewWriter(&value)
	writer, err := archive.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("nope")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "unsafe.zip")
	if err := os.WriteFile(filename, value.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(context.Background(), filename, filepath.Join(t.TempDir(), "stulp.json"))
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("unsafe archive was not rejected: %v", err)
	}
}

func TestLiveRestoreIncludesAnAnnouncedAppWithoutABundle(t *testing.T) {
	ctx := context.Background()
	announced, err := manifest.Parse([]byte(`{
  "id":"com.stulp.announced","version":"4.5.6","sdk":3,
  "name":{"nl":"Aangemeld"},"drivers":[{"id":"lamp","name":{"nl":"Lamp"}}],
  "ui":{"settings":"/settings/index.html"}
}`))
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Open(filepath.Join(t.TempDir(), "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.OfferApp(ctx, announced); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AcceptApp(ctx, announced.ID); err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice(ctx, store.Device{
		AppID: announced.ID, DriverID: "lamp", Name: "Bureaulamp", Class: "light",
		Data: map[string]any{"serial": "remote-1"}, Capabilities: []string{"onoff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetSetting(ctx, announced.ID, "bridge", "10.0.0.8"); err != nil {
		t.Fatal(err)
	}
	if err := source.SetAppState(ctx, announced.ID, json.RawMessage(`{"generation":"new","newOnly":true}`)); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := Write(ctx, source, &archive); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := readArchiveFile(reader.File, manifestPath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	backupManifest, err := decodeManifest(manifestData)
	if err != nil {
		t.Fatal(err)
	}
	if len(backupManifest.Apps) != 1 || backupManifest.Apps[0].Path != "" {
		t.Fatalf("announced app was treated as a disk bundle: %#v", backupManifest.Apps)
	}

	destination := filepath.Join(t.TempDir(), "destination.json")
	target, err := store.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.SetAppState(ctx, announced.ID, json.RawMessage(`{"generation":"old","oldOnly":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := target.SetAppState(ctx, "com.stulp.gone", json.RawMessage(`{"leftover":true}`)); err != nil {
		t.Fatal(err)
	}
	result, err := RestoreBytes(ctx, archive.Bytes(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousDocument == "" {
		t.Fatal("live restore did not retain the previous document")
	}
	if _, err := os.Stat(result.PreviousDocument); err != nil {
		t.Fatalf("previous document is not recoverable: %v", err)
	}
	restoredApp, err := target.App(ctx, announced.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Het manifest is applicatie-kennis en reist niet mee in een backup — en de
	// VERSIE dus ook niet: na de restore is de app een placeholder zónder
	// versie tot zijn eerstvolgende announce. Een versie die hier al gevuld was
	// zou een herinnering zijn, geen waarheid.
	if restoredApp.Root != "" || restoredApp.Version != "" || !restoredApp.Enabled {
		t.Fatalf("announced app identity was not restored: %#v", restoredApp)
	}
	if id, _ := restoredApp.Manifest["id"].(string); id != announced.ID {
		t.Fatalf("placeholder manifest misses the id: %#v", restoredApp.Manifest)
	}
	if refreshed, err := target.UpdateAnnouncedApp(ctx, announced); err != nil || !refreshed {
		t.Fatalf("announce after restore: changed=%v err=%v", refreshed, err)
	}
	if reannounced, err := target.App(ctx, announced.ID); err != nil || reannounced.Manifest["ui"] == nil ||
		reannounced.Version != announced.Version {
		t.Fatalf("announce did not refill the manifest and version: %#v err=%v", reannounced, err)
	}
	restoredDevice, err := target.Device(ctx, device.ID)
	if err != nil || restoredDevice.Name != device.Name {
		t.Fatalf("announced app device was not restored: %#v err=%v", restoredDevice, err)
	}
	setting, ok, err := target.Setting(ctx, announced.ID, "bridge")
	if err != nil || !ok || setting != "10.0.0.8" {
		t.Fatalf("app configuration was not restored: value=%v ok=%v err=%v", setting, ok, err)
	}
	appState, err := target.AppState(ctx, announced.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stateObject map[string]any
	if err := json.Unmarshal(appState, &stateObject); err != nil {
		t.Fatal(err)
	}
	wantState := map[string]any{"generation": "new", "newOnly": true}
	if !reflect.DeepEqual(stateObject, wantState) {
		t.Fatalf("old and restored appState were mixed: got %#v, want %#v", stateObject, wantState)
	}
	leftover, err := target.AppState(ctx, "com.stulp.gone")
	if err != nil || leftover != nil {
		t.Fatalf("appState from an app outside the backup survived: %s err=%v", leftover, err)
	}
}

func TestLiveRestorePublishesAndRetainsAppBundleTrees(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "source-app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.live-bundle","version":"7.8.9","sdk":3,"name":{"nl":"Bundel"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("module.exports = class {};\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Open(filepath.Join(t.TempDir(), "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := Write(ctx, source, &archive); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "target.json")
	target, err := store.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	appsRoot, err := target.AppsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appsRoot, "old-marker"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := RestoreBytes(ctx, archive.Bytes(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousAppsRoot == "" {
		t.Fatal("live restore did not retain the previous app tree")
	}
	if marker, err := os.ReadFile(filepath.Join(result.PreviousAppsRoot, "old-marker")); err != nil || string(marker) != "old" {
		t.Fatalf("previous app tree is not recoverable: marker=%q err=%v", marker, err)
	}
	restored, err := target.App(ctx, appManifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(restored.Root, appsRoot+string(filepath.Separator)) {
		t.Fatalf("restored bundle root = %q, want it below %q", restored.Root, appsRoot)
	}
	if loaded, _, err := manifest.Load(restored.Root); err != nil || loaded.ID != appManifest.ID {
		t.Fatalf("published app bundle is invalid: manifest=%#v err=%v", loaded, err)
	}
}

// Een backup is een ander tijdperk: velden die wij niet (meer) kennen zijn
// geschiedenis, geen fout. De backup van 19-08 droeg "version" per app; één
// release nadat dat veld verdween weigerde de strenge decoder de hele restore
// en hield hij de eigenaar zijn eigen huis uit.
func TestManifestFromAnotherEraStillDecodes(t *testing.T) {
	older := []byte(`{"format":1,"createdAt":"2026-08-19T21:00:00Z",
	  "apps":[{"id":"com.stulp.matter","version":"1.0.0"}],
	  "futureField":{"whatever":true}}`)
	decoded, err := decodeManifest(older)
	if err != nil {
		t.Fatalf("een ouder/nieuwer backup-manifest hoort te decoderen: %v", err)
	}
	if len(decoded.Apps) != 1 || decoded.Apps[0].ID != "com.stulp.matter" {
		t.Fatalf("manifest-inhoud verloren: %#v", decoded)
	}
}
