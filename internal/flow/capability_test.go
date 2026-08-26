package flow

import (
	"context"
	"reflect"
	"testing"

	"github.com/xinix00/stulp/internal/store"
)

// A boolean capability reports the direction it moved, so "smoke alarm went
// off" is its own card. Numbers additionally report their direction so a
// configured threshold can decide whether it was actually crossed; text can
// match the new enum value.
func TestCapabilityTriggerIDs(t *testing.T) {
	cases := []struct {
		name       string
		capability string
		value      any
		oldValue   any
		want       []string
	}{
		{"boolean became true", "alarm_smoke", true, false, []string{"capability.alarm_smoke.on"}},
		{"boolean became false", "alarm_smoke", false, true, []string{"capability.alarm_smoke.off"}},
		{"boolean first reading", "alarm_motion", true, nil, []string{"capability.alarm_motion.on"}},
		{"number rose", "measure_temperature", 21.5, 20.0, []string{"capability.measure_temperature.changed", "capability.measure_temperature.rose_above"}},
		{"number fell", "measure_temperature", 19.5, 20.0, []string{"capability.measure_temperature.changed", "capability.measure_temperature.fell_below"}},
		{"number first reading", "measure_temperature", 21.5, nil, []string{"capability.measure_temperature.changed"}},
		{"string changed", "speaker_track", "b", "a", []string{"capability.speaker_track.changed", "capability.speaker_track.became"}},
		{"type switched to boolean", "onoff", true, "on", []string{"capability.onoff.changed"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CapabilityTriggerIDs(testCase.capability, testCase.value, testCase.oldValue)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestDerivedTriggersMatchEnumValuesAndThresholdCrossings(t *testing.T) {
	engine := &Engine{}
	for _, testCase := range []struct {
		name   string
		step   store.FlowStep
		input  Trigger
		wanted bool
	}{
		{"rose across", store.FlowStep{AppID: "stulp", CardID: "capability.measure_power.rose_above", CardType: "trigger", Args: map[string]any{"value": 10.0}}, Trigger{State: map[string]any{"oldValue": 5.0, "value": 12.0}}, true},
		{"already above", store.FlowStep{AppID: "stulp", CardID: "capability.measure_power.rose_above", CardType: "trigger", Args: map[string]any{"value": 10.0}}, Trigger{State: map[string]any{"oldValue": 11.0, "value": 12.0}}, false},
		{"fell across", store.FlowStep{AppID: "stulp", CardID: "capability.measure_power.fell_below", CardType: "trigger", Args: map[string]any{"value": 10.0}}, Trigger{State: map[string]any{"oldValue": 12.0, "value": 8.0}}, true},
		{"became target", store.FlowStep{AppID: "stulp", CardID: "capability.grid_status.became", CardType: "trigger", Args: map[string]any{"value": "on_grid"}}, Trigger{State: map[string]any{"oldValue": "off_grid", "value": "on_grid"}}, true},
		{"became another", store.FlowStep{AppID: "stulp", CardID: "capability.grid_status.became", CardType: "trigger", Args: map[string]any{"value": "on_grid"}}, Trigger{State: map[string]any{"oldValue": "on_grid", "value": "off_grid"}}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			matched, err := engine.matchesTrigger(context.Background(), testCase.step, testCase.input)
			if err != nil || matched != testCase.wanted {
				t.Fatalf("matched=%v err=%v, want %v", matched, err, testCase.wanted)
			}
		})
	}
}

func TestCapabilityFromCardID(t *testing.T) {
	cases := []struct {
		cardID     string
		capability string
		action     string
		ok         bool
	}{
		{"capability.alarm_smoke.on", "alarm_smoke", "on", true},
		{"capability.measure_temperature.changed", "measure_temperature", "changed", true},
		{"capability.onoff.set", "onoff", "set", true},
		// Sub-capabilities contain dots themselves; only the last one splits.
		{"capability.onoff.socket1.on", "onoff.socket1", "on", true},
		{"device_capability_changed", "", "", false},
		{"capability.", "", "", false},
		{"capability.onoff", "", "", false},
		{"capability.onoff.", "", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.cardID, func(t *testing.T) {
			capability, action, ok := CapabilityFromCardID(testCase.cardID)
			if ok != testCase.ok || capability != testCase.capability || action != testCase.action {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)",
					capability, action, ok, testCase.capability, testCase.action, testCase.ok)
			}
		})
	}
}

func TestDerivedStableBooleanTriggerNeedsOnlySeconds(t *testing.T) {
	deviceID, capability, target, seconds, _, ok := stabilityConfiguration(store.FlowStep{
		AppID: "stulp", CardID: "capability.alarm_motion.off_for", CardType: "trigger",
		Args: map[string]any{"device": map[string]any{"$device": "hall"}, "seconds": 120.0},
	})
	if !ok || deviceID != "hall" || capability != "alarm_motion" || target != false || seconds != 120 {
		t.Fatalf("stable trigger = device %q capability %q target %#v seconds %v ok=%v", deviceID, capability, target, seconds, ok)
	}
}

// Round-tripping guarantees the card the editor offers is the card the
// engine fires.
func TestCapabilityCardIDsRoundTrip(t *testing.T) {
	for _, capability := range []string{"alarm_smoke", "measure_temperature", "onoff.socket1"} {
		for _, ids := range [][]string{
			CapabilityTriggerIDs(capability, true, false),
			CapabilityTriggerIDs(capability, 1.0, 2.0),
		} {
			for _, id := range ids {
				decoded, action, ok := CapabilityFromCardID(id)
				if !ok || decoded != capability || action == "" {
					t.Fatalf("%q decoded as (%q, %q, %v)", id, decoded, action, ok)
				}
			}
		}
	}
}
