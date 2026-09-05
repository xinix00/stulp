package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
)

type AppState struct {
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
	RetryAt      string `json:"retryAt,omitempty"`
	RestartCount int    `json:"restartCount,omitempty"`
}

type appRetry struct {
	cancel  context.CancelFunc
	attempt int
}

type appStart struct {
	cancel context.CancelFunc
}

// Supervisor owns the long-lived JavaScript runtime for each enabled app.
// API requests never create an app runtime of their own.
type Supervisor struct {
	store   *store.Store
	options plugin.Options
	logger  *slog.Logger

	mu      sync.RWMutex
	runners map[string]plugin.Runtime
	states  map[string]AppState
	retries map[string]*appRetry
	starts  map[string]*appStart
	// attached zijn de apps die zich zelf gemeld hebben. Hun proces is niet van
	// Stulp, dus valt er voor hen niets te herstarten -- zie appExited.
	attached map[string]bool
	closed   bool
	// paused is a short maintenance window used by live restore. Unlike closed,
	// it is reversible: current runners are stopped, new starts and attaches are
	// refused, and Resume starts the apps from the restored document.
	paused bool

	retryBase time.Duration
	retryMax  time.Duration

	// images is waar een gedeelde afbeelding wacht. Nil betekent dat deze Stulp
	// er geen deelt; zie UseImages.
	images *imageshare.Store

	// newRuntime is een naad voor de tests. In productie is dit altijd
	// plugin.NewRuntime; een test kan er een runtime in zetten waarvan hij kan
	// nagaan of hij netjes gesloten is.
	newRuntime func(context.Context, *store.Store, string, plugin.Options) (plugin.Runtime, error)
	// newAttachedRuntime is dezelfde naad voor een app die zich meldt. Nil
	// betekent plugin.NewAttached.
	newAttachedRuntime func(context.Context, string, *appproto.Conn) (plugin.Runtime, error)
}

func New(database *store.Store, options plugin.Options) *Supervisor {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	supervisor := &Supervisor{
		store: database, options: options, logger: logger,
		runners: make(map[string]plugin.Runtime), states: make(map[string]AppState),
		retries: make(map[string]*appRetry), starts: make(map[string]*appStart),
		attached:  make(map[string]bool),
		retryBase: time.Second, retryMax: 30 * time.Second,
		newRuntime: plugin.NewRuntime,
	}
	// De runtime meldt een proces dat uit zichzelf stopt. Dat kan alleen hier
	// gezet worden: de supervisor moet weten wíe er weg is, en de runtime kent
	// de supervisor niet.
	supervisor.options.OnExit = supervisor.appExited
	// Hetzelfde verhaal als OnExit: een proces kent alleen zijn eigen app, en het
	// afbeeldingsregister loopt over alle apps.
	supervisor.options.ImageSources = supervisor.ImageSources
	supervisor.options.ImageURL = supervisor.ImageURL
	return supervisor
}

// appExited verwerkt een plugin die uit zichzelf gestopt is.
//
// De toestand gaat naar crashed met de reden erbij, en de herstart wordt
// ingepland alsof de start mislukt was -- want dat is wat er gebeurd is, alleen
// later. Zonder dit bleef de app "running" heten terwijl elke aanroep op EOF
// liep.
func (s *Supervisor) appExited(appID string, cause error) {
	s.mu.Lock()
	if s.closed || s.runners[appID] == nil {
		// Al opgeruimd, of dit is een oude runner die vervangen is.
		s.mu.Unlock()
		return
	}
	delete(s.runners, appID)
	// Een app die zich gemeld had, is nu weg. Stulp heeft hem niet gestart en kan
	// hem dus niet herstarten: dat doet Docker, systemd of de ontwikkelaar. Zou
	// hij het toch proberen, dan startte hij de binary naast app.json -- een
	// tweede exemplaar naast het exemplaar dat net stukliep.
	wasAttached := s.attached[appID]
	delete(s.attached, appID)
	message := "the app stopped on its own"
	if cause != nil {
		message = cause.Error()
	}
	count := s.states[appID].RestartCount
	if wasAttached {
		s.states[appID] = AppState{State: "waiting", Error: attachedAppGone, RestartCount: count}
	} else {
		s.states[appID] = AppState{State: "crashed", Error: message, RestartCount: count}
	}
	state := s.states[appID]
	s.mu.Unlock()

	s.logger.Warn("plugin stopped on its own", "app", appID, "error", message, "attached", wasAttached)
	s.store.PublishAppRuntime(appID, state)
	if wasAttached {
		return
	}
	s.scheduleRetry(appID)
}

