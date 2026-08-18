package flow

import (
	"reflect"
	"testing"
)

// A boolean capability reports the direction it moved, so "smoke alarm went
// off" is its own card. Anything else only reports that it changed.
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
		{"number changed", "measure_temperature", 21.5, 20.0, []string{"capability.measure_temperature.changed"}},
		{"string changed", "speaker_track", "b", "a", []string{"capability.speaker_track.changed"}},
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
