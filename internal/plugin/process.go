package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/store"
)

// Process is een app die als eigen proces draait.
//
// Stulp start de binary, geeft hem één kant van een socketpair als fd 3 mee, en
// spreekt er appproto tegen. Dat is de hele koppeling: geen gedeelde heap, geen
// gedeelde event loop, en geen manier voor de ene app om bij de andere te komen
// -- een socketpair heeft geen adres, dus er is niets om te adresseren.
//
// Een app kan ook al draaien voordat Stulp hem kent: gestart door Docker,
// systemd of een debugger, en aangemeld op de attach-socket. Dan is er geen
// binary om te starten en geen proces om op te wachten, en is dit alles wat
// verschilt -- vanaf de handshake is het hetzelfde protocol over dezelfde soort
// socket. Zie NewAttached.
//
// De kant die Stulp hier speelt is het spiegelbeeld van internal/appsdk. Wat
// daar een lokale kopie is, wordt hier bij de handshake verstuurd en met events
// bijgehouden; wat daar een schrijfactie is, komt hier als request binnen en
// gaat naar de store.
type Process struct {
	store    *store.Store
	app      store.App
	manifest *manifest.Manifest
	options  Options

	command *exec.Cmd
	session *appproto.Session
	// attached is de verbinding van een app die zich zelf gemeld heeft. Nil voor
	// een app die Stulp start; dat onderscheid is wat Start uit elkaar houdt.
	attached *appproto.Conn

	mu      sync.Mutex
	started bool
	closed  bool
	// adopted zijn de apparaten die hun device.init gehad hebben. Een apparaat
	// dat er later bij komt hoort die ook te krijgen, en precies één keer.
	adopted map[string]bool
	done    chan struct{}
	// ready gaat dicht zodra de app zich gemeld heeft. Pas daarna heeft het zin
	// om hem iets te vragen.
	ready     chan struct{}
	readyOnce sync.Once
	// unsubscribe stopt het volgen van de store.
	unsubscribe func()

	// media is wat elk apparaat van deze app aanmeldde: welk beeld het heeft.
	// Bijgehouden en niet opgevraagd, want het register loopt over alle apps en
	// een app die er zelf om vraagt kan niet ondertussen antwoorden. Zie
	// media.register in handle.
	mediaMu sync.Mutex
	media   map[string][]MediaRegistration
}

// NewPlugin bouwt de runtime voor een app die een binary is.
func NewPlugin(ctx context.Context, database *store.Store, appID string, options Options) (*Process, error) {
	app, err := database.App(ctx, appID)
	if err != nil {
		return nil, err
	}
	// Het manifest komt van de bundel als die er is, en anders uit het document.
	//
	// Een app die zich aanmeldde heeft geen bundel: hij is een image dat iemand
	// heeft neergezet, en wat Stulp van hem weet is precies wat hij bij zijn
	// begroeting meestuurde. Zonder deze tak faalt zo'n app NA het accepteren,
	// op een app.json die nooit heeft bestaan -- gemeten op ijzer, als een app
	// die zich braaf meldde en toch elke keer omviel.
	var appManifest *manifest.Manifest
	if app.Root != "" {
		appManifest, _, err = manifest.Load(app.Root)
		if err != nil {
			return nil, err
		}
	} else if app.Manifest != nil {
		appManifest, err = manifest.FromRaw(app.Manifest)
		if err != nil {
			return nil, fmt.Errorf("app %q has no bundle and its stored manifest is unusable: %w", appID, err)
		}
	} else {
		return nil, fmt.Errorf("app %q has neither a bundle nor a manifest", appID)
	}
	return &Process{
		store: database, app: app, manifest: appManifest,
		options: options, done: make(chan struct{}), ready: make(chan struct{}),
	}, nil
}

// NewAttached bouwt de runtime voor een app die al draait en zich gemeld heeft
// op de attach-socket.
//
// De verbinding komt er al gewikkeld in, en dat moet ook: de begroeting is er al
// af gelezen, en een tweede laag eromheen zou de bytes die daarachter gebufferd
// staan kwijtraken.
func NewAttached(ctx context.Context, database *store.Store, appID string, conn *appproto.Conn, options Options) (*Process, error) {
	process, err := NewPlugin(ctx, database, appID, options)
	if err != nil {
		return nil, err
	}
	process.attached = conn
	return process, nil
}

