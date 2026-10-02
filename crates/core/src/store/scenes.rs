//! Scènes bewaren herstelstaat voordat een apparaat een opdracht ontvangt.
use super::{Storage, Store, replace_record};
use crate::{
    Error, Result,
    document::{Document, MAX_RECORDS},
    json::{self, TryClone, Value},
};
use alloc::vec::Vec;

pub(super) const APP_ID: &str = "com.stulp.scene";

pub(super) fn validate(scene: &mut Value, previous: Option<&Value>) -> Result {
    if previous.is_some_and(|s| json::boolean(s, "active")) {
        return Err(Error::Conflict("active scene cannot be edited or deleted"));
    }
    let name = json::copy(json::text(scene, "name").trim())?;
    if name.is_empty() || name.len() > 160 {
        return Err(Error::Invalid("scene name must contain 1 to 160 bytes"));
    }
    json::set(scene, "name", json::string(&name)?)?;
    if json::text(scene, "kind").is_empty() {
        json::set(scene, "kind", json::string("switch")?)?;
    }
    if !matches!(json::text(scene, "kind"), "switch" | "button") {
        return Err(Error::Invalid("scene kind must be switch or button"));
    }
    let states = json::array(scene, "states");
    if states.is_empty() {
        return Err(Error::Invalid("scene needs at least one state"));
    }
    validate_states(states)?;
    json::set(scene, "active", Value::Bool(false))?;
    json::set(scene, "previous", Value::Array(Vec::new()))
}

pub(super) fn validate_states(states: &[Value]) -> Result {
    if states.len() > 256 {
        return Err(Error::Full);
    }
    for (index, state) in states.iter().enumerate() {
        if json::text(state, "deviceId").trim().is_empty()
            || json::text(state, "capabilityId").trim().is_empty()
        {
            return Err(Error::Invalid(
                "scene state requires deviceId and capabilityId",
            ));
        }
        if states.iter().take(index).any(|s| {
            json::text(s, "deviceId") == json::text(state, "deviceId")
                && json::text(s, "capabilityId") == json::text(state, "capabilityId")
        }) {
            return Err(Error::Conflict("scene has duplicate state"));
        }
    }
    Ok(())
}

pub(super) fn device_id(id: &str) -> Result<alloc::string::String> {
    let mut out = json::copy("scene:")?;
    out.try_reserve(id.len()).map_err(|_| Error::Memory)?;
    out.push_str(id);
    Ok(out)
}

pub(super) fn reconcile(doc: &mut Document) -> Result {
    let mut records = Vec::new();
    for record in doc
        .records("devices")
        .iter()
        .filter(|d| json::text(d, "appId") != APP_ID)
    {
        json::push(&mut records, record.try_clone()?, MAX_RECORDS)?;
    }
    for scene in doc.records("scenes") {
        let id = device_id(json::text(scene, "id"))?;
        let mut device = match doc.record("devices", &id) {
            Ok(d) if json::text(d, "appId") == APP_ID => d.try_clone()?,
            Ok(_) => return Err(Error::Conflict("scene id collides with a physical device")),
            Err(_) => json::object(),
        };
        for (key, value) in [
            ("id", id.as_str()),
            ("appId", APP_ID),
            ("driverId", "scene"),
            ("class", "scene"),
            ("name", json::text(scene, "name")),
        ] {
            json::set(&mut device, key, json::string(value)?)?;
        }
        let cap = if json::text(scene, "kind") == "button" {
            "button"
        } else {
            "onoff"
        };
        let mut capabilities = Vec::new();
        json::push(&mut capabilities, json::string(cap)?, 1)?;
        json::set(&mut device, "capabilities", Value::Array(capabilities))?;
        json::set(
            &mut device,
            "data",
            json::fields(&[("sceneId", json::string(json::text(scene, "id"))?)])?,
        )?;
        json::push(&mut records, device, MAX_RECORDS)?;
    }
    doc.replace("devices", records)
}

pub(super) fn can_delete(doc: &Document, scene: &Value) -> Result {
    if json::boolean(scene, "active") {
        return Err(Error::Conflict("active scene cannot be edited or deleted"));
    }
    let id = device_id(json::text(scene, "id"))?;
    if doc.records("flows").iter().any(|f| references(f, &id)) {
        return Err(Error::Conflict("scene is used by a flow"));
    }
    Ok(())
}

fn references(value: &Value, id: &str) -> bool {
    match value {
        Value::Object(o) => {
            json::text(value, "$device") == id || o.iter().any(|(_, v)| references(v, id))
        }
        Value::Array(a) => a.iter().any(|v| references(v, id)),
        _ => false,
    }
}

