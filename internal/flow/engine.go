// Package flow executes persisted plugin Flow card graphs against the live app
// runtimes. App code remains responsible for card-specific behavior.
package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
)

const executionTimeout = 45 * time.Second

type Options struct {
	Timezone         string
	Ticks            <-chan time.Time
	InvokeCapability func(context.Context, string, string, any, map[string]any) error
	// SendPush bezorgt een pushbericht bij de aangemelde telefoons. Het zit hier
	// als functie en niet als afhankelijkheid, omdat de engine niets van
	// pushdiensten hoeft te weten: hij weet alleen dat er een kaart is die iemand
	// buiten het huis kan bereiken. Leeg betekent dat die kaart niet kan.
	SendPush func(ctx context.Context, request PushRequest) (any, error)
	// ReadToken maakt van een tokenwaarde de tekst die een mens leest: in de
	// eenheid die dit huis gekozen heeft, met die eenheid erbij. Een bericht met
	// "het is nu {{temperature}}" hoort "het is nu 71.8 °F" te worden in een huis
	// dat Fahrenheit leest, en dat weet de motor niet zelf: hij vraagt het, zodat
	// de omrekening op één plek blijft staan. ok is vals als er niets om te rekenen
	// valt, en dan blijft de waarde zoals hij gemeten is.
	ReadToken func(source Trigger, token string, value any) (string, bool)
	// ArgumentWantsNumber zegt of dit argument van deze kaart een getal verwacht.
	// Zo ja, dan krijgt het de meting: een plugin rekent canoniek. Al het andere is
	// tekst voor een mens, en die leest in zijn eigen eenheid. Leeg betekent: alles
	// blijft zoals het gemeten is.
	ArgumentWantsNumber func(step store.FlowStep, argument string) bool
}

// PushRequest is wat de pushkaart vraagt.
//
// Een struct en geen rij losse parameters: er komt vroeg of laat nog een veld
// bij, en dan is een aanroep met vier lege strings niet meer te lezen.
type PushRequest struct {
	Title string
	Body  string
	// Target is één aangemeld toestel. Leeg is elk toestel.
	Target string
	// CameraID is het apparaat waarvan een momentopname mee moet. Leeg is geen
	// afbeelding.
	CameraID string
}

type Trigger struct {
	AppID    string
	CardID   string
	CardType string
	DeviceID string
	Tokens   map[string]any
	State    map[string]any
}

type StepResult struct {
	AppID    string `json:"appId"`
	CardID   string `json:"cardId"`
	CardType string `json:"cardType"`
	Passed   *bool  `json:"passed,omitempty"`
	Result   any    `json:"result,omitempty"`
}

type RunResult struct {
	FlowID     string       `json:"flowId"`
	Success    bool         `json:"success"`
	Stopped    bool         `json:"stopped"`
	Conditions []StepResult `json:"conditions"`
	Actions    []StepResult `json:"actions"`
	Error      string       `json:"error,omitempty"`
	RanAt      string       `json:"ranAt"`
}

type Engine struct {
	store            *store.Store
	apps             *supervisor.Supervisor
	events           <-chan store.Event
	cancel           func()
	done             chan struct{}
	closeOnce        sync.Once
	stateMu          sync.Mutex
	deviceState      map[string]map[string]any
	location         *time.Location
	ticks            <-chan time.Time
	stopTicks        func()
	scheduleMu       sync.Mutex
	scheduled        map[string]string
	invokeCapability func(context.Context, string, string, any, map[string]any) error
	readToken        func(Trigger, string, any) (string, bool)
	wantsNumber      func(store.FlowStep, string) bool
}

func New(database *store.Store, apps *supervisor.Supervisor) *Engine {
	return NewWithOptions(database, apps, Options{})
}

