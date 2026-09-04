package scene

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
)

// Numeric tolerance for "already at this value". Zigbee and Matter report
// levels in 1/254 steps and apps round when converting units, so a dim of 0.3
// comes back as 0.2992 and a scene must not treat that as a change. The
// absolute part covers small 0..1 ranges, the relative part larger scales such
// as temperatures and Kelvin. Anything a person would call a different setting
// is well outside both.
const (
	absoluteTolerance = 0.01
	relativeTolerance = 0.01
)

// sameValue reports whether two capability values mean the same setting.
// Numbers compare within tolerance regardless of their Go type; everything
// else must be deeply equal. When in doubt the answer is false, which makes
// the callers err on the side of sending a restore and not ending a scene.
func sameValue(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftNumber, leftIsNumber := numberOf(left)
	rightNumber, rightIsNumber := numberOf(right)
	if leftIsNumber || rightIsNumber {
		if !leftIsNumber || !rightIsNumber {
			return false
		}
		if math.IsNaN(leftNumber) || math.IsNaN(rightNumber) || math.IsInf(leftNumber, 0) || math.IsInf(rightNumber, 0) {
			return false
		}
		scale := math.Max(math.Abs(leftNumber), math.Abs(rightNumber))
		return math.Abs(leftNumber-rightNumber) <= absoluteTolerance+relativeTolerance*scale
	}
	return reflect.DeepEqual(left, right)
}

func numberOf(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		parsed, err := strconv.ParseFloat(typed.String(), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
