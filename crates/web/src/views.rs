//! Kleine tegel- en automatiseringsweergaven delen dezelfde capabilitydefinities.
use super::{capability, copy, objects};
use alloc::vec::Vec;
use stulp_core::{
    Result,
    json::{self, Value},
    store::{Storage, Store},
};
const PRIORITY: &[&str] = &[
    "alarm_smoke",
    "alarm_fire",
    "alarm_co",
    "alarm_co2",
    "alarm_vape",
    "alarm_water",
    "alarm_heat",
    "onoff",
    "locked",
    "garagedoor_closed",
    "windowcoverings_state",
    "homealarm_state",
    "alarm_motion",
    "alarm_contact",
    "alarm_glassbreak",
    "alarm_vibration",
    "alarm_generic",
    "alarm_pressure",
    "alarm_night",
    "speaker_playing",
    "volume_mute",
    "vacuumcleaner_state",
    "dim",
    "target_temperature",
    "measure_temperature",
    "thermostat_mode",
    "measure_humidity",
    "measure_co2",
    "measure_co",
    "measure_pm25",
    "air_quality_state",
    "measure_luminance",
    "measure_pressure",
    "measure_noise",
    "measure_rain",
    "measure_water",
    "measure_wind_strength",
    "measure_gust_strength",
    "measure_wind_angle",
    "measure_ultraviolet",
    "measure_battery",
    "alarm_battery",
    "alarm_tamper",
    "measure_power",
    "meter_power",
    "measure_current",
    "measure_voltage",
    "meter_water",
    "meter_gas",
    "volume_set",
    "speaker_track",
    "speaker_artist",
    "light_temperature",
    "light_mode",
    "light_hue",
    "light_saturation",
    "windowcoverings_set",
    "lock_mode",
    "button",
];
fn base(s: &str) -> &str {
    s.split('.').next().unwrap_or(s)
}
fn first<'a>(device: &'a Value, wanted: &str) -> Option<&'a str> {
    json::array(device, "capabilities")
        .iter()
        .filter_map(Value::as_str)
        .find(|id| base(id) == wanted)
}
fn primary<'a, S: Storage>(store: &Store<S>, device: &'a Value) -> Result<&'a str> {
    let battery = if json::text(device, "class") == "battery" {
        &["measure_power", "measure_battery", "battery_charging_state"][..]
    } else {
        &[]
    };
    for wanted in battery.iter().chain(PRIORITY) {
        if let Some(id) = first(device, wanted) {
            return Ok(id);
        }
    }
    let caps = json::array(device, "capabilities");
    if let Some(id) = caps
        .iter()
        .filter_map(Value::as_str)
        .find(|id| base(id).starts_with("alarm_"))
    {
        return Ok(id);
    }
    for id in caps.iter().filter_map(Value::as_str) {
        let object = capability::output(store, device, id)?;
        if json::boolean(&object, "setable") && json::text(&object, "type") == "boolean" {
            return Ok(id);
        }
    }
    for prefix in ["measure_", "meter_"] {
        if let Some(id) = caps
            .iter()
            .filter_map(Value::as_str)
            .find(|id| base(id).starts_with(prefix))
        {
            return Ok(id);
        }
    }
    Ok(caps.first().and_then(Value::as_str).unwrap_or(""))
}
pub(super) fn device<S: Storage>(store: &Store<S>, id: &str, view: &str) -> Result<Value> {
    if view.is_empty() {
        return objects::device(store, id);
    }
    let device = store.device(id)?;
    let mut out = json::object();
    for key in [
        "id",
        "appId",
        "groupId",
        "sortOrder",
        "name",
        "class",
        "available",
        "unavailableMessage",
    ] {
        json::set(&mut out, key, copy(json::get(&device, key))?)?;
    }
    let app = json::text(&device, "appId");
    let mut owner = json::copy("stulp:app:")?;
    owner
        .try_reserve(app.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    owner.push_str(app);
    json::set(&mut out, "ownerUri", json::string(&owner)?)?;
    let driver = json::text(&device, "driverId");
    owner
        .try_reserve(driver.len() + 1)
        .map_err(|_| stulp_core::Error::Memory)?;
    owner.push(':');
    owner.push_str(driver);
    json::set(&mut out, "driverId", json::string(&owner)?)?;
    if app == "com.stulp.scene" {
        let id = json::get(&device, "data")
            .map(|d| json::text(d, "sceneId"))
            .unwrap_or("");
        if !id.is_empty() {
            json::set(&mut out, "sceneId", json::string(id)?)?;
        }
    }
    let mut caps = Vec::new();
    if view == "overview" {
        let id = primary(store, &device)?;
        json::set(&mut out, "quickCapability", json::string(id)?)?;
        if !id.is_empty() {
            json::push(&mut caps, json::string(id)?, 2)?;
            if base(id) == "windowcoverings_state"
                && let Some(position) = first(&device, "windowcoverings_set")
            {
                json::push(&mut caps, json::string(position)?, 2)?;
            }
        }
    } else {
        for cap in json::array(&device, "capabilities") {
            json::push(&mut caps, copy(Some(cap))?, 4096)?;
        }
        json::set(
            &mut out,
            "manufacturer",
            json::string(objects::manufacturer(store, &device))?,
        )?;
    }
    let mut metadata = json::object();
    for cap in caps.iter().filter_map(Value::as_str) {
        json::set(&mut metadata, cap, capability::output(store, &device, cap)?)?;
    }
    json::set(&mut out, "capabilities", Value::Array(caps))?;
    json::set(&mut out, "capabilitiesObj", metadata)?;
    json::set(
        &mut out,
        "capabilitiesComplete",
        Value::Bool(view == "automation"),
    )?;
    json::set(&mut out, "detailComplete", Value::Bool(false))?;
    Ok(out)
}
pub(super) fn devices<S: Storage>(store: &Store<S>, view: &str) -> Result<Value> {
    let mut result = json::Object::new();
    for d in store.document().records("devices") {
        let id = json::text(d, "id");
        result.push(id, device(store, id, view)?)?;
    }
    Ok(Value::Object(result))
}
