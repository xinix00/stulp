package webapi

import "strings"

// The standard capabilities carry their own names; only app-defined ones
// carry a title in the manifest. Without this table a Flow card would read
// "alarm_smoke ging af" instead of "Rookalarm ging af", which is exactly the
// guesswork the card list is meant to remove.
//
// The set below covers what an ordinary home actually has. An unknown
// capability falls back to its identifier, which is still better than
// hiding it.
var standardCapabilityTitles = map[string]string{
	"onoff":             "Aan/uit",
	"dim":               "Dimniveau",
	"light_hue":         "Kleur",
	"light_saturation":  "Verzadiging",
	"light_temperature": "Kleurtemperatuur",
	"light_mode":        "Lichtmodus",
	"button":            "Knop",
	"locked":            "Vergrendeld",
	"lock_mode":         "Slotstand",
	"garagedoor_closed": "Garagedeur gesloten",

	"measure_temperature":   "Temperatuur",
	"measure_humidity":      "Luchtvochtigheid",
	"measure_pressure":      "Luchtdruk",
	"measure_luminance":     "Lichtsterkte",
	"measure_battery":       "Batterijniveau",
	"measure_power":         "Vermogen",
	"measure_voltage":       "Spanning",
	"measure_current":       "Stroom",
	"measure_co":            "Koolmonoxide",
	"measure_co2":           "Kooldioxide",
	"measure_pm25":          "Fijnstof",
	"measure_noise":         "Geluidsniveau",
	"measure_rain":          "Neerslag",
	"measure_wind_strength": "Windkracht",
	"measure_wind_angle":    "Windrichting",
	"measure_gust_strength": "Windstoten",
	"measure_water":         "Waterdoorstroming",
	"measure_ultraviolet":   "UV-index",

	"meter_power": "Energieverbruik",
	"meter_water": "Waterverbruik",
	"meter_gas":   "Gasverbruik",

	"alarm_motion":     "Bewegingsalarm",
	"alarm_contact":    "Contactalarm",
	"alarm_smoke":      "Rookalarm",
	"alarm_co":         "Koolmonoxidealarm",
	"alarm_co2":        "Kooldioxidealarm",
	"alarm_tamper":     "Sabotagealarm",
	"alarm_water":      "Wateralarm",
	"alarm_battery":    "Batterijalarm",
	"alarm_heat":       "Hittealarm",
	"alarm_fire":       "Brandalarm",
	"alarm_generic":    "Alarm",
	"alarm_pressure":   "Drukalarm",
	"alarm_night":      "Nachtalarm",
	"alarm_vibration":  "Trillingsalarm",
	"alarm_glassbreak": "Glasbreukalarm",

	"target_temperature":    "Streeftemperatuur",
	"thermostat_mode":       "Thermostaatstand",
	"volume_set":            "Volume",
	"volume_mute":           "Gedempt",
	"speaker_playing":       "Speelt af",
	"speaker_next":          "Volgende nummer",
	"speaker_prev":          "Vorige nummer",
	"speaker_shuffle":       "Willekeurig afspelen",
	"speaker_repeat":        "Herhalen",
	"speaker_position":      "Afspeelpositie",
	"speaker_duration":      "Duur",
	"speaker_track":         "Nummer",
	"speaker_artist":        "Artiest",
	"windowcoverings_set":   "Positie",
	"windowcoverings_state": "Stand",
	"homealarm_state":       "Alarmstand",
	"vacuumcleaner_state":   "Stofzuigerstand",
}

// capabilityDisplayTitle prefers the app's own title, falls back to the
// standard name, and only then to the raw identifier.
func capabilityDisplayTitle(capability string, manifestTitle any, language string) string {
	if title := localized(manifestTitle, language); title != "" && title != capability {
		return title
	}
	base, suffix, hasSuffix := strings.Cut(capability, ".")
	if title, known := standardCapabilityTitles[base]; known {
		if hasSuffix {
			return title + " " + suffix
		}
		return title
	}
	return capability
}

// disambiguate appends the identifier to titles that several capabilities
// share, so a device never shows the same card twice with no way to tell
// them apart. UniFi, for instance, gives last_motion_at and last_motion_date
// the same name.
func disambiguate(titles map[string]string) {
	counts := make(map[string]int, len(titles))
	for _, title := range titles {
		counts[title]++
	}
	for capability, title := range titles {
		if counts[title] > 1 {
			titles[capability] = title + " (" + capability + ")"
		}
	}
}
