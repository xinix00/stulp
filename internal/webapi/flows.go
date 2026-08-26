package webapi

import (
	"context"
	"errors"

	"github.com/xinix00/stulp/internal/stulphttp"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/flow"
	"github.com/xinix00/stulp/internal/store"
)

func (s *Server) handleFlows() {
	s.mux.HandleFunc("POST /api/stulp/flow/autocomplete", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			AppID    string         `json:"appId"`
			CardID   string         `json:"cardId"`
			CardType string         `json:"cardType"`
			Argument string         `json:"argument"`
			Query    string         `json:"query"`
			Args     map[string]any `json:"args"`
		}
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		if body.AppID == "" || body.CardID == "" || body.Argument == "" {
			writeError(response, stulphttp.StatusBadRequest, errors.New("appId, cardId and argument are required"))
			return
		}
		values, err := s.supervisor.InvokeFlowAutocomplete(stulphttp.Context(request), body.AppID, body.CardType, body.CardID, body.Argument, body.Query, body.Args)
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, values)
	})
	s.mux.HandleFunc("GET /api/stulp/flow/cards", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		cards, err := s.flowCards(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, cards)
	})
	s.mux.HandleFunc("GET /api/manager/flow/flow", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		flows, err := s.store.Flows(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.showFlows(stulphttp.Context(request), flows))
	})
	s.mux.HandleFunc("POST /api/manager/flow/flow", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var definition store.Flow
		if err := decodeJSON(request, &definition); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		// Een drempel komt binnen in de eenheid van de gebruiker en gaat canoniek
		// het document in. Er is nog niets bewaard, dus er is ook niets om te
		// behouden.
		created, err := s.store.CreateFlow(stulphttp.Context(request), s.canonicalFlow(stulphttp.Context(request), definition, nil))
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusCreated, s.showFlow(stulphttp.Context(request), created, s.declaredArgumentUnits(stulphttp.Context(request))))
	})
	s.mux.HandleFunc("GET /api/manager/flow/flow/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		definition, err := s.store.Flow(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.showFlow(stulphttp.Context(request), definition, s.declaredArgumentUnits(stulphttp.Context(request))))
	})
	s.mux.HandleFunc("PUT /api/manager/flow/flow/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var definition store.Flow
		if err := decodeJSON(request, &definition); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		definition.ID = request.PathValue("id")
		// De bewaarde Flow gaat mee zodat een drempel die niemand aanraakte ook
		// niet verschuift: 12 m/s leest als 6 Bft, en 6 Bft terug is 10,8.
		previous, previousErr := s.store.Flow(stulphttp.Context(request), definition.ID)
		var before *store.Flow
		if previousErr == nil {
			before = &previous
		}
		updated, err := s.store.UpdateFlow(stulphttp.Context(request), s.canonicalFlow(stulphttp.Context(request), definition, before))
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.showFlow(stulphttp.Context(request), updated, s.declaredArgumentUnits(stulphttp.Context(request))))
	})
	s.mux.HandleFunc("DELETE /api/manager/flow/flow/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.store.DeleteFlow(stulphttp.Context(request), request.PathValue("id")); err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("PUT /api/manager/flow/flow/{id}/enabled", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeJSON(request, &body); err != nil || body.Enabled == nil {
			if err == nil {
				err = errors.New("enabled is required")
			}
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		if err := s.store.SetFlowEnabled(stulphttp.Context(request), request.PathValue("id"), *body.Enabled); err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("POST /api/stulp/flows/{id}/run", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		ctx, cancel := context.WithTimeout(stulphttp.Context(request), 45*time.Second)
		defer cancel()
		result, err := s.flows.Run(ctx, request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, result)
	})
}

