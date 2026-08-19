package supervisor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/plugin/plugintest"
	"github.com/xinix00/stulp/internal/store"
)

// houseWithACamera zet een echt plugin-proces neer dat een stilstaand beeld
// aanmeldt en het op zijn eigen luisteraar bedient -- precies wat een cameraplugin
// doet. examples/virtual is die plugin.
func houseWithACamera(t *testing.T) (*Supervisor, *store.Store, store.Device) {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	appManifest, root, err := manifest.Load(plugintest.Example(t, filepath.Join("..", "..", "examples", "virtual")))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallApp(ctx, appManifest, root, ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: appManifest.ID, DriverID: "switch", Name: "Voordeur", Class: "camera",
		Data: map[string]any{"id": "virtual-camera"}, Capabilities: []string{"onoff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(supervisor.Close)
	supervisor.UseImages(imageshare.New())
	if err := supervisor.Start(ctx, appManifest.ID); err != nil {
		t.Fatal(err)
	}
	return supervisor, database, device
}

// Het register noemt wat er in huis te halen valt, over alle apps heen.
//
// Dit is de weg die een plugin gebruikt om een foto bij een melding te doen. Het
// is met opzet geen plugin die een andere plugin aanroept -- die naad bestaat niet
// -- maar een vraag aan Stulp, die media toch al doorgeeft.
func TestImageSourcesNamesWhatTheHouseOffers(t *testing.T) {
	supervisor, _, device := houseWithACamera(t)
	sources, err := supervisor.ImageSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("het register noemt %d bronnen: %+v", len(sources), sources)
	}
	if sources[0].DeviceID != device.ID || sources[0].DeviceName != "Voordeur" {
		t.Fatalf("de bron is %+v", sources[0])
	}
	if sources[0].Slot == "" || sources[0].Title == "" {
		t.Fatalf("de bron zegt niet welk beeld het is: %+v", sources[0])
	}
}

// ImageURL bewaart alleen de vraag. De foto zelf hoort niet in de heap van de
// supervisor; de weblaag resolveert en streamt hem pas wanneer iemand het
// onraadbare adres opent.
func TestImageURLStoresOnlyALazySource(t *testing.T) {
	supervisor, _, device := houseWithACamera(t)
	address, err := supervisor.ImageURL(context.Background(), device.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(address, "/image/") {
		t.Fatalf("het adres is %q", address)
	}
	image, ok := supervisor.images.Get(strings.TrimPrefix(address, "/image/"))
	if !ok {
		t.Fatal("op dat adres staat niets")
	}
	source, err := image.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// De virtual-plugin meldt beeld én video op dezelfde slot aan. Komt hier een
	// videotype uit, dan is het gevraagde soort onderweg kwijtgeraakt.
	if source.ContentType != "image/png" || source.URL == "" {
		t.Fatalf("de opgeloste beeldbron is %+v", source)
	}
}

// Een apparaat dat niet bestaat hoort dat te zeggen, niet iets anders te leveren.
func TestImageURLRefusesAnUnknownDevice(t *testing.T) {
	supervisor, _, _ := houseWithACamera(t)
	if _, err := supervisor.ImageURL(context.Background(), "een-apparaat-dat-niet-bestaat", ""); err == nil {
		t.Fatal("een onbekend apparaat leverde een afbeelding")
	}
}

// Zonder gedeelde opslag hoort de vraag te falen in plaats van een adres te geven
// waar niets staat.
func TestImageURLNeedsSomewhereToPutThePicture(t *testing.T) {
	supervisor, _, device := houseWithACamera(t)
	supervisor.images = nil
	if _, err := supervisor.ImageURL(context.Background(), device.ID, ""); err == nil {
		t.Fatal("er kwam een adres terug zonder plek om iets te bewaren")
	}
}
