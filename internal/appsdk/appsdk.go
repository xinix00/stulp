// Package appsdk is de Go-kant van een Stulp-app.
//
// Twee soorten programma gebruiken hem. Een binary die appbuild uit JavaScript
// heeft gemaakt gebruikt State en Host: de lokale kopie plus de schrijfacties,
// met de JS-SDK erbovenop. Een met de hand geschreven Go-plugin gebruikt Run en
// raakt JavaScript nergens aan -- dit pakket importeert geen runtime, geen
// emitter en geen shims.
//
// De vorm volgt docs/app-processes.md. Lezen gaat naar een lokale kopie die
// Stulp bij de handshake vult en met events bijwerkt; schrijven is een request.
// Dat is wat een plugin toestaat om gewoon Go te zijn: een getName() is een
// mapopzoeking en geen round-trip.
//
// # Wat een plugin schrijft
//
//	func main() {
//		appsdk.Run(appsdk.Plugin{
//			Drivers: map[string]appsdk.Driver{"lamp": lampDriver{}},
//		})
//	}
//
// Meer is er niet: geen event loop om rekening mee te houden, geen microtasks,
// geen await. Een plugin die elke minuut wil pollen gebruikt een time.Ticker in
// zijn eigen goroutine, en zet zijn waarden weg met SetCapabilityValue.
//
// # Wanneer wat draait
//
// De lifecycle-callbacks (OnInit, OnCapability, flow-listeners, pairing) draaien
// op één worker en dus nooit tegelijk. Alles wat een plugin daarbuiten doet mag
// wél parallel: elke methode op Stulp en Device is veilig vanaf elke goroutine.
// Dat is de enige regel, en hij staat hier omdat het de eerste vraag is die
// iemand stelt die een socket openzet.
package appsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
)

// Plugin is wat een Go-app aanlevert.
type Plugin struct {
	// OnInit draait één keer, als Stulp de app start en voor het eerste device.
	// Zolang hij loopt geldt de app als startend; wat hier lang duurt houdt de
	// start op, dus zet een verbinding op in een eigen goroutine.
	OnInit func(*Stulp) error
	// Drivers koppelt een driver-id uit app.json aan zijn implementatie. Een
	// driver die niet in het manifest staat wordt nooit gevraagd.
	Drivers map[string]Driver
	// OnStop draait als Stulp de verbinding sluit. Ruim hier álles op wat
	// OnInit en de apparaten aan goroutines startten — ook de pollers per
	// apparaat. Op een host stopt het proces daarna toch, maar in een bundel
	// en bij elke her-aanmelding leeft het proces door: wat blijft draaien
	// wordt een zombie die tegen een dode sessie aan schrijft, naast de verse
	// generatie van de volgende aanmelding.
	OnStop func()

	// UI bevat de instellingenpagina en eventuele eigen koppelpagina's van de
	// app. Op een host leest Stulp die uit de app-map. Een slot-image deelt geen
	// bestandssysteem met Stulp, dus daar bakt de startcode ze met go:embed in en
	// levert deze FS ze alleen op verzoek over dezelfde geïsoleerde verbinding.
	UI fs.FS
}

// Driver maakt de handler voor één gekoppeld apparaat.
type Driver interface {
	NewDevice(*Device) (DeviceHandler, error)
}

// DeviceHandler is het leven van één apparaat.
type DeviceHandler interface {
	// OnInit is waar een plugin het apparaat gaat benaderen. Wat hier lang
	// duurt houdt de andere apparaten op, dus zet een verbinding op in een
	// eigen goroutine en meld je waarden later.
	OnInit() error
}

// De rest is optioneel en gaat via een type-assertie, zodat een driver die er
// niets mee doet er ook niets van hoeft te weten.

// Deleter hoort bij een apparaat dat uit Stulp verwijderd wordt.
type Deleter interface{ OnDeleted() }

// SettingsChanger hoort bij een apparaat waarvan de gebruiker instellingen
// aanpast. changed bevat alleen de sleutels die veranderd zijn.
type SettingsChanger interface {
	OnSettings(changed map[string]any) error
}

