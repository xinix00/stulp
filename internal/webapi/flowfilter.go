package webapi

import (
	"strings"

	"github.com/xinix00/stulp/internal/store"
)

// A plugin device argument carries a filter that says which devices the card
// applies to. That filter is what turns a flat list of app cards into
// "this is what your doorbell can tell you", so it has to be read exactly.
//
// The forms that occur in real app manifests:
//
//	driver_id=protectdoorbell                     one driver
//	driver_id=protectdoorbell|protectcamera       either driver
//	app_id=com.stulp.spotify&driver_id=player      one app's driver
//	driver_id=protectchime&capabilities=onoff     driver and capability
//	{"driver_id": "protectcamera"}                the same thing as an object
//
// Conditions are ANDed, the values within one condition are ORed.
type deviceFilter struct {
	conditions map[string][]string
}

func parseDeviceFilter(raw any) deviceFilter {
	filter := deviceFilter{conditions: make(map[string][]string)}
	switch value := raw.(type) {
	case nil:
		return filter
	case string:
		for _, condition := range strings.Split(value, "&") {
			key, values, found := strings.Cut(condition, "=")
			if !found {
				continue
			}
			filter.add(key, strings.Split(values, "|"))
		}
	case map[string]any:
		for key, entry := range value {
			switch typed := entry.(type) {
			case string:
				filter.add(key, strings.Split(typed, "|"))
			case []any:
				values := make([]string, 0, len(typed))
				for _, item := range typed {
					if text, ok := item.(string); ok {
						values = append(values, text)
					}
				}
				filter.add(key, values)
			}
		}
	}
	return filter
}

func (f deviceFilter) add(key string, values []string) {
	key = strings.TrimSpace(key)
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if key != "" && len(cleaned) > 0 {
		f.conditions[key] = append(f.conditions[key], cleaned...)
	}
}

// matches reports whether a device satisfies every condition. Unknown
// condition keys are ignored rather than treated as a mismatch: refusing to
// offer a card because of a filter key Stulp does not implement yet would
// hide working cards.
func (f deviceFilter) matches(device store.Device) bool {
	for key, values := range f.conditions {
		switch key {
		case "app_id":
			if !containsValue(values, device.AppID) {
				return false
			}
		case "driver_id":
			if !containsValue(values, driverName(device.DriverID)) {
				return false
			}
		case "capabilities":
			if !hasAnyCapability(device, values) {
				return false
			}
		case "class", "virtualClass":
			if !containsValue(values, device.Class) {
				return false
			}
		}
	}
	return true
}

// driverName strips the "stulp:app:<appId>:" prefix the Web API adds, so the
// bare driver ID from the manifest filter can be compared.
func driverName(driverID string) string {
	if index := strings.LastIndex(driverID, ":"); index >= 0 {
		return driverID[index+1:]
	}
	return driverID
}

func containsValue(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func hasAnyCapability(device store.Device, wanted []string) bool {
	for _, capability := range device.Capabilities {
		// Sub-capabilities are written as "onoff.socket1"; a filter on
		// "onoff" must still match those.
		base, _, _ := strings.Cut(capability, ".")
		if containsValue(wanted, capability) || containsValue(wanted, base) {
			return true
		}
	}
	return false
}

// deviceArgumentFilter returns the filter on a card's first device argument,
// plus whether the card has one at all. Cards without a device argument are
// app-wide: they fire for the whole integration, not for one device.
func deviceArgumentFilter(args any) (deviceFilter, string, bool) {
	list, _ := args.([]any)
	for _, raw := range list {
		argument, _ := raw.(map[string]any)
		if argumentType, _ := argument["type"].(string); argumentType != "device" {
			continue
		}
		name, _ := argument["name"].(string)
		return parseDeviceFilter(argument["filter"]), name, true
	}
	return deviceFilter{}, "", false
}