func NewWithOptions(database *store.Store, apps *supervisor.Supervisor, options Options) *Engine {
	events, cancel := database.Subscribe(256)
	location := time.UTC
	if options.Timezone != "" {
		if configured, err := time.LoadLocation(options.Timezone); err == nil {
			location = configured
		}
	}
	ticks := options.Ticks
	stopTicks := func() {}
	if ticks == nil {
		ticker := time.NewTicker(5 * time.Second)
		ticks = ticker.C
		stopTicks = ticker.Stop
	}
	engine := &Engine{
		store: database, apps: apps, events: events, cancel: cancel,
		done: make(chan struct{}), deviceState: make(map[string]map[string]any),
		location: location, ticks: ticks, stopTicks: stopTicks, scheduled: make(map[string]string),
		invokeCapability: options.InvokeCapability,
		readToken:        options.ReadToken, wantsNumber: options.ArgumentWantsNumber,
	}
	if engine.invokeCapability == nil {
		engine.invokeCapability = apps.InvokeCapability
	}
	engine.reloadDeviceState()
	go engine.loop()
	return engine
}

func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.stopTicks()
		e.cancel()
		<-e.done
	})
}

// reloadDeviceState replaces the cached capability values with what the store
// holds now. Triggers compare an update against this cache to decide what
// changed, so a stale entry is worse than an empty one.
func (e *Engine) reloadDeviceState() {
	devices, err := e.store.Devices(context.Background(), "")
	if err != nil {
		return
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.deviceState = make(map[string]map[string]any, len(devices))
	for _, device := range devices {
		e.deviceState[device.ID] = cloneMap(device.State)
	}
}

func (e *Engine) loop() {
	defer close(e.done)
	for {
		select {
		case event, open := <-e.events:
			if !open {
				return
			}
			switch {
			// The stream fell behind and was emptied. Every capability the
			// engine thinks it knows may have moved since, so comparing the
			// next update against that cache would invent or swallow an edge.
			case event.Manager == "store":
				e.reloadDeviceState()
			case event.Manager == "flow" && event.Type == "card.trigger":
				e.handleCardTrigger(event)
			case event.Manager == "devices" && event.Type == "device.create":
				if device, ok := event.Data.(store.Device); ok {
					e.setDeviceState(device.ID, device.State)
				}
			case event.Manager == "devices" && event.Type == "device.update":
				if device, ok := event.Data.(store.Device); ok {
					e.handleDeviceUpdate(device)
				}
			case event.Manager == "devices" && event.Type == "device.delete":
				e.stateMu.Lock()
				delete(e.deviceState, event.ID)
				e.stateMu.Unlock()
			}
		case now, open := <-e.ticks:
			if !open {
				e.ticks = nil
				continue
			}
			e.handleScheduledFlows(now)
		}
	}
}

// Reset clears transient execution memory after a live restore. A generic
// store.reload event can also mean an event subscriber fell behind, so this is
// explicit: clearing schedule de-duplication for ordinary overflow could run a
// time Flow twice in the same minute.
func (e *Engine) Reset() {
	e.reloadDeviceState()
	e.scheduleMu.Lock()
	e.scheduled = make(map[string]string)
	e.scheduleMu.Unlock()
}

func (e *Engine) handleScheduledFlows(now time.Time) {
	localNow := now.In(e.location)
	flows, err := e.store.Flows(context.Background())
	if err != nil {
		return
	}
	active := make(map[string]bool)
	for _, definition := range flows {
		if !definition.Enabled {
			continue
		}
		starts := make([]string, 0)
		for _, node := range definition.Nodes {
			if node.Step.AppID != "stulp" || node.Step.CardType != "trigger" || !isScheduledCard(node.Step.CardID) {
				continue
			}
			key := definition.ID + ":" + node.ID
			active[key] = true
			due, dueErr := scheduledFor(node.Step, localNow, e.location)
			if dueErr != nil || !due {
				continue
			}
			minute := localNow.Format("2006-01-02T15:04-0700")
			e.scheduleMu.Lock()
			alreadyRan := e.scheduled[key] == minute
			if !alreadyRan {
				e.scheduled[key] = minute
			}
			e.scheduleMu.Unlock()
			if !alreadyRan {
				starts = append(starts, node.ID)
			}
		}
		if len(starts) == 0 {
			continue
		}
		definition, starts := definition, starts
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), executionTimeout)
			defer cancel()
			input := Trigger{
				Tokens: map[string]any{"time": localNow.Format("15:04"), "date": localNow.Format("2006-01-02")},
				State:  map[string]any{"time": localNow.Format("15:04"), "date": localNow.Format("2006-01-02")},
			}
			_, _ = e.executeFrom(ctx, definition, input, starts)
		}()
	}
	e.scheduleMu.Lock()
	for key := range e.scheduled {
		if !active[key] {
			delete(e.scheduled, key)
		}
	}
	e.scheduleMu.Unlock()
}

