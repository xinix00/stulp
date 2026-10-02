//! Gedeelde capabilitydefaults voor bediening, Flow-argumenten en tokenweergave.
use crate::{
    Result,
    json::{self, Number, Value},
};
/// Standaardmetadata voor ingebouwde capabilities; een app mag deze aanvullen.
pub fn defaults(result: &mut Value, base: &str) -> Result {
    if matches!(
        base,
        "onoff"
            | "button"
            | "locked"
            | "volume_mute"
            | "speaker_playing"
            | "speaker_shuffle"
            | "speaker_next"
            | "speaker_prev"
    ) {
        json::set(result, "type", json::string("boolean")?)?;
    }
    if matches!(base, "speaker_next" | "speaker_prev") {
        json::set(result, "getable", Value::Bool(false))?;
        json::set(result, "setable", Value::Bool(true))?;
    }
    if base == "speaker_repeat" {
        json::set(result, "setable", Value::Bool(true))?;
    }
    if matches!(
        base,
        "speaker_position"
            | "speaker_duration"
            | "measure_co2"
            | "measure_co"
            | "measure_pm25"
            | "measure_luminance"
            | "measure_voltage"
            | "measure_current"
            | "meter_power"
    ) {
        json::set(result, "min", Value::uint(0))?;
    }
    let (unit, step) = match base {
        "measure_temperature" | "target_temperature" => ("°C", 0.01),
        "measure_humidity" | "measure_battery" => ("%", 0.01),
        "measure_pressure" => ("hPa", 0.1),
        "measure_co2" | "measure_co" => ("ppm", 1.0),
        "measure_pm25" => ("µg/m³", 0.1),
        "measure_luminance" => ("lx", 1.0),
        "measure_power" => ("W", 0.001),
        "measure_current" => ("A", 0.001),
        "measure_voltage" => ("V", 0.001),
        "meter_power" => ("kWh", 0.001),
        "speaker_position" | "speaker_duration" => ("s", 1.0),
        _ => ("", 0.0),
    };
    if !unit.is_empty() {
        json::set(result, "type", json::string("number")?)?;
        json::set(result, "units", json::string(unit)?)?;
        json::set(result, "step", Value::Number(Number::Float(step)))?;
    }
    if matches!(
        base,
        "dim" | "light_hue" | "light_saturation" | "windowcoverings_set" | "volume_set"
    ) {
        json::set(result, "type", json::string("number")?)?;
        for (key, n) in [("min", 0.0), ("max", 1.0), ("step", 0.01)] {
            json::set(result, key, Value::Number(Number::Float(n)))?;
        }
    }
    if matches!(base, "measure_humidity" | "measure_battery") {
        json::set(result, "min", Value::uint(0))?;
        json::set(result, "max", Value::uint(100))?;
    }
    let values: &[(&str, &str, &str)] = match base {
        "windowcoverings_state" => &[
            ("up", "Omhoog", "Up"),
            ("idle", "Stop", "Stop"),
            ("down", "Omlaag", "Down"),
        ],
        "speaker_repeat" => &[
            ("none", "Uit", "Off"),
            ("track", "Eén nummer", "One track"),
            ("playlist", "Afspeellijst", "Playlist"),
        ],
        "air_quality_state" => &[
            ("unknown", "Onbekend", "Unknown"),
            ("good", "Goed", "Good"),
            ("fair", "Redelijk", "Fair"),
            ("moderate", "Matig", "Moderate"),
            ("poor", "Slecht", "Poor"),
            ("very_poor", "Zeer slecht", "Very poor"),
            ("extremely_poor", "Extreem slecht", "Extremely poor"),
        ],
        _ => &[],
    };
    if !values.is_empty() {
        let mut array = alloc::vec::Vec::new();
        for (id, nl, en) in values {
            json::push(
                &mut array,
                json::fields(&[
                    ("id", json::string(id)?),
                    (
                        "title",
                        json::fields(&[("nl", json::string(nl)?), ("en", json::string(en)?)])?,
                    ),
                ])?,
                8,
            )?;
        }
        json::set(result, "values", Value::Array(array))?;
        json::set(result, "type", json::string("enum")?)?;
    }
    Ok(())
}
