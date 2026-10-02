//! Beheerhandelingen gebruiken dezelfde documenteigenaar als de appverbindingen.
use super::{Environment, Request, Response, objects};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};

pub(super) fn route<S: Storage>(
    store: &mut Store<S>,
    r: &Request,
    env: &mut impl Environment,
) -> Result<Option<Response>> {
    let method = if r.method == "HEAD" { "GET" } else { &r.method };
    if method == "GET"
        && let Some(id) = r
            .path
            .strip_prefix("/api/stulp/attach-token/")
            .filter(|id| !id.is_empty() && !id.contains('/'))
    {
        let mut system = json::get(store.document().root(), "system")
            .unwrap_or(&json::object())
            .try_clone()?;
        if json::text(&system, "attachSecret").is_empty() {
            let mut secret = env.id()?;
            let extra = env.id()?;
            secret.try_reserve(extra.len()).map_err(|_| Error::Memory)?;
            secret.push_str(&extra);
            json::set(&mut system, "attachSecret", json::string(&secret)?)?;
            store.system(system.try_clone()?)?;
        }
        let token = stulp_protocol::token::token(json::text(&system, "attachSecret"), id)?;
        return Ok(Some(Response::json(
            200,
            &json::fields(&[
                ("appId", json::string(id)?),
                ("token", json::string(&token)?),
                (
                    "known",
                    Value::Bool(store.document().record("apps", id).is_ok()),
                ),
            ])?,
        )?));
    }
    if method == "POST"
        && let Some(id) = r
            .path
            .strip_prefix("/api/stulp/apps/")
            .and_then(|v| v.strip_suffix("/install"))
            .filter(|v| !v.is_empty() && !v.contains('/'))
    {
        let mut app = store.document().record("apps", id)?.try_clone()?;
        if json::boolean(&app, "offered") {
            json::set(&mut app, "offered", Value::Bool(false))?;
            json::set(&mut app, "enabled", Value::Bool(true))?;
            store.put("apps", app, false, None, &env.now()?)?;
        }
        return Ok(Some(Response::json(200, &objects::app(store, id)?)?));
    }
    if method == "PUT"
        && let Some(id) = r
            .path
            .strip_prefix("/api/stulp/devices/")
            .and_then(|v| v.strip_suffix("/group"))
            .filter(|v| !v.is_empty() && !v.contains('/'))
    {
        let body = json::parse(&r.body)?;
        let group = json::get(&body, "groupId")
            .and_then(Value::as_str)
            .ok_or(Error::Invalid("groupId is required"))?;
        let mut d = store.device(id)?;
        json::set(&mut d, "groupId", json::string(group)?)?;
        store.put("devices", d, false, None, &env.now()?)?;
        return Ok(Some(Response::json(200, &objects::device(store, id)?)?));
    }
    if method == "PUT"
        && let Some(id) = r
            .path
            .strip_prefix("/api/manager/flow/flow/")
            .and_then(|v| v.strip_suffix("/enabled"))
            .filter(|v| !v.is_empty() && !v.contains('/'))
    {
        let body = json::parse(&r.body)?;
        let enabled = json::get(&body, "enabled")
            .and_then(Value::as_bool)
            .ok_or(Error::Invalid("enabled must be boolean"))?;
        let mut flow = store.document().record("flows", id)?.try_clone()?;
        json::set(&mut flow, "enabled", Value::Bool(enabled))?;
        store.put("flows", flow, false, None, &env.now()?)?;
        return Ok(Some(Response::json(
            200,
            store.document().record("flows", id)?,
        )?));
    }
    if method == "GET" && r.path == "/api/manager/devices/capability" {
        let mut catalog = json::object();
        for record in store.document().records("devices") {
            let d = store.device(json::text(record, "id"))?;
            for id in json::array(&d, "capabilities")
                .iter()
                .filter_map(Value::as_str)
            {
                if json::get(&catalog, id).is_none() {
                    let definition = super::capability::definition(store, &d, id)?;
                    json::set(
                        &mut catalog,
                        id,
                        json::fields(&[
                            ("id", json::string(id)?),
                            ("uri", json::string("stulp:manager:devices")?),
                            ("type", json::string(json::text(&definition, "type"))?),
                            ("getable", Value::Bool(true)),
                            ("setable", Value::Bool(true)),
                        ])?,
                    )?;
                }
            }
        }
        return Ok(Some(Response::json(200, &catalog)?));
    }
    if method == "GET"
        && let Some((id, cap)) = r
            .path
            .strip_prefix("/api/manager/devices/device/")
            .and_then(|v| v.split_once("/capability/"))
    {
        let d = store.device(id)?;
        if !json::array(&d, "capabilities")
            .iter()
            .any(|v| v.as_str() == Some(cap))
        {
            return Err(Error::Missing("capability does not exist"));
        }
        return Ok(Some(Response::json(
            200,
            json::get(&d, "state")
                .and_then(|v| json::get(v, cap))
                .unwrap_or(&Value::Null),
        )?));
    }
    Ok(None)
}

pub(super) fn metadata(record: &mut Value, patch: &Value) -> Result {
    let mut data = json::get(record, "store")
        .unwrap_or(&json::object())
        .try_clone()?;
    for (key, stored) in FIELDS {
        if let Some(value) = json::get(patch, key) {
            json::set(&mut data, stored, value.try_clone()?)?;
        }
    }
    json::set(record, "store", data)
}
pub(super) const FIELDS: &[(&str, &str)] = &[
    ("zone", "__stulp.api.zone"),
    ("note", "__stulp.api.note"),
    ("iconOverride", "__stulp.api.iconOverride"),
    ("virtualClass", "__stulp.api.virtualClass"),
    ("uiIndicator", "__stulp.api.uiIndicator"),
    ("hidden", "__stulp.api.hidden"),
];
