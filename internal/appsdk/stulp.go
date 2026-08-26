package appsdk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Stulp is alles wat niet bij één apparaat hoort: de instellingen van de app,
// de Flow-kaarten, notificaties en de log.
//
// Veilig vanaf elke goroutine.
type Stulp struct {
	host *Host

	mu           sync.Mutex
	cards        map[string]*flowCard
	handlers     map[string]Handler
	onChange     func(changed map[string]any)
	onHomeChange func(HomeDevice)
	devices      map[string]*Device
}

// HomeDevice is a privacy-reduced, read-only view of a device owned by another
// app. It contains only what a bridge needs to model and forward that device;
// plugin data, settings and private store never cross this boundary.
type HomeDevice struct {
	ID                 string
	AppID              string
	Name               string
	Class              string
	Available          bool
	UnavailableMessage string
	Capabilities       []string
	State              map[string]any
	Removed            bool
}

// Handler beantwoordt een aanroep van de eigen settings-pagina van de app.
//
// Die pagina draait in de browser en kan niet bij het apparaat; de plugin wel.
// Dit is de weg ertussen -- query komt uit de URL, body uit het verzoek.
type Handler func(query, body map[string]any) (any, error)

// Measure markeert een getal in het antwoord aan een pagina als een meting, met
// de eenheid waarin het gemeten is: `Measure(21.2, "°C")`.
//
// Stulp rekent het daarna om naar de eenheid die dit huis leest en zet de tekst
// erbij, zodat de pagina alleen `text` hoeft af te drukken. Zo leest de
// koppelpagina van een app hetzelfde als de tegel ernaast, zonder dat de plugin
// of de pagina iets van eenheden hoeft te weten.
//
// Canoniek erin: °C, m/s, mm, km, hPa. Een eenheid waar niets te kiezen valt --
// procenten, watts -- mag ook; die komt er ongemoeid uit met zijn label.
// {"$measure": …} is een markering zoals {"$device": …} in een Flow-argument:
// daarmee wordt nooit per ongeluk een gewoon veld omgerekend.
func Measure(value float64, unit string) map[string]any {
	return map[string]any{"$measure": value, "units": unit}
}

type flowCard struct {
	kind      string
	id        string
	action    func(args, state map[string]any) (any, error)
	condition func(args, state map[string]any) (bool, error)
	// complete levert per argument de keuzelijst.
	complete map[string]Autocomplete
}

// Autocomplete vult een keuzelijst op een Flow-kaart: de gebruiker typt, en dit
// levert wat daarbij past. query is leeg als het veld nog leeg is, en dan hoort
// er gewoon alles te komen.
type Autocomplete func(query string, args map[string]any) ([]AutocompleteItem, error)

// AutocompleteItem is één keuze. ID is wat de kaart bewaart, Name wat de
// gebruiker leest.
type AutocompleteItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
}

// ---------------------------------------------------------------------------
// Wat vastligt
// ---------------------------------------------------------------------------

func (h *Stulp) AppID() string            { return h.host.AppID() }
func (h *Stulp) Manifest() map[string]any { return h.host.Manifest() }
func (h *Stulp) Env() map[string]any      { return h.host.Env() }
func (h *Stulp) Language() string         { return h.host.Language() }
func (h *Stulp) Timezone() string         { return h.host.Timezone() }

// ImageSources en ImageURL zijn het afbeeldingsregister van Stulp; zie Host.
func (h *Stulp) ImageSources() ([]ImageRegistration, error) { return h.host.ImageSources() }
func (h *Stulp) ImageURL(deviceID, slot string) (string, error) {
	return h.host.ImageURL(deviceID, slot)
}

func (h *Stulp) HomeDevices() []HomeDevice {
	ids := h.host.state.HomeDeviceIDs()
	result := make([]HomeDevice, 0, len(ids))
	for _, id := range ids {
		if device, ok := h.homeDevice(id); ok {
			result = append(result, device)
		}
	}
	return result
}

