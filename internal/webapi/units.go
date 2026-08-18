package webapi

import (
	"context"
	"math"
	"strings"

	flowengine "github.com/xinix00/stulp/internal/flow"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/units"
)

// De eenheden waarin dit huis leest.
//
// Plugins melden canoniek: graden Celsius, meters per seconde, millimeters,
// kilometers, hectopascal. Dat staat zo in het document, dat vergelijkt een Flow
// en dat bewaart de statistiek. Hier, op de rand tussen de kern en de browser,
// wordt het omgerekend naar wat de gebruiker gekozen heeft -- en op de weg terug
// weer canoniek gemaakt.
//
// Twee redenen om het hier te doen en niet in de plugins. Een plugin ziet alleen
// zijn eigen apparaten, dus zou elke plugin het los moeten leren en zou een huis
// met acht apps acht keer dezelfde omrekening dragen. En het document blijft
// canoniek, dus een andere keuze verandert geen enkele bewaarde waarde: geen
// herschreven geschiedenis, geen Flow-drempel die van betekenis verandert.

func (s *Server) userUnits() units.Set {
	s.unitsMu.RLock()
	defer s.unitsMu.RUnlock()
	return s.unitsSet
}

// rememberUnits laat een nieuwe keuze meteen gelden, zonder herstart.
func (s *Server) rememberUnits(set units.Set) {
	s.unitsMu.Lock()
	s.unitsSet = set
	s.unitsMu.Unlock()
}

// showUnits rekent een definitie om naar wat de gebruiker leest: de waarde, de
// grenzen, de sprong van een invoerveld en het label. Wat niet omgerekend wordt
// blijft precies staan zoals de app het declareerde.
func (s *Server) showUnits(definition map[string]any) {
	canonical := canonicalUnitOf(definition["units"])
	set := s.userUnits()
	if !set.Converts(canonical) {
		return
	}
	for _, key := range []string{"value", "min", "max"} {
		if number, ok := numberOf(definition[key]); ok {
			shown, _ := set.Show(number, canonical)
			definition[key] = shown
		}
	}
	if step, ok := set.Step(canonical); ok {
		definition["step"] = step
	}
	definition["units"] = set.Label(canonical)
}

// showArguments levert de argumenten van een Flow-kaart zoals de gebruiker ze
// leest.
//
// De definities komen uit het manifest van de app, en dat is een levende map in
// het geheugen van Stulp. Wat hier omgerekend wordt gaat dus eerst door een
// kopie: anders zou één GET het manifest zelf veranderen, en dan staat er na de
// tweede keer Fahrenheit in de installatie.
func (s *Server) showArguments(args any) any {
	list, ok := args.([]any)
	if !ok {
		return args
	}
	converted := false
	out := make([]any, 0, len(list))
	for _, raw := range list {
		argument, ok := raw.(map[string]any)
		if !ok || !s.userUnits().Converts(canonicalUnitOf(argument["units"])) {
			out = append(out, raw)
			continue
		}
		copied := make(map[string]any, len(argument))
		for key, value := range argument {
			copied[key] = value
		}
		s.showUnits(copied)
		out = append(out, copied)
		converted = true
	}
	if !converted {
		return args
	}
	return out
}