func (s *Server) flowCards(ctx context.Context) (map[string][]map[string]any, error) {
	devices, err := s.store.Devices(ctx, "")
	if err != nil {
		return nil, err
	}
	result := map[string][]map[string]any{
		"triggers":   {builtinCapabilityTrigger(), builtinCapabilityStaysTrigger(), builtinMatterEventTrigger(), builtinTimeTrigger(), builtinSunTrigger("sunrise", "De zon komt op"), builtinSunTrigger("sunset", "De zon gaat onder")},
		"conditions": {builtinCapabilityCondition()},
		"actions":    {builtinCapabilityAction(), builtinDelayAction(), builtinNotificationAction()},
	}
	apps, err := s.store.Apps(ctx)
	if err != nil {
		return nil, err
	}
	for _, app := range apps {
		flowManifest, _ := app.Manifest["flow"].(map[string]any)
		if len(flowManifest) == 0 {
			continue
		}
		registrationMap := make(map[string]map[string]any)
		if registrations, registrationErr := s.supervisor.Registrations(ctx, app.ID); registrationErr == nil {
			for _, registration := range registrations.Flows {
				registrationMap[registration.Type+":"+registration.ID] = map[string]any{
					"type": registration.Type, "runListener": registration.RunListener,
					"autocomplete": registration.Autocomplete,
				}
			}
		}
		for _, definition := range []struct {
			manifestKey string
			resultKey   string
			cardType    string
		}{{"triggers", "triggers", "trigger"}, {"conditions", "conditions", "condition"}, {"actions", "actions", "action"}} {
			cards, _ := flowManifest[definition.manifestKey].([]any)
			for _, raw := range cards {
				card, _ := raw.(map[string]any)
				id, _ := card["id"].(string)
				if id == "" {
					continue
				}
				cardType := definition.cardType
				registration := registrationMap[cardType+":"+id]
				if definition.cardType == "trigger" {
					if deviceRegistration := registrationMap["device-trigger:"+id]; deviceRegistration != nil {
						cardType, registration = "device-trigger", deviceRegistration
					}
				}
				available := s.supervisor.State(app.ID).State == "running"
				if definition.cardType != "trigger" {
					available = available && registration != nil && registration["runListener"] == true
				}
				entry := map[string]any{
					"appId": app.ID, "appName": localized(app.Manifest["name"], s.options.Language),
					"id": id, "type": cardType, "title": localized(card["title"], s.options.Language),
					// titleFormatted is de kaart als zin, met [[argument]] erin.
					// Zonder dit door te geven staat er in een Flow "Windkracht komt
					// boven een grens" terwijl de gekozen grens ernaast in een veld
					// staat -- en dan lees je de Flow niet meer in één keer.
					"titleFormatted": localized(card["titleFormatted"], s.options.Language),
					// De argumenten in de eenheid van de gebruiker: grenzen,
					// sprong en label. Een huis dat Fahrenheit leest hoort geen
					// veld te krijgen dat van -40 tot 50 loopt.
					"hint": localized(card["hint"], s.options.Language), "args": s.showArguments(card["args"]),
					"tokens": card["tokens"], "available": available, "registration": registration,
				}
				// Only an app that limits a card to certain devices can say
				// which of them it applies to, so resolve that here rather
				// than re-deriving the filter grammar in the browser.
				annotateDeviceScope(entry, card["args"], devices)
				result[definition.resultKey] = append(result[definition.resultKey], entry)
			}
		}
	}
	for _, cards := range result {
		for _, card := range cards {
			if _, annotated := card["scope"]; !annotated {
				annotateDeviceScope(card, card["args"], devices)
			}
		}
	}
	for kind, cards := range s.capabilityCards(devices) {
		result[kind] = append(result[kind], cards...)
	}
	return result, nil
}

