package webapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

// The whole loop: an app installed from a link, a paired device, a Flow built
// on it, and one uninstall that has to leave the home in a sensible state.
func TestUninstallRemovesTheAppItsDevicesAndItsBundle(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	// De bundel staat ONDER de apps-root, want dat is wat een uninstall mag
	// opruimen: een app-root daarbuiten is de werkboom van de gebruiker.
	appsRoot, err := database.AppsRoot()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(appsRoot, "com.acme.installed")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "app.json"), []byte(
		`{"id":"com.acme.installed","version":"1.0.0","sdk":3,"name":{"en":"Installed","nl":"Geinstalleerd"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, root, err := manifest.Load(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallApp(ctx, appManifest, root, ""); err != nil {
		t.Fatal(err)
	}
	installed, err := database.App(ctx, "com.acme.installed")
	if err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: "com.acme.installed", DriverID: "bulb", Name: "Keuken", Class: "light",
		Capabilities: []string{"onoff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := database.CreateFlow(ctx, store.Flow{Name: "Keukenlicht", Enabled: true,
		Nodes: []store.FlowNode{
			{ID: "als", Step: store.FlowStep{AppID: "stulp", CardID: "capability.onoff.on", CardType: "trigger",
				Args: map[string]any{"device": map[string]any{"$device": device.ID}}}},
			{ID: "dan", X: 300, Step: store.FlowStep{AppID: "stulp", CardID: "notification", CardType: "action",
				Args: map[string]any{"text": "aan"}}},
		},
		Edges: []store.FlowEdge{{ID: "e", From: "als", To: "dan"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	response := request(t, server.Handler(), http.MethodDelete, "/api/manager/apps/app/com.acme.installed", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("uninstall returned %d: %s", response.Code, response.Body.String())
	}
	var result map[string]any
	decodeResponse(t, response, &result)
	if result["warning"] != nil {
		t.Fatalf("uninstall reported a problem: %v", result["warning"])
	}
	if result["devices"] != float64(1) || result["flows"] != float64(1) {
		t.Fatalf("uninstall reported %#v, want one device and one flow", result)
	}

	if _, err := database.App(ctx, "com.acme.installed"); err == nil {
		t.Error("the app is still installed")
	}
	if _, err := database.Device(ctx, device.ID); err == nil {
		t.Error("the app's device outlived it")
	}
	if _, err := os.Stat(installed.Root); !os.IsNotExist(err) {
		t.Errorf("the downloaded bundle survived at %s: %v", installed.Root, err)
	}
	// The Flow itself is kept: reinstalling the app should let the user switch
	// it back on rather than rebuild it.
	stored, err := database.Flow(ctx, flow.ID)
	if err != nil {
		t.Fatalf("the flow was deleted: %v", err)
	}
	if stored.Enabled || stored.LastError == "" {
		t.Errorf("flow is enabled=%v with reason %q; want off and explained", stored.Enabled, stored.LastError)
	}
	if state := apps.State("com.acme.installed"); state.State == "running" {
		t.Errorf("the app is still running after being uninstalled: %#v", state)
	}
}

func TestManageOffersUninstallWithItsConsequences(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	asset := request(t, server.Handler(), http.MethodGet, "/assets/app.js", nil, "")
	if asset.Code != http.StatusOK {
		t.Fatalf("Manage asset returned %d", asset.Code)
	}
	for _, needle := range []string{"uninstallApp", "appImpact", "wordt uitgeschakeld", "verdwijnt"} {
		if !strings.Contains(asset.Body.String(), needle) {
			t.Errorf("Manage asset does not mention %q", needle)
		}
	}
}

func TestUninstallReportsAnUnknownApp(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	response := request(t, server.Handler(), http.MethodDelete, "/api/manager/apps/app/com.acme.absent", nil, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("uninstalling an app that was never installed returned %d", response.Code)
	}
}
