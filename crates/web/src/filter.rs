//! Manifestfilters combineren voorwaarden met AND en alternatieven met OR.
use stulp_core::json::{self, Value};
fn condition(d: &Value, key: &str, values: &[&str]) -> bool {
    if values.iter().all(|v| v.trim().is_empty()) {
        return true;
    }
    let has = |value: &str| values.iter().any(|v| v.trim() == value);
    match key.trim() {
        "app_id" => has(json::text(d, "appId")),
        "driver_id" => has(json::text(d, "driverId").rsplit(':').next().unwrap_or("")),
        "class" | "virtualClass" => has(json::text(d, "class")),
        "capabilities" => json::array(d, "capabilities")
            .iter()
            .filter_map(Value::as_str)
            .any(|v| has(v) || has(v.split('.').next().unwrap_or(v))),
        _ => true,
    }
}
fn text_condition(d: &Value, key: &str, values: &str) -> bool {
    // Geen allocatie nodig: een voorwaarde is waar zodra één alternatief past.
    values.split('|').all(|v| v.trim().is_empty())
        || values
            .split('|')
            .filter(|v| !v.trim().is_empty())
            .any(|v| condition(d, key, &[v]))
}
pub(super) fn matches(d: &Value, filter: &Value) -> bool {
    if let Some(s) = filter.as_str() {
        return s
            .split('&')
            .filter_map(|s| s.split_once('='))
            .all(|(key, _)| {
                let mut values = s
                    .split('&')
                    .filter_map(|s| s.split_once('='))
                    .filter(|(k, _)| k.trim() == key.trim())
                    .flat_map(|(_, v)| v.split('|'))
                    .filter(|v| !v.trim().is_empty())
                    .peekable();
                values.peek().is_none() || values.any(|v| condition(d, key, &[v]))
            });
    }
    filter.as_object().is_none_or(|o| {
        o.iter().all(|(key, value)| {
            if let Some(s) = value.as_str() {
                return text_condition(d, key, s);
            }
            value.as_array().is_none_or(|a| {
                let mut strings = a
                    .iter()
                    .filter_map(Value::as_str)
                    .filter(|s| !s.trim().is_empty())
                    .peekable();
                strings.peek().is_none() || strings.any(|s| condition(d, key, &[s]))
            })
        })
    })
}