// capabilityCards derives Flow cards from what devices report, the way the
// does. An app declares "Doorbell pressed"; nobody declares "smoke alarm went
// off" because that follows from the alarm_smoke capability. Without these,
// half of what a device can tell you is only reachable through a generic
// "a value changed" card with a capability to guess.
func (s *Server) capabilityCards(devices []store.Device) map[string][]map[string]any {
	type entry struct {
		definition       map[string]any
		deviceIDs        []string
		setableDeviceIDs []string
	}
	byCapability := make(map[string]*entry)
	order := make([]string, 0, 16)
	for _, device := range devices {
		for _, capability := range device.Capabilities {
			definition := s.capabilityObject(device, capability, device.State[capability])
			existing, known := byCapability[capability]
			if !known {
				existing = &entry{definition: definition}
				byCapability[capability] = existing
				order = append(order, capability)
			}
			existing.deviceIDs = append(existing.deviceIDs, device.ID)
			if setable, _ := definition["setable"].(bool); setable {
				existing.setableDeviceIDs = append(existing.setableDeviceIDs, device.ID)
			}
		}
	}

	titles := make(map[string]string, len(order))
	for _, capability := range order {
		titles[capability] = capabilityDisplayTitle(capability, byCapability[capability].definition["title"], s.options.Language)
	}
	disambiguate(titles)

	result := map[string][]map[string]any{}
	for _, capability := range order {
		found := byCapability[capability]
		title := titles[capability]
		valueType, _ := found.definition["type"].(string)
		values, hasValues := found.definition["values"].([]any)

		card := func(kind, suffix, cardTitle string, extra ...map[string]any) {
			deviceIDs := found.deviceIDs
			if kind == "actions" {
				deviceIDs = found.setableDeviceIDs
			}
			args := []any{deviceArgument()}
			for _, argument := range extra {
				args = append(args, argument)
			}
			result[kind] = append(result[kind], map[string]any{
				"appId": "stulp", "appName": "Apparaat", "id": flow.CapabilityCardPrefix + capability + "." + suffix,
				"type": strings.TrimSuffix(kind, "s"), "title": cardTitle, "available": true,
				"capability": capability, "args": args,
				"scope": "device", "deviceArgument": "device", "deviceIds": deviceIDs,
				"tokens": []any{
					map[string]any{"name": "device", "type": "string", "title": "Apparaat"},
					map[string]any{"name": "value", "type": "string", "title": "Nieuwe waarde"},
					map[string]any{"name": "oldValue", "type": "string", "title": "Vorige waarde"},
				},
			})
		}
		valueArgument := map[string]any{"name": "value", "type": "capability-value", "title": "Waarde"}

		switch {
		case valueType == "boolean":
			// An alarm reads as an event, not as a switch being flipped.
			if strings.HasPrefix(capability, "alarm_") {
				card("triggers", "on", title+" ging af")
				card("triggers", "off", title+" is voorbij")
			} else {
				card("triggers", "on", title+" werd aan")
				card("triggers", "off", title+" werd uit")
			}
			card("conditions", "is", title+" is", valueArgument)
		case hasValues && len(values) > 0:
			card("triggers", "changed", title+" is veranderd")
			card("conditions", "is", title+" is", valueArgument)
		default:
			card("triggers", "changed", title+" is veranderd")
			card("conditions", "is", title+" is gelijk aan", valueArgument)
		}
		if len(found.setableDeviceIDs) > 0 {
			card("actions", "set", "Zet "+title, valueArgument)
		}
	}
	return result
}

// annotateDeviceScope records which devices a card can be used with. Cards
// without a device argument are app-wide; the rest carry the exact list, so
// the editor can offer a device's own cards instead of every card that
// exists.
func annotateDeviceScope(card map[string]any, args any, devices []store.Device) {
	filter, argumentName, hasDevice := deviceArgumentFilter(args)
	if !hasDevice {
		card["scope"] = "app"
		return
	}
	matching := make([]string, 0, len(devices))
	for _, device := range devices {
		if filter.matches(device) {
			matching = append(matching, device.ID)
		}
	}
	card["scope"] = "device"
	card["deviceArgument"] = argumentName
	card["deviceIds"] = matching
}

func builtinTimeTrigger() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "time_at", "type": "trigger",
		"title": "Het is een bepaald tijdstip", "available": true,
		"args": []any{map[string]any{"name": "time", "type": "time", "title": "Tijd"}},
		"tokens": []any{
			map[string]any{"name": "time", "type": "string", "title": "Tijd"},
			map[string]any{"name": "date", "type": "string", "title": "Datum"},
		},
	}
}

func builtinSunTrigger(id, title string) map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": id, "type": "trigger",
		"title": title, "available": true,
		"args": []any{
			map[string]any{"name": "latitude", "type": "number", "title": "Breedtegraad", "min": -90, "max": 90, "step": 0.0001},
			map[string]any{"name": "longitude", "type": "number", "title": "Lengtegraad", "min": -180, "max": 180, "step": 0.0001},
			map[string]any{"name": "offset", "type": "number", "title": "Verschuiving in minuten", "min": -180, "max": 180, "step": 1},
		},
		"tokens": []any{
			map[string]any{"name": "time", "type": "string", "title": "Tijd"},
			map[string]any{"name": "date", "type": "string", "title": "Datum"},
		},
	}
}

// matterEventChoices mirrors the names internal/matter/controller derives
// from the Switch and Door Lock clusters. Typing one of these by hand is
// guesswork, so the editor offers them.
func matterEventChoices() []any {
	choices := []any{map[string]any{"id": "", "title": "Elk event"}}
	for _, event := range []struct{ id, title string }{
		{"initial_press", "Knop ingedrukt"},
		{"short_release", "Knop losgelaten"},
		{"long_press", "Knop lang ingedrukt"},
		{"long_release", "Lange druk losgelaten"},
		{"multi_press_ongoing", "Meervoudige druk bezig"},
		{"multi_press_complete", "Meervoudige druk voltooid"},
		{"switch_latched", "Schakelaar vergrendeld"},
		{"lock_operation", "Slot bediend"},
		{"lock_operation_error", "Slotbediening mislukt"},
		{"door_state_change", "Deurstand veranderd"},
		{"door_lock_alarm", "Slotalarm"},
		{"lock_user_change", "Slotgebruiker gewijzigd"},
	} {
		choices = append(choices, map[string]any{"id": event.id, "title": event.title})
	}
	return choices
}