func isScheduledCard(cardID string) bool {
	return cardID == "time_at" || cardID == "sunrise" || cardID == "sunset"
}

func scheduledFor(step store.FlowStep, now time.Time, location *time.Location) (bool, error) {
	var target time.Time
	switch step.CardID {
	case "time_at":
		value, _ := step.Args["time"].(string)
		parsed, err := time.Parse("15:04", value)
		if err != nil {
			return false, fmt.Errorf("time must use HH:MM")
		}
		target = time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, location)
	case "sunrise", "sunset":
		latitude, okLatitude := numberValue(step.Args["latitude"])
		longitude, okLongitude := numberValue(step.Args["longitude"])
		if !okLatitude || !okLongitude || latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
			return false, errors.New("valid latitude and longitude are required")
		}
		var err error
		target, err = solarEvent(now, latitude, longitude, step.CardID == "sunrise", location)
		if err != nil {
			return false, err
		}
		if offset, ok := numberValue(step.Args["offset"]); ok {
			target = target.Add(time.Duration(offset * float64(time.Minute)))
		}
	default:
		return false, nil
	}
	return now.Year() == target.Year() && now.YearDay() == target.YearDay() &&
		now.Hour() == target.Hour() && now.Minute() == target.Minute(), nil
}

func solarEvent(date time.Time, latitude, longitude float64, sunrise bool, location *time.Location) (time.Time, error) {
	day := float64(date.YearDay())
	longitudeHour := longitude / 15
	hour := 18.0
	if sunrise {
		hour = 6
	}
	approximate := day + (hour-longitudeHour)/24
	meanAnomaly := 0.9856*approximate - 3.289
	trueLongitude := normalizeDegrees(meanAnomaly + 1.916*math.Sin(degrees(meanAnomaly)) +
		0.020*math.Sin(degrees(2*meanAnomaly)) + 282.634)
	rightAscension := normalizeDegrees(radiansToDegrees(math.Atan(0.91764 * math.Tan(degrees(trueLongitude)))))
	rightAscension += math.Floor(trueLongitude/90)*90 - math.Floor(rightAscension/90)*90
	rightAscension /= 15
	sineDeclination := 0.39782 * math.Sin(degrees(trueLongitude))
	cosineDeclination := math.Cos(math.Asin(sineDeclination))
	cosineHour := (math.Cos(degrees(90.833)) - sineDeclination*math.Sin(degrees(latitude))) /
		(cosineDeclination * math.Cos(degrees(latitude)))
	if cosineHour > 1 || cosineHour < -1 {
		return time.Time{}, errors.New("the sun does not cross the horizon on this date")
	}
	hourAngle := radiansToDegrees(math.Acos(cosineHour))
	if sunrise {
		hourAngle = 360 - hourAngle
	}
	hourAngle /= 15
	localMeanTime := hourAngle + rightAscension - 0.06571*approximate - 6.622
	utcHours := normalizeHours(localMeanTime - longitudeHour)
	utcMidnight := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	return utcMidnight.Add(time.Duration(utcHours * float64(time.Hour))).In(location), nil
}

func degrees(value float64) float64          { return value * math.Pi / 180 }
func radiansToDegrees(value float64) float64 { return value * 180 / math.Pi }
func normalizeDegrees(value float64) float64 {
	return math.Mod(math.Mod(value, 360)+360, 360)
}
func normalizeHours(value float64) float64 { return math.Mod(math.Mod(value, 24)+24, 24) }

