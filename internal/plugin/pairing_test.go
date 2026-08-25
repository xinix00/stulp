package plugin

import (
	"context"
	"testing"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/store"
)

func TestPairedDeviceRollbackIgnoresCanceledRequestContext(t *testing.T) {
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	appManifest, err := manifest.Parse([]byte(`{
  "id":"com.stulp.rollback-test","version":"1.0.0","sdk":3,
  "name":{"en":"Rollback"},
  "drivers":[{"id":"thing","name":{"en":"Thing"},"class":"other","capabilities":[]}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallApp(context.Background(), appManifest, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	created, err := database.AddDevice(context.Background(), store.Device{
		AppID: appManifest.ID, DriverID: "thing", Name: "Half gekoppeld",
		Data: map[string]any{"id": "half-paired"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	process := &Process{store: database}
	if err := process.rollbackPairedDevice(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Device(context.Background(), created.ID); err == nil {
		t.Fatal("canceled pairing rollback left a durable ghost device")
	}
}
