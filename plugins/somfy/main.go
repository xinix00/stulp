// De Somfy TaHoma-app.
//
// Eén account bij tahomalink.com, en daaruit de zonwering en de rolluiken die
// aan de TaHoma- of Connexoon-doos hangen. Standen komen van één plek: een poll
// op /setup die alleen doorgeeft wat veranderd is. Wat er wel en niet geport is
// staat in PORTED.md, met verwijzingen naar waar het in de oorspronkelijke app
// stond -- en dat begint met de reden dat het een poll is en geen stroom.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/somfy/internal/tahoma"
)

// app is de plugin als geheel: één account en de apparaten die eruit komen.
type app struct {
	mu      sync.RWMutex
	stulp   *appsdk.Stulp
	client  *tahoma.Client
	poller  *tahoma.Poller
	cancel  context.CancelFunc
	devices map[string]*covering     // deviceURL -> wie erop wacht
	latest  map[string]tahoma.Device // laatst geziene standen, voor een apparaat dat later start
	lastErr string
	lastOK  time.Time
}

var instance = &app{
	devices: map[string]*covering{},
	latest:  map[string]tahoma.Device{},
}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	// Zeven drivers, één implementatie. Ze verschillen alleen in welke
	// TaHoma-apparaten erbij horen en welke kant hun "omhoog" op staat; dat
	// staat in de tabel in covering.go en niet in zeven bestanden.
	drivers := make(map[string]appsdk.Driver, len(coverings))
	for id, kind := range coverings {
		drivers[id] = coveringDriver{kind: kind}
	}
	return appsdk.Plugin{
		OnInit:  instance.start,
		OnStop:  instance.stop,
		Drivers: drivers,
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.mu.Lock()
	a.stulp = stulp
	a.mu.Unlock()

	a.registerAPI(stulp)
	a.registerFlow(stulp)
	a.connect()
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

// connect zet de poll op met wat er in de state staat.
//
// Dit blokkeert niet: de eerste aanroep gebeurt tijdens OnInit, en een cloud die
// niet antwoordt zou daarmee elk apparaat van deze app ophouden. Wat er misgaat
// komt terug op de configuratiepagina.
func (a *app) connect() {
	account := a.account()

	a.stop()
	if account.Username == "" || account.Password == "" {
		// De oude client moet hier echt weg. Blijft hij staan, dan werkt na
		// "gegevens wissen" het koppelscherm gewoon door op een account dat de
		// gebruiker net heeft weggegooid.
		a.mu.Lock()
		a.client, a.poller = nil, nil
		a.mu.Unlock()
		a.setError("Vul je TaHoma-gebruikersnaam en wachtwoord in op de instellingenpagina.")
		return
	}

	client := tahoma.New(account.Username, account.Password)
	poller := tahoma.NewPoller(client, account.interval(), tahoma.Handlers{
		OnDevice: a.apply,
		OnRound:  a.round,
		OnError:  a.failed,
	})
	ctx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	a.client, a.poller, a.cancel, a.lastErr = client, poller, cancel, ""
	a.mu.Unlock()

	go poller.Run(ctx)
}

// apply brengt een gewijzigd apparaat bij het apparaat dat erop wacht.
func (a *app) apply(device tahoma.Device) {
	a.mu.RLock()
	target := a.devices[device.DeviceURL]
	a.mu.RUnlock()
	if target != nil {
		target.apply(device)
	}
}

// round verwerkt wat een geslaagde ronde opleverde: de laatste stand van alles,
// en wie er nog is.
//
// Een apparaat dat niet meer in de setup staat is uit de TaHoma-doos gehaald of
// buiten bereik. De bron zet hem dan op onbereikbaar en dat is hier hetzelfde --
// het is het enige signaal dat deze API erover geeft.
func (a *app) round(devices []tahoma.Device) {
	present := make(map[string]tahoma.Device, len(devices))
	for _, device := range devices {
		if device.DeviceURL != "" {
			present[device.DeviceURL] = device
		}
	}

	a.mu.Lock()
	a.latest = present
	a.lastOK = time.Now()
	a.lastErr = ""
	watching := make(map[string]*covering, len(a.devices))
	for url, target := range a.devices {
		watching[url] = target
	}
	a.mu.Unlock()

	// Buiten het slot melden: SetAvailable is een verzoek aan Stulp, en dat
	// hoort de poll niet met de lock in de hand te doen.
	for url, target := range watching {
		if _, still := present[url]; still {
			target.reachable()
		} else {
			target.gone()
		}
	}
}

// failed meldt een ronde die niet lukte.
//
// De apparaten blijven staan op wat ze het laatst wisten: een cloud die er even
// niet is zegt niets over waar een rolluik hangt, en alles op onbereikbaar
// zetten bij de eerste hapering levert een huis vol grijze tegels op.
func (a *app) failed(err error) {
	a.setError(err.Error())
	a.mu.RLock()
	stulp := a.stulp
	a.mu.RUnlock()
	if stulp != nil {
		stulp.Error("TaHoma: " + err.Error())
	}
}

func (a *app) watch(deviceURL string, target *covering) {
	a.mu.Lock()
	a.devices[deviceURL] = target
	known, ok := a.latest[deviceURL]
	poller := a.poller
	a.mu.Unlock()

	// Wat er al bekend was meteen doorgeven: een apparaat dat later start hoeft
	// niet tot de volgende ronde met een lege tegel te staan.
	if ok {
		target.apply(known)
	}
	if poller != nil {
		poller.Nudge()
	}
}

func (a *app) forget(deviceURL string) {
	a.mu.Lock()
	delete(a.devices, deviceURL)
	a.mu.Unlock()
}

// api levert de client, of een fout die zegt wat eraan mankeert.
func (a *app) api() (*tahoma.Client, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.client == nil {
		return nil, fmt.Errorf("er is nog geen TaHoma-account ingesteld; vul het in op de instellingenpagina")
	}
	return a.client, nil
}

// nudge vraagt de poll om een extra ronde. Bedoeld voor vlak na een commando.
func (a *app) nudge() {
	a.mu.RLock()
	poller := a.poller
	a.mu.RUnlock()
	if poller != nil {
		poller.Nudge()
	}
}

func (a *app) setError(message string) {
	a.mu.Lock()
	a.lastErr = message
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Het account
// ---------------------------------------------------------------------------

// account is wat deze app moet onthouden om te kunnen inloggen.
//
// Het staat in de eigen state van de plugin en niet in de instellingen. Dat is
// een bewuste keuze: TaHoma kent geen API-sleutel, dus dit is het echte
// wachtwoord van het Somfy-account van de gebruiker. State gaat door geen
// enkele API-route naar buiten en komt niet in Manage te staan; instellingen
// wel. De bron zet het wel in de instellingen (api.js, ManagerSettings.set) en
// leest het daar ook weer uit op de configuratiepagina -- dat hoeft hier niet,
// want de pagina hoeft een bewaard wachtwoord nooit terug te zien.
type account struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Seconds is de tijd tussen twee rondes. De bron laat dit instellen met
	// standaard tien seconden (app.js, INITIAL_SYNC_INTERVAL).
	Seconds int `json:"intervalSeconds"`
}

func (a account) interval() time.Duration {
	if a.Seconds <= 0 {
		return tahoma.DefaultInterval
	}
	interval := time.Duration(a.Seconds) * time.Second
	if interval < tahoma.MinInterval {
		return tahoma.MinInterval
	}
	return interval
}

func (a *app) account() account {
	a.mu.RLock()
	stulp := a.stulp
	a.mu.RUnlock()
	if stulp == nil {
		return account{}
	}
	raw := stulp.State()
	if len(raw) == 0 {
		return account{}
	}
	var stored account
	if err := json.Unmarshal(raw, &stored); err != nil {
		// Een state die niet te lezen is hoort op te vallen: hij wordt niet
		// stilletjes vervangen door lege gegevens, want dan is het wachtwoord
		// weg zonder dat iemand weet waarom.
		a.setError("De opgeslagen inloggegevens zijn onleesbaar: " + err.Error())
		return account{}
	}
	return stored
}

func (a *app) saveAccount(stored account) error {
	a.mu.RLock()
	stulp := a.stulp
	a.mu.RUnlock()
	if stulp == nil {
		return fmt.Errorf("de app is nog niet gestart")
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return stulp.SetState(raw)
}

// De koppelschermen vragen de driver wat er te vinden is. Dat gaat via een
// type-assertie, dus zonder deze regels merk je een verkeerde handtekening pas
// als iemand op koppelen drukt.
var (
	_ appsdk.Pairer            = coveringDriver{}
	_ appsdk.DeviceHandler     = (*covering)(nil)
	_ appsdk.CapabilityHandler = (*covering)(nil)
	_ appsdk.Deleter           = (*covering)(nil)
)