// canonicalUnitOf haalt de eenheid uit een definitie. Een app mag hem per taal
// opschrijven; een eenheid is in elke taal dezelfde, dus de eerste die er staat
// is goed.
func canonicalUnitOf(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, language := range []string{"en", "nl"} {
			if text, ok := typed[language].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func numberOf(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

// canonicalCapabilityValue maakt van een waarde die iemand intypte weer de
// eenheid waarin Stulp rekent. Dit hoort op de rand van de HTTP-laag en niet in
// invokeCapability: een Flow zet daar canonieke waarden in, en die zouden dan
// een tweede keer omgerekend worden.
func (s *Server) canonicalCapabilityValue(ctx context.Context, device store.Device, capability string, value any) any {
	number, ok := numberOf(value)
	if !ok {
		return value
	}
	canonical := s.deviceCapabilityUnit(ctx, device, capability)
	set := s.userUnits()
	if !set.Converts(canonical) {
		return value
	}
	return set.Canonical(number, canonical)
}

// deviceCapabilityUnit vraagt het aan de app van dit apparaat. Die is
// gezaghebbend: twee apps mogen dezelfde capability in een andere eenheid melden,
// en dan is die van de eigenaar de juiste.
func (s *Server) deviceCapabilityUnit(ctx context.Context, device store.Device, capability string) string {
	if app, err := s.store.App(ctx, device.AppID); err == nil {
		definitions, _ := app.Manifest["capabilities"].(map[string]any)
		definition, _ := definitions[capability].(map[string]any)
		if unit := canonicalUnitOf(definition["units"]); unit != "" {
			return unit
		}
	}
	probe := map[string]any{}
	applyDefaultCapabilityMetadata(probe, capability)
	return canonicalUnitOf(probe["units"])
}

// capabilityUnit is de canonieke eenheid van een capability: die van de kern als
// Stulp hem zelf kent, en anders die van de app die hem declareert.
func (s *Server) capabilityUnit(ctx context.Context, capability string) string {
	probe := map[string]any{}
	applyDefaultCapabilityMetadata(probe, capability)
	if unit := canonicalUnitOf(probe["units"]); unit != "" {
		return unit
	}
	apps, err := s.store.Apps(ctx)
	if err != nil {
		return ""
	}
	for _, app := range apps {
		definitions, _ := app.Manifest["capabilities"].(map[string]any)
		definition, _ := definitions[capability].(map[string]any)
		if unit := canonicalUnitOf(definition["units"]); unit != "" {
			return unit
		}
	}
	return ""
}

// stepUnits zegt welke argumenten van deze kaart een eenheid hebben, en welke.
//
// Drie soorten kaarten kunnen dat: een kaart van een app die het in app.json
// declareert, een van de kaarten die Stulp uit een capability afleidt (de
// capability staat in de id), en de algemene "zet een apparaatwaarde" (daar
// staat de capability in de argumenten van de stap zelf).
func (s *Server) stepUnits(ctx context.Context, step store.FlowStep, declared map[string]map[string]string) map[string]string {
	if found := declared[cardIndexKey(step.AppID, step.CardType, step.CardID)]; len(found) > 0 {
		return found
	}
	capability := ""
	switch {
	case strings.HasPrefix(step.CardID, flowengine.CapabilityCardPrefix):
		rest := strings.TrimPrefix(step.CardID, flowengine.CapabilityCardPrefix)
		if cut := strings.LastIndex(rest, "."); cut > 0 {
			capability = rest[:cut]
		}
	case step.CardID == "set_device_capability" || step.CardID == "device_capability_equals":
		capability, _ = step.Args["capability"].(string)
	}
	if capability == "" {
		return nil
	}
	if unit := s.capabilityUnit(ctx, capability); unit != "" {
		return map[string]string{"value": unit}
	}
	return nil
}

// declaredArgumentUnits leest uit elk app-manifest welke kaartargumenten een
// eenheid dragen. Uit het manifest en niet uit de kaartenlijst van de API: die
// is al omgerekend voor de weergave, en dan zou de weg terug van een
// omgerekende waarde uitgaan.
func (s *Server) declaredArgumentUnits(ctx context.Context) map[string]map[string]string {
	index := map[string]map[string]string{}
	apps, err := s.store.Apps(ctx)
	if err != nil {
		return index
	}
	for _, app := range apps {
		flowManifest, _ := app.Manifest["flow"].(map[string]any)
		for manifestKey, cardType := range map[string]string{
			"triggers": "trigger", "conditions": "condition", "actions": "action",
		} {
			cards, _ := flowManifest[manifestKey].([]any)
			for _, raw := range cards {
				card, _ := raw.(map[string]any)
				id, _ := card["id"].(string)
				if id == "" {
					continue
				}
				found := map[string]string{}
				arguments, _ := card["args"].([]any)
				for _, rawArgument := range arguments {
					argument, _ := rawArgument.(map[string]any)
					name, _ := argument["name"].(string)
					if unit := canonicalUnitOf(argument["units"]); name != "" && unit != "" {
						found[name] = unit
					}
				}
				if len(found) > 0 {
					index[cardIndexKey(app.ID, cardType, id)] = found
				}
			}
		}
	}
	return index
}

// cardIndexKey houdt een trigger en een conditie met dezelfde id uit elkaar. Een
// device-trigger is voor dit doel een trigger: het verschil zit in hoe hij
// aangeroepen wordt, niet in wat zijn argumenten betekenen.
func cardIndexKey(appID, cardType, cardID string) string {
	return appID + "\x00" + strings.TrimPrefix(cardType, "device-") + "\x00" + cardID
}

// showFlows levert Flows zoals de gebruiker ze leest.
func (s *Server) showFlows(ctx context.Context, flows []store.Flow) []store.Flow {
	if !s.convertsAnything() {
		return flows
	}
	declared := s.declaredArgumentUnits(ctx)
	out := make([]store.Flow, 0, len(flows))
	for _, definition := range flows {
		out = append(out, s.showFlow(ctx, definition, declared))
	}
	return out
}

// showFlow rekent de drempels van één Flow om naar de eenheid van de gebruiker.
//
// De nodes van een Flow uit de store delen hun argumentmappen met het document
// in het geheugen. Er wordt hier dus gekopieerd voordat er iets verandert; zonder
// die kopie zou het lezen van een Flow de drempels in de installatie zelf
// herschrijven, en dat is precies het soort stille schade waar niemand meer
// achter komt.
func (s *Server) showFlow(ctx context.Context, definition store.Flow, declared map[string]map[string]string) store.Flow {
	set := s.userUnits()
	nodes := make([]store.FlowNode, len(definition.Nodes))
	copy(nodes, definition.Nodes)
	touched := false
	for index := range nodes {
		found := s.stepUnits(ctx, nodes[index].Step, declared)
		if len(found) == 0 || len(nodes[index].Step.Args) == 0 {
			continue
		}
		args := map[string]any{}
		changed := false
		for name, value := range nodes[index].Step.Args {
			number, isNumber := numberOf(value)
			canonical := found[name]
			if !isNumber || !set.Converts(canonical) {
				args[name] = value
				continue
			}
			shown, _ := set.Show(number, canonical)
			args[name] = shown
			changed = true
		}
		if !changed {
			continue
		}
		nodes[index].Step.Args = args
		touched = true
	}
	if !touched {
		return definition
	}
	definition.Nodes = nodes
	return definition
}

// canonicalFlow is de weg terug: wat iemand in zijn eigen eenheid intypte wordt
// de eenheid waarin Stulp rekent.
//
// previous is de Flow zoals hij opgeslagen staat, en die is er om afrondingsdrift
// te voorkomen. Een drempel van 12 m/s leest als 6 Bft; zou iemand die Flow
// openen en op bewaren drukken zonder aan de wind te komen, dan zou 6 Bft
// terugkomen als 10,8 m/s en de drempel stilletjes verschuiven. Leest de
// binnenkomende waarde hetzelfde als de bewaarde, dan blijft de bewaarde staan.
func (s *Server) canonicalFlow(ctx context.Context, incoming store.Flow, previous *store.Flow) store.Flow {
	if !s.convertsAnything() {
		return incoming
	}
	set := s.userUnits()
	declared := s.declaredArgumentUnits(ctx)
	stored := map[string]store.FlowStep{}
	if previous != nil {
		for _, node := range previous.Nodes {
			stored[node.ID] = node.Step
		}
	}
	for index := range incoming.Nodes {
		node := &incoming.Nodes[index]
		found := s.stepUnits(ctx, node.Step, declared)
		if len(found) == 0 || len(node.Step.Args) == 0 {
			continue
		}
		before, hasBefore := stored[node.ID]
		for name, value := range node.Step.Args {
			number, isNumber := numberOf(value)
			canonical := found[name]
			if !isNumber || !set.Converts(canonical) {
				continue
			}
			if hasBefore && before.CardID == node.Step.CardID {
				if kept, ok := numberOf(before.Args[name]); ok {
					if shown, _ := set.Show(kept, canonical); nearly(shown, number) {
						node.Step.Args[name] = kept
						continue
					}
				}
			}
			node.Step.Args[name] = set.Canonical(number, canonical)
		}
	}
	return incoming
}

// convertsAnything zegt of er überhaupt iets te rekenen valt. Zonder keuze slaat
// dit alles in één keer over, en dan is een Flow lezen precies zo duur als
// voorheen.
func (s *Server) convertsAnything() bool {
	set := s.userUnits()
	for _, quantity := range units.Quantities() {
		if set.Converts(quantity.Canonical) {
			return true
		}
	}
	return false
}

// nearly vergelijkt twee getallen die uit een weergave komen. Een browser stuurt
// 71.8 terug als 71.80000000000001; dat is dezelfde waarde en geen wijziging.
func nearly(left, right float64) bool { return math.Abs(left-right) < 1e-6 }

// showMeasures rekent de gemeten waarden in het antwoord van een app-pagina om.
//
// Een plugin markeert zo'n waarde zelf, met `appsdk.Measure(21.2, "°C")`, en dat
// wordt hier `{"value": 71.8, "units": "°F", "text": "71.8 °F"}`. Een pagina
// drukt `text` af en hoeft niets te weten van eenheden; de plugin hoeft alleen te
// zeggen wát hij meet. Alleen gemarkeerde waarden worden aangeraakt -- een gewoon
// getal in een antwoord blijft een gewoon getal.
func (s *Server) showMeasures(value any) any {
	switch current := value.(type) {
	case map[string]any:
		if measured, ok := numberOf(current[measureMarker]); ok {
			canonical := canonicalUnitOf(current["units"])
			set := s.userUnits()
			shown, label := set.Show(measured, canonical)
			return map[string]any{
				"value": shown, "units": label, "text": set.Text(measured, canonical),
				// De meting gaat mee, zodat een pagina die zelf wil rekenen dat kan
				// zonder de omrekening terug te draaien.
				"measured": measured, "canonical": canonical,
			}
		}
		result := make(map[string]any, len(current))
		for key, child := range current {
			result[key] = s.showMeasures(child)
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, child := range current {
			result[index] = s.showMeasures(child)
		}
		return result
	}
	return value
}

// measureMarker is de afspraak waarmee een plugin een gemeten getal aanwijst. Een
// marker met een dollar, zoals `$device` in een Flow-argument: dan kan een
// antwoord nooit per ongeluk omgerekend worden omdat er een veld "value" in staat.
const measureMarker = "$measure"

// readFlowToken maakt van een tokenwaarde de tekst die een mens leest.
//
// Dit is wat de Flow-motor vraagt als hij een token in een zin zet. Welke eenheid
// een token heeft staat in app.json, naast de naam en de titel -- en voor de
// kaarten die Stulp uit een capability afleidt is het de eenheid van die
// capability, want `value` en `oldValue` zijn precies dat.
func (s *Server) readFlowToken(source flowengine.Trigger, token string, value any) (string, bool) {
	number, ok := numberOf(value)
	if !ok {
		return "", false
	}
	canonical := s.tokenUnit(context.Background(), source, token)
	if canonical == "" {
		return "", false
	}
	// Ook als er niets om te rekenen valt: een token waarvan de eenheid bekend is
	// leest met die eenheid erbij. "Buiten is het 21.5 °C" is een zin, "Buiten is
	// het 21.5" is een half bericht -- en het zou raar zijn dat het huis dat op
	// Fahrenheit staat wél zijn eenheid ziet en het huis op Celsius niet.
	return s.userUnits().Text(number, canonical), true
}

// tokenUnit zoekt de eenheid van één token.
func (s *Server) tokenUnit(ctx context.Context, source flowengine.Trigger, token string) string {
	// De afgeleide kaarten dragen hun capability in de id; hun waardetokens zijn
	// die capability. Zo leest "het is nu {{value}}" in de eenheid van de tegel
	// waar de Flow op wacht.
	if capability, _, ok := flowengine.CapabilityFromCardID(source.CardID); ok {
		if token == "value" || token == "oldValue" {
			return s.capabilityUnit(ctx, capability)
		}
	}
	if source.CardID == "device_capability_changed" && (token == "value" || token == "oldValue") {
		if capability, _ := source.Tokens["capability"].(string); capability != "" {
			return s.capabilityUnit(ctx, capability)
		}
	}
	app, err := s.store.App(ctx, source.AppID)
	if err != nil {
		return ""
	}
	flowManifest, _ := app.Manifest["flow"].(map[string]any)
	for _, manifestKey := range []string{"triggers", "conditions", "actions"} {
		cards, _ := flowManifest[manifestKey].([]any)
		for _, raw := range cards {
			card, _ := raw.(map[string]any)
			if id, _ := card["id"].(string); id != source.CardID {
				continue
			}
			tokens, _ := card["tokens"].([]any)
			for _, rawToken := range tokens {
				declared, _ := rawToken.(map[string]any)
				if name, _ := declared["name"].(string); name == token {
					return canonicalUnitOf(declared["units"])
				}
			}
		}
	}
	return ""
}

// flowArgumentWantsNumber zegt of dit argument van deze kaart een getal verwacht.
// Zo ja, dan hoort een token daar de meting in te zetten en niet een zin: een
// plugin vergelijkt met wat hij zelf meldde.
func (s *Server) flowArgumentWantsNumber(step store.FlowStep, argument string) bool {
	switch kind := s.flowArgumentKind(context.Background(), step, argument); kind {
	case "number", "range", "capability-value":
		return true
	default:
		return false
	}
}

func (s *Server) flowArgumentKind(ctx context.Context, step store.FlowStep, argument string) string {
	if step.AppID == "stulp" {
		return builtinArgumentKinds[step.CardID+"."+argument]
	}
	app, err := s.store.App(ctx, step.AppID)
	if err != nil {
		return ""
	}
	flowManifest, _ := app.Manifest["flow"].(map[string]any)
	for _, manifestKey := range []string{"triggers", "conditions", "actions"} {
		cards, _ := flowManifest[manifestKey].([]any)
		for _, raw := range cards {
			card, _ := raw.(map[string]any)
			if id, _ := card["id"].(string); id != step.CardID {
				continue
			}
			arguments, _ := card["args"].([]any)
			for _, rawArgument := range arguments {
				declared, _ := rawArgument.(map[string]any)
				if name, _ := declared["name"].(string); name == argument {
					kind, _ := declared["type"].(string)
					return kind
				}
			}
		}
	}
	return ""
}

// builtinArgumentKinds zijn de velden van de kaarten die Stulp zelf meebrengt.
// Ze staan hier omdat ze niet uit een manifest komen maar uit flows.go; een veld
// dat hier ontbreekt geldt als tekst, en dat is de veilige kant -- dan leest het
// als een zin en gaat er geen meting verloren.
var builtinArgumentKinds = map[string]string{
	"set_device_capability.value":     "capability-value",
	"device_capability_equals.value":  "capability-value",
	"device_capability_changed.value": "capability-value",
	"delay.seconds":                   "number",
	"sunrise.offset":                  "number",
	"sunset.offset":                   "number",
	"notification.excerpt":            "text",
	"push.title":                      "text",
	"push.message":                    "text",
}