func numberValue(value any) (float64, bool) {
	switch current := value.(type) {
	case float64:
		return current, !math.IsNaN(current) && !math.IsInf(current, 0)
	case float32:
		return float64(current), true
	case int:
		return float64(current), true
	case int64:
		return float64(current), true
	case json.Number:
		parsed, err := current.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (e *Engine) handleCardTrigger(event store.Event) {
	data, ok := event.Data.(map[string]any)
	if !ok {
		return
	}
	input := Trigger{
		AppID: stringMapValue(data, "appId"), CardID: event.ID,
		CardType: stringMapValue(data, "cardType"),
		Tokens:   anyMap(data["tokens"]), State: anyMap(data["state"]),
	}
	input.DeviceID, _ = input.State["deviceId"].(string)
	e.runMatching(input)
}

func (e *Engine) handleDeviceUpdate(device store.Device) {
	e.stateMu.Lock()
	previous := e.deviceState[device.ID]
	e.deviceState[device.ID] = cloneMap(device.State)
	e.stateMu.Unlock()
	if previous == nil {
		return
	}
	for capability, value := range device.State {
		oldValue := previous[capability]
		if reflect.DeepEqual(oldValue, value) {
			continue
		}
		tokens := map[string]any{"device": device.Name, "deviceId": device.ID, "capability": capability, "value": value, "oldValue": oldValue}
		state := map[string]any{"deviceId": device.ID, "capability": capability, "value": value, "oldValue": oldValue}
		e.runMatching(Trigger{
			AppID: "stulp", CardID: "device_capability_changed", CardType: "trigger", DeviceID: device.ID,
			Tokens: tokens, State: state,
		})
		// Every capability turns into its own Flow card, so a smoke
		// alarm reads as "Rookalarm werd aan" instead of a generic value
		// change that has to be narrowed down by hand. Emitting the specific
		// card alongside the generic one keeps both working.
		for _, cardID := range CapabilityTriggerIDs(capability, value, oldValue) {
			e.runMatching(Trigger{
				AppID: "stulp", CardID: cardID, CardType: "trigger", DeviceID: device.ID,
				Tokens: tokens, State: state,
			})
		}
	}
}

// CapabilityCardPrefix marks the Flow cards Stulp derives from a device
// capability rather than from an app manifest.
const CapabilityCardPrefix = "capability."

// CapabilityTriggerIDs lists the derived trigger cards a capability change
// fires. A boolean reports the direction it moved; anything else reports
// that it changed.
func CapabilityTriggerIDs(capability string, value, oldValue any) []string {
	if boolean, ok := value.(bool); ok {
		if _, wasBoolean := oldValue.(bool); wasBoolean || oldValue == nil {
			if boolean {
				return []string{CapabilityCardPrefix + capability + ".on"}
			}
			return []string{CapabilityCardPrefix + capability + ".off"}
		}
	}
	return []string{CapabilityCardPrefix + capability + ".changed"}
}

// CapabilityFromCardID reports the capability and the suffix a derived card
// refers to.
func CapabilityFromCardID(cardID string) (capability, action string, ok bool) {
	rest, found := strings.CutPrefix(cardID, CapabilityCardPrefix)
	if !found {
		return "", "", false
	}
	index := strings.LastIndex(rest, ".")
	if index <= 0 || index == len(rest)-1 {
		return "", "", false
	}
	return rest[:index], rest[index+1:], true
}

func (e *Engine) setDeviceState(id string, state map[string]any) {
	e.stateMu.Lock()
	e.deviceState[id] = cloneMap(state)
	e.stateMu.Unlock()
}

func (e *Engine) runMatching(input Trigger) {
	flows, err := e.store.Flows(context.Background())
	if err != nil {
		return
	}
	for _, definition := range flows {
		if !definition.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), executionTimeout)
		starts := make([]string, 0)
		var matchErr error
		for _, node := range definition.Nodes {
			if !isTriggerStep(node.Step) || !sameCard(node.Step, input) {
				continue
			}
			var matched bool
			matched, matchErr = e.matchesTrigger(ctx, node.Step, input)
			if matchErr != nil {
				break
			}
			if matched {
				starts = append(starts, node.ID)
			}
		}
		if matchErr != nil {
			_ = e.store.SetFlowResult(context.Background(), definition.ID, time.Now(), matchErr.Error())
			cancel()
			continue
		}
		if len(starts) > 0 {
			_, _ = e.executeFrom(ctx, definition, input, starts)
		}
		cancel()
	}
}

func isTriggerStep(step store.FlowStep) bool {
	return step.CardType == "trigger" || step.CardType == "device-trigger"
}

func sameCard(step store.FlowStep, input Trigger) bool {
	return step.AppID == input.AppID && step.CardID == input.CardID && step.CardType == input.CardType
}

