package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// De warmtepomp.
//
// Eén driver voor alle Nibe's die myUplink kent: het verschil tussen een S735 en
// een S320 zit in welke parameters ze melden, niet in hoe je ermee praat. Wat
// een pomp niet meldt, blijft leeg.

// pollInterval is de trage ronde: alle waarden plus de meterstand.
//
// Vijf minuten, uit de bron. Vaker heeft geen zin -- een aanvoertemperatuur
// beweegt niet sneller -- en het is een cloud-API waar we te gast zijn.
const pollInterval = 5 * time.Minute

// powerInterval is de snelle ronde: alleen vermogen en prioriteit.
//
// Elke minuut, en dat is niet alleen voor een levendige tegel: hier vandaan komt
// de verhouding waarmee het verbruik over verwarmen en warm water verdeeld
// wordt, en die verhouding is alleen zo goed als het aantal metingen.
const powerInterval = time.Minute

// callTimeout begrenst één aanroep naar de cloud. Ruim, want er zit een
// verbinding naar de pomp achter, maar niet oneindig: een poll die blijft hangen
// slaat de volgende over.
const callTimeout = 30 * time.Second

// premiumCapabilities zijn de bedieningen die myUplink alleen toestaat met een
// Premium-abonnement.
//
// availableFeatures van het apparaat zegt per stuk of het mag. Wat niet mag
// halen we weg, want een schuif die altijd 403 geeft is erger dan een schuif die
// er niet is -- en de dag dat iemand wel een abonnement neemt komt hij vanzelf
// terug.
var premiumCapabilities = map[string]string{
	"hot_water_boost":   "boostHotWater",
	"ventilation_boost": "boostVentilation",
	"ventilation_mode":  "setVentilationMode",
}

type heatpumpDriver struct{}

type heatpump struct {
	device *appsdk.Device
	id     string // het myUplink-apparaat-id
	cancel context.CancelFunc
	energy *split

	// Wat déze pomp kan, afgelezen van de pomp zelf.
	//
	// De twee series nummeren hun parameters anders en niet elke pomp heeft
	// alles: een grondgebonden F1255 kent geen ventilatie en meldt geen
	// vermogen. In plaats van dat aan het modelnummer af te leiden -- een lijst
	// die bij elke nieuwe pomp achterloopt -- volgt het hier uit de nummers die
	// binnenkomen. Leeg tot de eerste ronde binnen is.
	mu      sync.Mutex
	write   map[string]writablePoint
	meters  energyPoints
	booster boost
	allowed map[string]bool // wat het abonnement toestaat
	asked   bool            // is dat al eens opgevraagd
}

func (heatpumpDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen myUplink-id; koppel het opnieuw")
	}
	return &heatpump{device: device, id: id, energy: &split{}}, nil
}

// ListDevices vraagt myUplink welke pompen dit account heeft.
func (heatpumpDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	systems, err := cloud.Systems(ctx)
	if err != nil {
		return nil, err
	}
	found := []appsdk.PairedDevice{}
	for _, system := range systems {
		for _, device := range system.Devices {
			if device.ID == "" {
				continue
			}
			name := device.Product.Name
			if name == "" {
				name = "Nibe-warmtepomp"
			}
			found = append(found, appsdk.PairedDevice{
				Name: name,
				Data: map[string]any{"id": device.ID},
				Store: map[string]any{
					"systemId": system.SystemID,
					"system":   system.Name,
					"model":    device.Product.Name,
					"serial":   device.Product.SerialNumber,
				},
			})
		}
	}
	return found, nil
}

func (h *heatpump) OnInit() error {
	instance.watch(h.device.ID(), h)
	h.energy.restore(
		h.device.StoreNumber("heatingKwh"),
		h.device.StoreNumber("hotwaterKwh"),
		h.device.StoreNumber("lastTotal"),
		h.device.HasStore("lastTotal"),
	)

	// De eerste ronde loopt in een eigen goroutine: een cloud die niet antwoordt
	// zou anders elk ander apparaat van deze app ophouden.
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.run(ctx)
	return nil
}

