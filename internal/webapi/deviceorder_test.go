package webapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

func TestDeviceTileOrderAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	m := &manifest.Manifest{ID: "com.example.order", Version: "1.0.0", SDK: 3, Raw: map[string]any{
		"id": "com.example.order", "name": map[string]any{"en": "Order test"},
	}}
	if err := database.InstallApp(ctx, m, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	group, err := database.CreateDeviceGroup(ctx, store.DeviceGroup{Name: "Woonkamer"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := database.AddDevice(ctx, store.Device{
		AppID: m.ID, DriverID: "light", GroupID: group.ID, Name: "Eerste", Class: "light", Data: map[string]any{"id": "first"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.AddDevice(ctx, store.Device{
		AppID: m.ID, DriverID: "light", GroupID: group.ID, Name: "Tweede", Class: "light", Data: map[string]any{"id": "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	response := request(t, server.Handler(), http.MethodPut, "/api/stulp/devices/order", map[string]any{
		"groupId": group.ID, "deviceIds": []string{second.ID, first.ID},
	}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("reorder devices returned %d: %s", response.Code, response.Body.String())
	}
	response = request(t, server.Handler(), http.MethodGet, "/api/manager/devices/device", nil, "")
	var devices map[string]map[string]any
	decodeResponse(t, response, &devices)
	if devices[second.ID]["sortOrder"] != float64(10) || devices[first.ID]["sortOrder"] != float64(20) {
		t.Fatalf("device order was not exposed by the API: %#v", devices)
	}
}