func (e *Engine) matchesTrigger(ctx context.Context, step store.FlowStep, input Trigger) (bool, error) {
	if selected := selectedDevice(step.Args); selected != "" && selected != input.DeviceID {
		return false, nil
	}
	if step.AppID == "stulp" {
		if capability, _ := step.Args["capability"].(string); capability != "" && capability != input.State["capability"] {
			return false, nil
		}
		if event, _ := step.Args["event"].(string); event != "" && event != input.State["event"] {
			return false, nil
		}
		return true, nil
	}
	registrations, err := e.apps.Registrations(ctx, step.AppID)
	if err != nil {
		return false, err
	}
	hasListener := false
	for _, registration := range registrations.Flows {
		if registration.ID == step.CardID && registration.Type == step.CardType {
			hasListener = registration.RunListener
			break
		}
	}
	if hasListener {
		args := e.resolveArgs(step, input)
		state := mergeState(input.State, e.resolveState(step.State, input))
		result, err := e.apps.InvokeFlow(ctx, step.AppID, step.CardType, step.CardID, args, state)
		if err != nil {
			return false, err
		}
		matched, ok := result.(bool)
		if !ok {
			return false, fmt.Errorf("trigger filter %s:%s did not return a boolean", step.AppID, step.CardID)
		}
		return matched, nil
	}
	for name, configured := range step.Args {
		if _, device := configured.(map[string]any); device {
			continue
		}
		if actual, exists := input.State[name]; exists && !equivalent(configured, actual) {
			return false, nil
		}
	}
	return true, nil
}

// Run executes conditions and actions immediately. It deliberately bypasses
// the trigger and enabled flag, making it useful as the editor's Test button.
func (e *Engine) Run(ctx context.Context, id string) (RunResult, error) {
	definition, err := e.store.Flow(ctx, id)
	if err != nil {
		return RunResult{}, err
	}
	return e.execute(ctx, definition, Trigger{Tokens: map[string]any{}, State: map[string]any{}})
}

func (e *Engine) execute(ctx context.Context, definition store.Flow, input Trigger) (RunResult, error) {
	starts := make([]string, 0)
	for _, node := range definition.Nodes {
		if isTriggerStep(node.Step) {
			starts = append(starts, node.ID)
		}
	}
	return e.executeFrom(ctx, definition, input, starts)
}

