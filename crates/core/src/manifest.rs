//! De app.json-contracten die de controller nodig heeft.
use crate::{
    Error, Result,
    json::{self, Value},
};

/// Controleert SDK-versie en unieke driveridentiteiten; onbekende metadata blijft staan.
pub fn validate(value: &Value) -> Result {
    if json::text(value, "id").is_empty() {
        return Err(Error::Invalid("app.json: id is required"));
    }
    if json::text(value, "version").is_empty() {
        return Err(Error::Invalid("app.json: version is required"));
    }
    if json::uint(value, "sdk") != 3 {
        return Err(Error::Invalid(
            "app.json: only manifest version 3 is supported",
        ));
    }
    let drivers = json::array(value, "drivers");
    for (i, driver) in drivers.iter().enumerate() {
        let id = json::text(driver, "id");
        if id.is_empty() {
            return Err(Error::Invalid("app.json: every driver needs an id"));
        }
        if drivers.iter().take(i).any(|d| json::text(d, "id") == id) {
            return Err(Error::Conflict("app.json: duplicate driver id"));
        }
    }
    Ok(())
}

/// Taalvoorkeur, Engels, daarna de eerste beschikbare vertaling.
pub fn localized<'a>(value: &'a Value, language: &str) -> &'a str {
    if let Some(s) = value.as_str() {
        return s;
    }
    for name in [language, "en"] {
        let s = json::text(value, name);
        if !s.is_empty() {
            return s;
        }
    }
    value
        .as_object()
        .and_then(|o| o.iter().find_map(|(_, v)| v.as_str()))
        .unwrap_or("")
}
