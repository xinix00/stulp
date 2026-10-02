//! Kaartargumenten volgen het manifest, met echte apparaat- en capabilityselectie.
use super::*;
pub(super) fn contains_token(v: &Value) -> bool {
    match v {
        Value::String(s) => s
            .split_once("{{")
            .is_some_and(|(_, tail)| tail.contains("}}")),
        Value::Array(a) => a.iter().any(contains_token),
        Value::Object(o) => o.iter().any(|(_, v)| contains_token(v)),
        _ => false,
    }
}
pub(super) fn capability_value(def: &Value, value: &Value) -> Result {
    match json::text(def, "type") {
        "boolean" if value.as_bool().is_none() => {
            return Err(Error::Invalid("capability requires a boolean"));
        }
        "number" => {
            let Value::Number(n) = value else {
                return Err(Error::Invalid("capability requires a number"));
            };
            if !n.as_f64().is_finite() {
                return Err(Error::Invalid("capability requires a finite number"));
            }
            schema::bounds(n.as_f64(), def, "min", "max")?;
        }
        "string" if value.as_str().is_none() => {
            return Err(Error::Invalid("capability requires a string"));
        }
        "enum" if !choice(def, value) => {
            return Err(Error::Invalid("unknown capability enum value"));
        }
        _ => (),
    }
    schema::validate(value, &Value::Null)
}
fn choice(def: &Value, v: &Value) -> bool {
    json::array(def, "values").iter().any(|c| {
        json::equal(
            if c.as_object().is_some() {
                field(c, "id")
            } else {
                c
            },
            v,
        )
    })
}
pub(super) fn normalize<S: Storage>(
    store: &Store<S>,
    node: &mut Value,
    cards: &Value,
    available: bool,
    unknown: bool,
    partial: bool,
) -> Result {
    let mut step = field(node, "step").try_clone()?;
    let card = read::card(cards, &step).ok_or(Error::Missing(
        "Flow card does not exist; read flow_cards_list first",
    ))?;
    if available && !json::boolean(card, "available") {
        return Err(Error::Invalid("Flow card is currently unavailable"));
    }
    if json::boolean(&step, "inverted") && json::text(&step, "cardType") != "condition" {
        return Err(Error::Invalid("only conditions may be inverted"));
    }
    let mut args = field(&step, "args").try_clone()?;
    if args.is_null() {
        args = json::object();
    }
    let fields = args
        .as_object()
        .ok_or(Error::Invalid("card args must be an object"))?;
    if !unknown
        && fields.iter().any(|(k, _)| {
            !json::array(card, "args")
                .iter()
                .any(|a| json::text(a, "name") == k)
        })
    {
        return Err(Error::Invalid("card has no such argument"));
    }
    let mut device = None;
    for def in json::array(card, "args")
        .iter()
        .filter(|v| json::text(v, "type") == "device")
    {
        let name = json::text(def, "name");
        let raw = field(&args, name);
        if raw.is_null() && (partial || json::boolean(def, "optional")) {
            continue;
        }
        let id = raw
            .as_str()
            .unwrap_or_else(|| json::text(raw, "$device"))
            .trim();
        if id.is_empty() || raw.as_object().is_some_and(|o| o.len() != 1) {
            return Err(Error::Invalid(
                "device argument needs a single $device reference",
            ));
        }
        let d = store.device(id)?;
        if !read::allows(card, id) {
            return Err(Error::Invalid("card cannot be used with this device"));
        }
        let reference = json::fields(&[("$device", json::string(id)?)])?;
        if device.is_none() || json::text(card, "deviceArgument") == name {
            device = Some(d);
        }
        json::set(&mut args, name, reference)?;
    }
    if !partial {
        for def in json::array(card, "args")
            .iter()
            .filter(|v| json::text(v, "type") != "device")
        {
            let name = json::text(def, "name");
            if name.is_empty() {
                continue;
            }
            let value = field(&args, name);
            if value.is_null() {
                if json::boolean(def, "optional") {
                    continue;
                }
                return Err(Error::Invalid("required Flow card argument missing"));
            }
            if value.as_str().is_some_and(|_| contains_token(value)) {
                continue;
            }
            match json::text(def, "type") {
                "text" => {
                    if value.as_str().is_none() {
                        return Err(Error::Invalid("card argument must be text"));
                    }
                }
                "number" => {
                    let Value::Number(n) = value else {
                        return Err(Error::Invalid("card argument must be a number"));
                    };
                    schema::bounds(n.as_f64(), def, "min", "max")?;
                }
                "time" => {
                    let Some((hour, min)) = value.as_str().and_then(|s| s.split_once(':')) else {
                        return Err(Error::Invalid("time must use HH:MM"));
                    };
                    if hour.is_empty()
                        || hour.len() > 2
                        || min.len() != 2
                        || !hour.bytes().chain(min.bytes()).all(|c| c.is_ascii_digit())
                        || !hour.parse::<u8>().is_ok_and(|h| h <= 23)
                        || !min.parse::<u8>().is_ok_and(|m| m <= 59)
                    {
                        return Err(Error::Invalid("invalid 24-hour time"));
                    }
                }
                "dropdown" => {
                    if !choice(def, value) {
                        return Err(Error::Invalid("unknown dropdown id"));
                    }
                }
                "autocomplete" => {
                    if json::text(value, "id").trim().is_empty() {
                        return Err(Error::Invalid("autocomplete requires a choice with id"));
                    }
                }
                "capability" => {
                    let id = value.as_str().unwrap_or("");
                    if id.is_empty() || device.as_ref().is_none_or(|d| !read::has_cap(d, id)) {
                        return Err(Error::Invalid("device does not expose capability"));
                    }
                }
                "capability-value" => {
                    let d = device
                        .as_ref()
                        .ok_or(Error::Invalid("capability value needs selected device"))?;
                    let mut cap = json::text(card, "capability");
                    if cap.is_empty() {
                        cap = json::text(&args, "capability");
                    }
                    if !read::has_cap(d, cap) {
                        return Err(Error::Invalid("device does not expose capability"));
                    }
                    let def = crate::capability::output(store, d, cap)?;
                    if json::text(&step, "cardType") == "action" && !json::boolean(&def, "setable")
                    {
                        return Err(Error::Invalid("capability is read-only"));
                    }
                    capability_value(&def, value)?;
                }
                _ => return Err(Error::Invalid("unsupported Flow argument type")),
            }
        }
    }
    json::set(&mut step, "args", args)?;
    json::set(node, "step", step)
}