func (h *Stulp) homeDevice(id string) (HomeDevice, bool) {
	field := func(name string) any { value, _ := h.host.state.HomeDeviceField(id, name); return value }
	name, exists := field("name").(string)
	if !exists {
		return HomeDevice{}, false
	}
	appID, _ := field("appId").(string)
	class, _ := field("class").(string)
	available, _ := field("available").(bool)
	message, _ := field("unavailableMessage").(string)
	rawCapabilities, _ := field("capabilities").([]any)
	capabilities := make([]string, 0, len(rawCapabilities))
	for _, raw := range rawCapabilities {
		if capability, ok := raw.(string); ok {
			capabilities = append(capabilities, capability)
		}
	}
	state, _ := h.host.state.HomeDeviceMap(id, "state")
	return HomeDevice{ID: id, AppID: appID, Name: name, Class: class, Available: available,
		UnavailableMessage: message, Capabilities: capabilities, State: state}, true
}

func (h *Stulp) SetHomeCapability(deviceID, capabilityID string, value any) error {
	return h.host.SetHomeCapability(deviceID, capabilityID, value)
}

func (h *Stulp) OnHomeDeviceChanged(fn func(HomeDevice)) {
	h.mu.Lock()
	h.onHomeChange = fn
	h.mu.Unlock()
}

func (h *Stulp) homeDeviceChanged(deviceID string) {
	h.mu.Lock()
	fn := h.onHomeChange
	h.mu.Unlock()
	if fn == nil {
		return
	}
	if device, ok := h.homeDevice(deviceID); ok {
		fn(device)
	} else {
		fn(HomeDevice{ID: deviceID, Removed: true})
	}
}

// Translate lost een sleutel uit locales/ op, met {{tag}}-vervanging.
func (h *Stulp) Translate(key string, tags map[string]any) string {
	return h.host.Translate(key, tags)
}

// ---------------------------------------------------------------------------
// Instellingen van de app
// ---------------------------------------------------------------------------

func (h *Stulp) Setting(key string) (any, bool) { return h.host.Setting(key) }
func (h *Stulp) SettingKeys() []string          { return h.host.SettingKeys() }

func (h *Stulp) SetSetting(key string, value any) error { return h.host.SetSetting(key, value) }
func (h *Stulp) UnsetSetting(key string) error          { return h.host.UnsetSetting(key) }

// SettingText en SettingNumber halen eruit wat een plugin er werkelijk mee doet.
//
// Een instelling komt als any binnen -- JSON kent geen int -- en elke plugin
// schreef daar zijn eigen omzetting voor. Drie keer letterlijk hetzelfde
// settingText, twee keer hetzelfde settingNumber. Dat hoort hier, want het is
// steeds hetzelfde antwoord op dezelfde vraag: een adres is een tekst zonder
// spaties eromheen, en een poort is een positief getal of de standaard.
//
// De nil-controle staat erin omdat een plugin zijn instellingen soms al leest
// terwijl OnInit nog loopt.
func (h *Stulp) SettingText(key string) string {
	if h == nil {
		return ""
	}
	value, _ := h.Setting(key)
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// SettingNumber levert fallback als de instelling ontbreekt, geen getal is, of
// niet positief. Nul is bij een poort, een ritme of een wachttijd nooit wat
// iemand bedoelde.
func (h *Stulp) SettingNumber(key string, fallback int) int {
	if h == nil {
		return fallback
	}
	value, ok := h.Setting(key)
	if !ok {
		return fallback
	}
	switch number := value.(type) {
	case float64:
		if number > 0 {
			return int(number)
		}
	case int:
		if number > 0 {
			return number
		}
	}
	return fallback
}

// State levert de eigen state van deze plugin, of nil bij de eerste start.
//
// Settings zijn van de gebruiker; state is van de plugin. Wat hier in gaat komt
// niet in Manage en gaat door geen enkele API-route naar buiten -- dit is waar
// een token, een sessie of sleutelmateriaal thuishoort.
//
// Hij komt uit hetzelfde document als de rest, dus hij zit in een backup. Een
// plugin die dit in een eigen bestand zou zetten raakt het stilletjes kwijt bij
// een restore.
func (h *Stulp) State() json.RawMessage { return h.host.AppState() }

// SetState vervangt de state. Lege state wist hem.
func (h *Stulp) SetState(state json.RawMessage) error { return h.host.SetAppState(state) }

// OnSettingsChanged meldt dat de instellingen van de app veranderd zijn --
// door de settings-pagina, een andere app of de CLI. changed bevat alleen wat
// echt anders is.
//
// Dit is waar een plugin op reageert als de gebruiker een nieuwe sleutel of een
// ander adres invult: opnieuw verbinden hoort daar te gebeuren en niet pas bij
// de volgende herstart.
func (h *Stulp) OnSettingsChanged(fn func(changed map[string]any)) {
	h.mu.Lock()
	h.onChange = fn
	h.mu.Unlock()
}

func (h *Stulp) settingsChanged(changed map[string]any) {
	h.mu.Lock()
	fn := h.onChange
	h.mu.Unlock()
	if fn != nil {
		fn(changed)
	}
}

// OnRequest hangt een handler aan een naam die de settings-pagina kan aanroepen.
func (h *Stulp) OnRequest(name string, handle Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handlers == nil {
		h.handlers = map[string]Handler{}
	}
	h.handlers[name] = handle
}

func (h *Stulp) runRequest(name string, query, body map[string]any) (any, error) {
	h.mu.Lock()
	handle, ok := h.handlers[name]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no handler %q", name)
	}
	return handle(query, body)
}

