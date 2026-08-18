// De Nibe-app.
//
// Warmtepompen via de myUplink-cloud: één driver, en per pomp één apparaat met
// alles wat de cloud erover te melden heeft. Wat er wel en niet geport is staat
// in PORTED.md, met verwijzingen naar waar het in de oorspronkelijke app stond.
//
// De koppeling loopt over OAuth2 zonder tussenpersoon: de configuratiepagina
// bouwt de autorisatie-URL, de gebruiker plakt terug waar de browser uitkwam, en
// de app wisselt de code om. Zie session.go en internal/myuplink/oauth.go.
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// app is de plugin als geheel: één myUplink-account en de pompen die eruit
// komen.
type app struct {
	mu      sync.RWMutex
	stulp   *appsdk.Stulp
	session *session
	devices map[string]*heatpump // Stulp-device-id -> pomp
	cancel  context.CancelFunc

	// pending is de autorisatie die op de configuratiepagina begonnen is. Er is
	// er hooguit één tegelijk: twee mensen die dezelfde app koppelen zouden
	// elkaars code inwisselen, en dat kan niet goed aflopen.
	pending *myuplink.Authorization
}

var instance = &app{devices: map[string]*heatpump{}}

// cloud is de weg naar myUplink. Hij bestaat vanaf het begin en niet pas na het
// starten: wat er tussendoor binnenkomt hoort een melding te krijgen en geen
// nil-pointer.
var cloud = &myuplink.Client{HTTP: myuplink.DefaultHTTP(), Token: accessToken}

// accessToken vraagt het token aan de sessie, en zegt het als die er nog niet is.
func accessToken(ctx context.Context) (string, error) {
	instance.mu.RLock()
	session := instance.session
	instance.mu.RUnlock()
	if session == nil {
		return "", fmt.Errorf("de Nibe-app is nog niet gestart")
	}
	return session.token(ctx)
}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit:  instance.start,
		OnStop:  instance.stop,
		Drivers: map[string]appsdk.Driver{"heatpump": heatpumpDriver{}},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	session := newSession(
		func(tokens *myuplink.Tokens) error {
			raw, err := writeStored(tokens)
			if err != nil {
				return err
			}
			return stulp.SetState(raw)
		},
		func(message string) {
			stulp.Notify(message)
			stulp.Error(message)
		},
	)

	tokens, err := readStored(stulp.State())
	if err != nil {
		// Een onleesbare state is geen reden om te stoppen -- de gebruiker kan
		// opnieuw koppelen -- maar wel iets wat hij hoort te lezen.
		stulp.Error(err.Error())
	}
	session.tokens = tokens

	ctx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	a.stulp, a.session, a.cancel = stulp, session, cancel
	a.mu.Unlock()

	a.readConfig()
	stulp.OnSettingsChanged(func(map[string]any) { a.readConfig() })
	a.registerAPI(stulp)
	a.registerFlow(stulp)

	go session.maintain(ctx)
	return nil
}

func (a *app) stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// readConfig haalt de myUplink-registratie uit de instellingen.
func (a *app) readConfig() {
	a.mu.RLock()
	stulp, session := a.stulp, a.session
	a.mu.RUnlock()
	session.setConfig(myuplink.Config{
		ClientID:     stulp.SettingText("clientId"),
		ClientSecret: stulp.SettingText("clientSecret"),
		RedirectURI:  stulp.SettingText("redirectUri"),
	})
}

func (a *app) watch(deviceID string, pump *heatpump) {
	a.mu.Lock()
	a.devices[deviceID] = pump
	a.mu.Unlock()
}

func (a *app) forget(deviceID string) {
	a.mu.Lock()
	delete(a.devices, deviceID)
	a.mu.Unlock()
}

// refreshAll haalt meteen op wat er te halen valt. Bedoeld voor het moment vlak
// na een nieuwe koppeling: zonder dit blijft een pomp tot vijf minuten daarna op
// onbereikbaar staan terwijl alles allang werkt.
func (a *app) refreshAll() {
	a.mu.RLock()
	pumps := make([]*heatpump, 0, len(a.devices))
	for _, pump := range a.devices {
		pumps = append(pumps, pump)
	}
	a.mu.RUnlock()
	for _, pump := range pumps {
		go pump.refresh()
	}
}

func (a *app) pump(deviceID string) (*heatpump, error) {
	a.mu.RLock()
	pump := a.devices[deviceID]
	a.mu.RUnlock()
	if pump == nil {
		return nil, fmt.Errorf("deze warmtepomp draait niet in deze app")
	}
	return pump, nil
}

// registerFlow hangt de kaarten uit app.json aan hun handler.
func (a *app) registerFlow(stulp *appsdk.Stulp) {
	stulp.OnFlowAction("boost_hot_water", func(args, _ map[string]any) (any, error) {
		pump, err := a.pump(appsdk.DeviceArg(args, "device"))
		if err != nil {
			return nil, err
		}
		choice, _ := args["duration"].(string)
		hours, err := strconv.Atoi(strings.TrimSpace(choice))
		if err != nil {
			return nil, fmt.Errorf("onbekende duur %q voor extra warm water", choice)
		}
		return nil, pump.boostHotWater(hours)
	})
}

// De koppelschermen en de tegels vinden hun handler via een type-assertie, dus
// zonder deze regels merk je een verkeerde handtekening pas als iemand erop
// drukt.
var (
	_ appsdk.Pairer            = heatpumpDriver{}
	_ appsdk.DeviceHandler     = (*heatpump)(nil)
	_ appsdk.CapabilityHandler = (*heatpump)(nil)
	_ appsdk.Deleter           = (*heatpump)(nil)
)
