package flow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/plugin/plugintest"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/store/storetest"
	"github.com/xinix00/stulp/internal/supervisor"
)

func TestRunActionExecutesBuiltinWithoutPersistingFlow(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	engine := NewWithOptions(database, apps, Options{Ticks: make(chan time.Time)})
	defer engine.Close()

	result, err := engine.RunAction(ctx, store.FlowStep{
		AppID: "stulp", CardID: "notification", CardType: "action",
		Args: map[string]any{"excerpt": "Front door opened"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AppID != "stulp" || result.CardID != "notification" || result.CardType != "action" {
		t.Fatalf("unexpected action result metadata: %#v", result)
	}
	notification, ok := result.Result.(store.Notification)
	if !ok || notification.AppID != store.NativeMatterAppID || notification.Excerpt != "Front door opened" {
		t.Fatalf("unexpected action result: %#v", result.Result)
	}
	notifications, err := database.Notifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0] != notification {
		t.Fatalf("built-in notification was not executed: %#v", notifications)
	}
	flows, err := database.Flows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 0 {
		t.Fatalf("RunAction persisted a Flow: %#v", flows)
	}
}

func TestRunActionHonorsContextDeadline(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	engine := NewWithOptions(database, apps, Options{Ticks: make(chan time.Time)})
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := engine.RunAction(ctx, store.FlowStep{
		AppID: "stulp", CardID: "delay", CardType: "action",
		Args: map[string]any{"seconds": 1},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunAction did not stop at its context deadline: result=%#v err=%v", result, err)
	}
}

func TestSlowAutomaticRunDoesNotDelayNextSensorEdge(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallMatterApp(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Hall sensor", Class: "sensor",
		Data:         map[string]any{"node_id": "1", "endpoint": 1},
		Capabilities: []string{"alarm_motion", "alarm_contact", "onoff"},
		State:        map[string]any{"alarm_motion": false, "alarm_contact": false, "onoff": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.CreateFlow(ctx, store.Flow{
		Name: "Two sensor edges", Enabled: true,
		Nodes: []store.FlowNode{
			{ID: "motion", Step: store.FlowStep{
				AppID: "stulp", CardID: "capability.alarm_motion.on", CardType: "trigger",
				Args: map[string]any{"device": map[string]any{"$device": device.ID}},
			}},
			{ID: "slow", Step: store.FlowStep{
				AppID: "stulp", CardID: "capability.onoff.set", CardType: "action",
				Args: map[string]any{"device": map[string]any{"$device": device.ID}, "value": true},
			}},
			{ID: "contact", Step: store.FlowStep{
				AppID: "stulp", CardID: "capability.alarm_contact.on", CardType: "trigger",
				Args: map[string]any{"device": map[string]any{"$device": device.ID}},
			}},
			{ID: "fast", Step: store.FlowStep{
				AppID: "stulp", CardID: "capability.onoff.set", CardType: "action",
				Args: map[string]any{"device": map[string]any{"$device": device.ID}, "value": false},
			}},
		},
		Edges: []store.FlowEdge{{From: "motion", To: "slow"}, {From: "contact", To: "fast"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	slowStarted := make(chan struct{})
	fastStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var slowOnce, fastOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	defer release()

	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	engine := NewWithOptions(database, apps, Options{
		Ticks: make(chan time.Time),
		InvokeCapability: func(callCtx context.Context, _ string, _ string, value any, _ map[string]any) error {
			if value == true {
				slowOnce.Do(func() { close(slowStarted) })
				select {
				case <-releaseSlow:
					return nil
				case <-callCtx.Done():
					return callCtx.Err()
				}
			}
			fastOnce.Do(func() { close(fastStarted) })
			return nil
		},
	})
	defer engine.Close()

	device.State["alarm_motion"] = true
	if err := database.UpdateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("first sensor edge did not start its Flow")
	}

	device.State["alarm_contact"] = true
	if err := database.UpdateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fastStarted:
		// The first action is deliberately still blocked. Reaching this action
		// proves the store-event loop was free to capture and dispatch the edge.
	case <-time.After(250 * time.Millisecond):
		release()
		t.Fatal("second sensor edge waited behind the running Flow")
	}
	release()
}

func TestCapabilityStaysRequiresOneContinuousInterval(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallMatterApp(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Gang sensor", Class: "sensor",
		Data:         map[string]any{"node_id": "1", "endpoint": 1},
		Capabilities: []string{"alarm_motion"}, State: map[string]any{"alarm_motion": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.CreateFlow(ctx, store.Flow{
		Name: "Licht uit na aanhoudende rust", Enabled: true,
		Nodes: []store.FlowNode{
			{ID: "quiet", Step: store.FlowStep{
				AppID: "stulp", CardID: DeviceCapabilityStaysCardID, CardType: "trigger",
				Args: map[string]any{
					"device": map[string]any{"$device": device.ID}, "capability": "alarm_motion",
					"value": false, "seconds": 0.15,
				},
			}},
			{ID: "message", Step: store.FlowStep{
				AppID: "stulp", CardID: "notification", CardType: "action",
				Args: map[string]any{"excerpt": "Gang is rustig"},
			}},
		},
		Edges: []store.FlowEdge{{From: "quiet", To: "message"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	engine := NewWithOptions(database, apps, Options{Ticks: make(chan time.Time)})
	defer engine.Close()

	// Interrupt the first interval, then start a new quiet interval. The old
	// deadline must not be allowed to turn anything off early.
	time.Sleep(75 * time.Millisecond)
	device.State["alarm_motion"] = true
	if err := database.UpdateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	device.State["alarm_motion"] = false
	if err := database.UpdateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if notifications, listErr := database.Notifications(ctx, 10); listErr != nil || len(notifications) != 0 {
		t.Fatalf("interrupted interval fired at its old deadline: notifications=%#v err=%v", notifications, listErr)
	}

	deadline := time.Now().Add(time.Second)
	for {
		notifications, listErr := database.Notifications(ctx, 10)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(notifications) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("continuous matching interval did not fire")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A value that remains matching is one episode and must not repeat every
	// 150 ms.
	time.Sleep(200 * time.Millisecond)
	if notifications, listErr := database.Notifications(ctx, 10); listErr != nil || len(notifications) != 1 {
		t.Fatalf("one matching episode fired repeatedly: notifications=%#v err=%v", notifications, listErr)
	}
}

func TestConcurrentRunsKeepNewestFlowResult(t *testing.T) {
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	definition, err := database.CreateFlow(context.Background(), store.Flow{Name: "Concurrent result"})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	engine := NewWithOptions(database, apps, Options{Ticks: make(chan time.Time)})
	defer engine.Close()

	newer := time.Now().UTC()
	engine.recordFlowResult(definition.ID, newer, "newer run")
	engine.recordFlowResult(definition.ID, newer.Add(-time.Second), "older run finished late")

	stored, err := database.Flow(context.Background(), definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastRunAt != newer.Format(time.RFC3339Nano) || stored.LastError != "newer run" {
		t.Fatalf("late older run replaced newest result: %#v", stored)
	}
}

func TestExecutesPersistedFlowGraph(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	appManifest, root, err := manifest.Load(plugintest.Example(t, filepath.Join("..", "..", "examples", "virtual")))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InstallApp(ctx, appManifest, root, ""); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: appManifest.ID, DriverID: "switch", Name: "Flow switch", Class: "socket",
		Data: map[string]any{"id": "flow-switch"}, Capabilities: []string{"onoff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	if err := apps.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := apps.InvokeCapability(ctx, device.ID, "onoff", false, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	engine := New(database, apps)
	defer engine.Close()

	manual, err := database.CreateFlow(ctx, storetest.LinearFlow("Set the switch", false,
		store.FlowStep{AppID: "stulp", CardID: "device_capability_changed", CardType: "trigger"},
		[]store.FlowStep{{
			AppID: "stulp", CardID: "device_capability_equals", CardType: "condition",
			Args: map[string]any{"device": map[string]any{"$device": device.ID}, "capability": "onoff", "value": false},
		}},
		[]store.FlowStep{
			{AppID: appManifest.ID, CardID: "device_name", CardType: "action", Args: map[string]any{"device": map[string]any{"$device": device.ID}}},
			{AppID: "stulp", CardID: "set_device_capability", CardType: "action", Args: map[string]any{"device": map[string]any{"$device": device.ID}, "capability": "onoff", "value": true}},
		}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(ctx, manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Stopped || len(result.Actions) != 2 || result.Actions[0].Result != "Flow switch" {
		t.Fatalf("unexpected manual Flow result: %#v", result)
	}
	updated, err := database.Device(ctx, device.ID)
	if err != nil || updated.State["onoff"] != true {
		t.Fatalf("built-in capability action was not executed: state=%#v err=%v", updated.State, err)
	}

	automatic, err := database.CreateFlow(ctx, storetest.LinearFlow("Signal Flow", true,
		store.FlowStep{AppID: appManifest.ID, CardID: "signal", CardType: "trigger", Args: map[string]any{"match": "go"}},
		nil,
		[]store.FlowStep{{
			AppID: appManifest.ID, CardID: "ping", CardType: "action", Args: map[string]any{"value": "received:{{value}}"},
		}}))
	if err != nil {
		t.Fatal(err)
	}
	executed, err := engine.execute(ctx, automatic, Trigger{Tokens: map[string]any{"value": "woot"}, State: map[string]any{}})
	if err != nil || executed.Actions[0].Result != "pong:received:woot" {
		t.Fatalf("trigger token was not resolved: result=%#v err=%v", executed, err)
	}
	before, err := database.Flow(ctx, automatic.ID)
	if err != nil {
		t.Fatal(err)
	}
	matched, err := engine.matchesTrigger(ctx, automatic.Nodes[0].Step, Trigger{AppID: appManifest.ID, CardID: "signal", CardType: "trigger", State: map[string]any{"match": "stop"}})
	if err != nil || matched {
		t.Fatalf("app-owned trigger filter was ignored: matched=%v err=%v", matched, err)
	}
	matterStep := store.FlowStep{AppID: "stulp", CardID: "matter_event", CardType: "trigger",
		Args: map[string]any{"device": map[string]any{"$device": device.ID}, "event": "initial_press"}}
	matched, err = engine.matchesTrigger(ctx, matterStep, Trigger{AppID: "stulp", CardID: "matter_event",
		CardType: "trigger", DeviceID: device.ID, State: map[string]any{"event": "short_release"}})
	if err != nil || matched {
		t.Fatalf("Matter event name filter was ignored: matched=%v err=%v", matched, err)
	}
	matched, err = engine.matchesTrigger(ctx, matterStep, Trigger{AppID: "stulp", CardID: "matter_event",
		CardType: "trigger", DeviceID: device.ID, State: map[string]any{"event": "initial_press"}})
	if err != nil || !matched {
		t.Fatalf("matching Matter event was rejected: matched=%v err=%v", matched, err)
	}
	if err := database.RecordFlowEvent(ctx, appManifest.ID, "trigger", "signal", map[string]any{"value": "again"}, map[string]any{"match": "go"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, err := database.Flow(ctx, automatic.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.LastRunAt != before.LastRunAt {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recorded trigger did not execute the persisted Flow")
		}
		time.Sleep(10 * time.Millisecond)
	}

	graph, err := database.CreateFlow(ctx, store.Flow{
		Name: "Two starts and branches", Enabled: false,
		Nodes: []store.FlowNode{
			{ID: "start-one", Step: store.FlowStep{AppID: appManifest.ID, CardID: "signal", CardType: "trigger"}},
			{ID: "start-two", Y: 260, Step: store.FlowStep{AppID: appManifest.ID, CardID: "signal", CardType: "trigger"}},
			{ID: "false-condition", X: 400, Step: store.FlowStep{AppID: "stulp", CardID: "device_capability_equals", CardType: "condition", Args: map[string]any{
				"device": map[string]any{"$device": device.ID}, "capability": "onoff", "value": false,
			}}},
			{ID: "blocked", X: 800, Step: store.FlowStep{AppID: appManifest.ID, CardID: "ping", CardType: "action", Args: map[string]any{"value": "blocked"}}},
			{ID: "direct", X: 400, Y: 220, Step: store.FlowStep{AppID: appManifest.ID, CardID: "device_name", CardType: "action", Args: map[string]any{"device": map[string]any{"$device": device.ID}}}},
			{ID: "second", X: 400, Y: 440, Step: store.FlowStep{AppID: appManifest.ID, CardID: "ping", CardType: "action", Args: map[string]any{"value": "second"}}},
			{ID: "shared", X: 800, Y: 300, Step: store.FlowStep{AppID: appManifest.ID, CardID: "ping", CardType: "action", Args: map[string]any{"value": "shared"}}},
		},
		Edges: []store.FlowEdge{
			{From: "start-one", To: "false-condition"}, {From: "false-condition", To: "blocked"},
			{From: "start-one", To: "direct"}, {From: "start-two", To: "second"},
			{From: "start-one", To: "shared"}, {From: "start-two", To: "shared"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	branched, err := engine.Run(ctx, graph.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !branched.Success || branched.Stopped || len(branched.Conditions) != 1 || len(branched.Actions) != 3 {
		t.Fatalf("unexpected branched Flow result: %#v", branched)
	}
	results := map[any]bool{}
	for _, action := range branched.Actions {
		results[action.Result] = true
	}
	if !results["Flow switch"] || !results["pong:second"] || !results["pong:shared"] || results["pong:blocked"] {
		t.Fatalf("Flow branches executed the wrong endpoints: %#v", branched.Actions)
	}
}

func TestCapabilityActionUsesConfiguredInvokerForNativeMatter(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallMatterApp(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Matter lamp", Class: "light",
		Data: map[string]any{"node_id": "1", "endpoint": 1}, Capabilities: []string{"onoff"},
		State: map[string]any{"onoff": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	invocations := 0
	engine := NewWithOptions(database, apps, Options{
		Ticks: make(chan time.Time),
		InvokeCapability: func(_ context.Context, deviceID, capability string, value any, options map[string]any) error {
			invocations++
			if deviceID != device.ID || capability != "onoff" || value != true || len(options) != 0 {
				t.Fatalf("unexpected capability invocation: device=%q capability=%q value=%#v options=%#v", deviceID, capability, value, options)
			}
			return nil
		},
	})
	defer engine.Close()
	definition, err := database.CreateFlow(ctx, storetest.LinearFlow("Motion turns on Matter lamp", false,
		store.FlowStep{AppID: "stulp", CardID: "device_capability_changed", CardType: "trigger"},
		nil,
		[]store.FlowStep{{
			AppID: "stulp", CardID: "capability.onoff.set", CardType: "action",
			Args: map[string]any{"device": map[string]any{"$device": device.ID}, "value": true},
		}}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(ctx, definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || invocations != 1 {
		t.Fatalf("native Matter action was not dispatched once: result=%#v invocations=%d", result, invocations)
	}
}

func TestDerivedCommandActionSendsTrueWithoutAValueArgument(t *testing.T) {
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	called := false
	engine := NewWithOptions(database, apps, Options{
		Ticks: make(chan time.Time),
		InvokeCapability: func(_ context.Context, deviceID, capability string, value any, _ map[string]any) error {
			called = true
			if deviceID != "player" || capability != "speaker_next" || value != true {
				t.Fatalf("command = device %q capability %q value %#v", deviceID, capability, value)
			}
			return nil
		},
	})
	defer engine.Close()

	if _, err := engine.RunAction(context.Background(), store.FlowStep{
		AppID: "stulp", CardID: "capability.speaker_next.run", CardType: "action",
		Args: map[string]any{"device": map[string]any{"$device": "player"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("write-only command was not invoked")
	}
}

func TestDerivedBooleanCardsOfferDirectActionsAndConditions(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallMatterApp(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Blue indicator", Class: "light",
		Capabilities: []string{"onoff"}, State: map[string]any{"onoff": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	values := make([]any, 0, 3)
	engine := NewWithOptions(database, apps, Options{
		Ticks: make(chan time.Time),
		InvokeCapability: func(_ context.Context, deviceID, capability string, value any, _ map[string]any) error {
			if deviceID != device.ID || capability != "onoff" {
				t.Fatalf("invocation = device %q capability %q", deviceID, capability)
			}
			values = append(values, value)
			return nil
		},
	})
	defer engine.Close()

	deviceArg := map[string]any{"$device": device.ID}
	for _, cardID := range []string{"capability.onoff.turn_on", "capability.onoff.turn_off", "capability.onoff.toggle"} {
		if _, err := engine.RunAction(ctx, store.FlowStep{
			AppID: "stulp", CardID: cardID, CardType: "action", Args: map[string]any{"device": deviceArg},
		}); err != nil {
			t.Fatalf("%s: %v", cardID, err)
		}
	}
	if !reflect.DeepEqual(values, []any{true, false, true}) {
		t.Fatalf("boolean action values = %#v", values)
	}

	for cardID, want := range map[string]bool{
		"capability.onoff.is_on":  false,
		"capability.onoff.is_off": true,
	} {
		got, err := engine.runBuiltinCondition(ctx, cardID, map[string]any{"device": deviceArg})
		if err != nil || got != want {
			t.Fatalf("%s = %#v, %v; want %v", cardID, got, err, want)
		}
	}
}

func TestDerivedNumericConditionsReadTheCurrentValue(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InstallMatterApp(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: store.NativeMatterAppID, DriverID: "matter", Name: "Omvormer", Class: "sensor",
		Data: map[string]any{"node_id": "1", "endpoint": 1}, Capabilities: []string{"measure_power"},
		State: map[string]any{"measure_power": 125.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	engine := NewWithOptions(database, apps, Options{Ticks: make(chan time.Time)})
	defer engine.Close()

	for _, testCase := range []struct {
		cardID   string
		value    float64
		expected bool
	}{
		{"capability.measure_power.above", 100, true},
		{"capability.measure_power.above", 150, false},
		{"capability.measure_power.below", 150, true},
		{"capability.measure_power.below", 100, false},
	} {
		result, runErr := engine.runBuiltinCondition(ctx, testCase.cardID, map[string]any{
			"device": map[string]any{"$device": device.ID}, "value": testCase.value,
		})
		if runErr != nil || result != testCase.expected {
			t.Errorf("%s %v = %#v err=%v, want %v", testCase.cardID, testCase.value, result, runErr, testCase.expected)
		}
	}
}

func TestScheduledTimeFlowRunsOncePerMinuteAndCreatesNotification(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{})
	defer apps.Close()
	ticks := make(chan time.Time, 2)
	engine := NewWithOptions(database, apps, Options{Timezone: "Europe/Amsterdam", Ticks: ticks})
	defer engine.Close()

	_, err = database.CreateFlow(ctx, store.Flow{
		Name: "Middagmelding", Enabled: true,
		Nodes: []store.FlowNode{
			{ID: "clock", Step: store.FlowStep{AppID: "stulp", CardID: "time_at", CardType: "trigger", Args: map[string]any{"time": "12:34"}}},
			{ID: "pause", Step: store.FlowStep{AppID: "stulp", CardID: "delay", CardType: "action", Args: map[string]any{"seconds": 0.01}}},
			{ID: "message", Step: store.FlowStep{AppID: "stulp", CardID: "notification", CardType: "action", Args: map[string]any{"excerpt": "Het werkt om {{time}}"}}},
		},
		Edges: []store.FlowEdge{{From: "clock", To: "pause"}, {From: "pause", To: "message"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 8, 12, 34, 2, 0, location)
	ticks <- now
	deadline := time.Now().Add(2 * time.Second)
	for {
		notifications, listErr := database.Notifications(ctx, 10)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(notifications) == 1 {
			if notifications[0].Excerpt != "Het werkt om 12:34" {
				t.Fatalf("scheduled tokens were not resolved: %#v", notifications[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled Flow did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ticks <- now.Add(20 * time.Second)
	time.Sleep(50 * time.Millisecond)
	notifications, err := database.Notifications(ctx, 10)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("scheduled Flow ran twice in one minute: %#v err=%v", notifications, err)
	}
}

func TestSolarEventsUseConfiguredTimezone(t *testing.T) {
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, time.June, 21, 12, 0, 0, 0, location)
	sunrise, err := solarEvent(date, 52.3676, 4.9041, true, location)
	if err != nil {
		t.Fatal(err)
	}
	sunset, err := solarEvent(date, 52.3676, 4.9041, false, location)
	if err != nil {
		t.Fatal(err)
	}
	if sunrise.Hour() < 4 || sunrise.Hour() > 7 || sunset.Hour() < 20 || sunset.Hour() > 23 {
		t.Fatalf("unexpected Amsterdam solar times: sunrise=%s sunset=%s", sunrise, sunset)
	}
}