func (e *Engine) executeFrom(ctx context.Context, definition store.Flow, input Trigger, starts []string) (RunResult, error) {
	ranAt := time.Now().UTC()
	result := RunResult{FlowID: definition.ID, Success: false, RanAt: ranAt.Format(time.RFC3339Nano)}
	nodes := make(map[string]store.FlowNode, len(definition.Nodes))
	adjacency := make(map[string][]string, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range definition.Edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	visited := make(map[string]bool, len(definition.Nodes))
	queue := make([]string, 0)
	for _, start := range starts {
		if visited[start] {
			continue
		}
		visited[start] = true
		for _, next := range adjacency[start] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	blocked := false
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		node, exists := nodes[id]
		if !exists {
			continue
		}
		follow := true
		switch node.Step.CardType {
		case "condition":
			stepResult, passed, err := e.runCondition(ctx, node.Step, input)
			result.Conditions = append(result.Conditions, stepResult)
			if err != nil {
				result.Error = err.Error()
				_ = e.store.SetFlowResult(context.Background(), definition.ID, ranAt, result.Error)
				return result, err
			}
			follow = passed
			blocked = blocked || !passed
		case "action":
			stepResult, err := e.runAction(ctx, node.Step, input)
			result.Actions = append(result.Actions, stepResult)
			if err != nil {
				result.Error = err.Error()
				_ = e.store.SetFlowResult(context.Background(), definition.ID, ranAt, result.Error)
				return result, err
			}
		default:
			follow = false
		}
		if !follow {
			continue
		}
		for _, next := range adjacency[id] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	result.Success = true
	result.Stopped = blocked && len(result.Actions) == 0
	_ = e.store.SetFlowResult(context.Background(), definition.ID, ranAt, "")
	return result, nil
}

func (e *Engine) runCondition(ctx context.Context, step store.FlowStep, input Trigger) (StepResult, bool, error) {
	result := StepResult{AppID: step.AppID, CardID: step.CardID, CardType: step.CardType}
	var value any
	var err error
	args := e.resolveArgs(step, input)
	if step.AppID == "stulp" {
		value, err = e.runBuiltinCondition(ctx, step.CardID, args)
	} else {
		value, err = e.apps.InvokeFlow(ctx, step.AppID, "condition", step.CardID, args, mergeState(input.State, e.resolveState(step.State, input)))
	}
	if err != nil {
		return result, false, fmt.Errorf("condition %s:%s: %w", step.AppID, step.CardID, err)
	}
	passed, ok := value.(bool)
	if !ok {
		return result, false, fmt.Errorf("condition %s:%s did not return a boolean", step.AppID, step.CardID)
	}
	if step.Inverted {
		passed = !passed
	}
	result.Passed, result.Result = boolPointer(passed), value
	return result, passed, nil
}

func (e *Engine) runAction(ctx context.Context, step store.FlowStep, input Trigger) (StepResult, error) {
	result := StepResult{AppID: step.AppID, CardID: step.CardID, CardType: step.CardType}
	args := e.resolveArgs(step, input)
	if step.AppID == "stulp" {
		value, err := e.runBuiltinAction(ctx, step.CardID, args)
		result.Result = value
		if err != nil {
			return result, fmt.Errorf("action %s:%s: %w", step.AppID, step.CardID, err)
		}
		return result, nil
	}
	value, err := e.apps.InvokeFlow(ctx, step.AppID, "action", step.CardID, args, mergeState(input.State, e.resolveState(step.State, input)))
	result.Result = value
	if err != nil {
		return result, fmt.Errorf("action %s:%s: %w", step.AppID, step.CardID, err)
	}
	return result, nil
}

func (e *Engine) runBuiltinCondition(ctx context.Context, cardID string, args map[string]any) (any, error) {
	capability, _ := args["capability"].(string)
	if derived, action, ok := CapabilityFromCardID(cardID); ok && action == "is" {
		// A derived card names its capability in the card itself, so the
		// step only has to carry the device and the value to compare.
		capability, cardID = derived, "device_capability_equals"
	}
	if cardID != "device_capability_equals" {
		return nil, fmt.Errorf("unknown built-in condition %q", cardID)
	}
	deviceID := selectedDevice(args)
	if deviceID == "" || capability == "" {
		return nil, errors.New("device and capability are required")
	}
	device, err := e.store.Device(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	return equivalent(device.State[capability], args["value"]), nil
}

func (e *Engine) runBuiltinAction(ctx context.Context, cardID string, args map[string]any) (any, error) {
	capability, _ := args["capability"].(string)
	if derived, action, ok := CapabilityFromCardID(cardID); ok && action == "set" {
		capability, cardID = derived, "set_device_capability"
	}
	switch cardID {
	case "set_device_capability":
		deviceID := selectedDevice(args)
		if deviceID == "" || capability == "" {
			return nil, errors.New("device and capability are required")
		}
		if err := e.invokeCapability(ctx, deviceID, capability, args["value"], map[string]any{}); err != nil {
			return nil, err
		}
		return true, nil
	case "delay":
		seconds, ok := numberValue(args["seconds"])
		if !ok || seconds < 0 || seconds > 30 {
			return nil, errors.New("delay must be between 0 and 30 seconds")
		}
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-timer.C:
			return true, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case "notification":
		excerpt, _ := args["excerpt"].(string)
		return e.store.CreateNotification(ctx, store.NativeMatterAppID, excerpt)
	default:
		return nil, fmt.Errorf("unknown built-in action %q", cardID)
	}
}

// resolveArgs vult de tokens in de argumenten van één stap in.
//
// Per argument, want de bedoeling verschilt: een veld dat een getal verwacht
// krijgt de meting zoals Stulp die bewaart -- daar rekent een plugin mee -- en al
// het andere is tekst voor een mens en leest dus in de eenheid van dit huis. Zo
// wordt "boven {{temperature}}" een grens in graden Celsius en "het is nu
// {{temperature}}" een bericht met "71.8 °F".
func (e *Engine) resolveArgs(step store.FlowStep, input Trigger) map[string]any {
	if step.Args == nil {
		return map[string]any{}
	}
	resolved := make(map[string]any, len(step.Args))
	for name, value := range step.Args {
		resolved[name] = e.resolveValue(value, input, e.readsAsText(step, name, value))
	}
	return resolved
}

// readsAsText zegt of dit argument een zin voor een mens is. De vraag wordt
// alleen gesteld als er werkelijk een token in staat: bij een gewone waarde valt
// er niets in te vullen en dan hoeft er ook niets opgezocht te worden.
func (e *Engine) readsAsText(step store.FlowStep, argument string, value any) bool {
	if e.readToken == nil || e.wantsNumber == nil || !holdsToken(value) {
		return false
	}
	return !e.wantsNumber(step, argument)
}

func holdsToken(value any) bool {
	switch current := value.(type) {
	case string:
		return strings.Contains(current, "{{")
	case map[string]any:
		for _, child := range current {
			if holdsToken(child) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if holdsToken(child) {
				return true
			}
		}
	}
	return false
}

// resolveState vult de tokens in de verborgen staat van een stap in. Die is voor
// de plugin en niet voor een mens, dus daar blijft alles zoals het gemeten is.
func (e *Engine) resolveState(values map[string]any, input Trigger) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	resolved, _ := e.resolveValue(values, input, false).(map[string]any)
	return resolved
}

func (e *Engine) resolveValue(value any, input Trigger, asText bool) any {
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, child := range current {
			result[key] = e.resolveValue(child, input, asText)
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, child := range current {
			result[index] = e.resolveValue(child, input, asText)
		}
		return result
	case string:
		return e.resolveString(current, input, asText)
	default:
		return value
	}
}

func (e *Engine) resolveString(value string, input Trigger, asText bool) any {
	if strings.HasPrefix(value, "{{") && strings.HasSuffix(value, "}}") && strings.Count(value, "{{") == 1 {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "{{"), "}}"))
		if found, ok := lookup(name, input); ok {
			// Een veld dat een zin is leest ook één enkel token als zin: "{{value}}"
			// als heel bericht hoort "71.8 °F" te worden en niet "21.9".
			if asText {
				if text, converted := e.tokenText(input, name, found); converted {
					return text
				}
			}
			return found
		}
	}
	result := value
	for replacements := 0; replacements < 100; replacements++ {
		start := strings.Index(result, "{{")
		if start < 0 {
			break
		}
		end := strings.Index(result[start+2:], "}}")
		if end < 0 {
			break
		}
		end += start + 2
		name := strings.TrimSpace(result[start+2 : end])
		found, ok := lookup(name, input)
		replacement := ""
		if ok {
			replacement = fmt.Sprint(found)
			// Een token midden in een zin is altijd tekst: er staat immers al
			// tekst omheen.
			if text, converted := e.tokenText(input, name, found); converted {
				replacement = text
			}
		}
		result = result[:start] + replacement + result[end+2:]
	}
	return result
}