func (s *Supervisor) StartAll(ctx context.Context) error {
	apps, err := s.store.Apps(ctx)
	if err != nil {
		return err
	}
	var firstError error
	for _, app := range apps {
		if !app.Enabled {
			s.setState(app.ID, AppState{State: "stopped"})
			continue
		}
		if err := s.Start(ctx, app.ID); err != nil {
			s.logger.Error("plugin failed to start", "app", app.ID, "error", err)
			if firstError == nil {
				firstError = err
			}
		}
	}
	return firstError
}

func (s *Supervisor) Start(ctx context.Context, appID string) error {
	err := s.startOnce(ctx, appID)
	if err != nil {
		s.scheduleRetry(appID)
	}
	return err
}

func (s *Supervisor) startOnce(ctx context.Context, appID string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("app supervisor is closed")
	}
	if s.paused {
		s.mu.Unlock()
		return errors.New("app supervisor is paused for restore")
	}
	if s.runners[appID] != nil {
		s.mu.Unlock()
		return nil
	}
	if s.starts[appID] != nil {
		s.mu.Unlock()
		return nil
	}
	previous := s.states[appID]
	s.states[appID] = AppState{State: "starting", RestartCount: previous.RestartCount}
	startContext, cancel := context.WithCancel(ctx)
	start := &appStart{cancel: cancel}
	s.starts[appID] = start
	s.mu.Unlock()

	// Een app die Stulp niet mag starten wacht tot hij zich meldt. Dat is geen
	// fout en dus ook geen herstart: er valt niets te herstarten, want het proces
	// hoort bij Docker, systemd of degene die hem in een debugger heeft.
	//
	// Een app ZONDER bundel (Root leeg) is hetzelfde geval in een andere jas:
	// hij heeft zich aangemeld (HopOS-slot, container), dus zijn binary is
	// niet van ons en "no binary at <id>" was geen diagnose maar een
	// crashed-retry-molen die pas stopte als de app zich (opnieuw) meldde —
	// met een logregel per poging (de panic die hier ooit zat is al eerder
	// verholpen; dit is het kalme restje).
	if app, appErr := s.store.App(startContext, appID); appErr == nil && (plugin.External(app) || app.Root == "") {
		s.mu.Lock()
		if s.starts[appID] == start {
			delete(s.starts, appID)
		}
		s.mu.Unlock()
		cancel()
		s.setState(appID, AppState{State: "waiting", Error: waitingForAttach})
		return nil
	}

	// Loading app code and running onInit can legitimately perform network IO.
	// It must never hold the supervisor-wide lock: unrelated app state, media
	// and capability requests should remain available while this app starts.
	runner, err := s.newRuntime(startContext, s.store, appID, s.options)
	if err == nil {
		err = runner.Start(startContext)
	}
	return s.finishStart(appID, start, cancel, runner, err, false)
}

