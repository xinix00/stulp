//! Faalbare JSON-bewerkingen, met dezelfde parser als Hop.
use crate::{Error, Result};
use alloc::{string::String, vec::Vec};
pub use hop_types::{
    TryClone,
    json::{Number, Object, Value, parse, to_string},
};

/// De volledige invoer blijft begrensd, ook vóór parsing.
pub const MAX_DOCUMENT: usize = hop_types::json::MAX_INPUT;

/// Alleen opslag en appframes hebben een ruimere, expliciete grens; gewone API-JSON blijft 1 MiB.
pub fn parse_bounded(bytes: &[u8], limit: usize) -> hop_types::Result<Value> {
    crate::json_bounded::parse(bytes, limit.min(crate::document::MAX_BYTES))
}

/// Leeg object.
pub fn object() -> Value {
    Value::Object(Object::new())
}

/// Een faalbaar gekopieerde string.
pub fn string(value: &str) -> Result<Value> {
    Ok(Value::string(value)?)
}

/// Een veld zonder ontbrekend en null gelijk te maken bij validatie.
pub fn get<'a>(value: &'a Value, key: &str) -> Option<&'a Value> {
    value.as_object().and_then(|o| o.get(key))
}

/// Een optioneel tekstveld met de oude Go-nulwaarde.
pub fn text<'a>(value: &'a Value, key: &str) -> &'a str {
    get(value, key).and_then(Value::as_str).unwrap_or("")
}

/// Een geheel getal met de oude Go-nulwaarde.
pub fn uint(value: &Value, key: &str) -> u64 {
    get(value, key).and_then(Value::as_u64).unwrap_or(0)
}

/// Een bool met de oude Go-nulwaarde.
pub fn boolean(value: &Value, key: &str) -> bool {
    get(value, key).and_then(Value::as_bool).unwrap_or(false)
}

/// Een optionele lijst met de oude Go-nulwaarde.
pub fn array<'a>(value: &'a Value, key: &str) -> &'a [Value] {
    get(value, key).and_then(Value::as_array).unwrap_or(&[])
}

/// Vervangt een veld zonder verborgen, onbegrensde allocatie.
pub fn set(value: &mut Value, key: &str, replacement: Value) -> Result {
    let source = value.as_object().ok_or(Error::Invalid("expected object"))?;
    let mut out = Object::new();
    let mut replacement = Some(replacement);
    for (name, item) in source.iter() {
        if name == key {
            out.push(
                name,
                replacement
                    .take()
                    .ok_or(Error::Conflict("duplicate JSON key"))?,
            )?;
        } else {
            out.push(name, item.try_clone()?)?;
        }
    }
    if let Some(replacement) = replacement {
        out.push(key, replacement)?;
    }
    *value = Value::Object(out);
    Ok(())
}

/// Verwijdert een veld, met behoud van onbekende velden.
pub fn remove(value: &mut Value, key: &str) -> Result {
    let source = value.as_object().ok_or(Error::Invalid("expected object"))?;
    let mut out = Object::new();
    for (name, item) in source.iter() {
        if name != key {
            out.push(name, item.try_clone()?)?;
        }
    }
    *value = Value::Object(out);
    Ok(())
}

/// Bouwt een object, zonder macros met verborgen unwraps.
pub fn fields(values: &[(&str, Value)]) -> Result<Value> {
    let mut result = Object::new();
    for (key, value) in values {
        result.push(key, value.try_clone()?)?;
    }
    Ok(Value::Object(result))
}

/// Voegt maximaal `max` waarden toe en reserveert vóór de mutatie.
pub fn push<T>(values: &mut Vec<T>, value: T, max: usize) -> Result {
    if values.len() >= max {
        return Err(Error::Full);
    }
    values.try_reserve(1).map_err(|_| Error::Memory)?;
    values.push(value);
    Ok(())
}

/// Kopieert tekst zonder infallible heapallocatie.
pub fn copy(value: &str) -> Result<String> {
    Ok(hop_types::try_string(value)?)
}

/// JSON-objectvolgorde draagt geen betekenis, ook niet bij fysieke device-identiteit.
pub fn equal(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(a), Value::Number(b)) => number_equal(*a, *b),
        (Value::Object(a), Value::Object(b)) => {
            a.len() == b.len() && a.iter().all(|(k, v)| b.get(k).is_some_and(|w| equal(v, w)))
        }
        (Value::Array(a), Value::Array(b)) => {
            a.len() == b.len() && a.iter().zip(b).all(|(v, w)| equal(v, w))
        }
        _ => a == b,
    }
}

/// Getalnotatie verandert de waarde niet; integers boven 2^53 houden hun precisie.
fn number_equal(a: Number, b: Number) -> bool {
    match (a, b) {
        (Number::Int(a), Number::Uint(b)) | (Number::Uint(b), Number::Int(a)) => {
            u64::try_from(a) == Ok(b)
        }
        (Number::Float(a), Number::Uint(b)) | (Number::Uint(b), Number::Float(a)) => {
            a.is_finite()
                && (0.0..18_446_744_073_709_551_616.0).contains(&a)
                && a as u64 == b
                && b as f64 == a
        }
        (Number::Float(a), Number::Int(b)) | (Number::Int(b), Number::Float(a)) => {
            a.is_finite()
                && (-9_223_372_036_854_775_808.0..9_223_372_036_854_775_808.0).contains(&a)
                && a as i64 == b
                && b as f64 == a
        }
        _ => a == b,
    }
}

/// UTC voor documentvelden, zonder platformafhankelijkheid in de kern.
pub fn timestamp(nanoseconds: u64) -> Result<String> {
    let mut out = String::new();
    hop_types::Time(nanoseconds).write_rfc3339(&mut out)?;
    Ok(out)
}

/// Vergelijkt instants, zodat variabele fractieprecisie geen historie terugzet.
pub fn time_is_before(a: &str, b: &str) -> bool {
    match (
        hop_types::Time::parse_rfc3339(a),
        hop_types::Time::parse_rfc3339(b),
    ) {
        (Ok(a), Ok(b)) => a.0 < b.0,
        _ => false,
    }
}

/// RFC3339 naar Unix-seconden voor pluginprotocollen met een aparte tijdzone.
pub fn unix_seconds(value: &str) -> Option<u64> {
    hop_types::Time::parse_rfc3339(value)
        .ok()
        .map(|t| t.0 / 1_000_000_000)
}
