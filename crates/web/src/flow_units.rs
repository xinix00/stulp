//! Alleen de browserrand rekent Flow-drempels om; ongewijzigde afrondingen blijven exact.
use alloc::vec::Vec;
use stulp_core::{
    Result,
    json::{self, Number, TryClone, Value},
    manifest,
    store::{Storage, Store},
    units,
};
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
fn argument_units<S: Storage>(store: &Store<S>, step: &Value) -> Result<Value> {
    let app = json::text(step, "appId");
    let id = json::text(step, "cardId");
    let kind = json::text(step, "cardType").trim_start_matches("device-");
    let mut out = json::object();
    if let Some(m) = store.manifest(app) {
        let list = match kind {
            "trigger" => "triggers",
            "condition" => "conditions",
            _ => "actions",
        };
        if let Some(card) = json::array(field(m, "flow"), list)
            .iter()
            .find(|c| json::text(c, "id") == id)
        {
            for arg in json::array(card, "args") {
                let name = json::text(arg, "name");
                let unit = manifest::localized(field(arg, "units"), "en").trim();
                if !name.is_empty() && !unit.is_empty() {
                    json::set(&mut out, name, json::string(unit)?)?;
                }
            }
        }
    }
    if out.as_object().is_some_and(|o| o.iter().next().is_some()) {
        return Ok(out);
    }
    let cap = if let Some(rest) = id.strip_prefix("capability.") {
        rest.rsplit_once('.').map(|p| p.0).unwrap_or("")
    } else if matches!(
        id,
        "set_device_capability" | "device_capability_equals" | "device_capability_stays"
    ) {
        json::text(field(step, "args"), "capability")
    } else {
        ""
    };
    if cap.is_empty() {
        return Ok(out);
    }
    let unit = capability_unit(store, cap)?;
    if !unit.is_empty() {
        json::set(&mut out, "value", json::string(&unit)?)?;
    }
    Ok(out)
}
use stulp_core::display::capability_unit;
pub(super) fn convert<S: Storage>(
    store: &Store<S>,
    definition: &Value,
    incoming: bool,
    previous: Option<&Value>,
) -> Result<Value> {
    let settings = field(field(store.document().root(), "system"), "units");
    let mut nodes = Vec::new();
    for node in json::array(definition, "nodes") {
        let mut node = node.try_clone()?;
        let mut step = field(&node, "step").try_clone()?;
        let found = argument_units(store, &step)?;
        let mut args = field(&step, "args").try_clone()?;
        let old = previous
            .and_then(|v| {
                json::array(v, "nodes")
                    .iter()
                    .find(|n| json::text(n, "id") == json::text(&node, "id"))
            })
            .map(|n| field(n, "step"))
            .filter(|s| json::text(s, "cardId") == json::text(&step, "cardId"));
        if let Some(found) = found.as_object() {
            for (name, unit) in found.iter() {
                let unit = unit.as_str().unwrap_or("");
                if units::step(settings, unit).is_none() {
                    continue;
                }
                let Value::Number(n) = field(&args, name) else {
                    continue;
                };
                let n = n.as_f64();
                let value = if incoming {
                    if let Some(Value::Number(before)) = old.map(|s| field(field(s, "args"), name))
                        && (units::show(settings, before.as_f64(), unit).0 - n).abs() < 1e-6
                    {
                        Value::Number(*before)
                    } else {
                        Value::Number(Number::Float(units::canonical(settings, n, unit)))
                    }
                } else {
                    Value::Number(Number::Float(units::show(settings, n, unit).0))
                };
                json::set(&mut args, name, value)?;
            }
        }
        if !step.is_null() {
            if !args.is_null() {
                json::set(&mut step, "args", args)?;
            }
            json::set(&mut node, "step", step)?;
        }
        json::push(&mut nodes, node, stulp_core::flow::MAX_NODES)?;
    }
    let mut out = definition.try_clone()?;
    json::set(&mut out, "nodes", Value::Array(nodes))?;
    Ok(out)
}
