// Command com.stulp.matter is de Matter-controller als plugin.
//
// Stulp start hem als eigen proces. Wat hij doet is ongewijzigd -- dezelfde
// controller die eerst binnen Stulp draaide -- maar hij praat nu via de
// plugin-SDK in plaats van rechtstreeks met de store. Zie docs/plugins.md.
//
// Wat een plugin níet kan, kan hij dus ook niet meer: apparaten aanmaken of
// verwijderen doet Stulp, en Flows raakt hij niet aan.
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/matter/internal/bridge"
	mattercontroller "github.com/xinix00/stulp/plugins/matter/internal/controller"
)

// matterDriver is de enige driver: elk gecommissioned apparaat hoort erbij,
// ongeacht wat het is. Wat een apparaat kan staat in zijn capabilities.
type matterDriver struct{ app *app }

// Pair is wat de koppelpagina mag sturen.
//
// Koppelen begint bij een code: een QR-payload of de handmatige code op het
// apparaat. Zonder die code is er niets te vinden -- een Matter-apparaat laat
// zich niet ongevraagd toevoegen, en dat is precies de bedoeling.
// Commissioneren duurt minuten (wachten op de mDNS-advertentie plus het
// certificaat-gesprek) en HTTP-verzoeken die zo lang hangen sterven onderweg —
// de tunnel kapt ze af en dan ziet de gebruiker een 502 terwijl stulp gewoon
// doorwerkt (gemeten 20-08, exact op de proxy-timeout). Lange dingen zijn hier
// dus start-plus-poll, hetzelfde snapshot-patroon als scan/mesh/diagnostiek:
// élk verzoek is kort, en de pagina kijkt hoe het ervoor staat.
func (d matterDriver) Pair() map[string]appsdk.PairHandler {
	running := &scan{}
	ctx, cancel := context.WithCancel(context.Background())
	var candidatesMu sync.Mutex
	var candidates []appsdk.PairedDevice
	list := func() []appsdk.PairedDevice {
		candidatesMu.Lock()
		defer candidatesMu.Unlock()
		return append([]appsdk.PairedDevice(nil), candidates...)
	}
	return map[string]appsdk.PairHandler{
		"commission": func(data any) (any, error) {
			request, _ := data.(map[string]any)
			code, _ := request["code"].(string)
			address, _ := request["address"].(string)
			if strings.TrimSpace(code) == "" {
				return nil, fmt.Errorf("een koppelcode is nodig")
			}
			if !running.begin() {
				return running.snapshot(), nil
			}
			candidatesMu.Lock()
			candidates = nil
			candidatesMu.Unlock()
			if !d.app.beginCommission() {
				running.done(fmt.Errorf("er loopt al een andere Matter-koppeling"))
				return running.snapshot(), nil
			}
			go func() {
				defer d.app.endCommission()
				result, err := d.app.commission(ctx, code, address)
				if err == nil {
					candidatesMu.Lock()
					candidates = append([]appsdk.PairedDevice(nil), result...)
					candidatesMu.Unlock()
					running.put("found", map[string]any{"found": len(result)})
				}
				running.done(err)
			}()
			return running.snapshot(), nil
		},
		"commission_state": func(any) (any, error) {
			return running.snapshot(), nil
		},
		"list_devices": func(any) (any, error) {
			return list(), nil
		},
		"cancel": func(any) (any, error) {
			cancel()
			return nil, nil
		},
	}
}

// ListDevices levert voor de oude, sessieloze lijst-API wat er het laatst is
// gecommissioneerd. Een echte pair-sessie gebruikt zijn eigen list_devices-
// closure uit Pair, zodat twee schermen nooit elkaars kandidaten zien.
//
// Anders dan bij een hub die je kunt afzoeken valt hier niets te ontdekken
// zonder code: de vorige stap heeft het apparaat al in de fabric gehaald, en dit
// is de keuze die de gebruiker daarna maakt.
func (d matterDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return d.app.candidates(), nil
}

func (d matterDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	return &matterDevice{app: d.app, device: device}, nil
}

type matterDevice struct {
	app    *app
	device *appsdk.Device
}

// OnInit zoekt bewust geen verbinding -- dat zou de start ophouden voor een
// apparaat dat misschien uit staat. Wat hier wél hoort is niet-blokkerend:
//
//   - De subscription-worker van deze node zeker stellen. Commission start hem
//     te vroeg (de apparaten zijn dan nog prototypes) en dan sterft hij; dít is
//     het moment waarop het apparaat echt bestaat -- bij het koppelen, en ook
//     bij een adoptie na een restore. Zonder deze regel bleef een vers
//     gekoppelde stekker leeg staan tot een plugin-herstart (gemeten 19-08).
//   - De soort helen van apparaten die gekoppeld zijn toen de koppelstroom de
//     soort nog liet vallen: die staan op de driver-default "other" terwijl hun
//     eigen store weet dat ze een stekker zijn. Alleen vanaf "other", dus een
//     soort die iemand bewust koos blijft staan.
func (m *matterDevice) OnInit() error {
	controller := m.app.controller
	if controller == nil {
		return nil
	}
	if nodeText, ok := m.device.Data()["nodeId"].(string); ok {
		if nodeID, err := strconv.ParseUint(nodeText, 16, 64); err == nil && nodeID != 0 {
			controller.EnsureSubscription(nodeID)
		}
	}
	if m.device.Class() == "other" {
		if class := mattercontroller.StoredClass(m.device.Store()); class != "" && class != "other" {
			return m.device.SetClass(class)
		}
	}
	return nil
}

