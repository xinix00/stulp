//! Capabilitymetadata komt van de driver; displayconversie verandert zijn meting niet.
use stulp_core::{
    Result,
    json::{self, Number, TryClone, Value},
    manifest,
    store::{Storage, Store},
    units,
};

pub(super) fn definition<S: Storage>(store: &Store<S>, device: &Value, id: &str) -> Result<Value> {
    let base = id.split('.').next().unwrap_or(id);
    let setable = matches!(
        base,
        "onoff"
            | "dim"
            | "locked"
            | "light_hue"
            | "light_saturation"
            | "volume_set"
            | "volume_mute"
            | "speaker_playing"
            | "speaker_next"
            | "speaker_prev"
            | "speaker_shuffle"
            | "speaker_repeat"
            | "windowcoverings_state"
    ) || base.ends_with("_set")
        || base.starts_with("target_");
    let current = json::get(device, "state").and_then(|v| json::get(v, id));
    let kind = match current {
        Some(Value::Bool(_)) => "boolean",
        Some(Value::Number(_)) => "number",
        _ if matches!(base, "onoff" | "button" | "locked") || base.starts_with("alarm_") => {
            "boolean"
        }
        _ if base == "dim"
            || base.starts_with("measure_")
            || base.starts_with("meter_")
            || base.starts_with("target_") =>
        {
            "number"
        }
        _ => "string",
    };
    let mut result = json::fields(&[
        ("id", json::string(id)?),
        (
            "title",
            json::string(&super::cards::title(id, &Value::Null, store.language())?)?,
        ),
        ("type", json::string(kind)?),
        ("getable", Value::Bool(true)),
        ("setable", Value::Bool(setable)),
        ("lastUpdated", Value::Null),
    ])?;
    defaults(&mut result, base)?;
    if json::text(device, "appId") == "com.stulp.scene" && base == "button" {
        json::set(&mut result, "getable", Value::Bool(false))?;
        json::set(&mut result, "setable", Value::Bool(true))?;
    }
    if let Some(app) = store.manifest(json::text(device, "appId")) {
        if let Some(definition) = json::get(app, "capabilities").and_then(|v| json::get(v, id)) {
            apply(&mut result, definition)?;
        }
        if let Some(driver) = json::array(app, "drivers")
            .iter()
            .find(|d| json::text(d, "id") == json::text(device, "driverId"))
            && let Some(options) =
                json::get(driver, "capabilitiesOptions").and_then(|v| json::get(v, id))
        {
            apply(&mut result, options)?;
        }
    }
    Ok(result)
}

pub(super) use stulp_core::capability::defaults;

fn apply(result: &mut Value, definition: &Value) -> Result {
    for key in [
        "type", "getable", "setable", "title", "desc", "values", "min", "max", "step", "units",
    ] {
        if let Some(value) = json::get(definition, key).filter(|v| !v.is_null()) {
            json::set(result, key, value.try_clone()?)?;
        }
    }
    Ok(())
}

fn choices<S: Storage>(store: &Store<S>) -> Option<&Value> {
    json::get(store.document().root(), "system").and_then(|v| json::get(v, "units"))
}

pub(super) fn output<S: Storage>(store: &Store<S>, device: &Value, id: &str) -> Result<Value> {
    let mut result = definition(store, device, id)?;
    let value = json::get(device, "state")
        .and_then(|v| json::get(v, id))
        .unwrap_or(&Value::Null)
        .try_clone()?;
    json::set(&mut result, "value", value)?;
    show_units(store, &mut result)?;
    Ok(result)
}

pub(super) fn show_units<S: Storage>(store: &Store<S>, result: &mut Value) -> Result {
    let unit = json::copy(
        json::get(result, "units")
            .map(|v| manifest::localized(v, "en"))
            .unwrap_or(""),
    )?;
    let empty = json::object();
    let choices = choices(store).unwrap_or(&empty);
    for key in ["value", "min", "max"] {
        if let Some(Value::Number(number)) = json::get(result, key)
            && units::step(choices, &unit).is_some()
        {
            let (value, _) = units::show(choices, number.as_f64(), &unit);
            json::set(result, key, Value::Number(Number::Float(value)))?;
        }
    }
    if let Some(step) = units::step(choices, &unit) {
        json::set(result, "step", Value::Number(Number::Float(step)))?;
        json::set(
            result,
            "units",
            json::string(units::show(choices, 0.0, &unit).1)?,
        )?;
    }
    Ok(())
}

/// Controleert een schrijfopdracht en rekent gebruikersinvoer terug naar de app-eenheid.
pub fn input<S: Storage>(
    store: &Store<S>,
    device: &Value,
    id: &str,
    value: &Value,
) -> Result<Value> {
    use stulp_core::Error;
    let definition = definition(store, device, id)?;
    if !json::boolean(&definition, "setable") {
        return Err(Error::Invalid("capability is read-only"));
    }
    let kind = json::text(&definition, "type");
    if (kind == "boolean" && value.as_bool().is_none())
        || (kind == "number" && !matches!(value, Value::Number(_)))
    {
        return Err(Error::Invalid("invalid capability value type"));
    }
    if kind == "enum"
        && !json::array(&definition, "values")
            .iter()
            .any(|v| json::get(v, "id").is_some_and(|id| json::equal(id, value)))
    {
        return Err(Error::Invalid("unknown capability enum value"));
    }
    if let Value::Number(n) = value {
        let empty = json::object();
        let choices = choices(store).unwrap_or(&empty);
        let unit = json::get(&definition, "units")
            .map(|v| manifest::localized(v, "en"))
            .unwrap_or("");
        let number = units::canonical(choices, n.as_f64(), unit);
        for (key, below) in [("min", true), ("max", false)] {
            if let Some(Value::Number(limit)) = json::get(&definition, key)
                && ((below && number < limit.as_f64()) || (!below && number > limit.as_f64()))
            {
                return Err(Error::Invalid("capability value out of range"));
            }
        }
        if units::step(choices, unit).is_some() {
            return Ok(Value::Number(Number::Float(number)));
        }
    }
    Ok(value.try_clone()?)
}
