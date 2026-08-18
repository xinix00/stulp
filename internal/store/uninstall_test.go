package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
)

func uninstallFixture(t *testing.T) (*Store, Device) {
	t.Helper()
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	raw := map[string]any{"id": "com.acme.lights", "version": "1.0.0", "sdk": 3,
		"name": map[string]any{"nl": "Lampen"}}
	appManifest := &manifest.Manifest{ID: "com.acme.lights", Version: "1.0.0", SDK: 3, Raw: raw}
	if err := database.InstallApp(ctx, appManifest, bundle(t, raw), ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, Device{
		AppID: "com.acme.lights", DriverID: "bulb", Name: "Keuken", Class: "light",
		Capabilities: []string{"onoff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetSetting(ctx, "com.acme.lights", "token", "secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateNotification(ctx, "com.acme.lights", "Lamp offline"); err != nil {
		t.Fatal(err)
	}
	return database, device
}

// Uninstalling has to leave nothing of the app behind, including the rows that
// only reach it through a foreign key.
func TestUninstallAppRemovesEverythingItOwns(t *testing.T) {
	ctx := context.Background()
	database, device := uninstallFixture(t)

	app, devices, err := database.UninstallApp(ctx, "com.acme.lights")
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != "com.acme.lights" || app.Root == "" {
		t.Fatalf("uninstall did not report the app it removed: %#v", app)
	}
	if len(devices) != 1 || devices[0].ID != device.ID {
		t.Fatalf("uninstall reported devices %#v, want the one paired device", devices)
	}
	if _, err := database.App(ctx, "com.acme.lights"); err == nil {
		t.Fatal("the app is still installed")
	}
	if _, err := database.Device(ctx, device.ID); err == nil {
		t.Fatal("the app's device outlived the app")
	}
	settings, err := database.Settings(ctx, "com.acme.lights")
	if err != nil || len(settings) != 0 {
		t.Fatalf("settings survived the uninstall: %#v err=%v", settings, err)
	}
	notifications, err := database.Notifications(ctx, 10)
	if err != nil || len(notifications) != 0 {
		t.Fatalf("notifications survived the uninstall: %#v err=%v", notifications, err)
	}
	// The app's settings must be gone with it, not merely orphaned.
	if value, exists, _ := database.Setting(ctx, "com.acme.lights", "token"); exists {
		t.Fatalf("an uninstalled app's setting survived: %v", value)
	}
}

// Manage keeps an open page in sync from these events, so a bulk delete still
// has to announce every row it removed.
func TestUninstallAppPublishesEachRemoval(t *testing.T) {
	ctx := context.Background()
	database, device := uninstallFixture(t)
	events, cancel := database.Subscribe(8)
	defer cancel()

	if _, _, err := database.UninstallApp(ctx, "com.acme.lights"); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]string)
	for len(seen) < 2 {
		select {
		case event := <-events:
			seen[event.Type] = event.ID
		default:
			t.Fatalf("only saw %v", seen)
		}
	}
	if seen["device.delete"] != device.ID {
		t.Fatalf("device.delete carried %q, want %q", seen["device.delete"], device.ID)
	}
	if seen["app.delete"] != "com.acme.lights" {
		t.Fatalf("app.delete carried %q", seen["app.delete"])
	}
}

// The Matter controller occupies an app row so its devices behave like any
// other app's, but it is part of the runtime and has nothing to uninstall.
func TestUninstallReportsAnUnknownApp(t *testing.T) {
	ctx := context.Background()
	database, _ := uninstallFixture(t)
	if _, _, err := database.UninstallApp(ctx, "com.acme.absent"); err == nil {
		t.Fatal("uninstalling an app that was never installed succeeded")
	}
}
