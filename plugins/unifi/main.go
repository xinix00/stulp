// De UniFi Protect-app.
//
// Eén console, en daaruit camera's, deurbellen, schijnwerpers, sensoren, gongs
// en relais. Alles loopt over de v2-integratie-API met een API-key; er is geen
// tweede weg ingebouwd. Wat er wel en niet geport is staat in PORTED.md, met
// verwijzingen naar waar het in de oorspronkelijke app stond.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// app is de plugin als geheel: één console en de apparaten die eruit komen.
type app struct {
	mu      sync.RWMutex
	client  *protect.Client
	stulp   *appsdk.Stulp
	cancel  context.CancelFunc
	devices map[string]handler // protect-id -> wie erop wacht
	lastErr string
	// retry is de ronde die klaarstaat nadat een stand niet op te halen was,
	// en retryWait de pauze ervoor. Zie refreshSoon.
	retry     *time.Timer
	retryWait time.Duration
}

// settleTime laat alle apparaten die net gekoppeld zijn in ÉÉN ronde meelopen.
// Een apparaat vraagt zijn stand dus niet op tijdens zijn eigen OnInit: bij het
// opstarten komen die allemaal tegelijk, en elke seconde die zo'n aanroep bij
// een trage console wacht is een seconde waarin deze app niets anders kan doen --
// ook geen configuratiepagina uitleveren.
//
// Lukt een ronde niet, dan verdubbelt de pauze tot maxRefreshRetry. Een console
// die echt weg is hoort met rust gelaten te worden; hij meldt zich zelf weer via
// de gebeurtenislijn, en die doet dan een verse ronde.
const (
	settleTime      = 2 * time.Second
	maxRefreshRetry = 5 * time.Minute
)

// handler is wat een apparaat met een bericht van de console doet. Elk driver
// heeft er zijn eigen versie van.
type handler interface {
	apply(protect.DeviceMessage)
}

var instance = &app{devices: map[string]handler{}}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit: instance.start,
		OnStop: instance.stop,
		Drivers: map[string]appsdk.Driver{
			"camera": cameraDriver{},
			"light":  lightDriver{},
			"sensor": sensorDriver{},
			"chime":  chimeDriver{},
			"relay":  relayDriver{},
		},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.mu.Lock()
	a.stulp = stulp
	a.mu.Unlock()

	a.registerAPI(stulp)
	a.registerFlow(stulp)
	stulp.OnSettingsChanged(func(map[string]any) { a.connect() })
	a.connect()
	return nil
}

func (a *app) stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	// Een ronde die nog klaarstaat hoort bij de verbinding die nu weggaat.
	if a.retry != nil {
		a.retry.Stop()
		a.retry = nil
	}
	a.retryWait = 0
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// connect zet de verbinding op met wat er in de instellingen staat.
//
// Dit blokkeert niet: de eerste aanroep gebeurt tijdens OnInit, en een console
// die niet antwoordt zou daarmee elk apparaat van deze app ophouden. Wat er
// misgaat komt terug op de configuratiepagina.
func (a *app) connect() {
	host := a.stulp.SettingText("host")
	token := a.stulp.SettingText("apiKey")
	port := a.stulp.SettingNumber("port", 443)

	a.stop()
	if host == "" || token == "" {
		a.setError("Vul het adres van de console en een API-key in.")
		return
	}
	client := protect.New(host, port, token)
	ctx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	a.client, a.cancel, a.lastErr = client, cancel, ""
	a.mu.Unlock()

	client.Listen(ctx, protect.Handlers{
		OnDevice:  a.route,
		OnEvent:   a.event,
		OnConnect: a.refreshAll,
		OnError: func(err error) {
			a.setError(err.Error())
			a.stulp.Error("UniFi Protect: " + err.Error())
		},
	})
}

// route brengt een apparaatbericht bij het apparaat dat erop wacht.
func (a *app) route(message protect.DeviceMessage) {
	_, id, ok := message.Identify()
	if !ok {
		return
	}
	a.mu.RLock()
	target := a.devices[id]
	a.mu.RUnlock()
	if target != nil {
		target.apply(message)
	}
}

