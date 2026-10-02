//! Appmutaties hebben uitsluitend toegang tot de geauthenticeerde eigenaar.
use alloc::vec::Vec;
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_protocol::Frame;

pub(super) fn dispatch<S: Storage>(
    app: &str,
    store: &mut Store<S>,
    method: &str,
    params: &Value,
    time: (&str, u64),
    new_id: &str,
    out: &mut Vec<Value>,
) -> Result<Value> {
    let (now, clock) = time;
    match method {
        "images.list" => return store.image_sources(),
        "image.url" => {
            return store.share_image(
                new_id,
                json::text(params, "deviceId"),
                json::text(params, "slot"),
                clock,
            );
        }
        "device.replace" => store.replace_references(
            app,
            json::get(params, "replacements").unwrap_or(&Value::Null),
            now,
        )?,
        "flow.trigger" => store.trigger(app, params)?,
        "media.register" => store.register_media(app, params)?,
        "device.set" | "device.merge" | "capability.add" | "capability.remove" => {
            device(app, store, method, params, now, out)?
        }
        "setting.set" | "setting.unset" => {
            let key = json::text(params, "key");
            if key.is_empty() {
                return Err(Error::Invalid("setting key is required"));
            }
            let value = if method == "setting.set" {
                Some(
                    json::get(params, "value")
                        .unwrap_or(&Value::Null)
                        .try_clone()?,
                )
            } else {
                None
            };
            store.setting(app, key, value)?;
            json::push(
                out,
                Frame::request(
                    0,
                    "state.settings",
                    &super::app_field(store, "appSettings", app)?,
                )?,
                super::MAX_OUTBOX,
            )?;
        }
        "state.set" => store.app_state(
            app,
            json::get(params, "state")
                .unwrap_or(&Value::Null)
                .try_clone()?,
        )?,
        "notification" => {
            let notification = json::fields(&[
                ("id", json::string(new_id)?),
                ("appId", json::string(app)?),
                ("excerpt", json::string(json::text(params, "excerpt"))?),
            ])?;
            store.put("notifications", notification, true, None, now)?;
        }
        "capability.options" => {
            return Err(Error::Invalid(
                "capability options are declared in app.json and cannot be set at runtime",
            ));
        }
        _ => return Err(Error::Invalid("unknown app method")),
    }
    Ok(Value::Null)
}

fn device<S: Storage>(
    app: &str,
    store: &mut Store<S>,
    method: &str,
    params: &Value,
    now: &str,
    out: &mut Vec<Value>,
) -> Result {
    let id = json::text(params, "deviceId");
    let mut device = store.device(id)?;
    if json::text(&device, "appId") != app {
        return Err(Error::Invalid("device belongs to another app"));
    }
    let field = json::text(params, "field");
    match method {
        "device.set" => {
            let value =
                json::get(params, "value").ok_or(Error::Missing("device value is required"))?;
            if (matches!(field, "name" | "class" | "unavailableMessage")
                && value.as_str().is_some())
                || (field == "available" && value.as_bool().is_some())
            {
                json::set(&mut device, field, value.try_clone()?)?;
            } else {
                return Err(Error::Invalid("device field cannot be set to that value"));
            }
        }
        "device.merge" => {
            if !matches!(field, "settings" | "store" | "state") {
                return Err(Error::Invalid("device field is not a map"));
            }
            let patch = json::get(params, "patch")
                .and_then(Value::as_object)
                .ok_or(Error::Invalid("device patch must be an object"))?;
            let mut target = json::get(&device, field)
                .unwrap_or(&json::object())
                .try_clone()?;
            for (key, value) in patch.iter() {
                json::set(&mut target, key, value.try_clone()?)?;
            }
            json::set(&mut device, field, target)?;
        }
        "capability.add" | "capability.remove" => {
            let cap = json::text(params, "capability");
            if cap.is_empty() {
                return Err(Error::Invalid("capability id is required"));
            }
            let mut caps = Vec::new();
            for value in json::array(&device, "capabilities")
                .iter()
                .filter(|v| v.as_str() != Some(cap))
            {
                json::push(&mut caps, value.try_clone()?, 1024)?;
            }
            if method == "capability.add" {
                json::push(&mut caps, json::string(cap)?, 1024)?;
            }
            json::set(&mut device, "capabilities", Value::Array(caps))?;
        }
        _ => return Err(Error::Invalid("unknown device operation")),
    }
    if matches!(field, "state" | "available" | "unavailableMessage") {
        let state = json::get(&device, "state")
            .ok_or(Error::Missing("device state"))?
            .try_clone()?;
        store.observe(
            app,
            id,
            state,
            json::boolean(&device, "available"),
            json::text(&device, "unavailableMessage"),
        )?;
    } else {
        store.put("devices", device, false, None, now)?;
    }
    let updated = store.device(id)?;
    let event = json::fields(&[("deviceId", json::string(id)?), ("device", updated)])?;
    json::push(
        out,
        Frame::request(0, "state.device", &event)?,
        super::MAX_OUTBOX,
    )
}