pub(super) fn validate_references(doc: &Document, value: &Value) -> Result {
    match value {
        Value::Object(o) => {
            let id = json::text(value, "$device");
            if id.starts_with("scene:") && doc.record("devices", id).is_err() {
                return Err(Error::Missing("scene device does not exist"));
            }
            for (_, value) in o.iter() {
                validate_references(doc, value)?;
            }
        }
        Value::Array(a) => {
            for value in a {
                validate_references(doc, value)?;
            }
        }
        _ => (),
    }
    Ok(())
}

impl<S: Storage> Store<S> {
    /// Duurzame herstelstaat vóór device-I/O; een tweede activatie overschrijft hem niet.
    pub fn begin_scene(&mut self, id: &str, previous: Vec<Value>) -> Result<bool> {
        let mut scene = self.document.record("scenes", id)?.try_clone()?;
        if json::text(&scene, "kind") == "button" {
            return Err(Error::Invalid("button scene has no restore state"));
        }
        if json::boolean(&scene, "active") {
            return Ok(false);
        }
        validate_states(&previous)?;
        json::set(&mut scene, "previous", Value::Array(previous))?;
        json::set(&mut scene, "active", Value::Bool(true))?;
        self.commit_scene_state(id, scene)?;
        Ok(true)
    }

    /// Alleen nog mislukte herstelopdrachten blijven na een gedeeltelijke restore staan.
    pub fn scene_remaining(&mut self, id: &str, remaining: Vec<Value>) -> Result {
        validate_states(&remaining)?;
        let mut scene = self.document.record("scenes", id)?.try_clone()?;
        if json::text(&scene, "kind") == "button" {
            return Err(Error::Invalid("button scene has no restore state"));
        }
        json::set(&mut scene, "active", Value::Bool(!remaining.is_empty()))?;
        json::set(&mut scene, "previous", Value::Array(remaining))?;
        self.commit_scene_state(id, scene)
    }

    fn commit_scene_state(&mut self, id: &str, mut scene: Value) -> Result {
        let revision = json::uint(&scene, "revision")
            .checked_add(1)
            .ok_or(Error::Full)?;
        json::set(&mut scene, "revision", Value::uint(revision))?;
        let device = self.device(&device_id(id)?)?;
        let old = json::fields(&[(
            "onoff",
            Value::Bool(json::boolean(self.document.record("scenes", id)?, "active")),
        )])?;
        let new = json::fields(&[("onoff", Value::Bool(json::boolean(&scene, "active")))])?;
        let changes = super::triggers::changes(&device, &old, &new)?;
        if changes.len() > super::MAX_TRIGGERS.saturating_sub(self.triggers.len()) {
            return Err(Error::Full);
        }
        self.triggers
            .try_reserve(changes.len())
            .map_err(|_| Error::Memory)?;
        self.sequence.checked_add(2).ok_or(Error::Full)?;
        let mut candidate = self.document.candidate()?;
        replace_record(&mut candidate, "scenes", scene)?;
        let event = self.event("scene", "scene.state", id)?;
        self.commit(candidate, event)?;
        let event = self.event("devices", "device.update", &device_id(id)?)?;
        self.publish(event);
        self.collect(json::text(&device, "id"), json::text(&device, "name"), &new);
        self.triggers.extend(changes);
        Ok(())
    }
}

/// Rapportafronding mag een scene niet beëindigen; een echte afwijking wel.
pub(super) fn same(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(a), Value::Number(b)) => {
            let (a, b) = (a.as_f64(), b.as_f64());
            a.is_finite() && b.is_finite() && (a - b).abs() <= 0.01 + 0.01 * a.abs().max(b.abs())
        }
        (Value::Number(_), _) | (_, Value::Number(_)) => false,
        _ => json::equal(a, b),
    }
}
pub(super) fn trips(doc: &Document, id: &str, old: &Value, new: &Value) -> Result<Vec<Value>> {
    let mut out = Vec::new();
    for scene in doc
        .records("scenes")
        .iter()
        .filter(|s| json::boolean(s, "active") && json::text(s, "kind") != "button")
    {
        for desired in json::array(scene, "states")
            .iter()
            .filter(|s| json::text(s, "deviceId") == id)
        {
            let cap = json::text(desired, "capabilityId");
            let target = json::get(desired, "value").unwrap_or(&Value::Null);
            let before = json::get(old, cap).unwrap_or(&Value::Null);
            let current = json::get(new, cap).unwrap_or(&Value::Null);
            let held = json::array(scene, "previous")
                .iter()
                .any(|p| json::text(p, "deviceId") == id && json::text(p, "capabilityId") == cap);
            if held && !current.is_null() && same(before, target) && !same(current, target) {
                json::push(
                    &mut out,
                    json::fields(&[
                        ("sceneId", json::string(json::text(scene, "id"))?),
                        ("deviceId", json::string(id)?),
                        ("capabilityId", json::string(cap)?),
                    ])?,
                    256,
                )?;
            }
        }
    }
    Ok(out)
}