func (h *heatpump) OnDeleted() {
	instance.forget(h.device.ID())
	h.halt()
}

// halt breekt de ronde van deze pomp af: bij verwijderen én als de app stopt.
func (h *heatpump) halt() {
	if h.cancel != nil {
		h.cancel()
	}
}

func (h *heatpump) run(ctx context.Context) {
	h.syncFeatures(ctx)
	h.pollAll(ctx)
	h.pollPower(ctx)

	slow := time.NewTicker(pollInterval)
	defer slow.Stop()
	fast := time.NewTicker(powerInterval)
	defer fast.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			h.pollAll(ctx)
		case <-fast.C:
			h.pollPower(ctx)
		}
	}
}

// refresh haalt buiten de gewone ronde om alles op. Elke aanroep bewaakt zijn
// eigen tijd, dus er staat hier geen deadline over het geheel.
func (h *heatpump) refresh() {
	ctx := context.Background()
	h.syncFeatures(ctx)
	h.pollAll(ctx)
	h.pollPower(ctx)
}

// syncFeatures onthoudt welke bedieningen dit abonnement toestaat. Wat daarmee
// gebeurt, gebeurt in learn: daar ligt ook de andere helft van het antwoord --
// of de pomp de bediening überhaupt heeft.
func (h *heatpump) syncFeatures(ctx context.Context) {
	call, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	device, err := cloud.Device(call, h.id)
	if err != nil {
		// Niet weten is geen reden om iets weg te halen: dan zou een storing bij
		// myUplink de tegels van iemand met een abonnement leegtrekken. Daarom
		// blijft asked staan zoals hij stond.
		h.device.Error("kon niet opvragen welke bedieningen dit abonnement toestaat: " + err.Error())
		return
	}
	allowed := map[string]bool{}
	for _, feature := range premiumCapabilities {
		allowed[feature] = device.AvailableFeatures[feature]
	}
	h.mu.Lock()
	h.allowed, h.asked = allowed, true
	h.mu.Unlock()
}

// learn leest aan de binnengekomen nummers af wat deze pomp is en kan.
//
// De pomp vertelt dat zelf, en beter dan zijn modelnaam: wie 40004 meldt is een
// F-serie, wie 4 meldt een S-serie, en wie geen 22130 meldt heeft geen
// vermogensmeting -- ongeacht wat er op de kast staat.
func (h *heatpump) learn(points []myuplink.Point) {
	present := make(map[string]bool, len(points))
	recognised := 0
	for _, p := range points {
		present[p.ParameterID] = true
		if _, known := readable[p.ParameterID]; known {
			recognised++
		}
	}

	// Geen enkel bekend nummer betekent dat we niet weten waar we naar kijken --
	// een half antwoord van myUplink, of een pomp die geen van beide series is.
	// Daar tegels op weghalen zou een storing in een lege pagina veranderen, en
	// de volgende ronde is over vijf minuten.
	if recognised == 0 {
		h.device.Error("myUplink meldde geen enkele bekende parameter voor deze pomp")
		return
	}

	// De vermogensverdeling bestaat alleen in de S-serie. Een F-serie meldt geen
	// opgenomen vermogen en geen meterstand, alleen fasestromen in ampère, en
	// daar valt zonder spanning geen watt van te maken.
	meters := energyPoints{}
	if present[sEnergy.power] || present[sEnergy.meter] {
		meters = sEnergy
	}
	booster := boost{}
	switch {
	case present[sBoost.parameter]:
		booster = sBoost
	case present[fBoost.parameter]:
		booster = fBoost
	}

	h.mu.Lock()
	h.write, h.meters, h.booster = writeTable(present), meters, booster
	allowed, asked := h.allowed, h.asked
	h.mu.Unlock()

	h.applyCapabilities(present, meters, allowed, asked)
}

