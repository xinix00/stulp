//! De geadverteerde schema's zijn tegelijk de invoercontrole, inclusief totaalbudget.
use super::*;
pub(super) fn validate(value: &Value, schema: &Value) -> Result {
    validate_limit(value, schema, 8192, 65536)
}
pub(super) fn validate_limit(value: &Value, schema: &Value, values: usize, bytes: usize) -> Result {
    let mut budget = Budget { values, bytes };
    visit(value, schema, 0, &mut budget)
}
struct Budget {
    values: usize,
    bytes: usize,
}
impl Budget {
    fn take(&mut self, bytes: usize) -> Result {
        self.bytes = self
            .bytes
            .checked_sub(bytes)
            .ok_or(Error::Invalid("MCP total value budget exceeded"))?;
        Ok(())
    }
}
fn visit(v: &Value, s: &Value, depth: usize, b: &mut Budget) -> Result {
    if depth > 12 {
        return Err(Error::Invalid("MCP nesting depth exceeded"));
    }
    b.values = b
        .values
        .checked_sub(1)
        .ok_or(Error::Invalid("MCP value count exceeded"))?;
    b.take(4)?;
    let kind = json::text(s, "type");
    let actual = match v {
        Value::Null => "null",
        Value::Bool(_) => "boolean",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    };
    if !kind.is_empty() && kind != actual && !(kind == "integer" && actual == "number") {
        return Err(Error::Invalid("MCP argument has wrong type"));
    }
    match v {
        Value::String(text) => {
            if text.len() > 16384 {
                return Err(Error::Invalid("MCP string too large"));
            }
            b.take(text.len())?;
            bounds(text.chars().count() as f64, s, "minLength", "maxLength")?;
        }
        Value::Number(number) => {
            let n = number.as_f64();
            if !n.is_finite() || (kind == "integer" && n % 1.0 != 0.0) {
                return Err(Error::Invalid("MCP requires a finite whole number"));
            }
            bounds(n, s, "minimum", "maximum")?;
            b.take(json::to_string(v)?.len())?;
        }
        Value::Array(values) => {
            if values.len() > 512 {
                return Err(Error::Invalid("MCP array too large"));
            }
            bounds(values.len() as f64, s, "minItems", "maxItems")?;
            for child in values {
                visit(child, field(s, "items"), depth + 1, b)?;
            }
        }
        Value::Object(values) => {
            if values.len() > 512 {
                return Err(Error::Invalid("MCP object too large"));
            }
            for k in json::array(s, "required").iter().filter_map(Value::as_str) {
                if values.get(k).is_none() {
                    return Err(Error::Invalid("required MCP argument missing"));
                }
            }
            let properties = field(s, "properties");
            let allow = json::get(s, "additionalProperties")
                .and_then(Value::as_bool)
                .unwrap_or(properties.is_null());
            for (key, child) in values.iter() {
                if key.len() > 256 {
                    return Err(Error::Invalid("MCP object key too large"));
                }
                b.take(key.len())?;
                let schema = json::get(properties, key);
                if schema.is_none() && !allow {
                    return Err(Error::Invalid("unknown MCP argument"));
                }
                visit(child, schema.unwrap_or(&Value::Null), depth + 1, b)?;
            }
        }
        _ => (),
    }
    if let Some(values) = json::get(s, "enum").and_then(Value::as_array)
        && !values.iter().any(|entry| json::equal(entry, v))
    {
        return Err(Error::Invalid("MCP value is not an advertised enum"));
    }
    Ok(())
}
pub(super) fn bounds(n: f64, s: &Value, min: &str, max: &str) -> Result {
    for (key, lower) in [(min, true), (max, false)] {
        if let Value::Number(limit) = field(s, key)
            && ((lower && n < limit.as_f64()) || (!lower && n > limit.as_f64()))
        {
            return Err(Error::Invalid("MCP argument out of range"));
        }
    }
    Ok(())
}