// ---------------------------------------------------------------------------
// Flow
// ---------------------------------------------------------------------------

// OnFlowAction hangt een handler aan een DAN-kaart uit app.json.
//
// Wat de handler teruggeeft is het resultaat van de kaart: een volgende kaart in
// dezelfde Flow kan het als token gebruiken. Heeft de kaart niets op te leveren,
// geef dan nil terug.
func (h *Stulp) OnFlowAction(id string, run func(args, state map[string]any) (any, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cards["action:"+id] = &flowCard{kind: "action", id: id, action: run}
}

// OnFlowTrigger hangt een filter aan een ALS-kaart.
//
// Een trigger vuurt met een state; de handler zegt of díe gebeurtenis bij déze
// Flow hoort. Een deurbel die twee knoppen heeft vuurt één kaart, en de Flow
// kiest met zijn argument welke knop hij bedoelde:
//
//	h.OnFlowTrigger("pressed", func(args, state map[string]any) (bool, error) {
//		return args["button"] == state["button"], nil
//	})
//
// Zonder filter loopt elke Flow op elke gebeurtenis, en dat merkt de gebruiker
// als een lamp die aangaat om de verkeerde knop.
func (h *Stulp) OnFlowTrigger(id string, run func(args, state map[string]any) (bool, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cards["trigger:"+id] = &flowCard{kind: "trigger", id: id, condition: run}
}

// OnFlowCondition hangt een handler aan een EN-kaart. De handler zegt of de
// voorwaarde nu waar is.
func (h *Stulp) OnFlowCondition(id string, run func(args, state map[string]any) (bool, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cards["condition:"+id] = &flowCard{kind: "condition", id: id, condition: run}
}

// OnFlowAutocomplete hangt een keuzelijst aan één argument van een kaart.
//
// De kaart moet al geregistreerd zijn: de lijst hoort bij een kaart die iets
// doet, niet andersom.
func (h *Stulp) OnFlowAutocomplete(kind, cardID, argument string, complete Autocomplete) {
	h.mu.Lock()
	defer h.mu.Unlock()
	card, ok := h.cards[kind+":"+cardID]
	if !ok {
		return
	}
	if card.complete == nil {
		card.complete = map[string]Autocomplete{}
	}
	card.complete[argument] = complete
}

func (h *Stulp) runAutocomplete(kind, cardID, argument, query string, args map[string]any) (any, error) {
	h.mu.Lock()
	card, ok := h.cards[kind+":"+cardID]
	var complete Autocomplete
	if ok && card.complete != nil {
		complete = card.complete[argument]
	}
	h.mu.Unlock()
	if complete == nil {
		return nil, fmt.Errorf("card %q has no list for argument %q", cardID, argument)
	}
	items, err := complete(query, args)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []AutocompleteItem{}
	}
	return items, nil
}

// TriggerFlow vuurt een ALS-kaart af. tokens zijn de waarden die de kaart in het
// manifest belooft; state gaat naar de kaart-eigen filtering.
func (h *Stulp) TriggerFlow(id string, tokens, state map[string]any) error {
	return h.host.TriggerFlow("trigger", id, tokens, state)
}

// TriggerDeviceFlow vuurt een ALS-kaart die bij één apparaat hoort.
func (h *Stulp) TriggerDeviceFlow(id string, device *Device, tokens, state map[string]any) error {
	if state == nil {
		state = map[string]any{}
	}
	state["deviceId"] = device.ID()
	return h.host.TriggerFlow("device-trigger", id, tokens, state)
}

func (h *Stulp) runCard(kind, id string, args, state map[string]any) (any, error) {
	h.mu.Lock()
	card, ok := h.cards[kind+":"+id]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no %s card %q", kind, id)
	}
	switch {
	case card.action != nil:
		return card.action(args, state)
	case card.condition != nil:
		return card.condition(args, state)
	}
	return nil, fmt.Errorf("card %q has no handler", id)
}

func (h *Stulp) registrations() []flowRegistration {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.cards))
	for key := range h.cards {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]flowRegistration, 0, len(keys))
	for _, key := range keys {
		card := h.cards[key]
		arguments := make([]string, 0, len(card.complete))
		for argument := range card.complete {
			arguments = append(arguments, argument)
		}
		sort.Strings(arguments)
		out = append(out, flowRegistration{
			ID:           card.id,
			Type:         card.kind,
			RunListener:  true,
			Autocomplete: arguments,
		})
	}
	return out
}

