//! De editorcatalogus ontstaat uit manifesten, echte listeners en apparaatmetadata.
use super::{capability, filter};
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Result,
    json::{self, TryClone, Value},
    manifest,
    store::{Storage, Store},
};
const MAX_CARDS: usize = 8192;
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
fn append(parts: &[&str]) -> Result<String> {
    let mut s = String::new();
    s.try_reserve(parts.iter().map(|s| s.len()).sum())
        .map_err(|_| stulp_core::Error::Memory)?;
    for part in parts {
        s.push_str(part);
    }
    Ok(s)
}
#[derive(Default)]
struct Cards {
    triggers: Vec<Value>,
    conditions: Vec<Value>,
    actions: Vec<Value>,
}
fn push(out: &mut Cards, kind: &str, card: Value) -> Result {
    let list = match kind {
        "triggers" => &mut out.triggers,
        "conditions" => &mut out.conditions,
        _ => &mut out.actions,
    };
    json::push(list, card, MAX_CARDS)
}
fn scope(card: &mut Value, devices: &[Value]) -> Result {
    let Some(arg) = json::array(card, "args")
        .iter()
        .find(|a| json::text(a, "type") == "device")
    else {
        return json::set(card, "scope", json::string("app")?);
    };
    let name = json::string(json::text(arg, "name"))?;
    let mut ids = Vec::new();
    for d in devices
        .iter()
        .filter(|d| filter::matches(d, field(arg, "filter")))
    {
        json::push(&mut ids, json::string(json::text(d, "id"))?, 4096)?;
    }
    json::set(card, "scope", json::string("device")?)?;
    json::set(card, "deviceArgument", name)?;
    json::set(card, "deviceIds", Value::Array(ids))
}
/// Geen RPC's in de catalogusbouw: de adapter levert afgeronde registraties per app.
pub fn catalog<S: Storage>(store: &Store<S>, registrations: &Value) -> Result<Value> {
    let builtin = json::parse(include_bytes!("../data/cards.json"))?;
    let mut out = Cards::default();
    let mut devices = Vec::new();
    for d in store.document().records("devices") {
        json::push(&mut devices, store.device(json::text(d, "id"))?, 4096)?;
    }
    for kind in ["triggers", "conditions", "actions"] {
        for card in json::array(&builtin, kind) {
            let mut card = card.try_clone()?;
            scope(&mut card, &devices)?;
            push(&mut out, kind, card)?;
        }
    }
    let lang = language(store);
    for app in store.document().records("apps") {
        let id = json::text(app, "id");
        let Some(m) = store.manifest(id) else {
            continue;
        };
        let flow = field(m, "flow");
        let regs = field(registrations, id);
        for (kind, typ) in [
            ("triggers", "trigger"),
            ("conditions", "condition"),
            ("actions", "action"),
        ] {
            for c in json::array(flow, kind)
                .iter()
                .filter(|c| !json::text(c, "id").is_empty())
            {
                let cid = json::text(c, "id");
                let device = json::array(regs, "flows").iter().find(|r| {
                    json::text(r, "id") == cid && json::text(r, "type") == "device-trigger"
                });
                let registration = if typ == "trigger" && device.is_some() {
                    device
                } else {
                    json::array(regs, "flows")
                        .iter()
                        .find(|r| json::text(r, "id") == cid && json::text(r, "type") == typ)
                }
                .unwrap_or(&Value::Null);
                let actual_type = if registration.is_null() {
                    typ
                } else {
                    json::text(registration, "type")
                };
                let mut args = field(c, "args").try_clone()?;
                if let Value::Array(args) = &mut args {
                    for arg in args {
                        capability::show_units(store, arg)?;
                    }
                }
                let mut registration = registration.try_clone()?;
                if !registration.is_null() {
                    json::remove(&mut registration, "id")?;
                }
                let mut card = json::fields(&[
                    ("appId", json::string(id)?),
                    (
                        "appName",
                        json::string(manifest::localized(field(m, "name"), lang))?,
                    ),
                    ("id", json::string(cid)?),
                    ("type", json::string(actual_type)?),
                    (
                        "title",
                        json::string(manifest::localized(field(c, "title"), lang))?,
                    ),
                    (
                        "titleFormatted",
                        json::string(manifest::localized(field(c, "titleFormatted"), lang))?,
                    ),
                    (
                        "hint",
                        json::string(manifest::localized(field(c, "hint"), lang))?,
                    ),
                    ("args", args),
                    ("tokens", field(c, "tokens").try_clone()?),
                    (
                        "available",
                        Value::Bool(
                            store.app_status(id) == "running"
                                && (typ == "trigger"
                                    || json::boolean(&registration, "runListener")),
                        ),
                    ),
                    ("registration", registration),
                ])?;
                scope(&mut card, &devices)?;
                push(&mut out, kind, card)?;
            }
        }
    }
    let mut caps: Vec<Cap> = Vec::new();
    for d in &devices {
        for id in json::array(d, "capabilities")
            .iter()
            .filter_map(Value::as_str)
        {
            let def = capability::output(store, d, id)?;
            let i = if let Some(i) = caps.iter().position(|c| c.id == id) {
                i
            } else {
                let i = caps.len();
                json::push(
                    &mut caps,
                    Cap {
                        id: json::copy(id)?,
                        title: title(id, field(&def, "title"), lang)?,
                        def: def.try_clone()?,
                        read: Vec::new(),
                        write: Vec::new(),
                        stateful: false,
                    },
                    4096,
                )?;
                i
            };
            let c = caps.get_mut(i).ok_or(stulp_core::Error::Full)?;
            if json::boolean(&def, "getable") {
                json::push(&mut c.read, json::string(json::text(d, "id"))?, 4096)?;
            }
            if json::boolean(&def, "setable") {
                json::push(&mut c.write, json::string(json::text(d, "id"))?, 4096)?;
            }
            c.stateful |= json::boolean(&def, "getable") && json::boolean(&def, "setable");
        }
    }
    for (i, c) in caps.iter().enumerate() {
        let title = if caps
            .iter()
            .enumerate()
            .any(|(j, other)| j != i && other.title == c.title)
        {
            append(&[&c.title, " (", &c.id, ")"])?
        } else {
            json::copy(&c.title)?
        };
        c.emit(&mut out, &title)?;
    }
    json::fields(&[
        ("triggers", Value::Array(out.triggers)),
        ("conditions", Value::Array(out.conditions)),
        ("actions", Value::Array(out.actions)),
    ])
}
pub(super) fn language<S: Storage>(store: &Store<S>) -> &str {
    store.language()
}
pub(super) fn title(id: &str, declared: &Value, language: &str) -> Result<String> {
    let own = manifest::localized(declared, language);
    if !own.is_empty() && own != id {
        return json::copy(own);
    }
    let (base, suffix) = id.split_once('.').unwrap_or((id, ""));
    let name = super::titles::standard(base);
    if name.is_empty() {
        return json::copy(id);
    }
    if suffix.is_empty() {
        json::copy(name)
    } else {
        append(&[name, " ", suffix])
    }
}
struct Cap {
    id: String,
    title: String,
    def: Value,
    read: Vec<Value>,
    write: Vec<Value>,
    stateful: bool,
}
impl Cap {
    fn emit(&self, out: &mut Cards, title: &str) -> Result {
        let value_type = json::text(&self.def, "type");
        let mut value = json::fields(&[
            ("name", json::string("value")?),
            ("type", json::string("capability-value")?),
            ("title", json::string("Waarde")?),
        ])?;
        for key in ["min", "max", "step", "units", "values"] {
            if let Some(v) = json::get(&self.def, key).filter(|v| !v.is_null()) {
                json::set(&mut value, key, v.try_clone()?)?;
            }
        }
        let seconds=json::parse(br#"{"name":"seconds","type":"number","title":"Seconden","min":1,"max":86400,"step":1}"#)?;
        let base = self.id.split('.').next().unwrap_or(&self.id);
        if !self.read.is_empty() {
            match value_type {
                "boolean" => {
                    let variants: &[(&str, &str)] = if base == "button" {
                        &[
                            ("on", " werd ingedrukt"),
                            ("off", " werd losgelaten"),
                            ("on_for", " bleef ingedrukt"),
                        ]
                    } else if self.id.starts_with("alarm_") {
                        &[
                            ("on", " ging af"),
                            ("off", " is voorbij"),
                            ("on_for", " bleef actief"),
                            ("off_for", " bleef rustig"),
                        ]
                    } else {
                        &[
                            ("on", " werd aan"),
                            ("off", " werd uit"),
                            ("on_for", " bleef aan"),
                            ("off_for", " bleef uit"),
                        ]
                    };
                    for (suffix, text) in variants {
                        self.card(
                            out,
                            "triggers",
                            suffix,
                            &append(&[title, text])?,
                            if suffix.ends_with("_for") {
                                Some(&seconds)
                            } else {
                                None
                            },
                        )?;
                    }
                    if base != "button" {
                        self.card(
                            out,
                            "conditions",
                            "is",
                            &append(&[title, " is"])?,
                            Some(&value),
                        )?;
                    }
                    for (suffix, text) in [
                        (
                            "is_on",
                            if base == "button" {
                                " is ingedrukt"
                            } else {
                                " is aan"
                            },
                        ),
                        (
                            "is_off",
                            if base == "button" {
                                " is losgelaten"
                            } else {
                                " is uit"
                            },
                        ),
                    ] {
                        self.card(out, "conditions", suffix, &append(&[title, text])?, None)?;
                    }
                }
                "number" => {
                    for (kind, suffix, text, arg) in [
                        ("triggers", "changed", " is veranderd", false),
                        ("triggers", "rose_above", " kwam boven", true),
                        ("triggers", "fell_below", " kwam onder", true),
                        ("conditions", "is", " is gelijk aan", true),
                        ("conditions", "above", " is hoger dan", true),
                        ("conditions", "below", " is lager dan", true),
                    ] {
                        self.card(
                            out,
                            kind,
                            suffix,
                            &append(&[title, text])?,
                            arg.then_some(&value),
                        )?;
                    }
                }
                _ => {
                    self.card(
                        out,
                        "triggers",
                        "changed",
                        &append(&[title, " is veranderd"])?,
                        None,
                    )?;
                    let enumerated = !json::array(&self.def, "values").is_empty();
                    if enumerated {
                        self.card(
                            out,
                            "triggers",
                            "became",
                            &append(&[title, " werd"])?,
                            Some(&value),
                        )?;
                    }
                    self.card(
                        out,
                        "conditions",
                        "is",
                        &append(&[title, if enumerated { " is" } else { " is gelijk aan" }])?,
                        Some(&value),
                    )?;
                }
            }
        }
        if !self.write.is_empty() {
            if !self.stateful {
                self.card(
                    out,
                    "actions",
                    "run",
                    &append(&[title, " uitvoeren"])?,
                    None,
                )?;
            } else {
                self.card(
                    out,
                    "actions",
                    "set",
                    &append(&["Zet ", title])?,
                    Some(&value),
                )?;
                if value_type == "boolean" {
                    for (suffix, prefix, end) in [
                        ("turn_on", "Zet ", " aan"),
                        ("turn_off", "Zet ", " uit"),
                        ("toggle", "Wissel ", ""),
                    ] {
                        self.card(
                            out,
                            "actions",
                            suffix,
                            &append(&[prefix, title, end])?,
                            None,
                        )?;
                    }
                }
            }
        }
        Ok(())
    }
    fn card(
        &self,
        out: &mut Cards,
        kind: &str,
        suffix: &str,
        title: &str,
        extra: Option<&Value>,
    ) -> Result {
        let mut args = Vec::new();
        json::push(
            &mut args,
            json::fields(&[
                ("name", json::string("device")?),
                ("type", json::string("device")?),
                ("title", json::string("Apparaat")?),
            ])?,
            2,
        )?;
        if let Some(extra) = extra {
            json::push(&mut args, extra.try_clone()?, 2)?;
        }
        let mut card = json::fields(&[
            ("appId", json::string("stulp")?),
            ("appName", json::string("Apparaat")?),
            (
                "id",
                json::string(&append(&["capability.", &self.id, ".", suffix])?)?,
            ),
            (
                "type",
                json::string(kind.strip_suffix('s').unwrap_or(kind))?,
            ),
            ("title", json::string(title)?),
            ("available", Value::Bool(true)),
            ("capability", json::string(&self.id)?),
            ("args", Value::Array(args)),
            ("scope", json::string("device")?),
            ("deviceArgument", json::string("device")?),
            (
                "deviceIds",
                Value::Array(if kind == "actions" {
                    self.write.try_clone()?
                } else {
                    self.read.try_clone()?
                }),
            ),
        ])?;
        if kind == "triggers" {
            let typ = json::text(&self.def, "type");
            let typ = if matches!(typ, "number" | "boolean") {
                typ
            } else {
                "string"
            };
            let (last, lt, ln) = if suffix.ends_with("_for") {
                ("seconds", "number", "Seconden")
            } else {
                ("oldValue", typ, "Vorige waarde")
            };
            let mut tokens = Vec::new();
            for (name, typ, title) in [
                ("device", "string", "Apparaat"),
                ("value", typ, "Nieuwe waarde"),
                (last, lt, ln),
            ] {
                json::push(
                    &mut tokens,
                    json::fields(&[
                        ("name", json::string(name)?),
                        ("type", json::string(typ)?),
                        ("title", json::string(title)?),
                    ])?,
                    3,
                )?;
            }
            json::set(&mut card, "tokens", Value::Array(tokens))?;
        }
        if let Some(extra) = extra {
            let tail = if json::text(extra, "name") == "seconds" {
                " gedurende [[seconds]] seconden"
            } else {
                " [[value]]"
            };
            json::set(
                &mut card,
                "titleFormatted",
                json::string(&append(&[title, tail])?)?,
            )?;
        }
        push(out, kind, card)
    }
}