// External zegt of Stulp deze app met rust moet laten.
//
// Het staat in app.json omdat daar staat wat een app is, en omdat een app die in
// een container zit toch al een map bij Stulp heeft: zijn manifest, zijn
// instellingenpagina en zijn koppelpagina's komen daaruit. Eén regel erbij is
// dan de hele aankondiging.
//
// Zonder dit zou "geen binary" het teken moeten zijn, en dat betekent hier al
// iets anders: een half uitgepakte installatie, die hoort te klagen en het
// opnieuw te proberen.
func External(app store.App) bool {
	external, _ := app.Manifest["external"].(bool)
	return external
}

// binaryPath is waar het uitvoerbare bestand van de app staat: zijn eigen app-id
// naast het manifest. Eén app is één map -- binary, app.json, locales, assets.
func (p *Process) binaryPath() string { return filepath.Join(p.app.Root, p.app.ID) }

func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("plugin is already started")
	}
	if p.closed {
		p.mu.Unlock()
		return errors.New("plugin is closed")
	}
	p.started = true
	p.mu.Unlock()

	if p.attached != nil {
		// Niets te starten en niets te reapen: het proces is niet van ons. De
		// verbinding staat er al, dus dit is meteen de handshake.
		p.serve(p.attached, nil)
		return p.initialize(ctx)
	}

	binary := p.binaryPath()
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("app %s has no binary at %s: %w", p.app.ID, binary, err)
	}

	pair, err := appproto.NewPair()
	if err != nil {
		return err
	}
	// Bewust exec.Command en niet CommandContext: de context van Start hoort bij
	// het starten, niet bij de levensduur van het proces. Eraan koppelen zou de
	// app doodmaken zodra de aanroeper zijn context loslaat.
	command := exec.Command(binary)
	command.Dir = p.app.Root
	command.ExtraFiles = []*os.File{pair.Child}
	// Wat de app uitspuugt gaat naar de log van Stulp, met zijn naam erbij.
	// Beide stromen: een plugin logt via de SDK naar stderr, maar een panic of
	// een bibliotheek die zelf schrijft komt langs stdout binnen, en dat wil je
	// net zo goed zien.
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	errors, err := command.StderrPipe()
	if err != nil {
		return err
	}
	// STULP_APP_DATA is waar een app zijn eigen bestanden mag neerzetten.
	command.Env = append(os.Environ(), "STULP_APP_DATA="+filepath.Join(p.app.Root, ".data"))

	if err := command.Start(); err != nil {
		pair.Parent.Close()
		pair.Child.Close()
		return fmt.Errorf("start app %s: %w", p.app.ID, err)
	}
	// De kant van het kind hoort bij het kind; hem hier openhouden zou betekenen
	// dat het kanaal niet dichtgaat als het proces verdwijnt.
	pair.Child.Close()

	go p.readOutput(output)
	go p.readOutput(errors)

	conn, err := pair.Conn()
	if err != nil {
		command.Process.Kill()
		return err
	}
	p.command = command
	p.serve(appproto.NewConn(conn), command.Wait)

	return p.initialize(ctx)
}

// serve begint het protocol op een verbinding die er al staat.
//
// reap wacht op het proces nadat de verbinding weg is en levert de reden. Voor
// een app die zich gemeld heeft is hij nil: dat proces is niet van Stulp, dus
// valt er niets te oogsten en is de verbroken verbinding het hele verhaal.
func (p *Process) serve(conn *appproto.Conn, reap func() error) {
	p.session = appproto.NewSession(conn, p.handle, func(string, json.RawMessage) {})

	go func() {
		defer close(p.done)
		p.session.Serve()
		var err error
		if reap != nil {
			err = reap()
		}
		// Melden dat dit proces weg is.
		//
		// Zonder deze regel merkt niemand het: de supervisor blijft "running"
		// tonen en elke aanroep loopt op EOF. Een huis dat er in Manage goed
		// uitziet en niets meer doet is erger dan een dat zegt dat het stuk is.
		p.mu.Lock()
		deliberate := p.closed
		notify := p.options.OnExit
		p.mu.Unlock()
		if !deliberate && notify != nil {
			notify(p.app.ID, err)
		}
	}()

	// Wat een ander verandert -- de interface, een Flow, een andere app -- moet
	// ook bij deze app aankomen. Loopt de wachtrij over, dan meldt de store dat
	// met een reload-marker en gaat de hele stand er opnieuw naartoe, in plaats
	// van dat de app stilletjes met een verouderde kopie doorwerkt.
	events, cancel := p.store.Subscribe(64)
	p.unsubscribe = cancel
	go p.follow(events)
}

