package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/plugin/plugintest"
	"github.com/xinix00/stulp/internal/store"
)

func TestFailedAppRestartsWithBoundedBackoff(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.retry","version":"1.0.0","sdk":3,"name":{"en":"Retry"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Geen binary: een app die niet te starten is. Dat is wat de supervisor
	// hoort op te vangen, en het is meteen het geval dat in het echt voorkomt --
	// een half uitgepakte installatie.
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	events, cancelEvents := database.Subscribe(16)
	defer cancelEvents()
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	apps.retryBase = 20 * time.Millisecond
	apps.retryMax = 40 * time.Millisecond
	defer apps.Close()

	if err := apps.Start(ctx, appManifest.ID); err == nil {
		t.Fatal("invalid app unexpectedly started")
	}
	crashed := apps.State(appManifest.ID)
	if crashed.State != "crashed" || crashed.Error == "" || crashed.RetryAt == "" {
		t.Fatalf("failed app has no actionable retry state: %#v", crashed)
	}
	// De binary alsnog neerzetten: de volgende poging hoort te slagen.
	plugintest.Install(t, appRoot, appManifest.ID)

	// Ruim: hier start een echt proces, en onder een volle testrun -- alle
	// pakketten parallel, waaronder Matter-integraties die over echte sockets
	// praten -- kost dat soms seconden. Een test die faalt omdat de machine het
	// druk had zegt niets over de supervisor.
	deadline := time.Now().Add(30 * time.Second)
	for apps.State(appManifest.ID).State != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("app was not recovered: %#v", apps.State(appManifest.ID))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state := apps.State(appManifest.ID); state.RestartCount < 1 || state.RetryAt != "" {
		t.Fatalf("recovered app retained an invalid retry state: %#v", state)
	}
	foundRunningEvent := false
	for !foundRunningEvent {
		select {
		case event := <-events:
			state, _ := event.Data.(AppState)
			foundRunningEvent = event.Manager == "apps" && event.Type == "app.runtime" &&
				event.ID == appManifest.ID && state.State == "running"
		default:
			if !foundRunningEvent {
				t.Fatal("runtime recovery did not publish a live app state")
			}
		}
	}
}