// DeviceArg leest een device-argument van een Flow-kaart.
//
// Stulp geeft een gekozen apparaat door als {"$device": "<id>"}: de kaart bewaart
// een verwijzing en niet een kopie, zodat hernoemen de Flow niet breekt. Dit is
// de opzoeking die daarbij hoort.
func DeviceArg(args map[string]any, name string) string {
	switch value := args[name].(type) {
	case string:
		return value
	case map[string]any:
		id, _ := value["$device"].(string)
		return id
	}
	return ""
}

// Device levert de greep op één van de eigen apparaten, of nil als het er niet
// is. Handig voor een plugin die zijn apparaten niet per stuk vasthoudt maar ze
// bij naam opzoekt.
func (h *Stulp) Device(id string) *Device {
	if _, known := h.host.State().DeviceField(id, "name"); !known {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if device, ok := h.devices[id]; ok {
		return device
	}
	driverID, _ := h.host.State().DeviceField(id, "driverId")
	name, _ := driverID.(string)
	device := &Device{id: id, driverID: name, host: h.host, stulp: h, media: map[string]mediaSlot{}}
	if h.devices == nil {
		h.devices = map[string]*Device{}
	}
	h.devices[id] = device
	return device
}

// DeviceField leest één veld van een eigen apparaat uit de lokale kopie.
func (h *Stulp) DeviceField(id, field string) (any, bool) {
	return h.host.State().DeviceField(id, field)
}

// TriggerSystemFlow vuurt een kaart af die van Stulp zelf is in plaats van van
// deze app. Bedoeld voor een app die een ingebouwde kaart voedt.
func (h *Stulp) TriggerSystemFlow(cardType, cardID string, tokens, state any) error {
	return h.host.TriggerFlow(cardType, cardID, tokens, state)
}

// ReplaceDeviceReferences meldt dat een apparaat door een ander vervangen is,
// zodat Stulp de Flows die ernaar wezen bijwerkt.
//
// Een plugin kan Flows niet lezen of schrijven -- ook niet die van zichzelf. Dit
// is de enige mededeling die hij erover doet, en Stulp bepaalt wat ermee gebeurt.
func (h *Stulp) ReplaceDeviceReferences(replacements map[string]DeviceReplacement) error {
	return h.host.ReplaceDeviceReferences(replacements)
}

// DeviceReplacement zegt waar een apparaat naartoe ging en welke capabilities
// een andere naam kregen.
type DeviceReplacement struct {
	DeviceID     string            `json:"deviceId"`
	Capabilities map[string]string `json:"capabilities,omitempty"`
}

// DeviceIDs levert de apparaten van deze app, uit de lokale kopie.
func (h *Stulp) DeviceIDs() []string { return h.host.State().DeviceIDs() }

// DeviceName levert de naam van een apparaat van deze app. Flow-kaarten krijgen
// een device als id binnen, en dit is de opzoeking die daarbij hoort -- uit de
// lokale kopie, dus zonder vraag aan Stulp.
func (h *Stulp) DeviceName(deviceID string) string {
	value, _ := h.host.State().DeviceField(deviceID, "name")
	name, _ := value.(string)
	return name
}

// ---------------------------------------------------------------------------
// Rest
// ---------------------------------------------------------------------------

// Notify zet een bericht op de tijdlijn.
func (h *Stulp) Notify(excerpt string) error { return h.host.Notification(excerpt) }

func (h *Stulp) Log(message string)   { h.host.Log("info", message) }
func (h *Stulp) Error(message string) { h.host.Log("error", message) }