// CapabilityHandler ontvangt een opdracht: iemand zet een capability, vanuit de
// interface of vanuit een Flow.
type CapabilityHandler interface {
	OnCapability(name string, value any) error
}

// CapabilitiesHandler hoort bij een apparaat dat meer capabilities in één
// opdracht aankan: lamp aan én op die helderheid én in die kleur, in zo weinig
// berichten als het apparaat toelaat. Een scene levert zijn waarden zo aan.
// Het antwoord zegt per capability wat mislukte; wat er niet in staat is
// gelukt. Zonder deze interface krijgt OnCapability ze één voor één, in de
// volgorde die Stulp koos: aan vóór de rest, uit erna.
type CapabilitiesHandler interface {
	OnCapabilities(values map[string]any) map[string]error
}

// Pairer levert de apparaten die nu toegevoegd kunnen worden. Dat is de
// list_devices-stap van het koppelen.
type Pairer interface {
	ListDevices() ([]PairedDevice, error)
}

// PairPages hoort bij een driver met een eigen koppelpagina: een formulier voor
// een adres, een knop om te bevestigen. Die pagina draait in de browser en kan
// niet bij het apparaat; de plugin wel, en dit is de weg ertussen.
//
// De handlers zijn per sessie: twee mensen die tegelijk koppelen zitten elkaar
// niet in de weg.
type PairPages interface {
	Pair() map[string]PairHandler
}

// PairHandler beantwoordt wat de koppelpagina stuurt.
type PairHandler func(data any) (any, error)

// listDevices is de naam waaronder het gelijknamige sjabloon om zijn lijst
// vraagt. Een driver mag hem zelf beantwoorden; doet hij dat niet, dan
// beantwoordt ListDevices hem.
const listDevices = "list_devices"

