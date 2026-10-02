//! Eenheden worden alleen aan de menselijke rand toegepast, niet op opgeslagen waarden.
use crate::{
    Result,
    json::{self, Value},
    manifest,
    store::{Storage, Store},
    units,
};
use alloc::string::String;
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
/// Zoekt de canonieke eenheid van een capability, met kernmetadata vóór appmetadata.
pub fn capability_unit<S: Storage>(store: &Store<S>, cap: &str) -> Result<alloc::string::String> {
    let mut definition = json::object();
    crate::capability::defaults(&mut definition, cap.split('.').next().unwrap_or(cap))?;
    let unit = manifest::localized(field(&definition, "units"), "en").trim();
    if !unit.is_empty() {
        return json::copy(unit);
    }
    for app in store.document().records("apps") {
        if let Some(m) = store.manifest(json::text(app, "id")) {
            let unit =
                manifest::localized(field(field(field(m, "capabilities"), cap), "units"), "en")
                    .trim();
            if !unit.is_empty() {
                return json::copy(unit);
            }
        }
    }
    json::copy("")
}

/// Een numeriek triggertoken wordt tekst wanneer de ontvangende kaart tekst verlangt.
pub fn token<S: Storage>(
    store: &Store<S>,
    input: &Value,
    name: &str,
    value: &Value,
) -> Result<Option<String>> {
    let Value::Number(number) = value else {
        return Ok(None);
    };
    let token = name.strip_prefix("tokens.").unwrap_or(name);
    let token = token.strip_prefix("state.").unwrap_or(token);
    let id = json::text(input, "id");
    let cap = if matches!(token, "value" | "oldValue") {
        if let Some(rest) = id.strip_prefix("capability.") {
            rest.rsplit_once('.').map(|p| p.0).unwrap_or("")
        } else if id == "device_capability_changed" {
            json::text(field(input, "tokens"), "capability")
        } else {
            ""
        }
    } else {
        ""
    };
    let mut unit = capability_unit(store, cap)?;
    if unit.is_empty()
        && let Some(m) = store.manifest(json::text(input, "appId"))
    {
        'cards: for list in ["triggers", "conditions", "actions"] {
            for card in json::array(field(m, "flow"), list) {
                if json::text(card, "id") != id {
                    continue;
                }
                for t in json::array(card, "tokens") {
                    if json::text(t, "name") == token {
                        unit = json::copy(manifest::localized(field(t, "units"), "en").trim())?;
                        break 'cards;
                    }
                }
            }
        }
    }
    if unit.is_empty() {
        return Ok(None);
    }
    let settings = field(field(store.document().root(), "system"), "units");
    Ok(Some(units::text(settings, number.as_f64(), &unit)?))
}
/// Alleen numerieke argumenten behouden een los token als canoniek getal.
pub fn wants_number<S: Storage>(store: &Store<S>, step: &Value, name: &str) -> bool {
    let id = json::text(step, "cardId");
    if json::text(step, "appId") == "stulp" {
        if let Some((_, action)) = id
            .strip_prefix("capability.")
            .and_then(|s| s.rsplit_once('.'))
        {
            return (name == "value"
                && matches!(
                    action,
                    "is" | "set" | "above" | "below" | "rose_above" | "fell_below" | "became"
                ))
                || (name == "seconds" && matches!(action, "on_for" | "off_for"));
        }
        return matches!(
            (id, name),
            (
                "set_device_capability"
                    | "device_capability_equals"
                    | "device_capability_changed"
                    | "device_capability_stays",
                "value"
            ) | ("device_capability_stays" | "delay", "seconds")
                | ("sunrise" | "sunset", "offset")
        );
    }
    let Some(m) = store.manifest(json::text(step, "appId")) else {
        return false;
    };
    let list = match json::text(step, "cardType") {
        "trigger" | "device-trigger" => "triggers",
        "condition" => "conditions",
        _ => "actions",
    };
    json::array(field(m, "flow"), list)
        .iter()
        .find(|c| json::text(c, "id") == id)
        .and_then(|c| {
            json::array(c, "args")
                .iter()
                .find(|a| json::text(a, "name") == name)
        })
        .is_some_and(|a| {
            matches!(
                json::text(a, "type"),
                "number" | "range" | "capability-value"
            )
        })
}