// finishStart legt vast wat een start opleverde: de runner erin, de toestand
// erbij, en het opruimen als het toch niet doorgaat.
//
// Eén plek voor twee soorten start -- de app die Stulp zelf startte en de app die
// zich meldde. Wat daarvoor gebeurt verschilt (een binary starten of een
// verbinding aannemen), wat erna moet gebeuren niet, en dat is precies het stuk
// waar een tweede kopie stiekem anders zou gaan doen.
func (s *Supervisor) finishStart(appID string, start *appStart, cancel context.CancelFunc, runner plugin.Runtime, startErr error, attached bool) error {
	s.mu.Lock()
	if s.starts[appID] != start {
		s.mu.Unlock()
		cancel()
		if runner != nil {
			runner.Close()
		}
		return nil
	}
	delete(s.starts, appID)
	cancel()
	if startErr != nil {
		count := s.states[appID].RestartCount
		s.states[appID] = AppState{State: "crashed", Error: startErr.Error(), RestartCount: count}
		state := s.states[appID]
		s.mu.Unlock()
		// Het proces opruimen. NewRuntime kan geslaagd zijn -- de binary draait
		// dan al -- en pas Start mislukken, bijvoorbeeld doordat OnInit te lang
		// duurt. Zonder dit blijft dat proces staan, probeert de supervisor het
		// even later opnieuw, en stapelen ze op tot er niets meer start.
		if runner != nil {
			runner.Close()
		}
		s.store.PublishAppRuntime(appID, state)
		return fmt.Errorf("start app %q: %w", appID, startErr)
	}
	if s.closed {
		s.mu.Unlock()
		runner.Close()
		return errors.New("app supervisor is closed")
	}
	s.runners[appID] = runner
	if attached {
		s.attached[appID] = true
	}
	count := s.states[appID].RestartCount
	s.states[appID] = AppState{State: "running", RestartCount: count}
	if retry := s.retries[appID]; retry != nil {
		retry.cancel()
		delete(s.retries, appID)
	}
	state := s.states[appID]
	s.mu.Unlock()
	s.store.PublishAppRuntime(appID, state)
	return nil
}

func (s *Supervisor) scheduleRetry(appID string) {
	app, err := s.store.App(context.Background(), appID)
	if err != nil || !app.Enabled {
		return
	}
	s.mu.Lock()
	if s.closed || s.paused || s.runners[appID] != nil || s.retries[appID] != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	retry := &appRetry{cancel: cancel}
	s.retries[appID] = retry
	delay := s.retryBase
	state := s.states[appID]
	state.RetryAt = time.Now().Add(delay).UTC().Format(time.RFC3339Nano)
	s.states[appID] = state
	s.mu.Unlock()
	s.store.PublishAppRuntime(appID, state)
	go s.retryLoop(ctx, appID, retry, delay)
}

func (s *Supervisor) retryLoop(ctx context.Context, appID string, retry *appRetry, delay time.Duration) {
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.mu.Lock()
		if s.closed || s.retries[appID] != retry {
			s.mu.Unlock()
			return
		}
		retry.attempt++
		state := s.states[appID]
		state.RestartCount = retry.attempt
		state.RetryAt = ""
		s.states[appID] = state
		s.mu.Unlock()
		if err := s.startOnce(ctx, appID); err == nil {
			return
		}
		s.mu.Lock()
		if s.closed || s.retries[appID] != retry {
			s.mu.Unlock()
			return
		}
		delay = s.retryBase << min(retry.attempt, 20)
		if delay > s.retryMax || delay <= 0 {
			delay = s.retryMax
		}
		state = s.states[appID]
		state.RetryAt = time.Now().Add(delay).UTC().Format(time.RFC3339Nano)
		s.states[appID] = state
		s.mu.Unlock()
		s.store.PublishAppRuntime(appID, state)
		s.logger.Warn("plugin restart failed; retrying", "app", appID, "attempt", retry.attempt, "delay", delay, "error", state.Error)
	}
}

func (s *Supervisor) cancelRetryLocked(appID string) {
	if retry := s.retries[appID]; retry != nil {
		retry.cancel()
		delete(s.retries, appID)
	}
}

func (s *Supervisor) setState(appID string, state AppState) {
	s.mu.Lock()
	s.states[appID] = state
	s.mu.Unlock()
	s.store.PublishAppRuntime(appID, state)
}

func (s *Supervisor) Stop(appID string) error {
	s.mu.Lock()
	s.cancelRetryLocked(appID)
	if start := s.starts[appID]; start != nil {
		start.cancel()
		delete(s.starts, appID)
	}
	runner := s.runners[appID]
	if runner == nil {
		s.states[appID] = AppState{State: "stopped"}
		s.mu.Unlock()
		s.store.PublishAppRuntime(appID, AppState{State: "stopped"})
		return nil
	}
	delete(s.runners, appID)
	delete(s.attached, appID)
	s.states[appID] = AppState{State: "stopping"}
	s.mu.Unlock()
	runner.Close()
	s.mu.Lock()
	s.states[appID] = AppState{State: "stopped"}
	s.mu.Unlock()
	s.store.PublishAppRuntime(appID, AppState{State: "stopped"})
	return nil
}

