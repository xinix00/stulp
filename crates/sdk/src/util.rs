//! Begrensde tekst- en formulierbewerkingen voor externe API's.
use crate::{Error, Result};
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
/// Een optioneel veld zonder een tijdelijke allocatie.
pub fn field<'a>(v: &'a Value, key: &str) -> &'a Value {
    json::get(v, key).unwrap_or(&Value::Null)
}
/// JSON-getallen, zonder tekst stilzwijgend als een getal te behandelen.
pub fn number(v: &Value) -> Option<f64> {
    match v {
        Value::Number(n) => Some(n.as_f64()).filter(|n| n.is_finite()),
        _ => None,
    }
}
/// Samenvoegen met één voorafgaande, faalbare reservering.
pub fn join(parts: &[&str]) -> Result<String> {
    let len = parts.iter().try_fold(0usize, |n, p| {
        n.checked_add(p.len()).ok_or(stulp_core::Error::Full)
    })?;
    let mut out = String::new();
    out.try_reserve(len)
        .map_err(|_| stulp_core::Error::Memory)?;
    for p in parts {
        out.push_str(p);
    }
    Ok(out)
}
/// Een getal dat in JSON altijd eindig moet blijven.
pub fn float(n: f64) -> Result<Value> {
    if !n.is_finite() {
        return Err(Error::Invalid("non-finite number"));
    }
    Ok(Value::Number(json::Number::Float(n)))
}
/// Een formulier gebruikt dezelfde bytecodering als queryparameters.
pub fn form(fields: &[(&str, &str)]) -> Result<String> {
    let mut out = String::new();
    for (i, (k, v)) in fields.iter().enumerate() {
        let k = crate::query(k)?;
        let v = crate::query(v)?;
        out.try_reserve(k.len() + v.len() + 2)
            .map_err(|_| stulp_core::Error::Memory)?;
        if i > 0 {
            out.push('&');
        }
        out.push_str(&k);
        out.push('=');
        out.push_str(&v);
    }
    Ok(out)
}
/// Leest queryparameters zonder een ongeldig procentteken te accepteren.
pub fn unquery(s: &str) -> Result<String> {
    let mut bytes = Vec::new();
    bytes
        .try_reserve(s.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    let mut input = s.bytes();
    while let Some(b) = input.next() {
        bytes.push(match b {
            b'+' => b' ',
            b'%' => {
                let a = input
                    .next()
                    .and_then(|v| char::from(v).to_digit(16))
                    .ok_or(Error::Invalid("invalid percent encoding"))?;
                let b = input
                    .next()
                    .and_then(|v| char::from(v).to_digit(16))
                    .ok_or(Error::Invalid("invalid percent encoding"))?;
                ((a << 4) | b) as u8
            }
            b => b,
        });
    }
    String::from_utf8(bytes).map_err(|_| Error::Invalid("query is not UTF-8"))
}
/// Device-argumenten komen uit de kaart als tekst of als een getypeerde verwijzing.
pub fn device_arg(args: &Value) -> &str {
    let d = field(args, "device");
    d.as_str().unwrap_or_else(|| json::text(d, "$device"))
}
