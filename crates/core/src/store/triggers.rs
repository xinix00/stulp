//! Een begrensde vluchtige rij tussen appmutaties en de Flow-eigenaar.
use super::*;
/// Hoogstens 256 nog niet overgenomen triggerberichten; vol wordt aan de zender gemeld.
pub const MAX_TRIGGERS: usize = 256;
impl<S: Storage> Store<S> {
    /// Neemt een app-event pas aan na controle van de geauthenticeerde eigenaar.
    pub fn trigger(&mut self, app: &str, params: &Value) -> Result {
        let record = self.document.record("apps", app)?;
        if !json::boolean(record, "enabled") {
            return Err(Error::Invalid("app is disabled"));
        }
        let id = json::text(params, "id");
        let kind = json::text(params, "kind");
        if id.is_empty() || id.len() > 256 || !matches!(kind, "trigger" | "device-trigger") {
            return Err(Error::Invalid("invalid flow trigger"));
        }
        let state = json::get(params, "state")
            .filter(|v| !v.is_null())
            .unwrap_or(&Value::Null);
        let tokens = json::get(params, "tokens")
            .filter(|v| !v.is_null())
            .unwrap_or(&Value::Null);
        if (!state.is_null() && state.as_object().is_none())
            || (!tokens.is_null() && tokens.as_object().is_none())
        {
            return Err(Error::Invalid("flow context must be an object"));
        }
        let device = json::text(state, "deviceId");
        if !device.is_empty()
            && json::text(self.document.record("devices", device)?, "appId") != app
        {
            return Err(Error::Invalid("trigger device belongs to another app"));
        }
        if kind == "device-trigger" && device.is_empty() {
            return Err(Error::Invalid("device trigger requires deviceId"));
        }
        let event = json::fields(&[
            (
                "appId",
                json::string(if json::boolean(params, "system") {
                    "stulp"
                } else {
                    app
                })?,
            ),
            ("kind", json::string(kind)?),
            ("id", json::string(id)?),
            ("tokens", tokens.try_clone()?),
            ("state", state.try_clone()?),
        ])?;
        json::push(&mut self.triggers, event, MAX_TRIGGERS)
    }
    /// Hoeveel events nog op de Flow-eigenaar wachten (voor zijn meting).
    pub fn pending_triggers(&self) -> usize {
        self.triggers.len()
    }
    /// De Flow-eigenaar neemt één event over; geen schijfwrite en geen browserstate met geheimen.
    pub fn take_trigger(&mut self) -> Option<Value> {
        if self.triggers.is_empty() {
            None
        } else {
            Some(self.triggers.remove(0))
        }
    }
}

pub(super) fn changes(device: &Value, old: &Value, new: &Value) -> Result<Vec<Value>> {
    let mut out = Vec::new();
    let Some(values) = new.as_object() else {
        return Ok(out);
    };
    for (cap, value) in values.iter() {
        let before = json::get(old, cap).unwrap_or(&Value::Null);
        if json::equal(value, before) {
            continue;
        }
        let state = json::fields(&[
            ("deviceId", json::string(json::text(device, "id"))?),
            ("capability", json::string(cap)?),
            ("value", value.try_clone()?),
            ("oldValue", before.try_clone()?),
        ])?;
        let mut tokens = state.try_clone()?;
        json::set(
            &mut tokens,
            "device",
            json::string(json::text(device, "name"))?,
        )?;
        let mut cards = Vec::new();
        json::push(&mut cards, json::copy("device_capability_changed")?, 4)?;
        let mut card = json::copy("capability.")?;
        card.try_reserve(cap.len() + 16)
            .map_err(|_| Error::Memory)?;
        card.push_str(cap);
        card.push('.');
        if let Some(on) = value.as_bool()
            && (before.is_null() || before.as_bool().is_some())
        {
            card.push_str(if on { "on" } else { "off" });
            json::push(&mut cards, card, 4)?;
        } else {
            let mut changed = json::copy(&card)?;
            changed.try_reserve(7).map_err(|_| Error::Memory)?;
            changed.push_str("changed");
            json::push(&mut cards, changed, 4)?;
            let numeric = |v: &Value| match v {
                Value::Number(n) => Some(n.as_f64()),
                _ => None,
            };
            let suffix = if value.as_str().is_some() {
                Some("became")
            } else if let (Some(a), Some(b)) = (numeric(value), numeric(before)) {
                if a > b {
                    Some("rose_above")
                } else if a < b {
                    Some("fell_below")
                } else {
                    None
                }
            } else {
                None
            };
            if let Some(suffix) = suffix {
                card.push_str(suffix);
                json::push(&mut cards, card, 4)?;
            }
        }
        for card in cards {
            json::push(
                &mut out,
                json::fields(&[
                    ("appId", json::string("stulp")?),
                    ("id", json::string(&card)?),
                    ("kind", json::string("trigger")?),
                    ("tokens", tokens.try_clone()?),
                    ("state", state.try_clone()?),
                ])?,
                MAX_TRIGGERS,
            )?;
        }
    }
    Ok(out)
}