// initialize wacht op de handshake en start daarna wat er al gekoppeld is.
//
// Een app die net op gang komt kent zijn apparaten pas als Stulp ze noemt: het
// manifest zegt welke drivers er zijn, de store welke apparaten. Zonder deze
// stap draait er een proces dat niets te doen heeft.
func (p *Process) initialize(ctx context.Context) error {
	timeout := p.options.CallTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	select {
	case <-p.ready:
	case <-p.done:
		return fmt.Errorf("app %s stopped before it said hello", p.app.ID)
	case <-time.After(timeout):
		return fmt.Errorf("app %s did not say hello within %s", p.app.ID, timeout)
	}

	if err := p.call(ctx, "app.init", map[string]any{}, nil); err != nil {
		return err
	}
	devices, err := p.store.Devices(ctx, p.app.ID)
	if err != nil {
		return err
	}
	for _, device := range devices {
		if err := p.call(ctx, "driver.init", map[string]any{"driverId": device.DriverID}, nil); err != nil {
			return err
		}
		if err := p.startDevice(ctx, device); err != nil {
			return err
		}
	}
	return nil
}

func (p *Process) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	unsubscribe := p.unsubscribe
	p.mu.Unlock()

	if unsubscribe != nil {
		unsubscribe()
	}
	if p.session != nil {
		p.session.Close()
	}
	if p.command != nil && p.command.Process != nil {
		p.command.Process.Kill()
		<-p.done
	}
	// Een app die zich gemeld heeft kan Stulp niet omleggen -- dat proces is niet
	// van hem. De socket sluiten is het sterkste wat hij kan zeggen, en de app
	// hoort daarop te stoppen. Wachten tot zijn kant weg is houdt Stop synchroon,
	// net als bij een app die Stulp zelf startte.
	if p.attached != nil && p.session != nil {
		<-p.done
	}
}

// ---------------------------------------------------------------------------
// Wat de app aan Stulp vraagt
// ---------------------------------------------------------------------------