func (s *Supervisor) Restart(ctx context.Context, appID string) error {
	if err := s.Stop(appID); err != nil {
		return err
	}
	return s.Start(ctx, appID)
}

// Pause stops every app and closes the attach door without permanently closing
// the supervisor. It is intentionally idempotent so a failed restore can use
// the same Resume path as a successful one.
func (s *Supervisor) Pause() {
	s.mu.Lock()
	s.paused = true
	ids := make(map[string]bool, len(s.states)+len(s.runners)+len(s.retries)+len(s.starts))
	for id := range s.states {
		ids[id] = true
	}
	for id := range s.runners {
		ids[id] = true
	}
	for id := range s.retries {
		ids[id] = true
	}
	for id := range s.starts {
		ids[id] = true
	}
	s.mu.Unlock()
	for id := range ids {
		_ = s.Stop(id)
	}
}

// Resume forgets runtime state belonging to the previous document and starts
// the enabled apps from the current one. Rootless apps move to waiting until
// their existing reconnect loop announces them again.
func (s *Supervisor) Resume(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("app supervisor is closed")
	}
	s.states = make(map[string]AppState)
	s.paused = false
	s.mu.Unlock()
	return s.StartAll(ctx)
}

func (s *Supervisor) Enable(ctx context.Context, appID string) error {
	if err := s.store.SetAppEnabled(ctx, appID, true); err != nil {
		return err
	}
	return s.Start(ctx, appID)
}

func (s *Supervisor) Disable(ctx context.Context, appID string) error {
	if err := s.Stop(appID); err != nil {
		return err
	}
	return s.store.SetAppEnabled(ctx, appID, false)
}

// Uninstall stops an app before the database forgets it, and returns what was
// removed so the caller can clean up the bundle and the Flows that used it.
//
// The app's own onDeleted handlers do not run for its devices. An uninstall
// removes the integration wholesale rather than unpairing device by device,
// and a stopped app cannot be asked to tidy up anyway. Devices that need to be
// released at the other end — a Matter fabric, a cloud subscription — should
// be deleted individually before the app goes.
func (s *Supervisor) Uninstall(ctx context.Context, appID string) (store.App, []store.Device, error) {
	if _, err := s.store.App(ctx, appID); err != nil {
		return store.App{}, nil, err
	}
	if err := s.Stop(appID); err != nil {
		return store.App{}, nil, err
	}
	removed, devices, err := s.store.UninstallApp(ctx, appID)
	if err != nil {
		return store.App{}, nil, err
	}
	// A reinstall should not inherit the crash count or retry state of the app
	// that just left.
	s.mu.Lock()
	delete(s.states, appID)
	s.mu.Unlock()
	return removed, devices, nil
}

func (s *Supervisor) State(appID string) AppState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.states[appID]
	if !ok {
		return AppState{State: "stopped"}
	}
	return state
}

// runnerForApp snapshots a stable Runtime pointer without retaining the
// supervisor-wide lock across app IPC. Stop and Close may remove and close that
// runtime concurrently; Process.Close is idempotent and closes its Session,
// which is precisely what interrupts an in-flight call to an unresponsive app.
func (s *Supervisor) runnerForApp(appID string) plugin.Runtime {
	s.mu.RLock()
	runner := s.runners[appID]
	s.mu.RUnlock()
	return runner
}

func (s *Supervisor) Registrations(ctx context.Context, appID string) (plugin.RegistrationSnapshot, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return plugin.RegistrationSnapshot{}, fmt.Errorf("app %q is not running", appID)
	}
	return runner.Registrations(ctx)
}

func (s *Supervisor) PairListDevices(ctx context.Context, appID, driverID string) ([]map[string]any, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", appID)
	}
	return runner.PairListDevices(ctx, driverID)
}

func (s *Supervisor) StartPairSession(ctx context.Context, appID, driverID, sessionID string) ([]string, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", appID)
	}
	return runner.StartPairSession(ctx, driverID, sessionID)
}