func TestSlowAppStartDoesNotBlockSupervisorReaders(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.slow","version":"1.0.0","sdk":3,"name":{"en":"Slow"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STULP_TEST_PLUGIN", "slow")
	// De app blijft in OnInit staan tot dit bestand bestaat, zodat de toets
	// hieronder gegarandeerd tijdens het starten valt en niet erna.
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("STULP_TEST_RELEASE", release)
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	plugintest.Install(t, appRoot, appManifest.ID)
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	// Ruim, want app.init blijft openstaan tot deze test de app loslaat -- dat
	// is precies wat er getoetst wordt. Een krappe grens meet dan hoe druk de
	// machine het heeft en niet of de supervisor aanspreekbaar blijft.
	apps := New(database, plugin.Options{
		CallTimeout: 60 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer apps.Close()
	startResult := make(chan error, 1)
	go func() { startResult <- apps.Start(ctx, appManifest.ID) }()
	// Ruim, want er start hier een echt proces. De app blijft in OnInit staan
	// tot wij hem loslaten, dus zodra dit klaar is weten we zeker dat hij er
	// nog in zit -- en dat is precies de toestand die getoetst moet worden.
	deadline := time.Now().Add(30 * time.Second)
	for {
		entered, exists, settingErr := database.Setting(ctx, appManifest.ID, "entered")
		if settingErr != nil {
			t.Fatal(settingErr)
		}
		if exists && entered == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow app never entered onInit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stateResult := make(chan AppState, 1)
	go func() { stateResult <- apps.State(appManifest.ID) }()
	select {
	case state := <-stateResult:
		if state.State != "starting" {
			t.Fatalf("state during onInit = %#v", state)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("supervisor read blocked behind app onInit")
	}
	if err := os.WriteFile(release, []byte("los"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-startResult; err != nil {
		t.Fatal(err)
	}
}

// countingRuntime doet niets behalve onthouden of hij gesloten is.
type countingRuntime struct {
	plugin.Runtime
	startErr error
	closed   atomic.Int32
}

func (r *countingRuntime) Start(context.Context) error { return r.startErr }
func (r *countingRuntime) Close()                      { r.closed.Add(1) }

// Een mislukte start moet het proces opruimen.
//
// Het proces draait al voordat Start faalt -- de binary is gestart, alleen
// app.init kwam er niet doorheen. Bleef die staan, dan zette de supervisor er
// bij elke herstartpoging een nieuwe naast, en een app met een trage OnInit
// legde de machine om.
func TestFailedStartClosesTheProcess(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.failstart","version":"1.0.0","sdk":3,"name":{"en":"Fail"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}

	runtime := &countingRuntime{startErr: errors.New("onInit duurde te lang")}
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	apps.newRuntime = func(context.Context, *store.Store, string, plugin.Options) (plugin.Runtime, error) {
		return runtime, nil
	}
	defer apps.Close()

	if err := apps.Start(ctx, appManifest.ID); err == nil {
		t.Fatal("een mislukte start meldde succes")
	}
	if closed := runtime.closed.Load(); closed != 1 {
		t.Fatalf("runtime %d keer gesloten na een mislukte start, wil 1", closed)
	}
	if state := apps.State(appManifest.ID); state.State != "crashed" || state.Error == "" {
		t.Fatalf("toestand na mislukte start = %#v", state)
	}
}

// Een plugin die uit zichzelf stopt moet als crashed te zien zijn.
//
// Dit is het geval dat een half huis stil kan laten vallen: het proces is weg,
// maar Manage bleef "running" tonen en de tegels bleven op hun laatste waarde
// staan. Alles zag er goed uit en niets werkte meer.
func TestSupervisorNoticesAPluginThatStops(t *testing.T) {
	ctx := context.Background()
	t.Setenv("STULP_TEST_PLUGIN", "exit")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.exit","version":"1.0.0","sdk":3,"name":{"en":"Exit"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	plugintest.Install(t, appRoot, appManifest.ID)
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	apps.retryBase = time.Hour // geen herstart tijdens deze toets
	apps.retryMax = time.Hour
	defer apps.Close()

	if err := apps.Start(ctx, appManifest.ID); err != nil {
		t.Fatalf("de app startte niet: %v", err)
	}
	if state := apps.State(appManifest.ID); state.State != "running" {
		t.Fatalf("na het starten = %#v", state)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		state := apps.State(appManifest.ID)
		if state.State == "crashed" {
			if state.Error == "" {
				t.Fatal("crashed gemeld zonder reden")
			}
			if state.RetryAt == "" {
				t.Fatal("crashed gemeld zonder herstart in te plannen")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("het weggevallen proces werd niet opgemerkt: %#v", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Een net gekoppeld apparaat moet bij de app bekend zijn vóórdat hij het start.
//
// NewDevice leest Data() -- het serienummer, het node-id, het adres -- en zonder
// dat weigert vrijwel elke driver terecht. AddDevice stuurt daar wel een
// gebeurtenis over, maar die loopt langs een eigen goroutine en haalt het niet
// altijd; dan mislukt het toevoegen met "dit apparaat heeft geen id" en is het
// de volgende keer zomaar weer goed. Precies zo'n fout die je niet vindt door
// nog eens te klikken.
func TestAFreshlyPairedDeviceKnowsItsDataBeforeItStarts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.pairing","version":"1.0.0","sdk":3,"name":{"en":"Pairing"},
  "drivers":[{"id":"thing","name":{"en":"Thing"},"class":"other","capabilities":[]}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	plugintest.Install(t, appRoot, appManifest.ID)

	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	if err := apps.Start(ctx, appManifest.ID); err != nil {
		t.Fatal(err)
	}

	// Meer dan één, want het is een race: de eerste na het starten is de meest
	// waarschijnlijke om hem te verliezen, maar één keer goed bewijst niets.
	for i := range 5 {
		candidate := map[string]any{
			"name": fmt.Sprintf("Ding %d", i),
			"data": map[string]any{"id": fmt.Sprintf("ding-%d", i)},
		}
		device, err := apps.AddPairedDevice(ctx, appManifest.ID, "thing", candidate)
		if err != nil {
			t.Fatalf("apparaat %d koppelen: %v", i, err)
		}
		if device.Data["id"] != fmt.Sprintf("ding-%d", i) {
			t.Fatalf("apparaat %d kreeg data %v", i, device.Data)
		}
	}
}

var errHungRuntimeClosed = errors.New("hung runtime was closed")

type hungRuntime struct {
	plugin.Runtime
	entered   chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func newHungRuntime() *hungRuntime {
	return &hungRuntime{entered: make(chan struct{}), closed: make(chan struct{})}
}

func (r *hungRuntime) InvokeFlow(ctx context.Context, _, _ string, _, _ map[string]any) (any, error) {
	r.enterOnce.Do(func() { close(r.entered) })
	select {
	case <-r.closed:
		return nil, errHungRuntimeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *hungRuntime) Close() {
	r.closeOnce.Do(func() { close(r.closed) })
}

func TestHungRuntimeDoesNotBlockStopRestartOrClose(t *testing.T) {
	for _, lifecycle := range []string{"stop", "restart", "close"} {
		t.Run(lifecycle, func(t *testing.T) {
			database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			defer apps.Close()

			const appID = "com.stulp.hung"
			runtime := newHungRuntime()
			apps.mu.Lock()
			apps.runners[appID] = runtime
			apps.states[appID] = AppState{State: "running"}
			apps.mu.Unlock()

			callDone := make(chan error, 1)
			go func() {
				_, err := apps.InvokeFlow(context.Background(), appID, "action", "hang", nil, nil)
				callDone <- err
			}()
			select {
			case <-runtime.entered:
			case <-time.After(time.Second):
				t.Fatal("runtime call did not start")
			}

			// Restart gets a replacement without needing an installed bundle; the
			// point under test is that its Stop half can close the hung runtime.
			apps.newRuntime = func(context.Context, *store.Store, string, plugin.Options) (plugin.Runtime, error) {
				return &countingRuntime{}, nil
			}
			lifecycleDone := make(chan error, 1)
			go func() {
				switch lifecycle {
				case "stop":
					lifecycleDone <- apps.Stop(appID)
				case "restart":
					lifecycleDone <- apps.Restart(context.Background(), appID)
				case "close":
					apps.Close()
					lifecycleDone <- nil
				}
			}()

			select {
			case err := <-lifecycleDone:
				if err != nil {
					t.Fatalf("%s: %v", lifecycle, err)
				}
			case <-time.After(time.Second):
				runtime.Close()
				t.Fatalf("%s blocked behind the hung runtime call", lifecycle)
			}
			select {
			case err := <-callDone:
				if !errors.Is(err, errHungRuntimeClosed) {
					t.Fatalf("hung call returned %v", err)
				}
			case <-time.After(time.Second):
				runtime.Close()
				t.Fatal("hung call survived runtime Close")
			}
		})
	}
}

func TestConcurrentStopAndCloseReleaseHungCall(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()

	const appID = "com.stulp.hung-race"
	runtime := newHungRuntime()
	apps.mu.Lock()
	apps.runners[appID] = runtime
	apps.states[appID] = AppState{State: "running"}
	apps.mu.Unlock()

	callDone := make(chan error, 1)
	go func() {
		_, err := apps.InvokeFlow(context.Background(), appID, "action", "hang", nil, nil)
		callDone <- err
	}()
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("runtime call did not start")
	}

	lifecycleDone := make(chan error, 2)
	go func() { lifecycleDone <- apps.Stop(appID) }()
	go func() {
		apps.Close()
		lifecycleDone <- nil
	}()
	for range 2 {
		select {
		case err := <-lifecycleDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			runtime.Close()
			t.Fatal("concurrent Stop/Close blocked behind the hung call")
		}
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, errHungRuntimeClosed) {
			t.Fatalf("hung call returned %v", err)
		}
	case <-time.After(time.Second):
		runtime.Close()
		t.Fatal("hung call survived concurrent Stop/Close")
	}
}

// Een apparaat dat buiten de start om verschijnt -- alles wat een backup
// terugzet -- moet zijn device.init nog krijgen. Zonder dat heeft de app de
// toestand wel en het apparaat niet, en dan volgt een weer-app 0 locaties
// terwijl Stulp ze alle twee laat zien.
func TestRestoredDeviceIsAdoptedByARunningApp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"com.stulp.adopt","version":"1.0.0","sdk":3,"name":{"en":"Adopt"},
  "drivers":[{"id":"thing","name":{"en":"Thing"},"class":"other","capabilities":[]}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	plugintest.Install(t, appRoot, appManifest.ID)

	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}
	if err := database.SetAppEnabled(ctx, appManifest.ID, true); err != nil {
		t.Fatal(err)
	}
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	if err := apps.Start(ctx, appManifest.ID); err != nil {
		t.Fatal(err)
	}

	// Zoals een restore hem neerzet: het apparaat staat er, de app draait al.
	device, err := database.AddDevice(ctx, store.Device{
		AppID: appManifest.ID, DriverID: "thing", Name: "Teruggezet",
		Data: map[string]any{"id": "restored-1"}, Available: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := database.Device(ctx, device.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Store["testplugin.initialised"] == true {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("de app heeft het teruggezette apparaat niet geïnitialiseerd: %#v", stored.Store)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