// tokenText vraagt Stulp hoe deze tokenwaarde gelezen hoort te worden.
func (e *Engine) tokenText(input Trigger, name string, value any) (string, bool) {
	if e == nil || e.readToken == nil {
		return "", false
	}
	return e.readToken(input, strings.TrimPrefix(strings.TrimPrefix(name, "tokens."), "state."), value)
}

func lookup(name string, input Trigger) (any, bool) {
	if strings.HasPrefix(name, "state.") {
		value, ok := input.State[strings.TrimPrefix(name, "state.")]
		return value, ok
	}
	if strings.HasPrefix(name, "tokens.") {
		value, ok := input.Tokens[strings.TrimPrefix(name, "tokens.")]
		return value, ok
	}
	if value, ok := input.Tokens[name]; ok {
		return value, true
	}
	value, ok := input.State[name]
	return value, ok
}

func mergeState(base, overlay map[string]any) map[string]any {
	result := cloneMap(base)
	for key, value := range overlay {
		result[key] = value
	}
	return result
}

func selectedDevice(args map[string]any) string {
	for _, value := range args {
		if reference, ok := value.(map[string]any); ok {
			if id, ok := reference["$device"].(string); ok {
				return id
			}
		}
	}
	return ""
}

func equivalent(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, child := range value {
		result[key] = child
	}
	return result
}

func anyMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return cloneMap(result)
}

func stringMapValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func boolPointer(value bool) *bool { return &value }