func (s *Supervisor) PairEmit(ctx context.Context, appID, sessionID, event string, data any) (any, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", appID)
	}
	return runner.PairEmit(ctx, sessionID, event, data)
}

func (s *Supervisor) ClosePairSession(ctx context.Context, appID, sessionID string) error {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil
	}
	return runner.ClosePairSession(ctx, sessionID)
}

func (s *Supervisor) AddPairedDevice(ctx context.Context, appID, driverID string, candidate map[string]any) (store.Device, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return store.Device{}, fmt.Errorf("app %q is not running", appID)
	}
	return runner.AddPairedDevice(ctx, driverID, candidate)
}

func (s *Supervisor) InvokeAppAPI(ctx context.Context, appID, handler string, query, body map[string]any) (any, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", appID)
	}
	return runner.InvokeAPI(ctx, handler, query, body)
}

// ReadUIAsset vraagt een aangemelde app om één bestand uit zijn ingebedde UI.
// Een gelijktijdige Stop mag de verbinding juist onder deze call sluiten: zo
// onderbreekt hij een app die niet meer antwoordt zonder de hele supervisor te
// blokkeren.
func (s *Supervisor) ReadUIAsset(ctx context.Context, appID, path string) (plugin.UIAsset, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return plugin.UIAsset{}, fmt.Errorf("app %q is not running", appID)
	}
	return runner.ReadUIAsset(ctx, path)
}

func (s *Supervisor) InvokeFlow(ctx context.Context, appID, cardType, cardID string, args, state map[string]any) (any, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", appID)
	}
	return runner.InvokeFlow(ctx, cardType, cardID, args, state)
}

func (s *Supervisor) InvokeFlowAutocomplete(ctx context.Context, appID, cardType, cardID, argument, query string, args map[string]any) (any, error) {
	runner := s.runnerForApp(appID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", appID)
	}
	return runner.InvokeFlowAutocomplete(ctx, cardType, cardID, argument, query, args)
}

func (s *Supervisor) SetAppSetting(ctx context.Context, appID, name string, value any) error {
	runner := s.runnerForApp(appID)
	if runner != nil {
		return runner.SetSetting(ctx, name, value)
	}
	return s.store.SetSetting(ctx, appID, name, value)
}

func (s *Supervisor) UnsetAppSetting(ctx context.Context, appID, name string) error {
	runner := s.runnerForApp(appID)
	if runner != nil {
		return runner.UnsetSetting(ctx, name)
	}
	return s.store.UnsetSetting(ctx, appID, name)
}

func (s *Supervisor) runnerForDevice(ctx context.Context, deviceID string) (plugin.Runtime, error) {
	device, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	runner := s.runnerForApp(device.AppID)
	if runner == nil {
		return nil, fmt.Errorf("app %q is not running", device.AppID)
	}
	return runner, nil
}

func (s *Supervisor) InvokeCapability(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) error {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	return runner.InvokeCapability(ctx, deviceID, capabilityID, value, options)
}

func (s *Supervisor) InvokeCapabilities(ctx context.Context, deviceID string, commands []plugin.CapabilityCommand, options map[string]any) (map[string]error, error) {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	return runner.InvokeCapabilities(ctx, deviceID, commands, options)
}

func (s *Supervisor) DeviceMedia(ctx context.Context, deviceID string) ([]plugin.MediaRegistration, error) {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	return runner.DeviceMedia(ctx, deviceID)
}

func (s *Supervisor) ResolveMedia(ctx context.Context, deviceID, slot, kind string) (plugin.VideoStream, error) {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return plugin.VideoStream{}, err
	}
	return runner.ResolveMedia(ctx, deviceID, slot, kind)
}

func (s *Supervisor) UpdateDeviceSettings(ctx context.Context, deviceID string, patch map[string]any) (store.Device, error) {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return store.Device{}, err
	}
	return runner.UpdateDeviceSettings(ctx, deviceID, patch)
}

func (s *Supervisor) RenameDevice(ctx context.Context, deviceID, name string) (store.Device, error) {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return store.Device{}, err
	}
	return runner.RenameDevice(ctx, deviceID, name)
}

