//! Volledige REST-waarnemingen en schaarse eventpatches blijven onderscheiden.
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Result, Transport, clone,
    util::{field, number},
};
pub(super) fn values(driver: &str, output: &str, item: &Value, full: bool) -> Result<Value> {
    let mut out = json::object();
    let mut boolean = |key, cap| -> Result {
        if let Some(v) = field(item, key).as_bool().or(full.then_some(false)) {
            json::set(&mut out, cap, Value::Bool(v))?;
        }
        Ok(())
    };
    match driver {
        "light" => {
            boolean("isLightOn", "onoff")?;
            boolean("isPirMotionDetected", "alarm_motion")?;
            if let Some(v) =
                number(field(field(item, "lightDeviceSettings"), "ledLevel")).or(full.then_some(1.))
            {
                json::set(
                    &mut out,
                    "dim",
                    stulp_sdk::util::float(v.clamp(1., 6.) / 6.)?,
                )?;
            }
        }
        "sensor" => {
            boolean("isOpened", "alarm_contact")?;
            boolean("isMotionDetected", "alarm_motion")?;
            if number(field(item, "tamperingDetectedAt")).is_some() {
                json::set(&mut out, "alarm_tamper", Value::Bool(true))?;
            } else if full {
                json::set(&mut out, "alarm_tamper", Value::Bool(false))?;
            }
            let battery = field(item, "batteryStatus");
            if let Some(v) = number(field(battery, "percentage")).or(full.then_some(0.)) {
                json::set(&mut out, "measure_battery", stulp_sdk::util::float(v)?)?;
            }
            if let Some(v) = field(battery, "isLow").as_bool().or(full.then_some(false)) {
                json::set(&mut out, "alarm_battery", Value::Bool(v))?;
            }
            for (key, cap) in [
                ("temperature", "measure_temperature"),
                ("humidity", "measure_humidity"),
                ("light", "measure_luminance"),
            ] {
                if let Some(v) = number(field(field(field(item, "stats"), key), "value")) {
                    json::set(&mut out, cap, stulp_sdk::util::float(v)?)?;
                }
            }
        }
        "chime" => {
            if let Some(v) = number(field(item, "volume")).or(full.then_some(0.)) {
                json::set(&mut out, "onoff", Value::Bool(v > 0.))?;
                json::set(&mut out, "volume_set", stulp_sdk::util::float(v / 100.)?)?;
            }
        }
        "relay" => {
            if let Some(o) = json::array(item, "outputs")
                .iter()
                .find(|o| json::text(o, "id") == output)
            {
                json::set(
                    &mut out,
                    "onoff",
                    Value::Bool(json::text(o, "state") == "on"),
                )?;
            }
        }
        _ => (),
    }
    Ok(out)
}
pub(super) async fn apply<T: Transport>(
    c: &mut Client<T>,
    id: &str,
    item: &Value,
    full: bool,
) -> Result {
    let d = c.state().device(id)?;
    let driver = json::text(d, "driverId");
    let output = json::text(field(d, "data"), "output");
    let values = values(driver, output, item, full)?;
    let availability = if driver == "camera" {
        field(item, "state")
            .as_str()
            .map(|s| s == "CONNECTED")
            .or(full.then_some(false))
    } else {
        field(item, "isConnected")
            .as_bool()
            .or(full.then_some(false))
    };
    let missing = driver == "relay"
        && full
        && !json::array(item, "outputs")
            .iter()
            .any(|o| json::text(o, "id") == output);
    let volume = if driver == "chime" {
        number(field(item, "volume")).filter(|v| *v > 0.)
    } else {
        None
    };
    let volume_changed =
        volume.is_some_and(|v| number(field(field(d, "store"), "lastVolume")) != Some(v));
    let mut changed = json::object();
    if let Some(values) = values.as_object() {
        for (k, v) in values.iter() {
            if !json::equal(v, field(field(d, "state"), k)) {
                json::set(&mut changed, k, clone(v)?)?;
            }
        }
    }
    if volume_changed && let Some(volume) = volume {
        c.store(
            id,
            json::fields(&[("lastVolume", stulp_sdk::util::float(volume)?)])?,
        )
        .await?;
    }
    if changed.as_object().is_some_and(|o| !o.is_empty()) {
        c.values(id, changed).await?;
    }
    if missing {
        c.unavailable(id, "Deze uitgang bestaat niet meer op het relais.")
            .await?;
    } else if let Some(available) = availability {
        if available {
            if !json::boolean(c.state().device(id)?, "available") {
                c.available(id, true).await?;
            }
        } else {
            c.unavailable(id, "Dit apparaat is niet verbonden met de console.")
                .await?;
        }
    }
    Ok(())
}
pub(super) fn audio(raw: &str) -> &str {
    match raw {
        "alrmSmoke" => "smoke",
        "alrmCmonx" => "co",
        "alrmSiren" => "siren",
        "alrmBabyCry" => "baby_cry",
        "alrmSpeak" => "speaking",
        "alrmBark" => "bark",
        "alrmBurglar" => "burglar_alarm",
        "alrmCarHorn" => "car_horn",
        "alrmGlassBreak" => "glass_break",
        _ => raw,
    }
}