// applyCapabilities zet de tegels gelijk aan wat deze pomp werkelijk voedt.
//
// Een tegel zonder bron blijft voor altijd leeg en een schuif die de pomp niet
// kent geeft een fout bij elke aanraking; allebei zijn ze erger dan een tegel
// die er niet is. Alleen de tegels uit deze tabellen worden aangeraakt, zodat
// wat een gebruiker zelf toevoegde blijft staan.
func (h *heatpump) applyCapabilities(present map[string]bool, meters energyPoints, allowed map[string]bool, asked bool) {
	for capability, backed := range backedCapabilities(present, meters) {
		// Bij een premium-bediening telt ook of het abonnement hem toestaat.
		// Zolang dat niet opgevraagd kon worden blijft hij staan: een storing bij
		// myUplink hoort geen tegels op te ruimen.
		if feature, premium := premiumCapabilities[capability]; premium && backed && asked {
			backed = allowed[feature]
		}
		has := h.device.HasCapability(capability)
		switch {
		case backed && !has:
			if err := h.device.AddCapability(capability); err != nil {
				h.device.Error("kon " + capability + " niet toevoegen: " + err.Error())
			}
		case !backed && has:
			if err := h.device.RemoveCapability(capability); err != nil {
				h.device.Error("kon " + capability + " niet weghalen: " + err.Error())
			}
		}
	}
}

// energyCapabilities zijn de zes die uit de vermogensverdeling komen in plaats
// van uit één parameter.
var energyCapabilities = []string{
	"measure_power", "measure_power.heating", "measure_power.hotwater",
	"meter_power", "meter_power.heating", "meter_power.hotwater",
}

// backedCapabilities zegt per tegel of deze pomp hem voedt.
func backedCapabilities(present map[string]bool, meters energyPoints) map[string]bool {
	out := map[string]bool{}
	for parameter, entry := range readable {
		// Of, want een capability kan in beide series voorkomen en dan telt de
		// serie die deze pomp werkelijk is.
		out[entry.capability] = out[entry.capability] || present[parameter]
	}
	for _, capability := range energyCapabilities {
		out[capability] = meters.power != "" || meters.meter != ""
	}
	return out
}

// pollAll leest alles uit en ijkt de meterstanden.
func (h *heatpump) pollAll(ctx context.Context) {
	call, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	points, err := cloud.Points(call, h.id)
	if err != nil {
		h.device.SetUnavailable("myUplink antwoordt niet: " + err.Error())
		return
	}

	// Eerst vaststellen wat deze pomp kan, dan pas waarden wegzetten: anders
	// klaagt de allereerste ronde over elke tegel die er nog niet is.
	h.learn(points)

	values := map[string]any{}
	for _, p := range points {
		entry, mapped := readable[p.ParameterID]
		if !mapped || !h.device.HasCapability(entry.capability) {
			continue
		}
		value, known := capabilityValue(entry, p)
		if !known {
			continue
		}
		values[entry.capability] = value
	}
	h.applyMeter(points, values)
	h.report(values)

	if err := h.device.SetAvailable(); err != nil {
		h.device.Error(err.Error())
	}
}

// applyMeter zet de meterstand van de pomp door en verdeelt de toename.
func (h *heatpump) applyMeter(points []myuplink.Point, values map[string]any) {
	h.mu.Lock()
	meter := h.meters.meter
	h.mu.Unlock()
	if meter == "" {
		return
	}
	total, known := pointValue(points, meter)
	if !known {
		return
	}
	values["meter_power"] = total

	// De verdeelde standen gaan er ook heen als er niets te verdelen viel: na een
	// herstart, of bij de allereerste ronde waarin alleen geijkt wordt, zouden de
	// twee tegels anders leeg blijven terwijl de standen er wel zijn.
	moved := h.energy.anchor(total)
	values["meter_power.heating"] = h.energy.heatingKwh
	values["meter_power.hotwater"] = h.energy.hotwaterKwh
	if !moved {
		return
	}
	// Bewaren, anders begint elke meter na een herstart weer bij nul terwijl de
	// pomp gewoon doorgeteld heeft.
	if err := h.device.SetStore(map[string]any{
		"heatingKwh":  h.energy.heatingKwh,
		"hotwaterKwh": h.energy.hotwaterKwh,
		"lastTotal":   h.energy.lastTotal,
	}); err != nil {
		h.device.Error(err.Error())
	}
}

