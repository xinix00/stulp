//! Smalle MCP-projecties geven ids en bediening door, nooit appState, store of pairingdata.
use super::*;
fn limited(out: &mut Value, v: &Value, keys: &[&str], max: usize) -> Result {
    for key in keys {
        let s = json::text(v, key);
        if !s.is_empty() {
            json::set(out, key, trim(s, max)?)?;
        }
    }
    Ok(())
}
fn ancestry<'a, S: Storage>(store: &'a Store<S>, mut id: &'a str) -> Result<Vec<&'a Value>> {
    let mut out = Vec::new();
    for _ in 0..16 {
        if id.is_empty() {
            break;
        }
        let Ok(g) = store.document().record("deviceGroups", id) else {
            break;
        };
        json::push(&mut out, g, 16)?;
        id = json::text(g, "parentId");
    }
    Ok(out)
}
fn group_path<S: Storage>(store: &Store<S>, id: &str) -> Result<String> {
    let mut path = String::new();
    for g in ancestry(store, id)?.iter().rev() {
        let Value::String(name) = trim(json::text(g, "name"), 500)? else {
            return Err(Error::Full);
        };
        path.try_reserve(name.len() + 3)
            .map_err(|_| Error::Memory)?;
        if !path.is_empty() {
            path.push_str(" / ");
        }
        path.push_str(&name);
    }
    Ok(path)
}
fn in_group<S: Storage>(store: &Store<S>, d: &Value, id: &str) -> Result<bool> {
    Ok(ancestry(store, json::text(d, "groupId"))?
        .iter()
        .any(|g| json::text(g, "id") == id))
}
fn groups<S: Storage>(store: &Store<S>) -> Result<Value> {
    let mut out = Vec::new();
    for group in store.document().records("deviceGroups") {
        let id = json::text(group, "id");
        let mut direct = 0;
        let mut nested = 0;
        for d in store.document().records("devices") {
            direct += usize::from(json::text(d, "groupId") == id);
            nested += usize::from(in_group(store, d, id)?);
        }
        let mut g = json::fields(&[
            ("id", trim(id, 256)?),
            ("name", trim(json::text(group, "name"), 500)?),
            ("path", json::string(&group_path(store, id)?)?),
            ("deviceCount", Value::uint(direct as u64)),
        ])?;
        if nested != direct {
            json::set(
                &mut g,
                "deviceCountIncludingSubgroups",
                Value::uint(nested as u64),
            )?;
        }
        limited(&mut g, group, &["parentId"], 256)?;
        json::push(&mut out, g, 4096)?;
    }
    Ok(Value::Array(out))
}
pub(super) fn page(values: Vec<Value>, key: &str, args: &Value) -> Result<Value> {
    let total = values.len();
    let offset = usize::try_from(json::uint(args, "offset")).unwrap_or(usize::MAX);
    let limit = json::get(args, "limit")
        .and_then(Value::as_u64)
        .unwrap_or(50)
        .clamp(1, 100) as usize;
    let end = offset.saturating_add(limit);
    let mut page = Vec::new();
    for v in values.into_iter().skip(offset).take(limit) {
        json::push(&mut page, v, 100)?;
    }
    let mut out = json::fields(&[
        (key, Value::Array(page)),
        ("total", Value::uint(total as u64)),
    ])?;
    if end < total {
        json::set(&mut out, "nextOffset", Value::uint(end as u64))?;
    }
    Ok(out)
}
fn contains(haystack: &str, needle: &str) -> Result<bool> {
    let lower = |s: &str| -> Result<String> {
        let mut out = String::new();
        out.try_reserve(s.len().saturating_mul(3))
            .map_err(|_| Error::Memory)?;
        for c in s.chars().flat_map(char::to_lowercase) {
            out.push(c);
        }
        Ok(out)
    };
    Ok(lower(haystack)?.contains(&lower(needle)?))
}
pub(super) fn has_cap(d: &Value, cap: &str) -> bool {
    json::array(d, "capabilities")
        .iter()
        .any(|c| c.as_str() == Some(cap))
}
fn hardware(d: &Value) -> &str {
    let saved = json::text(field(d, "store"), "__stulp.hardwareName");
    if saved.is_empty() {
        json::text(d, "name")
    } else {
        saved
    }
}
pub(super) fn device<S: Storage>(
    store: &Store<S>,
    d: &Value,
    only: &str,
    detail: bool,
) -> Result<Value> {
    let mut out = json::fields(&[
        ("id", trim(json::text(d, "id"), 256)?),
        ("name", trim(json::text(d, "name"), 500)?),
        ("class", trim(json::text(d, "class"), 256)?),
        ("available", Value::Bool(json::boolean(d, "available"))),
    ])?;
    if detail {
        limited(&mut out, d, &["appId"], 256)?;
        json::set(&mut out, "hardwareName", trim(hardware(d), 500)?)?;
        let mut caps = json::Object::new();
        for cap in json::array(d, "capabilities")
            .iter()
            .filter_map(Value::as_str)
            .filter(|cap| only.is_empty() || *cap == only)
        {
            let def = crate::capability::output(store, d, cap)?;
            let mut projected = json::fields(&[
                ("id", trim(cap, 256)?),
                (
                    "title",
                    trim(
                        &crate::cards::title(cap, field(&def, "title"), store.language())?,
                        500,
                    )?,
                ),
                ("type", field(&def, "type").try_clone()?),
                ("getable", Value::Bool(json::boolean(&def, "getable"))),
                ("setable", Value::Bool(json::boolean(&def, "setable"))),
                ("hasValue", Value::Bool(false)),
            ])?;
            let value = field(&def, "value");
            if matches!(value, Value::Bool(_) | Value::Number(_) | Value::String(_)) {
                json::set(&mut projected, "hasValue", Value::Bool(true))?;
                json::set(
                    &mut projected,
                    "value",
                    if let Some(s) = value.as_str() {
                        trim(s, 2048)?
                    } else {
                        value.try_clone()?
                    },
                )?;
            }
            for k in ["min", "max", "step", "units"] {
                if let Some(v) = json::get(&def, k).filter(|v| !v.is_null()) {
                    json::set(&mut projected, k, v.try_clone()?)?;
                }
            }
            let choices = enums(field(&def, "values"), store.language())?;
            if !choices.as_array().unwrap_or(&[]).is_empty() {
                json::set(&mut projected, "values", choices)?;
            }
            caps.push(cap, projected)?;
        }
        json::set(&mut out, "capabilities", Value::Object(caps))?;
    } else {
        let mut caps = Vec::new();
        for cap in json::array(d, "capabilities")
            .iter()
            .filter_map(Value::as_str)
        {
            json::push(&mut caps, trim(cap, 256)?, 4096)?;
        }
        json::set(&mut out, "capabilities", Value::Array(caps))?;
    }
    let path = group_path(store, json::text(d, "groupId"))?;
    if !path.is_empty() {
        json::set(&mut out, "group", json::string(&path)?)?;
    }
    if (detail || !json::boolean(d, "available")) && !json::text(d, "unavailableMessage").is_empty()
    {
        json::set(
            &mut out,
            "unavailableMessage",
            trim(json::text(d, "unavailableMessage"), 2048)?,
        )?;
    }
    Ok(out)
}
pub(super) fn enums(raw: &Value, lang: &str) -> Result<Value> {
    let mut out = Vec::new();
    for v in raw.as_array().unwrap_or(&[]).iter().take(128) {
        let id = if v.as_object().is_some() {
            field(v, "id")
        } else {
            v
        };
        if !matches!(id, Value::Bool(_) | Value::Number(_) | Value::String(_))
            || id
                .as_str()
                .is_some_and(|s| s.is_empty() || s.chars().count() > 256)
        {
            continue;
        }
        let label = manifest::localized(field(v, "title"), lang);
        let label = if label.is_empty() || label.chars().count() > 500 {
            if let Some(s) = id.as_str() {
                json::copy(s)?
            } else {
                json::to_string(id)?
            }
        } else {
            json::copy(label)?
        };
        json::push(
            &mut out,
            json::fields(&[("id", id.try_clone()?), ("title", json::string(&label)?)])?,
            128,
        )?;
    }
    Ok(Value::Array(out))
}
fn devices<S: Storage>(store: &Store<S>, args: &Value) -> Result<Value> {
    let id = text(args, "deviceId");
    let cap = text(args, "capabilityId");
    let group = text(args, "groupId");
    if !group.is_empty() {
        store.document().record("deviceGroups", group)?;
    }
    let mut out = Vec::new();
    for record in store.document().records("devices") {
        if !id.is_empty() && json::text(record, "id") != id {
            continue;
        }
        let d = store.device(json::text(record, "id"))?;
        if !cap.is_empty() && !has_cap(&d, cap) {
            if !id.is_empty() {
                return Err(Error::Missing("device capability does not exist"));
            }
            continue;
        }
        if id.is_empty() {
            if (!group.is_empty() && !in_group(store, &d, group)?)
                || (json::boolean(args, "availableOnly") && !json::boolean(&d, "available"))
            {
                continue;
            }
            let search = text(args, "search");
            if !search.is_empty()
                && !contains(json::text(&d, "id"), search)?
                && !contains(json::text(&d, "name"), search)?
                && !contains(hardware(&d), search)?
                && !contains(&group_path(store, json::text(&d, "groupId"))?, search)?
            {
                continue;
            }
            if json::boolean(args, "writableOnly") {
                let mut writable = false;
                for c in json::array(&d, "capabilities")
                    .iter()
                    .filter_map(Value::as_str)
                    .filter(|c| cap.is_empty() || *c == cap)
                {
                    writable |=
                        json::boolean(&crate::capability::definition(store, &d, c)?, "setable");
                }
                if !writable {
                    continue;
                }
            }
        }
        json::push(
            &mut out,
            device(store, &d, cap, !id.is_empty() || !cap.is_empty())?,
            4096,
        )?;
    }
    if !id.is_empty() && out.is_empty() {
        return Err(Error::Missing("device does not exist"));
    }
    page(
        out,
        "devices",
        if id.is_empty() { args } else { &Value::Null },
    )
}
pub(super) fn card<'a>(cards: &'a Value, step: &Value) -> Option<&'a Value> {
    let kind = json::text(step, "cardType").trim_start_matches("device-");
    let list = match kind {
        "trigger" => "triggers",
        "condition" => "conditions",
        "action" => "actions",
        _ => return None,
    };
    json::array(cards, list).iter().find(|c| {
        json::text(c, "appId") == json::text(step, "appId")
            && json::text(c, "id") == json::text(step, "cardId")
            && json::text(c, "type") == json::text(step, "cardType")
    })
}
pub(super) fn allows(card: &Value, id: &str) -> bool {
    json::text(card, "scope") != "device"
        || json::array(card, "deviceIds")
            .iter()
            .any(|v| v.as_str() == Some(id))
}
fn definitions(raw: &Value, lang: &str) -> Result<Value> {
    let mut out = Vec::new();
    for v in raw.as_array().unwrap_or(&[]) {
        if v.as_object().is_none() {
            continue;
        }
        let mut p = json::object();
        limited(&mut p, v, &["name", "type"], 256)?;
        for key in ["title", "hint", "placeholder", "filter", "units"] {
            let s = manifest::localized(field(v, key), lang);
            if !s.is_empty() {
                json::set(
                    &mut p,
                    key,
                    trim(s, if key == "units" { 256 } else { 2048 })?,
                )?;
            }
        }
        for k in ["optional", "min", "max", "step"] {
            if let Some(v) =
                json::get(v, k).filter(|v| matches!(v, Value::Bool(_) | Value::Number(_)))
            {
                json::set(&mut p, k, v.try_clone()?)?;
            }
        }
        let values = enums(field(v, "values"), lang)?;
        if !values.as_array().unwrap_or(&[]).is_empty() {
            json::set(&mut p, "values", values)?;
        }
        json::push(&mut out, p, 512)?;
    }
    Ok(Value::Array(out))
}
fn cards_list<S: Storage>(store: &Store<S>, args: &Value, cards: &Value) -> Result<Value> {
    let device = text(args, "deviceId");
    if !device.is_empty() {
        store.document().record("devices", device)?;
    }
    let detail = !text(args, "cardId").is_empty();
    let mut out = Vec::new();
    for (kind, list) in [
        ("trigger", "triggers"),
        ("condition", "conditions"),
        ("action", "actions"),
    ] {
        if !text(args, "kind").is_empty() && text(args, "kind") != kind {
            continue;
        }
        for c in json::array(cards, list) {
            if json::get(args, "availableOnly")
                .and_then(Value::as_bool)
                .unwrap_or(true)
                && !json::boolean(c, "available")
            {
                continue;
            }
            if (!device.is_empty() && !allows(c, device))
                || (!text(args, "cardId").is_empty() && text(args, "cardId") != json::text(c, "id"))
                || (!text(args, "appId").is_empty()
                    && text(args, "appId") != json::text(c, "appId"))
            {
                continue;
            }
            let search = text(args, "search");
            if !search.is_empty()
                && !contains(json::text(c, "id"), search)?
                && !contains(json::text(c, "title"), search)?
                && !contains(json::text(c, "appName"), search)?
            {
                continue;
            }
            let mut p = json::object();
            limited(&mut p, c, &["appId", "id", "type"], 256)?;
            limited(&mut p, c, &["appName", "title"], 2048)?;
            json::set(
                &mut p,
                "available",
                Value::Bool(json::boolean(c, "available")),
            )?;
            if detail {
                limited(&mut p, c, &["scope", "deviceArgument", "capability"], 256)?;
                limited(&mut p, c, &["titleFormatted", "hint"], 2048)?;
                for key in ["args", "tokens"] {
                    let v = definitions(field(c, key), store.language())?;
                    if !v.as_array().unwrap_or(&[]).is_empty() {
                        json::set(&mut p, key, v)?;
                    }
                }
            }
            json::push(&mut out, p, 8192)?;
        }
    }
    page(out, "cards", args)
}
pub(super) fn flow_summary(f: &Value) -> Result<Value> {
    let mut p = json::fields(&[
        ("id", field(f, "id").try_clone()?),
        ("name", field(f, "name").try_clone()?),
        ("enabled", Value::Bool(json::boolean(f, "enabled"))),
        (
            "nodeCount",
            Value::uint(json::array(f, "nodes").len() as u64),
        ),
        (
            "edgeCount",
            Value::uint(json::array(f, "edges").len() as u64),
        ),
        ("createdAt", json::string(json::text(f, "createdAt"))?),
        ("updatedAt", json::string(json::text(f, "updatedAt"))?),
    ])?;
    limited(&mut p, f, &["lastRunAt"], 256)?;
    limited(&mut p, f, &["lastError"], 2048)?;
    Ok(p)
}
fn flow_object<S: Storage>(store: &Store<S>, f: &Value, cards: &Value) -> Result<Value> {
    let f = crate::flow_units::convert(store, f, false, None)?;
    let mut p = flow_summary(&f)?;
    let mut nodes = Vec::new();
    for n in json::array(&f, "nodes") {
        let s = field(n, "step");
        let definition = card(cards, s).unwrap_or(&Value::Null);
        let mut step = json::object();
        limited(&mut step, s, &["appId", "cardId", "cardType"], 256)?;
        let mut args = json::object();
        for def in json::array(definition, "args") {
            let name = json::text(def, "name");
            let Some(value) = json::get(field(s, "args"), name) else {
                continue;
            };
            let value = match json::text(def, "type") {
                "device" => {
                    let id = json::text(value, "$device");
                    if id.is_empty() {
                        continue;
                    }
                    json::fields(&[("$device", trim(id, 256)?)])?
                }
                "autocomplete" => {
                    let mut choice = json::object();
                    for k in ["id", "name", "description"] {
                        let value = field(value, k);
                        if !value.is_null() && schema::validate(value, &Value::Null).is_ok() {
                            json::set(&mut choice, k, value.try_clone()?)?;
                        }
                    }
                    if field(&choice, "id").is_null() {
                        continue;
                    }
                    choice
                }
                _ => {
                    if schema::validate(value, &Value::Null).is_err() {
                        continue;
                    }
                    value.try_clone()?
                }
            };
            json::set(&mut args, name, value)?;
        }
        if args.as_object().is_some_and(|o| o.iter().next().is_some()) {
            json::set(&mut step, "args", args)?;
        }
        if json::boolean(s, "inverted") {
            json::set(&mut step, "inverted", Value::Bool(true))?;
        }
        json::push(
            &mut nodes,
            json::fields(&[
                ("id", field(n, "id").try_clone()?),
                (
                    "x",
                    json::get(n, "x").unwrap_or(&Value::uint(0)).try_clone()?,
                ),
                (
                    "y",
                    json::get(n, "y").unwrap_or(&Value::uint(0)).try_clone()?,
                ),
                ("step", step),
            ])?,
            128,
        )?;
    }
    let mut edges = Vec::new();
    for e in json::array(&f, "edges") {
        json::push(
            &mut edges,
            json::fields(&[
                ("id", field(e, "id").try_clone()?),
                ("from", field(e, "from").try_clone()?),
                ("to", field(e, "to").try_clone()?),
            ])?,
            256,
        )?;
    }
    json::set(&mut p, "nodes", Value::Array(nodes))?;
    json::set(&mut p, "edges", Value::Array(edges))?;
    Ok(p)
}
pub(super) fn dispatch<S: Storage>(
    store: &Store<S>,
    name: &str,
    args: &Value,
    cards: &Value,
) -> Result<Option<Value>> {
    let value = match name {
        "system_context" => json::fields(&[(
            "context",
            json::fields(&[
                ("stulpVersion", json::string(env!("CARGO_PKG_VERSION"))?),
                ("language", json::string(store.language())?),
                ("timezone", json::string(store.timezone())?),
                (
                    "units",
                    stulp_core::units::filled(
                        json::get(field(store.document().root(), "system"), "units")
                            .unwrap_or(&json::object()),
                    )?,
                ),
                ("statistics", Value::Bool(store.statistics_enabled())),
                ("statisticsRunning", Value::Bool(store.statistics_enabled())),
                ("deviceGroups", groups(store)?),
            ])?,
        )])?,
        "devices_list" => devices(store, args)?,
        "flow_cards_list" => cards_list(store, args, cards)?,
        "flows_list" => {
            let id = text(args, "flowId");
            let mut out = Vec::new();
            for f in store.document().records("flows") {
                if !id.is_empty() {
                    if json::text(f, "id") == id {
                        json::push(&mut out, flow_object(store, f, cards)?, 4096)?;
                    }
                    continue;
                }
                let search = text(args, "search");
                if !search.is_empty()
                    && !contains(json::text(f, "id"), search)?
                    && !contains(json::text(f, "name"), search)?
                {
                    continue;
                }
                if let Some(enabled) = json::get(args, "enabled").and_then(Value::as_bool)
                    && enabled != json::boolean(f, "enabled")
                {
                    continue;
                }
                json::push(&mut out, flow_summary(f)?, 4096)?;
            }
            if !id.is_empty() && out.is_empty() {
                return Err(Error::Missing("flow does not exist"));
            }
            page(
                out,
                "flows",
                if id.is_empty() { args } else { &Value::Null },
            )?
        }
        _ => return Ok(None),
    };
    Ok(Some(value))
}
