package scene

import (
	"encoding/json"
	"math"
	"testing"
)

func TestSameValueToleratesReportingStepsButNotChanges(t *testing.T) {
	cases := []struct {
		name        string
		left, right any
		want        bool
	}{
		{"same bool", true, true, true},
		{"other bool", true, false, false},
		{"level step", 0.3, 0.2992, true},        // 76/254
		{"small level step", 0.02, 0.0197, true}, // 5/254
		{"slider nudge", 0.3, 0.35, false},
		{"half degree rounding", 21.3, 21.5, true},
		{"thermostat step", 21.0, 21.5, false},
		{"kelvin rounding", 2700, 2703.0, true},
		{"integer and float", 3, 3.0, true},
		{"json number", json.Number("0.5"), 0.5, true},
		{"number versus text", 0.5, "0.5", false},
		{"same enum", "movie", "movie", true},
		{"other enum", "movie", "day", false},
		{"unknown versus known", nil, 0.5, false},
		{"both unknown", nil, nil, true},
		{"nan", math.NaN(), math.NaN(), false},
		{"infinite", math.Inf(1), math.Inf(1), false},
	}
	for _, tc := range cases {
		if got := sameValue(tc.left, tc.right); got != tc.want {
			t.Errorf("%s: sameValue(%v, %v) = %t, want %t", tc.name, tc.left, tc.right, got, tc.want)
		}
		if got := sameValue(tc.right, tc.left); got != tc.want {
			t.Errorf("%s: sameValue(%v, %v) is not symmetric", tc.name, tc.right, tc.left)
		}
	}
}