// pollPower is de snelle ronde: vermogen en prioriteit, meer niet.
func (h *heatpump) pollPower(ctx context.Context) {
	h.mu.Lock()
	meters := h.meters
	h.mu.Unlock()
	// Een pomp zonder vermogensmeting heeft hier niets te halen. Dat is niet
	// hetzelfde als een mislukte ronde, dus er komt ook geen melding van.
	if meters.power == "" {
		return
	}

	call, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	points, err := cloud.Points(call, h.id, meters.power, meters.priority)
	if err != nil {
		// Hier niet op onbereikbaar zetten: de trage ronde gaat daarover, en één
		// gemiste minuut is geen pomp die weg is.
		h.device.Error("vermogen ophalen mislukte: " + err.Error())
		return
	}
	watt, known := pointValue(points, meters.power)
	if !known {
		return
	}
	// Geen prioriteit gemeld betekent niet "warm water", en daarmee valt het
	// vermogen bij verwarmen -- dezelfde complementregel als overal hier.
	priority := -1
	if value, ok := pointValue(points, meters.priority); ok {
		priority = int(value)
	}

	heating, hotwater := h.energy.power(time.Now(), watt, priority)
	h.report(map[string]any{
		"measure_power":          watt,
		"measure_power.heating":  heating,
		"measure_power.hotwater": hotwater,
	})
}

// report commit alle waarden uit één myUplink-antwoord tegelijk en klaagt als
// een capability niet bestaat. Dat is een tikfout in deze app en geen fout van
// de pomp, dus hij hoort meteen op te vallen.
func (h *heatpump) report(values map[string]any) {
	if err := h.device.SetCapabilityValues(values); err != nil {
		h.device.Error(err.Error())
	}
}

// OnCapability stuurt een bediening door naar de pomp.
func (h *heatpump) OnCapability(name string, value any) error {
	h.mu.Lock()
	entry, ok := h.write[name]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s is niet iets wat deze pomp aanneemt", name)
	}
	raw, err := apiValue(name, entry.point, value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := cloud.SetPoints(ctx, h.id, map[string]any{entry.parameter: raw}); err != nil {
		return err
	}
	// myUplink neemt de waarde aan en zet hem daarna door naar de pomp. Wat er
	// werkelijk staat komt van de volgende ronde; hier alvast de gevraagde
	// waarde neerzetten is beweren dat de pomp al om is.
	go h.refresh()
	return nil
}

// boostHotWater zet warm water voor een aantal uur aan. Dat is een ander punt
// dan de aan/uit-schakelaar, die alleen aan kent en geen duur.
//
// De kaart biedt uren aan; wat de pomp daarvan wil zien verschilt per serie, en
// de F-serie kent bovendien geen 24 en 48 uur. Dat is een duidelijke fout waard
// en geen stilzwijgend afronden naar twaalf.
func (h *heatpump) boostHotWater(hours int) error {
	h.mu.Lock()
	booster := h.booster
	h.mu.Unlock()
	if booster.parameter == "" {
		return fmt.Errorf("deze pomp kent geen extra warm water, of hij is nog niet één keer uitgelezen")
	}
	raw, ok := booster.raw[hours]
	if !ok {
		return fmt.Errorf("%d uur is geen stand die deze pomp kent voor extra warm water", hours)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return cloud.SetPoints(ctx, h.id, map[string]any{booster.parameter: raw})
}

// pointValue zoekt één parameter op in wat er binnenkwam.
func pointValue(points []myuplink.Point, parameter string) (float64, bool) {
	for _, p := range points {
		if p.ParameterID == parameter {
			return p.Number()
		}
	}
	return 0, false
}
