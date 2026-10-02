//! Alleen expliciet gemarkeerde metingen in pluginantwoorden worden omgerekend.
use stulp_core::{
    Error, Result,
    json::{self, Number, TryClone, Value},
    manifest,
    store::{Storage, Store},
    units,
};
/// Zet geneste `$measure`-waarden om naar de eenheden van het huis.
pub fn show<S: Storage>(store: &Store<S>, value: &Value) -> Result<Value> {
    let settings = json::get(store.document().root(), "system")
        .and_then(|v| json::get(v, "units"))
        .unwrap_or(&Value::Null);
    convert(settings, value, 0)
}
fn convert(settings: &Value, value: &Value, depth: usize) -> Result<Value> {
    if depth > 64 {
        return Err(Error::Full);
    }
    match value {
        Value::Object(object) => {
            if let Some(Value::Number(n)) = json::get(value, "$measure") {
                let unit =
                    manifest::localized(json::get(value, "units").unwrap_or(&Value::Null), "en")
                        .trim();
                let (shown, label) = units::show(settings, n.as_f64(), unit);
                return json::fields(&[
                    ("value", Value::Number(Number::Float(shown))),
                    ("units", json::string(label)?),
                    (
                        "text",
                        Value::String(units::text(settings, n.as_f64(), unit)?),
                    ),
                    ("measured", Value::Number(*n)),
                    ("canonical", json::string(unit)?),
                ]);
            }
            let mut out = json::Object::new();
            for (k, v) in object.iter() {
                out.push(k, convert(settings, v, depth + 1)?)?;
            }
            Ok(Value::Object(out))
        }
        Value::Array(values) => {
            let mut out = alloc::vec::Vec::new();
            for v in values {
                json::push(
                    &mut out,
                    convert(settings, v, depth + 1)?,
                    json::MAX_DOCUMENT,
                )?;
            }
            Ok(Value::Array(out))
        }
        _ => Ok(value.try_clone()?),
    }
}
