//! Controller-owned consolidation: devices, references and live routes commit together.
use super::*;

fn field<'a>(v: &'a Value, key: &str) -> &'a Value {
    json::get(v, key).unwrap_or(&Value::Null)
}

fn reference(v: &Value, replacements: &Value) -> Result<Value> {
    let mut out = v.try_clone()?;
    if let Some(r) = json::get(replacements, json::text(v, "deviceId")) {
        json::set(&mut out, "deviceId", field(r, "deviceId").try_clone()?)?;
        for key in ["capability", "capabilityId"] {
            if let Some(cap) = json::get(field(r, "capabilities"), json::text(v, key)) {
                json::set(&mut out, key, cap.try_clone()?)?;
            }
        }
    }
    Ok(out)
}

fn queued(v: &Value, replacements: &Value) -> Result<Value> {
    let mut out = reference(v, replacements)?;
    for key in ["state", "tokens"] {
        if let Some(value) = json::get(v, key) {
            json::set(&mut out, key, reference(value, replacements)?)?;
        }
    }
    if let Some(r) = json::get(replacements, json::text(field(v, "state"), "deviceId"))
        && let Some((cap, action)) = json::text(v, "id")
            .strip_prefix("capability.")
            .and_then(|id| id.rsplit_once('.'))
        && let Some(new) = json::get(field(r, "capabilities"), cap).and_then(Value::as_str)
    {
        let mut id = json::copy("capability.")?;
        id.try_reserve(new.len() + action.len() + 1)
            .map_err(|_| Error::Memory)?;
        id.push_str(new);
        id.push('.');
        id.push_str(action);
        json::set(&mut out, "id", Value::String(id))?;
    }
    Ok(out)
}

impl<S: Storage> Store<S> {
    /// Controller migration only; deliberately not exposed as a plugin mutation.
    /// Every source capability needs a distinct destination on the surviving device;
    /// the trusted controller may include newly supported hardware capabilities.
    /// Paired identity and the user's name remain immutable. No event or runtime
    /// route changes before the entire document has been durably written.
    pub fn consolidate_devices(
        &mut self,
        app: &str,
        mut merged: Value,
        replacements: &Value,
        now: &str,
    ) -> Result {
        self.validate_replacements(app, replacements)?;
        let entries = replacements
            .as_object()
            .ok_or(Error::Invalid("missing replacements"))?;
        let id = json::copy(json::text(&merged, "id"))?;
        if entries.len() < 2 || json::get(replacements, &id).is_none() {
            return Err(Error::Invalid(
                "consolidation requires a surviving source device",
            ));
        }
        let previous = self.document.record("devices", &id)?;
        // Data includes the original pairing identity; endpoint routes live in store.
        for key in ["appId", "driverId", "data", "name", "createdAt"] {
            if let Some(value) = json::get(previous, key) {
                json::set(&mut merged, key, value.try_clone()?)?;
            } else {
                json::remove(&mut merged, key)?;
            }
        }
        let mut destinations = Vec::new();
        let mut state = json::object();
        let mut revisions = json::object();
        for (old, r) in entries.iter() {
            let source = self.document.record("devices", old)?;
            if json::text(r, "deviceId") != id
                || json::text(source, "driverId") != json::text(previous, "driverId")
            {
                return Err(Error::Invalid("consolidation crosses device identity"));
            }
            let caps = field(r, "capabilities")
                .as_object()
                .ok_or(Error::Invalid("consolidation requires capability routes"))?;
            if json::array(source, "capabilities")
                .iter()
                .any(|cap| cap.as_str().is_none_or(|cap| caps.get(cap).is_none()))
            {
                return Err(Error::Invalid("consolidation would lose a capability"));
            }
            for (cap, value) in caps.iter() {
                let target = value
                    .as_str()
                    .ok_or(Error::Invalid("invalid capability route"))?;
                if target.is_empty()
                    || destinations.contains(&target)
                    || !json::array(&merged, "capabilities")
                        .iter()
                        .any(|v| v.as_str() == Some(target))
                {
                    return Err(Error::Invalid("invalid consolidation capability route"));
                }
                json::push(&mut destinations, target, MAX_RECORDS)?;
                if let Some(live) = self.live.iter().find(|o| o.id == old) {
                    if let Some(value) = json::get(&live.state, cap) {
                        json::set(&mut state, target, value.try_clone()?)?;
                    }
                    if let Some(value) = json::get(&live.revisions, cap) {
                        json::set(&mut revisions, target, value.try_clone()?)?;
                    }
                }
            }
        }
        if destinations.len() != json::array(&merged, "capabilities").len() {
            return Err(Error::Invalid(
                "consolidation introduced an unmapped capability",
            ));
        }
        let observation = Observation {
            id: json::copy(&id)?,
            state,
            revisions,
            available: json::boolean(&merged, "available"),
            message: json::copy(json::text(&merged, "unavailableMessage"))?,
        };
        self.live.try_reserve(1).map_err(|_| Error::Memory)?;
        let cache = self.prepare_cache(&id, &merged)?;
        let previous = self.document.record("devices", &id)?;
        self.normalize_device(&mut merged, Some(previous))?;
        json::set(&mut merged, "updatedAt", json::string(now)?)?;
        let mut candidate = self.document.candidate()?;
        replace_record(&mut candidate, "devices", merged)?;
        let mut events = Vec::new();
        let mut sequence = self.sequence;
        self.rewrite_references(
            &mut candidate,
            replacements,
            now,
            &mut events,
            &mut sequence,
        )?;
        for (old, _) in entries.iter() {
            let kind = if old == id {
                "device.update"
            } else {
                remove_record(&mut candidate, "devices", old)?;
                "device.delete"
            };
            sequence = sequence.checked_add(1).ok_or(Error::Full)?;
            let mut event = self.event("devices", kind, old)?;
            event.sequence = sequence;
            json::push(&mut events, event, MAX_RECORDS * 4)?;
        }
        scenes::reconcile(&mut candidate)?;
        let mut triggers = Vec::new();
        for event in &self.triggers {
            json::push(&mut triggers, queued(event, replacements)?, MAX_TRIGGERS)?;
        }
        let mut trips = Vec::new();
        for event in &self.scene_trips {
            json::push(&mut trips, reference(event, replacements)?, 256)?;
        }
        self.storage.save(candidate.encode()?.as_bytes())?;
        self.document = candidate;
        self.live
            .retain(|o| json::get(replacements, &o.id).is_none());
        self.live.push(observation);
        self.caches
            .retain(|(id, _)| json::get(replacements, id).is_none());
        if let Some(cache) = cache {
            self.caches.push(cache);
        }
        self.media
            .retain(|(id, _)| json::get(replacements, id).is_none());
        self.triggers = triggers;
        self.scene_trips = trips;
        for event in events {
            self.publish(event);
        }
        Ok(())
    }
}