func (s *Supervisor) DeleteDevice(ctx context.Context, deviceID string) error {
	runner, err := s.runnerForDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	return runner.DeleteDevice(ctx, deviceID)
}

func (s *Supervisor) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for appID := range s.retries {
		s.cancelRetryLocked(appID)
	}
	for appID, start := range s.starts {
		start.cancel()
		delete(s.starts, appID)
		s.states[appID] = AppState{State: "stopped"}
	}
	runners := s.runners
	s.runners = make(map[string]plugin.Runtime)
	for appID := range runners {
		s.states[appID] = AppState{State: "stopped"}
	}
	s.mu.Unlock()
	for _, runner := range runners {
		runner.Close()
	}
}

// ---------------------------------------------------------------------------
// Het afbeeldingsregister
// ---------------------------------------------------------------------------

// UseImages geeft de supervisor de plek waar een gedeelde afbeelding wacht.
// Zonder blijft het register er wel, en zegt het dat er niets te delen valt --
// duidelijker dan een SDK-call die niet bestaat.
func (s *Supervisor) UseImages(store *imageshare.Store) { s.images = store }

// ImageSources loopt de apparaten langs en vraagt hun eigen app welk stilstaand
// beeld ze aanmelden.
//
// Per apparaat en niet per app: een app weet welke slots hij heeft aangemeld,
// Stulp niet. Een app die niet draait of niet antwoordt levert niets en houdt de
// rest niet op -- het register is een aanbod, geen inventarisatie die moet
// kloppen.
func (s *Supervisor) ImageSources(ctx context.Context) ([]plugin.ImageRegistration, error) {
	devices, err := s.store.Devices(ctx, "")
	if err != nil {
		return nil, err
	}
	sources := make([]plugin.ImageRegistration, 0, 4)
	for _, device := range devices {
		registrations, mediaErr := s.DeviceMedia(ctx, device.ID)
		if mediaErr != nil {
			continue
		}
		for _, registration := range registrations {
			if registration.Kind != "image" {
				continue
			}
			title := registration.Title
			if title == "" {
				title = device.Name
			}
			sources = append(sources, plugin.ImageRegistration{
				DeviceID: device.ID, DeviceName: device.Name,
				Slot: registration.Slot, Title: title,
			})
		}
	}
	return sources, nil
}

// ImageURL geeft een kortlevend, onraadbaar adres voor het beeld.
//
// Het beeld zelf wordt pas opgehaald wanneer de service worker dat adres opent.
// Daardoor hoeft geen proces een complete camerafoto in zijn heap te bewaren.
// Een leeg slot betekent het eerste stilstaande beeld dat dit apparaat aanmeldt,
// want een camera heeft er meestal één.
func (s *Supervisor) ImageURL(ctx context.Context, deviceID, slot string) (string, error) {
	if s.images == nil {
		return "", errors.New("deze Stulp deelt geen afbeeldingen")
	}
	device, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return "", err
	}
	if slot == "" {
		registrations, mediaErr := s.DeviceMedia(ctx, device.ID)
		if mediaErr != nil {
			return "", fmt.Errorf("%s vragen om beeld: %w", device.Name, mediaErr)
		}
		for _, registration := range registrations {
			if registration.Kind == "image" {
				slot = registration.Slot
				break
			}
		}
		if slot == "" {
			return "", fmt.Errorf("%s heeft geen stilstaand beeld", device.Name)
		}
	}
	// Bewaar alleen hoe de bron gevonden wordt. ResolveMedia draait bewust pas
	// tijdens GET /image/: zo levert de app een verse, eventueel kortlevende URL
	// en blijven zowel de camera-aanroep als de beeldbytes uit deze SDK-aanroep.
	id, err := s.images.Put(func(fetchContext context.Context) (imageshare.Source, error) {
		source, resolveErr := s.ResolveMedia(fetchContext, device.ID, slot, "image")
		if resolveErr != nil {
			return imageshare.Source{}, fmt.Errorf("%s vragen om beeld: %w", device.Name, resolveErr)
		}
		return imageshare.Source{URL: source.URL, ContentType: source.ContentType}, nil
	})
	if err != nil {
		return "", err
	}
	return imageshare.Path(id), nil
}