// OnCapability is een opdracht uit de interface of een Flow: zet die lamp aan.
func (m *matterDevice) OnCapability(name string, value any) error {
	controller := m.app.controller
	if controller == nil {
		return fmt.Errorf("Matter controller is not running")
	}
	return controller.SetCapability(context.Background(), m.device.ID(), name, value)
}

type app struct {
	controller *mattercontroller.Controller
	backing    *backing
	bridge     *bridge.Manager

	mu            sync.Mutex
	found         []appsdk.PairedDevice
	commissionMu  sync.Mutex
	commissioning bool

	// Wat de config-pagina laat zien. Eén verkenning tegelijk per soort: de
	// pagina mag zo vaak kijken als hij wil, de nodes worden er niet vaker om
	// gevraagd. Diagnostiek is per apparaat en staat onder a.mu.
	discovery scan
	mesh      scan
	diagnoses map[string]*scan
	shares    map[string]*scan
}

// maxDiagnoses begrenst de diagnose-map. Boven het aantal Matter-apparaten dat
// een huis heeft (deze fabric: 23), en het plafond bestaat omdat de sleutel van
// buiten komt.
const maxDiagnoses = 64

// commission haalt het apparaat in de fabric en onthoudt wat eruit kwam.
//
// De plugin maakt zelf geen apparaten aan: wat hier gevonden wordt gaat als
// keuze naar de koppelpagina, en Stulp bewaart wat de gebruiker overneemt.
func (a *app) commission(parent context.Context, code, address string) ([]appsdk.PairedDevice, error) {
	if a.controller == nil {
		return nil, fmt.Errorf("Matter controller is not running")
	}
	// Ruim, want er zitten twee wachttijden in elkaar: eerst tot een minuut
	// wachten tot het koppelvenster van het andere systeem op het netwerk
	// verschijnt, en daarna het gesprek van meerdere rondes met een apparaat dat
	// net wakker wordt (de controller houdt daar twee minuten voor aan). Op 90
	// seconden at het wachten het commissioneren op.
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()

	prototypes, err := a.controller.Commission(ctx, mattercontroller.CommissionRequest{
		Code: code, Address: address,
	})
	if err != nil {
		return nil, err
	}

	found := make([]appsdk.PairedDevice, 0, len(prototypes))
	for _, prototype := range prototypes {
		found = append(found, appsdk.PairedDevice{
			Name: prototype.Name,
			// Wat dit apparaat moet onthouden staat bij het apparaat zelf: het
			// node-id, het endpoint, waar het over gaat. Niet in de instellingen
			// van de app -- er is er niet één van, en de volgende heeft andere.
			Data:     prototype.Data,
			Settings: prototype.Settings,
			Store:    prototype.Store,
			// De soort komt uit de Descriptor van het endpoint (0x010A =
			// stekker, 0x0100 = lamp): de controller weet dat per apparaat
			// beter dan de driver-default in het manifest.
			Class:        prototype.Class,
			Capabilities: prototype.Capabilities,
		})
	}

	a.mu.Lock()
	a.found = found
	a.mu.Unlock()
	return found, nil
}

func (a *app) candidates() []appsdk.PairedDevice {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]appsdk.PairedDevice(nil), a.found...)
}

func (a *app) beginCommission() bool {
	a.commissionMu.Lock()
	defer a.commissionMu.Unlock()
	if a.commissioning {
		return false
	}
	a.commissioning = true
	return true
}

func (a *app) endCommission() {
	a.commissionMu.Lock()
	a.commissioning = false
	a.commissionMu.Unlock()
}

func main() { start(plugin()) }

// plugin bouwt de plugin-beschrijving; start (start_host.go/start_tamago.go)
// kiest de gedaante: eigen proces naast stulp, of HopOS-slot-app.
func plugin() appsdk.Plugin {
	instance := &app{}
	return appsdk.Plugin{
		OnInit: func(stulp *appsdk.Stulp) error {
			backing, err := newBacking(stulp)
			if err != nil {
				return err
			}
			// Via de SDK en niet rechtstreeks naar stderr: dan komt een
			// waarschuwing bij Stulp ook als waarschuwing binnen, in plaats van
			// als een INFO-regel met de echte melding erin geplakt.
			controller, err := mattercontroller.New(context.Background(), backing, stulp.Logger())
			if err != nil {
				return fmt.Errorf("start Matter controller: %w", err)
			}
			instance.controller = controller
			instance.backing = backing
			bridgeManager, err := bridge.NewManager(backing.bridgeRecord(), stulp.HomeDevices(), backing.saveBridgeRecord,
				func(deviceID, capability string, value any) error {
					return stulp.SetHomeCapability(deviceID, capability, value)
				})
			if err != nil {
				controller.Close()
				return fmt.Errorf("start Matter bridge: %w", err)
			}
			instance.bridge = bridgeManager
			stulp.OnHomeDeviceChanged(bridgeManager.UpdateDevice)
			instance.registerAPI(stulp)
			stulp.Log("Matter controller running")
			return nil
		},
		Drivers: map[string]appsdk.Driver{"matter": matterDriver{app: instance}},
		OnStop: func() {
			if instance.controller != nil {
				instance.controller.Close()
			}
		},
	}
}