// PairedDevice is één vondst uit het koppelen.
type PairedDevice struct {
	// Name is wat de gebruiker te zien krijgt.
	Name string `json:"name"`
	// Data is de identiteit van het apparaat en ligt daarna vast. Stulp
	// gebruikt hem om een apparaat te herkennen; hij hoort dus geen adres of
	// iets anders veranderlijks te bevatten.
	Data     map[string]any `json:"data"`
	Settings map[string]any `json:"settings,omitempty"`
	Store    map[string]any `json:"store,omitempty"`
	// Class is de apparaatsoort als die afwijkt van de driver-default. Een
	// driver die één soort koppelt heeft hem niet nodig; een driver die van
	// alles koppelt (matter) weet per apparaat beter dan zijn manifest. Zonder
	// dit veld werd élk matter-apparaat de driver-default "other" — de
	// controller rekende "socket" keurig uit en de koppelstroom liet hem vallen.
	Class        string   `json:"class,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Run start de plugin en keert pas terug als Stulp de verbinding sluit.
func Run(plugin Plugin) {
	if err := run(plugin); err != nil {
		fmt.Fprintln(os.Stderr, "app:", err)
		os.Exit(1)
	}
}

func run(plugin Plugin) error {
	// Twee manieren om bij Stulp te komen, en de plugin merkt van geen van beide
	// iets: main blijft appsdk.Run(plugin).
	//
	// STULP_ATTACH betekent dat dit proces al draaide -- een container, een
	// systemd-unit, een debugger -- en zich moet melden. Staat hij er niet, dan is
	// dit proces door Stulp gestart en ligt de verbinding al klaar op fd 3: geen
	// pad, geen rechten, niets om op te ruimen, en geen manier om bij het kanaal
	// van een andere app te komen.
	if config := AttachConfigFromEnv(); config.Target != "" {
		return Attach(config, plugin)
	}
	conn, err := net.FileConn(os.NewFile(3, "stulp"))
	if err != nil {
		return fmt.Errorf("no control connection on fd 3: %w", err)
	}
	return Serve(conn, plugin)
}

// Serve draait een plugin over een al opgezette verbinding. Run gebruikt hem
// met fd 3; een test kan er een net.Pipe in stoppen en zo de hele plugin
// aansturen zonder een proces te starten.
func Serve(conn net.Conn, plugin Plugin) error {
	return serve(appproto.NewConn(conn), plugin)
}

// serve is Serve op een verbinding die al ingepakt is.
//
// Attach heeft die nodig: daar is de begroeting er al af gelezen, en die bytes
// staan gebufferd in precies dit omhulsel. Er een tweede om heen zou ze
// kwijtraken.
func serve(conn *appproto.Conn, plugin Plugin) error {
	return serveWithHeartbeat(conn, plugin, sessionHeartbeatInterval, sessionHeartbeatTimeout)
}

func serveWithHeartbeat(conn *appproto.Conn, plugin Plugin, heartbeatInterval, heartbeatTimeout time.Duration) error {
	app := &process{plugin: plugin, state: NewState(), devices: map[string]*Device{},
		sessions: map[string]map[string]PairHandler{}, ready: make(chan struct{})}
	app.session = appproto.NewSession(conn, app.handle, app.event)
	// Een configuratie- of koppelpagina leest zijn bestanden uit het ingebedde
	// bestandssysteem van deze app: geen app-toestand, geen netwerk. Zonder deze
	// zijbaan staan die leesacties achter de handler die er nu is, en een app die
	// een trage peer bevraagt laat zijn eigen pagina's dan minuten leeg.
	app.session.AnswerBesideQueue("ui.asset")
	app.host = NewHost(app.session, app.state)
	app.stulp = &Stulp{host: app.host, cards: map[string]*flowCard{}}

	done := make(chan error, 1)
	go func() { done <- app.session.Serve() }()

	if err := app.host.Handshake(context.Background()); err != nil {
		// A failed greeting is not a session that may linger next to the next
		// Attach attempt. Tear the whole stream down before returning to the
		// reconnect loop.
		_ = app.session.Close()
		<-done
		return fmt.Errorf("handshake failed: %w", err)
	}
	close(app.ready)

	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	ticker := time.NewTicker(heartbeatInterval)
	heartbeatFailed := make(chan error, 1)
	go func() {
		defer ticker.Stop()
		if err := monitorSession(heartbeatCtx, app.session, ticker.C, heartbeatTimeout); err != nil {
			heartbeatFailed <- err
		}
	}()

	var err error
	select {
	case err = <-done:
	case heartbeatErr := <-heartbeatFailed:
		// Closing our end is the deliberate bridge from liveness detection to
		// the existing lifecycle: Serve returns, and an attached HopOS app goes
		// around its Attach loop and performs a completely fresh handshake.
		_ = app.session.Close()
		<-done
		err = fmt.Errorf("stulp stopped answering heartbeats: %w", heartbeatErr)
	}
	stopHeartbeat()
	if plugin.OnStop != nil {
		plugin.OnStop()
	}
	// EOF is een nette stop door de supervisor. Een kapot frame of een andere
	// kanaalfout is een crash en hoort een niet-nul status te krijgen.
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("control connection failed: %w", err)
}

// process is de plugin plus zijn verbinding met Stulp.
type process struct {
	plugin  Plugin
	session *appproto.Session
	state   *State
	host    *Host
	stulp   *Stulp

	// ready gaat dicht als de handshake verwerkt is. Stulp beschouwt hem als
	// klaar zodra hij het antwoord verstuurt, wij pas als we het verwerkt
	// hebben -- en daartussen kan zijn eerste verzoek al binnen zijn. Zonder
	// deze poort draait OnInit dan op een lege kopie.
	ready chan struct{}

	mu       sync.Mutex
	devices  map[string]*Device
	sessions map[string]map[string]PairHandler
}

// startPair opent een koppelsessie en meldt welke berichten de pagina mag sturen.
func (p *process) startPair(driverID, sessionID string) ([]string, error) {
	driver, ok := p.plugin.Drivers[driverID]
	if !ok {
		return nil, fmt.Errorf("no driver %q", driverID)
	}
	handlers := map[string]PairHandler{}
	if pages, ok := driver.(PairPages); ok {
		for name, handle := range pages.Pair() {
			// De naam van een koppelbericht wordt één padsegment in de
			// emit-URL (/api/stulp/pair/{id}/emit/{event}), dus een '/' erin
			// overleeft de reis niet: de browser codeert hem als %2F en
			// leanhttp weigert die dubbelzinnigheid met een 400 -- een mysterie
			// dat pas bij de gebruiker opduikt (gemeten 20-08, toen een handler
			// "commission/state" heette naar het voorbeeld van de settings-API,
			// waar paden wél expliciet in app.json staan). Hier weigeren is het
			// enige eerlijke moment: bij het openen van de sessie, met de naam
			// erbij.
			if strings.ContainsAny(name, "/?#%") || name == "" {
				return nil, fmt.Errorf("driver %q has an unusable pair event name %q: it becomes one URL path segment, so /, ?, # and %% cannot occur in it",
					driverID, name)
			}
			handlers[name] = handle
		}
	}

	// Een driver met ListDevices beantwoordt daarmee het list_devices-sjabloon.
	// Zonder dit zou zo'n driver een sessie zonder handlers opleveren, en dan
	// loopt de pagina die het sjabloon toont vast op zijn eerste bericht -- wat
	// hij vraagt bestaat immers wel, alleen niet onder die naam.
	if pairer, ok := driver.(Pairer); ok {
		if _, own := handlers[listDevices]; !own {
			handlers[listDevices] = func(any) (any, error) { return pairer.ListDevices() }
		}
	}

	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)

	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = map[string]map[string]PairHandler{}
	}
	p.sessions[sessionID] = handlers
	p.mu.Unlock()
	return names, nil
}

// handle draait op de geordende worker van appproto: verzoeken komen daar één
// voor één binnen, dus twee lifecycle-callbacks lopen nooit door elkaar.
func (p *process) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	switch method {
	case "app.init":
		// OnInit hoort hier en niet vlak na de handshake: Stulp bepaalt wanneer
		// een app start, en zolang OnInit loopt is de app nog niet klaar. Dit
		// antwoord komt dus pas als hij het is -- anders zou Stulp hem als
		// draaiend tonen terwijl hij nog verbinding zoekt.
		if p.plugin.OnInit == nil {
			return nil, nil
		}
		return nil, p.plugin.OnInit(p.stulp)

	case "driver.init":
		var p2 struct {
			DriverID string `json:"driverId"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		if _, ok := p.plugin.Drivers[p2.DriverID]; !ok {
			return nil, fmt.Errorf("no driver %q", p2.DriverID)
		}
		return nil, nil

	case "device.init":
		var p2 struct {
			DeviceID string `json:"deviceId"`
			DriverID string `json:"driverId"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		return nil, p.initDevice(p2.DeviceID, p2.DriverID)

	case "device.delete":
		// Stulp gaat dit apparaat verwijderen. De app krijgt het eerst te horen,
		// zodat hij kan opruimen wat buiten Stulp ligt -- een Matter-node hoort
		// deze fabric te vergeten voordat hij hier verdwijnt.
		var p2 struct {
			DeviceID string `json:"deviceId"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		device, ok := p.device(p2.DeviceID)
		if !ok {
			return nil, nil
		}
		if deleter, handles := device.handler.(Deleter); handles {
			deleter.OnDeleted()
		}
		p.mu.Lock()
		delete(p.devices, p2.DeviceID)
		p.mu.Unlock()
		return nil, nil

	case "capability.invoke":
		var p2 struct {
			DeviceID   string `json:"deviceId"`
			Capability string `json:"capability"`
			Value      any    `json:"value"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		device, ok := p.device(p2.DeviceID)
		if !ok {
			return nil, fmt.Errorf("device %s is not running", p2.DeviceID)
		}
		handler, ok := device.handler.(CapabilityHandler)
		if !ok {
			return nil, fmt.Errorf("device %s takes no capability commands", p2.DeviceID)
		}
		return nil, handler.OnCapability(p2.Capability, p2.Value)

	case "capabilities.invoke":
		var p2 struct {
			DeviceID string `json:"deviceId"`
			Commands []struct {
				Capability string `json:"capability"`
				Value      any    `json:"value"`
			} `json:"commands"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		device, ok := p.device(p2.DeviceID)
		if !ok {
			return nil, fmt.Errorf("device %s is not running", p2.DeviceID)
		}
		failed := make(map[string]string)
		if batch, combines := device.handler.(CapabilitiesHandler); combines {
			values := make(map[string]any, len(p2.Commands))
			for _, command := range p2.Commands {
				values[command.Capability] = command.Value
			}
			for capability, err := range batch.OnCapabilities(values) {
				if err != nil {
					failed[capability] = err.Error()
				}
			}
			return failed, nil
		}
		handler, ok := device.handler.(CapabilityHandler)
		if !ok {
			return nil, fmt.Errorf("device %s takes no capability commands", p2.DeviceID)
		}
		for _, command := range p2.Commands {
			if err := handler.OnCapability(command.Capability, command.Value); err != nil {
				failed[command.Capability] = err.Error()
			}
		}
		return failed, nil

	case "flow.run":
		var p2 struct {
			Kind  string         `json:"kind"`
			ID    string         `json:"id"`
			Args  map[string]any `json:"args"`
			State map[string]any `json:"state"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		return p.stulp.runCard(p2.Kind, p2.ID, p2.Args, p2.State)

	case "api.invoke":
		var p2 struct {
			Handler string         `json:"handler"`
			Query   map[string]any `json:"query"`
			Body    map[string]any `json:"body"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		return p.stulp.runRequest(p2.Handler, p2.Query, p2.Body)

	case "ui.asset":
		var p2 struct {
			Path string `json:"path"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		return p.uiAsset(p2.Path)

	case "flow.autocomplete":
		var p2 struct {
			Kind     string         `json:"kind"`
			ID       string         `json:"id"`
			Argument string         `json:"argument"`
			Query    string         `json:"query"`
			Args     map[string]any `json:"args"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		return p.stulp.runAutocomplete(p2.Kind, p2.ID, p2.Argument, p2.Query, p2.Args)

	case "pair.list":
		var p2 struct {
			DriverID string `json:"driverId"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		driver, ok := p.plugin.Drivers[p2.DriverID]
		if !ok {
			return nil, fmt.Errorf("no driver %q", p2.DriverID)
		}
		pairer, ok := driver.(Pairer)
		if !ok {
			return []PairedDevice{}, nil
		}
		return pairer.ListDevices()

	case "pair.start":
		var p2 struct {
			DriverID  string `json:"driverId"`
			SessionID string `json:"sessionId"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		return p.startPair(p2.DriverID, p2.SessionID)

	case "pair.emit":
		var p2 struct {
			SessionID string `json:"sessionId"`
			Event     string `json:"event"`
			Data      any    `json:"data"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		p.mu.Lock()
		handlers, ok := p.sessions[p2.SessionID]
		p.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("pairing session %s is not open", p2.SessionID)
		}
		handle, ok := handlers[p2.Event]
		if !ok {
			return nil, fmt.Errorf("pairing has no handler for %q", p2.Event)
		}
		return handle(p2.Data)

	case "pair.close":
		var p2 struct {
			SessionID string `json:"sessionId"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		p.mu.Lock()
		handlers := p.sessions[p2.SessionID]
		delete(p.sessions, p2.SessionID)
		p.mu.Unlock()
		// Een pair-pagina kan minutenwerk hebben gestart. Sluiten betekent ook
		// dat die sessie het mag annuleren; anders verdwijnt alleen de handler
		// terwijl de netwerkjob op de achtergrond doorloopt. De handler is
		// best-effort: opruimen mag het idempotente sluiten niet laten falen.
		if cancel := handlers["cancel"]; cancel != nil {
			_, _ = cancel(nil)
		}
		return nil, nil

	case "registrations":
		return p.registrations(), nil

	case "video.resolve":
		var p2 struct {
			DeviceID string `json:"deviceId"`
			Slot     string `json:"slot"`
			Kind     string `json:"kind"`
		}
		if err := decode(method, params, &p2); err != nil {
			return nil, err
		}
		device, ok := p.device(p2.DeviceID)
		if !ok {
			return nil, fmt.Errorf("device %s is not running", p2.DeviceID)
		}
		source, known, err := device.resolveMedia(p2.Slot, p2.Kind)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, fmt.Errorf("device %s has no %s in slot %q", p2.DeviceID, mediaKindName(p2.Kind), p2.Slot)
		}
		return source, nil
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

func (p *process) initDevice(deviceID, driverID string) error {
	driver, ok := p.plugin.Drivers[driverID]
	if !ok {
		return fmt.Errorf("no driver %q", driverID)
	}
	device := &Device{id: deviceID, driverID: driverID, host: p.host, stulp: p.stulp,
		media: map[string]mediaSlot{}}
	handler, err := driver.NewDevice(device)
	if err != nil {
		return err
	}
	device.handler = handler

	p.mu.Lock()
	p.devices[deviceID] = device
	p.mu.Unlock()

	return handler.OnInit()
}

func (p *process) device(id string) (*Device, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	device, ok := p.devices[id]
	return device, ok
}

// registrations vertelt Stulp wat deze app aankan, zodat de interface geen
// kaarten aanbiedt waar niets achter zit.
func (p *process) registrations() registration {
	out := registration{Drivers: []string{}, Devices: []string{}, Flows: []flowRegistration{}}
	for id := range p.plugin.Drivers {
		out.Drivers = append(out.Drivers, id)
	}
	sort.Strings(out.Drivers)

	p.mu.Lock()
	for id := range p.devices {
		out.Devices = append(out.Devices, id)
	}
	p.mu.Unlock()
	sort.Strings(out.Devices)

	out.Flows = p.stulp.registrations()
	return out
}

// event handelt eenrichtingsberichten af. Stulp duwt statewijzigingen zo door,
// en dat is wat de lokale kopie bij houdt zonder dat de app ooit vraagt.
func (p *process) event(method string, params json.RawMessage) {
	if method == "state.settings" {
		previous := map[string]any{}
		for _, key := range p.state.SettingKeys() {
			value, _ := p.state.Setting(key)
			previous[key] = value
		}
		p.state.Apply(method, params)
		current := map[string]any{}
		for _, key := range p.state.SettingKeys() {
			value, _ := p.state.Setting(key)
			current[key] = value
		}
		if changed := changedKeys(previous, current); len(changed) > 0 {
			p.stulp.settingsChanged(changed)
		}
		return
	}

	before := map[string]map[string]any{}
	if method == "state.device" {
		var probe struct {
			DeviceID string `json:"deviceId"`
		}
		if json.Unmarshal(params, &probe) == nil {
			if settings, ok := p.state.DeviceMap(probe.DeviceID, "settings"); ok {
				before[probe.DeviceID] = settings
			}
		}
	}

	p.state.Apply(method, params)

	// Na het bijwerken kijken of er instellingen veranderd zijn; de plugin krijgt
	// alleen de sleutels die echt anders zijn.
	if method != "state.device" {
		return
	}
	for deviceID, old := range before {
		device, running := p.device(deviceID)
		if !running {
			continue
		}
		changer, ok := device.handler.(SettingsChanger)
		if !ok {
			continue
		}
		current, _ := p.state.DeviceMap(deviceID, "settings")
		if changed := changedKeys(old, current); len(changed) > 0 {
			if err := changer.OnSettings(changed); err != nil {
				device.Error("onSettings: " + err.Error())
			}
		}
	}
}

// changedKeys levert de sleutels waarvan de waarde anders is. De vergelijking
// gaat via JSON: dat is precies de vorm waarin de waarden binnenkomen, en het
// vermijdt een reflect-diep-gelijk die op maps van any toch niet klopt.
func changedKeys(old, current map[string]any) map[string]any {
	changed := map[string]any{}
	for key, value := range current {
		if !sameJSON(old[key], value) {
			changed[key] = value
		}
	}
	for key := range old {
		if _, still := current[key]; !still {
			changed[key] = nil
		}
	}
	return changed
}

func sameJSON(left, right any) bool {
	a, err1 := json.Marshal(left)
	b, err2 := json.Marshal(right)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

func decode(method string, params json.RawMessage, target any) error {
	if err := json.Unmarshal(params, target); err != nil {
		return fmt.Errorf("decode %s params: %w", method, err)
	}
	return nil
}

type registration struct {
	Drivers []string           `json:"drivers"`
	Devices []string           `json:"devices"`
	Flows   []flowRegistration `json:"flows"`
}

type flowRegistration struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	RunListener  bool     `json:"runListener"`
	Autocomplete []string `json:"autocomplete"`
}