func builtinMatterEventTrigger() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "matter_event", "type": "trigger",
		"title": "Een Matter-event is ontvangen", "available": true,
		"args": []any{deviceArgument(),
			map[string]any{"name": "event", "type": "dropdown", "title": "Event", "values": matterEventChoices()}},
		"tokens": []any{
			map[string]any{"name": "device", "type": "string", "title": "Apparaat"},
			map[string]any{"name": "event", "type": "string", "title": "Event"},
			map[string]any{"name": "eventNumber", "type": "number", "title": "Eventnummer"},
			map[string]any{"name": "cluster", "type": "string", "title": "Cluster"},
			map[string]any{"name": "eventId", "type": "string", "title": "Event-ID"},
			map[string]any{"name": "data", "type": "json", "title": "Data"},
		},
	}
}

func builtinCapabilityTrigger() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "device_capability_changed", "type": "trigger",
		"title": "Een apparaatwaarde is veranderd", "available": true,
		"args": []any{deviceArgument(), capabilityArgument("Waarde", true)},
		"tokens": []any{
			map[string]any{"name": "device", "type": "string", "title": "Apparaat"},
			map[string]any{"name": "capability", "type": "string", "title": "Capability"},
			map[string]any{"name": "value", "type": "string", "title": "Nieuwe waarde"},
			map[string]any{"name": "oldValue", "type": "string", "title": "Vorige waarde"},
		},
	}
}

func builtinCapabilityStaysTrigger() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": flow.DeviceCapabilityStaysCardID, "type": "trigger",
		"title": "Een apparaatwaarde blijft gelijk", "available": true,
		"args": []any{
			deviceArgument(), capabilityArgument("Waarde", false),
			map[string]any{"name": "value", "type": "capability-value", "title": "Blijft gelijk aan"},
			map[string]any{"name": "seconds", "type": "number", "title": "Seconden", "min": 1, "max": 86400, "step": 1},
		},
		"tokens": []any{
			map[string]any{"name": "device", "type": "string", "title": "Apparaat"},
			map[string]any{"name": "capability", "type": "string", "title": "Capability"},
			map[string]any{"name": "value", "type": "string", "title": "Waarde"},
			map[string]any{"name": "seconds", "type": "number", "title": "Seconden"},
		},
	}
}

func builtinCapabilityCondition() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "device_capability_equals", "type": "condition",
		"title": "Een apparaatwaarde is gelijk aan", "available": true,
		"args": []any{deviceArgument(), capabilityArgument("Waarde", false),
			map[string]any{"name": "value", "type": "capability-value", "title": "Is gelijk aan"}},
	}
}

func builtinCapabilityAction() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "set_device_capability", "type": "action",
		"title": "Zet een apparaatwaarde", "available": true,
		"args": []any{deviceArgument(), capabilityArgument("Waarde", false),
			map[string]any{"name": "value", "type": "capability-value", "title": "Zet op"}},
	}
}

func builtinDelayAction() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "delay", "type": "action",
		"title": "Wacht even", "available": true,
		"args": []any{map[string]any{"name": "seconds", "type": "number", "title": "Seconden", "min": 0, "max": 30, "step": 0.1}},
	}
}

func builtinNotificationAction() map[string]any {
	return map[string]any{
		"appId": "stulp", "appName": "Stulp", "id": "notification", "type": "action",
		"title": "Maak een melding", "available": true,
		"args": []any{map[string]any{"name": "excerpt", "type": "text", "title": "Bericht"}},
	}
}

func deviceArgument() map[string]any {
	return map[string]any{"name": "device", "type": "device", "title": "Apparaat"}
}

// capabilityArgument is filled from the capabilities the chosen device
// actually reports, so nobody has to know that a dimmer is called "dim".
func capabilityArgument(title string, optional bool) map[string]any {
	return map[string]any{
		"name": "capability", "type": "capability", "title": title, "optional": optional,
	}
}