func (p *Process) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "hello":
		answer, err := p.welcome(ctx)
		if err == nil {
			p.readyOnce.Do(func() { close(p.ready) })
		}
		return answer, err

	case "device.set":
		var q struct {
			DeviceID string `json:"deviceId"`
			Field    string `json:"field"`
			Value    any    `json:"value"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		return nil, p.updateDevice(ctx, q.DeviceID, func(device *store.Device) error {
			return applyDeviceField(device, q.Field, q.Value)
		})

	case "device.merge":
		var q struct {
			DeviceID string         `json:"deviceId"`
			Field    string         `json:"field"`
			Patch    map[string]any `json:"patch"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		return nil, p.updateDevice(ctx, q.DeviceID, func(device *store.Device) error {
			target, err := deviceMap(device, q.Field)
			if err != nil {
				return err
			}
			for key, value := range q.Patch {
				(*target)[key] = value
			}
			return nil
		})

	case "setting.set":
		var q struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		if err := p.store.SetSetting(ctx, p.app.ID, q.Key, q.Value); err != nil {
			return nil, err
		}
		return nil, p.pushSettings(ctx)

	case "setting.unset":
		var q struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		if err := p.store.UnsetSetting(ctx, p.app.ID, q.Key); err != nil {
			return nil, err
		}
		return nil, p.pushSettings(ctx)

	case "capability.add", "capability.remove":
		var q struct {
			DeviceID   string `json:"deviceId"`
			Capability string `json:"capability"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		return nil, p.updateDevice(ctx, q.DeviceID, func(device *store.Device) error {
			if method == "capability.add" {
				if !slices.Contains(device.Capabilities, q.Capability) {
					device.Capabilities = append(device.Capabilities, q.Capability)
				}
				return nil
			}
			device.Capabilities = slices.DeleteFunc(device.Capabilities,
				func(name string) bool { return name == q.Capability })
			return nil
		})

	case "capability.options":
		// Capability-opties staan per driver in het manifest en zijn daarmee niet
		// van de app om te veranderen. Weigeren is eerlijker dan stil accepteren.
		return nil, errors.New("capability options are declared in app.json and cannot be set at runtime")

	case "device.replace":
		var q struct {
			Replacements map[string]store.DeviceReplacement `json:"replacements"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		for oldID, replacement := range q.Replacements {
			if err := p.ownsDevice(ctx, oldID); err != nil {
				return nil, err
			}
			if err := p.ownsDevice(ctx, replacement.DeviceID); err != nil {
				return nil, err
			}
		}
		return nil, p.store.ReplaceDeviceReferences(ctx, q.Replacements)

	case "state.set":
		var q struct {
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		return nil, p.store.SetAppState(ctx, p.app.ID, q.State)

	case "flow.trigger":
		var q struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Tokens any    `json:"tokens"`
			State  any    `json:"state"`
			System bool   `json:"system"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		if q.System {
			return nil, p.store.RecordSystemFlowEvent(ctx, q.Kind, q.ID, q.Tokens, q.State)
		}
		return nil, p.store.RecordFlowEvent(ctx, p.app.ID, q.Kind, q.ID, q.Tokens, q.State)

	case "notification":
		var q struct {
			Excerpt string `json:"excerpt"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		_, err := p.store.CreateNotification(ctx, p.app.ID, q.Excerpt)
		return nil, err

	case "media.register":
		var q struct {
			DeviceID string              `json:"deviceId"`
			Media    []MediaRegistration `json:"media"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		p.mediaMu.Lock()
		if p.media == nil {
			p.media = map[string][]MediaRegistration{}
		}
		if len(q.Media) == 0 {
			delete(p.media, q.DeviceID)
		} else {
			p.media[q.DeviceID] = q.Media
		}
		p.mediaMu.Unlock()
		return nil, nil

	case "images.list":
		if p.options.ImageSources == nil {
			return nil, errors.New("this Stulp does not share images")
		}
		sources, err := p.options.ImageSources(ctx)
		if err != nil {
			return nil, err
		}
		if sources == nil {
			sources = []ImageRegistration{}
		}
		return sources, nil

	case "image.url":
		var q struct {
			DeviceID string `json:"deviceId"`
			Slot     string `json:"slot"`
		}
		if err := json.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		if p.options.ImageURL == nil {
			return nil, errors.New("this Stulp does not share images")
		}
		address, err := p.options.ImageURL(ctx, q.DeviceID, q.Slot)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": address}, nil
	}
	return nil, fmt.Errorf("app %s asked for unknown method %q", p.app.ID, method)
}

// welcome is alles wat de app bij de start moet weten.
func (p *Process) welcome(ctx context.Context) (any, error) {
	settings, err := p.store.Settings(ctx, p.app.ID)
	if err != nil {
		return nil, err
	}
	appState, err := p.store.AppState(ctx, p.app.ID)
	if err != nil {
		return nil, err
	}
	devices, err := p.store.Devices(ctx, p.app.ID)
	if err != nil {
		return nil, err
	}
	table := map[string]map[string]any{}
	for _, device := range devices {
		table[device.ID] = deviceSnapshot(device)
	}
	return map[string]any{
		"protocol": ProtocolVersion, "appId": p.app.ID,
		"stulpId": p.options.StulpID, "stulpVersion": p.options.StulpVersion,
		"language": p.options.Language, "timezone": p.options.Timezone,
		"manifest": p.app.Manifest, "env": map[string]any{}, "locale": map[string]any{},
		"settings": settings, "devices": table, "appState": appState,
	}, nil
}

// ProtocolVersion moet gelijk zijn aan appsdk.ProtocolVersion. Bij verschil
// faalt de start met een duidelijke melding in plaats van zich later vreemd te
// gedragen.
//
// Geëxporteerd omdat een app die zich meldt zijn versie in de begroeting zet, en
// de supervisor die moet kunnen narekenen voordat er een handshake begint.
const ProtocolVersion = 1

// readOutput leest wat de app schrijft en zet het in de log van Stulp.
//
// De SDK schrijft "niveau\tbericht"; alles wat die vorm niet heeft komt van de
// app zelf of van een bibliotheek eronder en gaat als info door. Een lange regel
// wordt niet afgekapt maar in stukken gemeld -- een afgekapte stack trace is
// precies de regel die je nodig had.
func (p *Process) readOutput(stream io.ReadCloser) {
	defer stream.Close()
	reader := bufio.NewReader(stream)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			level, message := "info", trimmed
			if name, rest, split := strings.Cut(trimmed, "\t"); split && isLogLevel(name) {
				level, message = name, rest
			}
			p.log(level, message)
		}
		if err != nil {
			return
		}
	}
}

func isLogLevel(name string) bool {
	switch name {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

func (p *Process) log(level, message string) {
	logger := p.options.Logger
	if logger == nil {
		return
	}
	switch level {
	case "error":
		logger.Error(message, "app", p.app.ID)
	case "warn":
		logger.Warn(message, "app", p.app.ID)
	case "debug":
		logger.Debug(message, "app", p.app.ID)
	default:
		logger.Info(message, "app", p.app.ID)
	}
}

// ownsDevice weigert een app die het over andermans apparaat heeft. Zonder deze
// controle zou een plugin Flows kunnen laten wijzen naar apparaten waar hij
// niets mee te maken heeft.
func (p *Process) ownsDevice(ctx context.Context, deviceID string) error {
	device, err := p.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	if device.AppID != p.app.ID {
		return fmt.Errorf("device %s does not belong to app %s", deviceID, p.app.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wat Stulp aan de app vraagt
// ---------------------------------------------------------------------------

func (p *Process) call(ctx context.Context, method string, params any, out any) error {
	if p.session == nil {
		return fmt.Errorf("app %s is not running", p.app.ID)
	}
	raw, err := p.session.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (p *Process) PairListDevices(ctx context.Context, driverID string) ([]map[string]any, error) {
	var found []map[string]any
	err := p.call(ctx, "pair.list", map[string]any{"driverId": driverID}, &found)
	return found, err
}

// Een koppelsessie is het gesprek met de koppelpagina van een driver. Welke
// berichten die pagina mag sturen bepaalt de plugin.
func (p *Process) StartPairSession(ctx context.Context, driverID, sessionID string) ([]string, error) {
	var handlers []string
	err := p.call(ctx, "pair.start", map[string]any{
		"driverId": driverID, "sessionId": sessionID,
	}, &handlers)
	return handlers, err
}

func (p *Process) PairEmit(ctx context.Context, sessionID, event string, data any) (any, error) {
	var result any
	err := p.call(ctx, "pair.emit", map[string]any{
		"sessionId": sessionID, "event": event, "data": data,
	}, &result)
	return result, err
}

func (p *Process) ClosePairSession(ctx context.Context, sessionID string) error {
	return p.call(ctx, "pair.close", map[string]any{"sessionId": sessionID}, nil)
}

func (p *Process) AddPairedDevice(ctx context.Context, driverID string, candidate map[string]any) (store.Device, error) {
	device, err := pairedDevice(p.manifest, p.app.ID, driverID, candidate)
	if err != nil {
		return store.Device{}, err
	}
	created, err := p.store.AddDevice(ctx, device)
	if err != nil {
		// POST /pair/devices kan zijn antwoord verliezen nadat AddDevice al op
		// schijf stond. Een retry met dezelfde fysieke identiteit hoort dan het
		// bestaande apparaat terug te krijgen, niet een onherstelbare duplicate-
		// fout. Dit maakt de generieke pair-naad idempotent voor iedere plugin.
		if existing, ok := p.existingPairedDevice(ctx, device); ok {
			return existing, nil
		}
		return store.Device{}, err
	}

	// De app moet dit apparaat kennen vóórdat hij het start: NewDevice leest
	// Data() -- het serienummer, het node-id, het adres -- en zonder dat weigert
	// vrijwel elke driver terecht met "dit apparaat heeft geen id".
	//
	// AddDevice stuurt zelf ook een gebeurtenis, maar die loopt langs follow op
	// een eigen goroutine en haalt het dus niet altijd. Hier duwen is niet
	// dubbelop maar het enige moment waarop de volgorde vastligt: een event komt
	// bij de app in de leesgoroutine binnen en een verzoek gaat in de wachtrij,
	// dus wat eerst geschreven is, is eerst toegepast.
	if err := p.pushDevice(created); err != nil {
		if rollbackErr := p.rollbackPairedDevice(ctx, created.ID); rollbackErr != nil {
			return store.Device{}, errors.Join(err, fmt.Errorf("rollback paired device: %w", rollbackErr))
		}
		return store.Device{}, err
	}

	if err := p.startDevice(ctx, created); err != nil {
		// Een device dat niet wil starten hoort niet als gekoppeld te blijven
		// staan; anders zit er een dode tegel in de interface.
		if rollbackErr := p.rollbackPairedDevice(ctx, created.ID); rollbackErr != nil {
			return store.Device{}, errors.Join(err, fmt.Errorf("rollback paired device: %w", rollbackErr))
		}
		return store.Device{}, err
	}
	return created, nil
}

// rollbackPairedDevice gebruikt bewust niet de request-context waarmee het
// koppelen binnenkwam. Die kan precies verlopen nadat AddDevice duurzaam heeft
// geschreven maar voordat pushDevice/device.init klaar is. Met diezelfde
// geannuleerde context zou DeleteDevice onmiddellijk weigeren en meldt de
// aanroeper een fout terwijl er toch een dood apparaat achterblijft. De store-
// rollback is lokaal en begrensd; contextwaarden blijven voor diagnostiek mee.
func (p *Process) rollbackPairedDevice(ctx context.Context, deviceID string) error {
	return p.store.DeleteDevice(context.WithoutCancel(ctx), deviceID)
}

func (p *Process) existingPairedDevice(ctx context.Context, candidate store.Device) (store.Device, bool) {
	devices, err := p.store.Devices(ctx, candidate.AppID)
	if err != nil {
		return store.Device{}, false
	}
	for _, device := range devices {
		if device.DriverID == candidate.DriverID && reflect.DeepEqual(device.Data, candidate.Data) {
			return device, true
		}
	}
	return store.Device{}, false
}

func (p *Process) startDevice(ctx context.Context, device store.Device) error {
	if err := p.call(ctx, "device.init", map[string]any{
		"deviceId": device.ID, "driverId": device.DriverID,
	}, nil); err != nil {
		return err
	}
	p.mu.Lock()
	if p.adopted == nil {
		p.adopted = map[string]bool{}
	}
	p.adopted[device.ID] = true
	p.mu.Unlock()
	return nil
}

// adopt geeft een apparaat dat deze app nog niet kent zijn driver en zijn init.
// Zonder dit blijft een apparaat dat buiten de start om verschijnt -- alles wat
// een backup terugzet -- voor de app onbekend: hij krijgt de toestand wel, maar
// heeft geen apparaat dat erop reageert. Dat is precies wat een weer-app die
// "0 locaties" volgt en een speler-app die zijn twee spelers niet kent zeggen.
func (p *Process) adopt(ctx context.Context, device store.Device) {
	p.mu.Lock()
	known := p.adopted[device.ID]
	p.mu.Unlock()
	// De toestand gaat altijd eerst: device.init laat de driver het apparaat
	// bouwen uit zijn eigen data, en die kent hij pas als hij hem gekregen heeft.
	p.pushDevice(device)
	if known {
		return
	}
	if err := p.call(ctx, "driver.init", map[string]any{"driverId": device.DriverID}, nil); err != nil {
		p.log("warn", "device "+device.ID+" could not start its driver: "+err.Error())
		return
	}
	if err := p.startDevice(ctx, device); err != nil {
		p.log("warn", "device "+device.ID+" could not be initialised: "+err.Error())
	}
}

// InvokeAPI is hoe de settings-pagina van een app zijn eigen plugin aanspreekt.
// Die pagina draait in de browser en kan niet bij het apparaat; de plugin wel.
func (p *Process) InvokeAPI(ctx context.Context, handler string, query, body map[string]any) (any, error) {
	var result any
	err := p.call(ctx, "api.invoke", map[string]any{
		"handler": handler, "query": query, "body": body,
	}, &result)
	return result, err
}

// ReadUIAsset haalt één ingebed frontendbestand bij een aangemelde app op. Een
// gebundelde app wordt door de weblaag rechtstreeks van schijf gelezen en komt
// hier niet langs.
func (p *Process) ReadUIAsset(ctx context.Context, path string) (UIAsset, error) {
	var asset UIAsset
	err := p.call(ctx, "ui.asset", map[string]any{"path": path}, &asset)
	return asset, err
}

func (p *Process) SetSetting(ctx context.Context, name string, value any) error {
	if err := p.store.SetSetting(ctx, p.app.ID, name, value); err != nil {
		return err
	}
	return p.pushSettings(ctx)
}

func (p *Process) UnsetSetting(ctx context.Context, name string) error {
	if err := p.store.UnsetSetting(ctx, p.app.ID, name); err != nil {
		return err
	}
	return p.pushSettings(ctx)
}

func (p *Process) InvokeCapability(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) error {
	return p.call(ctx, "capability.invoke", map[string]any{
		"deviceId": deviceID, "capability": capabilityID, "value": value, "options": options,
	}, nil)
}

// InvokeCapabilities stuurt één apparaat al zijn waarden in één verzoek, zodat
// de app kan bundelen wat het apparaat bundelt: lamp aan én op die helderheid
// is voor een Matter-lamp één commando. Het antwoord zegt per capability wat
// mislukte. Een app van vóór deze methode kent haar niet en zegt dat; dan gaan
// de opdrachten één voor één, in de volgorde die Stulp koos.
func (p *Process) InvokeCapabilities(ctx context.Context, deviceID string, commands []CapabilityCommand, options map[string]any) (map[string]error, error) {
	var failed map[string]string
	err := p.call(ctx, "capabilities.invoke", map[string]any{
		"deviceId": deviceID, "commands": commands, "options": options,
	}, &failed)
	result := make(map[string]error, len(failed))
	if err != nil {
		if !strings.Contains(err.Error(), "unknown method") {
			return nil, err
		}
		for _, command := range commands {
			if callErr := p.InvokeCapability(ctx, deviceID, command.Capability, command.Value, options); callErr != nil {
				result[command.Capability] = callErr
			}
		}
		return result, nil
	}
	for capability, message := range failed {
		result[capability] = errors.New(message)
	}
	return result, nil
}

func (p *Process) UpdateDeviceSettings(ctx context.Context, deviceID string, patch map[string]any) (store.Device, error) {
	if err := p.updateDevice(ctx, deviceID, func(device *store.Device) error {
		for key, value := range patch {
			device.Settings[key] = value
		}
		return nil
	}); err != nil {
		return store.Device{}, err
	}
	return p.store.Device(ctx, deviceID)
}

func (p *Process) RenameDevice(ctx context.Context, deviceID, name string) (store.Device, error) {
	if err := p.updateDevice(ctx, deviceID, func(device *store.Device) error {
		device.Name = name
		return nil
	}); err != nil {
		return store.Device{}, err
	}
	return p.store.Device(ctx, deviceID)
}

func (p *Process) DeleteDevice(ctx context.Context, deviceID string) error {
	// De app eerst: wat hij buiten Stulp moet opruimen kan hij daarna niet meer.
	// Een fout daar houdt het verwijderen niet tegen -- de gebruiker vroeg erom,
	// en een app die niet meewerkt mag dat niet blokkeren.
	if err := p.call(ctx, "device.delete", map[string]any{"deviceId": deviceID}, nil); err != nil {
		p.options.Logger.Warn("app could not clean up before device removal",
			"app", p.app.ID, "device", deviceID, "error", err)
	}
	return p.store.DeleteDevice(ctx, deviceID)
}

func (p *Process) InvokeFlow(ctx context.Context, cardType, cardID string, args, state map[string]any) (any, error) {
	var result any
	err := p.call(ctx, "flow.run", map[string]any{
		"kind": cardType, "id": cardID, "args": args, "state": state,
	}, &result)
	return result, err
}

func (p *Process) InvokeFlowAutocomplete(ctx context.Context, cardType, cardID, argument, query string, args map[string]any) (any, error) {
	var items any
	err := p.call(ctx, "flow.autocomplete", map[string]any{
		"kind": cardType, "id": cardID, "argument": argument, "query": query, "args": args,
	}, &items)
	return items, err
}

func (p *Process) Registrations(ctx context.Context) (RegistrationSnapshot, error) {
	var answer struct {
		Drivers []string           `json:"drivers"`
		Flows   []FlowRegistration `json:"flows"`
	}
	if err := p.call(ctx, "registrations", nil, &answer); err != nil {
		return RegistrationSnapshot{}, err
	}
	return RegistrationSnapshot{
		Drivers: answer.Drivers, Flows: answer.Flows,
		Tokens: []map[string]any{}, Widgets: []map[string]any{},
		Media: []MediaRegistration{},
	}, nil
}

// DeviceMedia antwoordt uit wat de app zelf aanmeldde. Zie media.register in
// handle: het staat hier bijgehouden en wordt niet opgevraagd.
func (p *Process) DeviceMedia(_ context.Context, deviceID string) ([]MediaRegistration, error) {
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	return append([]MediaRegistration(nil), p.media[deviceID]...), nil
}

// ResolveMedia vraagt de app waar het beeld van dit slot staat. kind is "image",
// "video", of leeg voor "video, en anders het beeld".
func (p *Process) ResolveMedia(ctx context.Context, deviceID, slot, kind string) (VideoStream, error) {
	var source VideoStream
	err := p.call(ctx, "video.resolve", map[string]any{"deviceId": deviceID, "slot": slot, "kind": kind}, &source)
	source.DeviceID, source.Slot = deviceID, slot
	return source, err
}

// ---------------------------------------------------------------------------
// De lokale kopie van de app bijhouden
// ---------------------------------------------------------------------------

// updateDevice past een device aan en duwt de nieuwe stand naar de app vóór het
// antwoord teruggaat. Dat is wat read-your-own-writes waarmaakt: als de app zijn
// schrijfactie terugkrijgt, klopt zijn kopie al.
func (p *Process) updateDevice(ctx context.Context, deviceID string, change func(*store.Device) error) error {
	device, err := p.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	if device.AppID != p.app.ID {
		return fmt.Errorf("device %s does not belong to app %s", deviceID, p.app.ID)
	}
	if err := change(&device); err != nil {
		return err
	}
	if err := p.store.UpdateDevice(ctx, device); err != nil {
		return err
	}
	return p.pushDevice(device)
}

func (p *Process) pushDevice(device store.Device) error {
	if p.session == nil {
		return nil
	}
	return p.session.Notify("state.device", map[string]any{
		"deviceId": device.ID, "device": deviceSnapshot(device),
	})
}

func (p *Process) pushSettings(ctx context.Context) error {
	settings, err := p.store.Settings(ctx, p.app.ID)
	if err != nil {
		return err
	}
	if p.session == nil {
		return nil
	}
	return p.session.Notify("state.settings", settings)
}

// follow duwt elke wijziging uit de store door naar de app.
func (p *Process) follow(events <-chan store.Event) {
	ctx := context.Background()
	for event := range events {
		switch event.Manager {
		case "store":
			if event.Type == "store.reload" {
				// De wachtrij liep over, dus welke deltas we gemist hebben is
				// niet te zeggen. Alles opnieuw sturen is het enige antwoord dat
				// klopt; doorgaan zou de app met een gat laten zitten.
				p.pushSettings(ctx)
				if devices, err := p.store.Devices(ctx, p.app.ID); err == nil {
					for _, device := range devices {
						p.adopt(ctx, device)
					}
				}
			}
		case "devices":
			device, err := p.store.Device(ctx, event.ID)
			if err != nil {
				// Weg is weg: de app hoort te weten dat het device er niet meer
				// is. Welke app het was weten we niet meer, dus dit gaat naar
				// iedereen die het aangaat -- een id dat de app niet kent laat
				// hij vallen.
				p.notify("state.device", map[string]any{"deviceId": event.ID, "device": nil})
				continue
			}
			if device.AppID == p.app.ID {
				p.adopt(ctx, device)
			}
		case "apps":
			if event.Type == "app.settings" && event.ID == p.app.ID {
				p.pushSettings(ctx)
			}
		}
	}
}

func (p *Process) notify(method string, params any) {
	if p.session != nil {
		p.session.Notify(method, params)
	}
}

// ---------------------------------------------------------------------------

func deviceSnapshot(device store.Device) map[string]any {
	return map[string]any{
		"name": device.Name, "class": device.Class, "available": device.Available,
		// De reden hoort bij de vlag: zonder dit veld kan een app zijn eigen
		// onbereikbaar-melding niet teruglezen, en dus ook niet zien dat een
		// nieuwe melding een oude moet vervangen.
		"unavailableMessage": device.Message,
		"data":               device.Data, "settings": device.Settings, "store": device.Store,
		"state": device.State, "capabilities": device.Capabilities,
	}
}

func deviceMap(device *store.Device, field string) (*map[string]any, error) {
	var target *map[string]any
	switch field {
	case "settings":
		target = &device.Settings
	case "store":
		target = &device.Store
	case "state":
		target = &device.State
	default:
		return nil, fmt.Errorf("device field %q is not a map", field)
	}
	if *target == nil {
		*target = map[string]any{}
	}
	return target, nil
}

func applyDeviceField(device *store.Device, field string, value any) error {
	switch field {
	case "name":
		device.Name, _ = value.(string)
	case "class":
		device.Class, _ = value.(string)
	case "available":
		device.Available, _ = value.(bool)
	case "unavailableMessage":
		device.Message, _ = value.(string)
	default:
		return fmt.Errorf("device field %q cannot be set", field)
	}
	return nil
}

// pairedDevice bouwt het store-device uit wat het koppelen opleverde, met de
// standaardwaarden uit het manifest eronder.
func pairedDevice(appManifest *manifest.Manifest, appID, driverID string, candidate map[string]any) (store.Device, error) {
	driver, ok := appManifest.Driver(driverID)
	if !ok {
		return store.Device{}, fmt.Errorf("driver %q does not exist", driverID)
	}
	name, _ := candidate["name"].(string)
	if strings.TrimSpace(name) == "" {
		return store.Device{}, errors.New("paired device needs a name")
	}
	device := store.Device{
		AppID: appID, DriverID: driverID, Name: name, Class: driver.Class,
		Capabilities: slices.Clone(driver.Capabilities), Settings: driverSettingDefaults(driver),
	}
	if value, ok := candidate["class"].(string); ok && value != "" {
		device.Class = value
	}
	for key, target := range map[string]*map[string]any{
		"data": &device.Data, "settings": &device.Settings, "store": &device.Store,
	} {
		value, exists := candidate[key]
		if !exists {
			continue
		}
		var decoded map[string]any
		if err := remarshal(value, &decoded); err != nil {
			return store.Device{}, fmt.Errorf("decode paired device %s: %w", key, err)
		}
		if key == "settings" {
			for setting, settingValue := range decoded {
				device.Settings[setting] = settingValue
			}
			continue
		}
		*target = decoded
	}
	if value, exists := candidate["capabilities"]; exists {
		var capabilities []string
		if err := remarshal(value, &capabilities); err != nil {
			return store.Device{}, fmt.Errorf("decode paired device capabilities: %w", err)
		}
		device.Capabilities = capabilities
	}
	return device, nil
}
