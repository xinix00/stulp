//! De gebruikersboom bewaart kinderen bij verwijderen van hun groep.
use super::{Storage, Store};
use crate::{
    Error, Result,
    document::{Document, MAX_RECORDS},
    json::{self, TryClone, Value},
};
use alloc::vec::Vec;

pub(super) fn validate(doc: &Document, group: &mut Value) -> Result {
    let name = json::copy(json::text(group, "name").trim())?;
    let parent = json::copy(json::text(group, "parentId").trim())?;
    json::set(group, "parentId", json::string(&parent)?)?;
    let id = json::text(group, "id");
    if name.is_empty() || name.len() > 80 {
        return Err(Error::Invalid("group name must contain 1 to 80 bytes"));
    }
    if json::get(group, "sortOrder").is_some_and(|v| v.as_u64().is_none()) {
        return Err(Error::Invalid("invalid group sortOrder"));
    }
    for previous in doc.records("deviceGroups") {
        if json::text(previous, "id") != id
            && json::text(previous, "name").eq_ignore_ascii_case(&name)
        {
            return Err(Error::Conflict("a group with that name already exists"));
        }
    }
    let mut parent = json::text(group, "parentId").trim();
    for _ in 0..=doc.records("deviceGroups").len() {
        if parent.is_empty() {
            json::set(group, "name", json::string(&name)?)?;
            if json::uint(group, "sortOrder") == 0 {
                let highest = doc
                    .records("deviceGroups")
                    .iter()
                    .filter(|g| json::text(g, "parentId") == json::text(group, "parentId"))
                    .map(|g| json::uint(g, "sortOrder"))
                    .max()
                    .unwrap_or(0);
                json::set(
                    group,
                    "sortOrder",
                    Value::uint(highest.checked_add(1).ok_or(Error::Full)?),
                )?;
            }
            return Ok(());
        }
        if parent == id {
            return Err(Error::Invalid("group parent would create a cycle"));
        }
        parent = json::text(doc.record("deviceGroups", parent)?, "parentId");
    }
    Err(Error::Invalid("group parent would create a cycle"))
}

pub(super) fn lift_children(doc: &mut Document, id: &str) -> Result {
    let parent = json::copy(json::text(doc.record("deviceGroups", id)?, "parentId"))?;
    for (collection, key) in [("deviceGroups", "parentId"), ("devices", "groupId")] {
        let mut records = Vec::new();
        for record in doc.records(collection) {
            let mut record = record.try_clone()?;
            if json::text(&record, key) == id {
                json::set(&mut record, key, json::string(&parent)?)?;
            }
            json::push(&mut records, record, MAX_RECORDS)?;
        }
        doc.replace(collection, records)?;
    }
    Ok(())
}

impl<S: Storage> Store<S> {
    /// Accepteert uitsluitend de volledige actuele groep, zonder dubbele identifiers.
    pub fn reorder_devices(&mut self, group: &str, ids: &[&str]) -> Result {
        if !group.is_empty() {
            self.document.record("deviceGroups", group)?;
        }
        let count = self
            .document
            .records("devices")
            .iter()
            .filter(|d| json::text(d, "groupId") == group)
            .count();
        if count != ids.len() {
            return Err(Error::Changed);
        }
        for (index, id) in ids.iter().enumerate() {
            if ids.iter().take(index).any(|p| p == id)
                || json::text(self.document.record("devices", id)?, "groupId") != group
            {
                return Err(Error::Invalid("device is not a unique group member"));
            }
        }
        let mut candidate = self.document.candidate()?;
        let mut records = Vec::new();
        for device in candidate.records("devices") {
            let mut device = device.try_clone()?;
            if let Some(index) = ids.iter().position(|id| *id == json::text(&device, "id")) {
                json::set(
                    &mut device,
                    "sortOrder",
                    Value::uint(((index + 1) * 10) as u64),
                )?;
            }
            json::push(&mut records, device, MAX_RECORDS)?;
        }
        candidate.replace("devices", records)?;
        let event = self.event("devices", "device.reorder", group)?;
        self.commit(candidate, event)
    }
}
