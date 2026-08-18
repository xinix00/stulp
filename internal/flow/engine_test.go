package flow

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/plugin/plugintest"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/store/storetest"
	"github.com/xinix00/stulp/internal/supervisor"
)

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
