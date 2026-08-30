package appsdk_test

// De SDK wordt getest zoals Stulp hem gebruikt: over een echte appproto-sessie,
// met een nep-Stulp aan de andere kant. Een net.Pipe in plaats van een
// socketpair scheelt een proces en verandert niets aan wat er over de lijn gaat.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/appsdk"
)

// lamp is de kleinst mogelijke plugin: één driver, één apparaat, één capability.
type lamp struct{ device *appsdk.Device }

type lampDriver struct{ made chan *lamp }

func (d lampDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	lamp := &lamp{device: device}
	if d.made != nil {
		d.made <- lamp
	}
	return lamp, nil
}

func (d lampDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return []appsdk.PairedDevice{{
		Name:         "Keukenlamp",
		Data:         map[string]any{"id": "abc"},
		Capabilities: []string{"onoff"},
	}}, nil
}

func (l *lamp) OnInit() error { return l.device.SetCapabilityValue("onoff", false) }

func (l *lamp) OnCapability(name string, value any) error {
	// Een echte lamp zou hier het apparaat aansturen en daarna bevestigen.
	return l.device.SetCapabilityValue(name, value)
}

func TestPluginAnswersStulp(t *testing.T) {
	stulp := newFakeStulp(t, appsdk.Plugin{
		OnInit: func(h *appsdk.Stulp) error {
			h.OnFlowAction("ping", func(args, state map[string]any) (any, error) {
				return args["wat"], h.SetSetting("laatste", args["wat"])
			})
			h.OnFlowCondition("is_aan", func(args, state map[string]any) (bool, error) {
				value, _ := h.Setting("aan")
				on, _ := value.(bool)
				return on, nil
			})
			return nil
		},
		Drivers: map[string]appsdk.Driver{"switch": lampDriver{}},
	})
	defer stulp.close()

	// Stulp start de app; pas daarna staan zijn Flow-kaarten er.
	stulp.call(t, "app.init", map[string]any{}, nil)

	// Koppelen: wat kan er toegevoegd worden?
	var found []appsdk.PairedDevice
	stulp.call(t, "pair.list", map[string]any{"driverId": "switch"}, &found)
	if len(found) != 1 || found[0].Name != "Keukenlamp" {
		t.Fatalf("pair.list gaf %+v", found)
	}

	// Een apparaat starten. OnInit zet de capability, dus dat is meteen een
	// schrijfactie terug naar Stulp.
	stulp.call(t, "device.init", map[string]any{"deviceId": "dev-1", "driverId": "switch"}, nil)
	if got := stulp.deviceState("dev-1", "onoff"); got != false {
		t.Errorf("na OnInit is onoff %v, verwacht false", got)
	}

	// Een opdracht vanuit de interface of een Flow.
	stulp.call(t, "capability.invoke", map[string]any{
		"deviceId": "dev-1", "capability": "onoff", "value": true,
	}, nil)
	if got := stulp.deviceState("dev-1", "onoff"); got != true {
		t.Errorf("na capability.invoke is onoff %v, verwacht true", got)
	}

	// Een DAN-kaart.
	stulp.call(t, "flow.run", map[string]any{
		"kind": "action", "id": "ping", "args": map[string]any{"wat": "pong"},
	}, nil)
	if got := stulp.setting("laatste"); got != "pong" {
		t.Errorf("flow-actie zette %v, verwacht pong", got)
	}

	// Een EN-kaart levert een waarde op.
	stulp.set(t, "state.settings", map[string]any{"aan": true})
	var answer bool
	stulp.call(t, "flow.run", map[string]any{"kind": "condition", "id": "is_aan"}, &answer)
	if !answer {
		t.Error("de conditie zag de instelling niet")
	}

	// En Stulp moet kunnen opvragen wat deze app aankan.
	var registered struct {
		Drivers []string `json:"drivers"`
		Devices []string `json:"devices"`
		Flows   []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"flows"`
	}
	stulp.call(t, "registrations", nil, &registered)
	if len(registered.Drivers) != 1 || registered.Drivers[0] != "switch" {
		t.Errorf("drivers = %v", registered.Drivers)
	}
	if len(registered.Devices) != 1 || registered.Devices[0] != "dev-1" {
		t.Errorf("devices = %v", registered.Devices)
	}
	if len(registered.Flows) != 2 {
		t.Errorf("flows = %+v", registered.Flows)
	}
}

func TestPluginKeepsItsOwnStateAcrossTheHandshake(t *testing.T) {
	// Eigen state is waar sleutelmateriaal thuishoort: een Matter-fabric, een
	// token, een sessie. Hij gaat naar hetzelfde document als de rest, dus hij
	// zit in een backup -- en hij komt bij de volgende start weer terug.
	saved := make(chan json.RawMessage, 1)
	stulp := newFakeStulp(t, appsdk.Plugin{
		OnInit: func(h *appsdk.Stulp) error {
			if before := h.State(); before != nil {
				return fmt.Errorf("verse plugin had al state: %s", before)
			}
			if err := h.SetState(json.RawMessage(`{"fabric":"geheim"}`)); err != nil {
				return err
			}
			// Meteen terug te lezen: een plugin die zijn eigen schrijfactie niet
			// terugziet zou hem twee keer doen.
			saved <- h.State()
			return nil
		},
	})
	defer stulp.close()

	stulp.call(t, "app.init", map[string]any{}, nil)
	select {
	case got := <-saved:
		if string(got) != `{"fabric":"geheim"}` {
			t.Errorf("teruggelezen state = %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("de plugin heeft zijn state niet teruggelezen")
	}
	if got := stulp.appState(); string(got) != `{"fabric":"geheim"}` {
		t.Errorf("Stulp bewaarde %s", got)
	}
}

func TestUnknownCapabilityIsAnErrorAndNotSilence(t *testing.T) {
	// Een tikfout in een capability-naam hoort meteen op te vallen, niet pas als
	// iemand zich afvraagt waarom de tegel leeg blijft.
	stulp := newFakeStulp(t, appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"switch": lampDriver{}},
	})
	defer stulp.close()

	stulp.call(t, "device.init", map[string]any{"deviceId": "dev-1", "driverId": "switch"}, nil)

	_, err := stulp.session.Call(context.Background(), "capability.invoke", map[string]any{
		"deviceId": "dev-1", "capability": "dim", "value": 0.5,
	})
	if err == nil {
		t.Fatal("een onbekende capability werd stil geaccepteerd")
	}
}

func TestCapabilityReportCommitsAsOneDeviceMerge(t *testing.T) {
	made := make(chan *lamp, 1)
	stulp := newFakeStulpWithCapabilities(t, appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"switch": lampDriver{made: made}},
	}, "onoff", "dim")
	defer stulp.close()

	stulp.call(t, "device.init", map[string]any{"deviceId": "dev-1", "driverId": "switch"}, nil)
	device := (<-made).device
	before := stulp.mergeCount()
	if err := device.SetCapabilityValues(map[string]any{"onoff": true, "dim": 0.42}); err != nil {
		t.Fatal(err)
	}
	if got := stulp.mergeCount() - before; got != 1 {
		t.Fatalf("one capability report used %d device.merge requests", got)
	}
	if got := stulp.deviceState("dev-1", "onoff"); got != true {
		t.Errorf("onoff = %v", got)
	}
	if got := stulp.deviceState("dev-1", "dim"); got != 0.42 {
		t.Errorf("dim = %v", got)
	}

	before = stulp.mergeCount()
	err := device.SetCapabilityValues(map[string]any{"onoff": false, "unknown": true})
	if err == nil {
		t.Fatal("a report containing an unknown capability was accepted")
	}
	if got := stulp.mergeCount() - before; got != 0 {
		t.Fatalf("an invalid report used %d device.merge requests", got)
	}
	if got := stulp.deviceState("dev-1", "onoff"); got != true {
		t.Errorf("invalid report partially changed onoff to %v", got)
	}
}

func TestSystemFlowTriggerIsMarkedForStulp(t *testing.T) {
	stulp := newFakeStulp(t, appsdk.Plugin{
		OnInit: func(h *appsdk.Stulp) error {
			return h.TriggerSystemFlow("trigger", "capability.button.on",
				map[string]any{"value": true}, map[string]any{"deviceId": "dev-1"})
		},
	})
	defer stulp.close()

	stulp.call(t, "app.init", map[string]any{}, nil)
	flows := stulp.flowTriggers()
	if len(flows) != 1 || !flows[0].System || flows[0].Kind != "trigger" || flows[0].ID != "capability.button.on" {
		t.Fatalf("system flow trigger = %#v", flows)
	}
}

// ---------------------------------------------------------------------------

type fakeFlowTrigger struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Tokens any    `json:"tokens"`
	State  any    `json:"state"`
	System bool   `json:"system"`
}

// fakeStulp is de kant van Stulp: hij beantwoordt de handshake en de
// schrijfacties, en houdt bij wat de plugin gezet heeft.
type fakeStulp struct {
	session *appproto.Session
	conn    net.Conn

	mu       sync.Mutex
	greeted  bool
	settings map[string]any
	state    json.RawMessage
	devices  map[string]map[string]any
	merges   int
	flows    []fakeFlowTrigger
	done     chan struct{}
}

func newFakeStulp(t *testing.T, plugin appsdk.Plugin) *fakeStulp {
	return newFakeStulpWithCapabilities(t, plugin, "onoff")
}

func newFakeStulpWithCapabilities(t *testing.T, plugin appsdk.Plugin, capabilities ...string) *fakeStulp {
	t.Helper()
	ours, theirs := net.Pipe()
	capabilityValues := make([]any, len(capabilities))
	for i, capability := range capabilities {
		capabilityValues[i] = capability
	}

	s := &fakeStulp{
		conn:     ours,
		settings: map[string]any{},
		devices: map[string]map[string]any{
			"dev-1": {
				"name": "Keukenlamp", "class": "light", "available": true,
				"data": map[string]any{"id": "abc"}, "settings": map[string]any{},
				"store": map[string]any{}, "state": map[string]any{},
				"capabilities": capabilityValues,
			},
		},
		done: make(chan struct{}),
	}
	s.session = appproto.NewSession(appproto.NewConn(ours), s.handle, func(string, json.RawMessage) {})
	go s.session.Serve()

	go func() {
		defer close(s.done)
		if err := appsdk.Serve(theirs, plugin); err != nil {
			t.Errorf("plugin: %v", err)
		}
	}()

	// Wachten tot de handshake rond is: pas daarna kent de plugin zijn devices.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		greeted := s.greeted
		s.mu.Unlock()
		if greeted {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("de plugin heeft geen hello gestuurd")
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *fakeStulp) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case "hello":
		s.greeted = true
		return appsdk.Welcome{
			Protocol: appsdk.ProtocolVersion, AppID: "com.demo",
			StulpID: "stulp-1", StulpVersion: "12.0.0",
			Language: "nl", Timezone: "Europe/Amsterdam",
			Manifest: map[string]any{"id": "com.demo"},
			Settings: s.settings, Devices: s.devices, AppState: s.state,
		}, nil
	case "device.merge":
		var p struct {
			DeviceID string         `json:"deviceId"`
			Field    string         `json:"field"`
			Patch    map[string]any `json:"patch"`
		}
		json.Unmarshal(params, &p)
		s.merges++
		device := s.devices[p.DeviceID]
		target, _ := device[p.Field].(map[string]any)
		for key, value := range p.Patch {
			target[key] = value
		}
		// Stulp duwt de nieuwe stand door vóór hij antwoordt; dat is wat
		// read-your-own-writes waarmaakt.
		s.session.Notify("state.device", map[string]any{"deviceId": p.DeviceID, "device": device})
		return nil, nil
	case "setting.set":
		var p struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		}
		json.Unmarshal(params, &p)
		s.settings[p.Key] = p.Value
		s.session.Notify("state.settings", s.settings)
		return nil, nil
	case "state.set":
		var p struct {
			State json.RawMessage `json:"state"`
		}
		json.Unmarshal(params, &p)
		s.state = p.State
		return nil, nil
	case "flow.trigger":
		var p fakeFlowTrigger
		json.Unmarshal(params, &p)
		s.flows = append(s.flows, p)
		return nil, nil
	}
	return nil, nil
}

func (s *fakeStulp) call(t *testing.T, method string, params any, out any) {
	t.Helper()
	raw, err := s.session.Call(context.Background(), method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s antwoord: %v", method, err)
		}
	}
}

func (s *fakeStulp) set(t *testing.T, event string, params any) {
	t.Helper()
	if err := s.session.Notify(event, params); err != nil {
		t.Fatal(err)
	}
	// Een event is eenrichtingsverkeer; een lege call erachteraan wacht tot de
	// plugin hem verwerkt heeft.
	s.call(t, "registrations", nil, nil)
}

func (s *fakeStulp) deviceState(deviceID, capability string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, _ := s.devices[deviceID]["state"].(map[string]any)
	return state[capability]
}

func (s *fakeStulp) mergeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.merges
}

func (s *fakeStulp) flowTriggers() []fakeFlowTrigger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeFlowTrigger(nil), s.flows...)
}

func (s *fakeStulp) appState() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *fakeStulp) setting(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings[key]
}

func (s *fakeStulp) close() {
	s.session.Close()
	s.conn.Close()
	<-s.done
}

// Een driver zonder eigen koppelpagina beantwoordt het list_devices-sjabloon met
// ListDevices.
//
// Zonder dat leverde pair.start zo'n driver een sessie zonder handlers op --
// sterker nog, hij registreerde de sessie helemaal niet, en het eerste bericht
// van de pagina kwam terug als "pairing session ... is not open". Dat is een
// misleidende melding voor een driver die de lijst gewoon kan leveren, en het
// trof elke plugin die op de eenvoudige manier koppelt.
func TestASimpleDriverAnswersTheListDevicesTemplate(t *testing.T) {
	stulp := newFakeStulp(t, appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"switch": lampDriver{}},
	})
	defer stulp.close()
	stulp.call(t, "app.init", map[string]any{}, nil)

	var handlers []string
	stulp.call(t, "pair.start", map[string]any{"driverId": "switch", "sessionId": "s1"}, &handlers)
	if len(handlers) != 1 || handlers[0] != "list_devices" {
		t.Fatalf("pair.start meldde de berichten %v, wil [list_devices]", handlers)
	}

	var found []appsdk.PairedDevice
	stulp.call(t, "pair.emit", map[string]any{
		"sessionId": "s1", "event": "list_devices",
	}, &found)
	if len(found) != 1 || found[0].Name != "Keukenlamp" {
		t.Fatalf("pair.emit gaf %+v", found)
	}
}

// Een eigen handler wint van de ingebouwde. Anders zou een driver die zijn lijst
// zelf samenstelt -- met een filter, of met wat de pagina meestuurde -- stilletjes
// omzeild worden.
func TestAnOwnListDevicesHandlerWins(t *testing.T) {
	stulp := newFakeStulp(t, appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"switch": pickyDriver{}},
	})
	defer stulp.close()
	stulp.call(t, "app.init", map[string]any{}, nil)

	var handlers []string
	stulp.call(t, "pair.start", map[string]any{"driverId": "switch", "sessionId": "s1"}, &handlers)

	var found []appsdk.PairedDevice
	stulp.call(t, "pair.emit", map[string]any{"sessionId": "s1", "event": "list_devices"}, &found)
	if len(found) != 1 || found[0].Name != "Alleen deze" {
		t.Fatalf("pair.emit gaf %+v, wil de lijst van de driver zelf", found)
	}
}

// Een pair.close ruimt niet alleen handlers op: een plugin kan er een lopende
// BLE-/netwerkjob mee afbreken. Dat is best-effort en close blijft idempotent.
func TestClosingPairInvokesItsCancelHandler(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	stulp := newFakeStulp(t, appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"switch": cancellingDriver{cancelled: cancelled}},
	})
	defer stulp.close()
	stulp.call(t, "app.init", map[string]any{}, nil)
	stulp.call(t, "pair.start", map[string]any{"driverId": "switch", "sessionId": "s1"}, nil)
	stulp.call(t, "pair.close", map[string]any{"sessionId": "s1"}, nil)

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("pair.close riep de cancel-handler niet aan")
	}
	// Nogmaals sluiten heeft niets meer om te annuleren en blijft geldig.
	stulp.call(t, "pair.close", map[string]any{"sessionId": "s1"}, nil)
}

// pickyDriver heeft allebei: ListDevices én een eigen list_devices-handler.
type pickyDriver struct{ lampDriver }

func (pickyDriver) Pair() map[string]appsdk.PairHandler {
	return map[string]appsdk.PairHandler{
		"list_devices": func(any) (any, error) {
			return []appsdk.PairedDevice{{Name: "Alleen deze", Data: map[string]any{"id": "x"}}}, nil
		},
	}
}

type cancellingDriver struct {
	lampDriver
	cancelled chan struct{}
}

func (d cancellingDriver) Pair() map[string]appsdk.PairHandler {
	return map[string]appsdk.PairHandler{
		"cancel": func(any) (any, error) {
			d.cancelled <- struct{}{}
			return nil, nil
		},
	}
}

// De koppelstroom vervoert kandidaten als JSON; de soort moet die reis
// overleven. Dit veld ontbrak en dan werd élk matter-apparaat "other".
func TestPairedDeviceCarriesItsClass(t *testing.T) {
	encoded, err := json.Marshal(appsdk.PairedDevice{Name: "Stekker", Class: "socket",
		Data: map[string]any{"id": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["class"] != "socket" {
		t.Fatalf("de soort reist niet mee: %s", encoded)
	}
}
