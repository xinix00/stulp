//! Alleen de controller herschrijft Flow- en scenereferenties namens een geauthenticeerde app.
use super::{Document, Event, Storage, Store, scenes};
use crate::{
    Error, Result,
    document::MAX_RECORDS,
    json::{self, TryClone, Value},
};
use alloc::vec::Vec;
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
fn replace(value: &Value, old: &str, new: &str, depth: usize) -> Result<Value> {
    if depth > 64 {
        return Err(Error::Full);
    }
    match value {
        Value::String(s) if s == old => json::string(new),
        Value::Object(o) => {
            let mut out = json::Object::new();
            for (k, v) in o.iter() {
                out.push(k, replace(v, old, new, depth + 1)?)?;
            }
            Ok(Value::Object(out))
        }
        Value::Array(a) => {
            let mut out = Vec::new();
            for v in a {
                json::push(
                    &mut out,
                    replace(v, old, new, depth + 1)?,
                    json::MAX_DOCUMENT,
                )?;
            }
            Ok(Value::Array(out))
        }
        _ => Ok(value.try_clone()?),
    }
}
fn step(original: &Value, replacements: &Value) -> Result<Value> {
    let Some(args) = field(original, "args").as_object() else {
        return Ok(original.try_clone()?);
    };
    let old = args
        .iter()
        .find_map(|(_, v)| {
            let id = json::text(v, "$device");
            (!id.is_empty()).then_some(id)
        })
        .unwrap_or("");
    let Some(replacement) = json::get(replacements, old) else {
        return Ok(original.try_clone()?);
    };
    let new = json::text(replacement, "deviceId");
    let caps = field(replacement, "capabilities");
    let mut out = original.try_clone()?;
    for key in ["args", "state"] {
        if let Some(value) = json::get(original, key) {
            json::set(&mut out, key, replace(value, old, new, 0)?)?;
        }
    }
    let mut args = field(&out, "args").try_clone()?;
    let renamed = json::text(caps, json::text(&args, "capability"));
    if !renamed.is_empty() {
        json::set(&mut args, "capability", json::string(renamed)?)?;
    }
    json::set(&mut out, "args", args)?;
    if let Some((cap, action)) = json::text(original, "cardId")
        .strip_prefix("capability.")
        .and_then(|s| s.rsplit_once('.'))
    {
        let new = json::text(caps, cap);
        if !new.is_empty() && new != cap {
            let mut id = json::copy("capability.")?;
            id.try_reserve(new.len() + action.len() + 1)
                .map_err(|_| Error::Memory)?;
            id.push_str(new);
            id.push('.');
            id.push_str(action);
            json::set(&mut out, "cardId", Value::String(id))?;
        }
    }
    Ok(out)
}
fn flow(original: &Value, replacements: &Value) -> Result<Value> {
    let mut out = original.try_clone()?;
    let mut nodes = Vec::new();
    for node in json::array(original, "nodes") {
        let mut node = node.try_clone()?;
        let updated = step(field(&node, "step"), replacements)?;
        json::set(&mut node, "step", updated)?;
        json::push(&mut nodes, node, crate::flow::MAX_NODES)?;
    }
    json::set(&mut out, "nodes", Value::Array(nodes))?;
    Ok(out)
}
fn scene(original: &Value, replacements: &Value) -> Result<Value> {
    let mut out = original.try_clone()?;
    for key in ["states", "previous"] {
        if json::get(original, key).is_none() {
            continue;
        }
        let mut states = Vec::new();
        for state in json::array(original, key) {
            let mut state = state.try_clone()?;
            if let Some(replacement) = json::get(replacements, json::text(&state, "deviceId")) {
                let cap = json::text(
                    field(replacement, "capabilities"),
                    json::text(&state, "capabilityId"),
                );
                if !cap.is_empty() {
                    json::set(&mut state, "capabilityId", json::string(cap)?)?;
                }
                json::set(
                    &mut state,
                    "deviceId",
                    json::string(json::text(replacement, "deviceId"))?,
                )?;
            }
            json::push(&mut states, state, 256)?;
        }
        scenes::validate_states(&states)?;
        json::set(&mut out, key, Value::Array(states))?;
    }
    Ok(out)
}
impl<S: Storage> Store<S> {
    /// Een app kan alleen zijn eigen oude én nieuwe apparaten aanwijzen.
    /// Alle gewijzigde Flows en actieve herstelbaselines worden samen opgeslagen.
    pub fn replace_references(&mut self, app: &str, replacements: &Value, now: &str) -> Result {
        self.validate_replacements(app, replacements)?;
        if replacements
            .as_object()
            .is_some_and(|entries| entries.is_empty())
        {
            return Ok(());
        }
        let mut candidate = self.document.candidate()?;
        let mut events = Vec::new();
        let mut sequence = self.sequence;
        self.rewrite_references(
            &mut candidate,
            replacements,
            now,
            &mut events,
            &mut sequence,
        )?;
        if events.is_empty() {
            return Ok(());
        }
        scenes::reconcile(&mut candidate)?;
        self.storage.save(candidate.encode()?.as_bytes())?;
        self.document = candidate;
        for event in events {
            self.publish(event);
        }
        Ok(())
    }
    pub(super) fn validate_replacements(&self, app: &str, replacements: &Value) -> Result {
        let entries = replacements
            .as_object()
            .ok_or(Error::Invalid("replacements must be an object"))?;
        if entries.len() > MAX_RECORDS {
            return Err(Error::Full);
        }
        if entries.is_empty() {
            return Ok(());
        }
        for (old, r) in entries.iter() {
            let new = json::text(r, "deviceId");
            if new.is_empty() {
                return Err(Error::Invalid("replacement device id missing"));
            }
            for id in [old, new] {
                if json::text(self.document.record("devices", id)?, "appId") != app {
                    return Err(Error::Invalid("replacement device belongs to another app"));
                }
            }
            if let Some(caps) = json::get(r, "capabilities").filter(|v| !v.is_null()) {
                let caps = caps
                    .as_object()
                    .ok_or(Error::Invalid("replacement capabilities must be an object"))?;
                if caps
                    .iter()
                    .any(|(k, v)| k.is_empty() || v.as_str().is_none_or(|s| s.is_empty()))
                {
                    return Err(Error::Invalid("invalid capability replacement"));
                }
            }
        }
        Ok(())
    }
    pub(super) fn rewrite_references(
        &self,
        candidate: &mut Document,
        replacements: &Value,
        now: &str,
        events: &mut Vec<Event>,
        sequence: &mut u64,
    ) -> Result {
        for (collection, manager, kind) in [
            ("flows", "flow", "flow.update"),
            ("scenes", "scene", "scene.update"),
        ] {
            let mut records = Vec::new();
            for old in self.document.records(collection) {
                let mut next = if collection == "flows" {
                    flow(old, replacements)?
                } else {
                    scene(old, replacements)?
                };
                if !json::equal(old, &next) {
                    if collection == "flows" {
                        crate::flow::validate(&next)?;
                    }
                    json::set(
                        &mut next,
                        "revision",
                        Value::uint(
                            json::uint(old, "revision")
                                .checked_add(1)
                                .ok_or(Error::Full)?,
                        ),
                    )?;
                    json::set(&mut next, "updatedAt", json::string(now)?)?;
                    let mut event = self.event(manager, kind, json::text(old, "id"))?;
                    *sequence = sequence.checked_add(1).ok_or(Error::Full)?;
                    event.sequence = *sequence;
                    json::push(events, event, MAX_RECORDS * 4)?;
                    if collection == "scenes" {
                        let mut event = self.event(
                            "devices",
                            "device.update",
                            &scenes::device_id(json::text(old, "id"))?,
                        )?;
                        *sequence = sequence.checked_add(1).ok_or(Error::Full)?;
                        event.sequence = *sequence;
                        json::push(events, event, MAX_RECORDS * 4)?;
                    }
                }
                json::push(&mut records, next, MAX_RECORDS)?;
            }
            candidate.replace(collection, records)?;
        }
        Ok(())
    }
}