// refreshAll haalt de stand opnieuw op na elke verbinding. Wat er tijdens een
// onderbreking veranderde is nooit langsgekomen, en de tegels zouden anders
// blijven staan op wat er gold voordat de lijn wegviel.
func (a *app) refreshAll() {
	a.mu.RLock()
	targets := make([]handler, 0, len(a.devices))
	for _, target := range a.devices {
		targets = append(targets, target)
	}
	a.mu.RUnlock()
	failed := false
	for _, target := range targets {
		if refresher, ok := target.(interface{ refresh() error }); ok && refresher.refresh() != nil {
			failed = true
		}
	}
	if failed {
		a.refreshSoon()
		return
	}
	a.mu.Lock()
	a.retryWait = 0
	a.mu.Unlock()
}

// refreshSoon vraagt om een ronde: na het koppelen van een apparaat, en opnieuw
// zolang een stand niet op te halen is.
//
// Deze app is verder gebeurtenis-gestuurd: de console vertelt wat er verandert.
// Dat is precies waarom een mislukte poging bleef staan -- een camera die daarna
// niks doet stuurt ook niks, dus bleef de tegel op "de console antwoordt niet"
// tot de gebeurtenislijn zelf wegviel. Eén ronde per keer, met een oplopende
// pauze, en de pauze gaat terug naar het begin zodra alles lukt.
func (a *app) refreshSoon() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retry != nil {
		return // er staat al een ronde klaar
	}
	if a.client == nil {
		return // zonder console valt er niks op te halen; OnConnect doet de ronde
	}
	a.retryWait *= 2
	if a.retryWait < settleTime {
		a.retryWait = settleTime
	}
	if a.retryWait > maxRefreshRetry {
		a.retryWait = maxRefreshRetry
	}
	a.retry = time.AfterFunc(a.retryWait, func() {
		a.mu.Lock()
		a.retry = nil
		a.mu.Unlock()
		a.refreshAll()
	})
}

func (a *app) watch(protectID string, target handler) {
	a.mu.Lock()
	a.devices[protectID] = target
	a.mu.Unlock()
}

func (a *app) forget(protectID string) {
	a.mu.Lock()
	delete(a.devices, protectID)
	a.mu.Unlock()
}

// api levert de client, of een fout die zegt wat eraan mankeert.
func (a *app) api() (*protect.Client, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.client == nil {
		return nil, fmt.Errorf("er is nog geen console ingesteld")
	}
	return a.client, nil
}

func (a *app) setError(message string) {
	a.mu.Lock()
	a.lastErr = message
	a.mu.Unlock()
}

// De koppelschermen vragen de driver wat er te vinden is. Dat gaat via een
// type-assertie, dus zonder deze regels merk je een verkeerde handtekening pas
// als iemand op koppelen drukt.
var (
	_ appsdk.Pairer = cameraDriver{}
	_ appsdk.Pairer = lightDriver{}
	_ appsdk.Pairer = sensorDriver{}
	_ appsdk.Pairer = chimeDriver{}
	_ appsdk.Pairer = relayDriver{}
)

// En hetzelfde voor wat een apparaat kan.
var (
	_ appsdk.SettingsChanger   = (*cameraDevice)(nil)
	_ appsdk.Deleter           = (*cameraDevice)(nil)
	_ appsdk.CapabilityHandler = (*lightDevice)(nil)
	_ appsdk.CapabilityHandler = (*chimeDevice)(nil)
	_ appsdk.CapabilityHandler = (*relayDevice)(nil)
	_ appsdk.DeviceHandler     = (*sensorDevice)(nil)
)

// report commit alle capabilityvelden uit één consolebericht tegelijk.
//
// Stulp weigert een capability die dit apparaat niet heeft. Dat is een tikfout
// in app.json of hier, en die hoort op te vallen zodra hij gebeurt in plaats van
// pas als iemand zich afvraagt waarom een tegel voor altijd leeg blijft.
//
// Eén functie voor alle vijf de apparaatsoorten in plaats van dezelfde methode
// vijf keer: ze dragen allemaal hetzelfde *appsdk.Device.
func report(device *appsdk.Device, values map[string]any) {
	if err := device.SetCapabilityValues(values); err != nil {
		device.Error(err.Error())
	}
}
