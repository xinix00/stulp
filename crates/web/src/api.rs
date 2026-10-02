//! REST-dispatch deelt dezelfde mutaties als het appkanaal.
use super::{Environment, Request, Response, copy, objects};
use alloc::vec::Vec;
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};

pub(super) fn dispatch<S: Storage>(
    store: &mut Store<S>,
    req: &Request,
    env: &mut impl Environment,
) -> Result<Response> {
    if let Some(response) = super::manager::route(store, req, env)? {
        return Ok(response);
    }
    let path = req.path.as_str();
    let method = if req.method == "HEAD" {
        "GET"
    } else {
        &req.method
    };
    if method == "OPTIONS" {
        return Response::json(204, &Value::Null);
    }
    if method == "GET" {
        if path == "/api/stulp/statistics" {
            return Response::json(200, &store.statistics().list()?);
        }
        if let Some((id, cap)) = path
            .strip_prefix("/api/stulp/statistics/")
            .and_then(|s| s.split_once('/'))
        {
            let window = req
                .query
                .split('&')
                .filter_map(|q| q.split_once('='))
                .find(|(k, _)| *k == "window")
                .map(|(_, v)| v)
                .unwrap_or("");
            return Response::json(200, &store.statistics().window(id, cap, window)?);
        }

        if let Some(id) = path
            .strip_prefix("/api/stulp/devices/")
            .and_then(|p| p.strip_suffix("/media"))
            .filter(|id| !id.contains('/'))
        {
            return Response::json(200, &store.device_media(id)?);
        }
        if path == "/api/stulp/events" {
            super::query_value(&req.query, "manager")?;
            if !matches!(
                super::query_value(&req.query, "view")?.as_str(),
                "" | "overview"
            ) {
                return Response::error(400, "unknown event view");
            }
            return Ok(Response {
                status: 200,
                content_type: "text/event-stream",
                body: super::Body::Static(b": connected\n\n"),
                cookie: None,
                headers: Vec::new(),
            });
        }
        let result = match path {
            "/api/stulp/health" => Some(json::fields(&[
                ("ok", Value::Bool(true)),
                ("stulpVersion", json::string(env!("CARGO_PKG_VERSION"))?),
            ])?),
            "/api/stulp/manage/bootstrap" => Some(json::fields(&[
                ("ok", Value::Bool(true)),
                ("stulpVersion", json::string(env!("CARGO_PKG_VERSION"))?),
                ("devices", super::views::devices(store, "overview")?),
                (
                    "deviceGroups",
                    copy(json::get(store.document().root(), "deviceGroups"))?,
                ),
            ])?),
            "/api/manager/devices/device" => {
                let view = super::query_value(&req.query, "view")?;
                if !matches!(view.as_str(), "" | "overview" | "automation") {
                    return Response::error(400, "unknown device view");
                }
                Some(super::views::devices(store, &view)?)
            }
            "/api/manager/apps/app" => Some(objects::apps(store)?),
            "/api/manager/drivers/driver" => Some(objects::drivers(store)?),
            "/api/manager/system/ping" => Some(Value::Bool(true)),
            "/api/manager/system/name" => Some(json::string("Stulp")?),
            "/api/manager/system" => Some(json::fields(&[
                ("name", json::string("Stulp")?),
                ("version", json::string(env!("CARGO_PKG_VERSION"))?),
                ("language", json::string(store.language())?),
            ])?),
            "/api/stulp/system" => Some(system(store)?),
            _ => None,
        };
        if let Some(value) = result {
            return Response::json(200, &value);
        }
        if let Some(path) = path.strip_prefix("/api/manager/apps/app/") {
            return app_read(store, path);
        }
        if let Some(id) = path.strip_prefix("/api/manager/drivers/driver/") {
            let drivers = objects::drivers(store)?;
            return Response::json(
                200,
                json::get(&drivers, id).ok_or(Error::Missing("driver does not exist"))?,
            );
        }
    }
    if let Some(path) = path.strip_prefix("/api/manager/apps/app/")
        && let Some((id, tail)) = path.split_once('/')
    {
        if let Some(key) = tail.strip_prefix("setting/").filter(|key| !key.is_empty()) {
            match method {
                "PUT" => {
                    let body = json::parse(&req.body)?;
                    let value = json::get(&body, "value")
                        .ok_or(Error::Invalid("value is required"))?
                        .try_clone()?;
                    store.setting(id, key, Some(value))?;
                }
                "DELETE" => store.setting(id, key, None)?,
                _ => return Response::error(405, "method not allowed"),
            }
            return Response::json(200, &Value::Bool(true));
        }
        if method == "PUT" && matches!(tail, "enable" | "disable") {
            let mut record = store.document().record("apps", id)?.try_clone()?;
            if json::boolean(&record, "offered") {
                return Err(Error::Invalid("install the offered app first"));
            }
            json::set(&mut record, "enabled", Value::Bool(tail == "enable"))?;
            store.put("apps", record, false, None, &env.now()?)?;
            return Response::json(200, &Value::Bool(true));
        }
    }
    if path == "/api/stulp/system" && method == "PUT" {
        let body = json::parse(&req.body)?;
        let mut system = copy(json::get(store.document().root(), "system"))?;
        if let Some(value) = json::get(&body, "statistics") {
            if value.as_bool().is_none() {
                return Err(Error::Invalid("statistics must be boolean"));
            }
            json::set(&mut system, "statistics", value.try_clone()?)?;
        }
        if let Some(patch) = json::get(&body, "units") {
            let mut units = match json::get(&system, "units") {
                Some(value) => value.try_clone()?,
                None => json::object(),
            };
            stulp_core::units::update(&mut units, patch)?;
            json::set(&mut system, "units", units)?;
        }
        store.system(system)?;
        return Response::json(200, &self::system(store)?);
    }
    if path == "/api/stulp/devices/order" && method == "PUT" {
        let body = json::parse(&req.body)?;
        let mut ids = Vec::new();
        for id in json::array(&body, "deviceIds") {
            json::push(
                &mut ids,
                id.as_str().ok_or(Error::Invalid("invalid device id"))?,
                4096,
            )?;
        }
        store.reorder_devices(json::text(&body, "groupId"), &ids)?;
        return Response::json(200, &Value::Bool(true));
    }
    for (prefix, collection, keyed) in [
        ("/api/stulp/device-groups", "deviceGroups", false),
        ("/api/stulp/scenes", "scenes", false),
        ("/api/manager/flow/flow", "flows", true),
        (
            "/api/manager/notifications/notification",
            "notifications",
            false,
        ),
        ("/api/manager/devices/device", "devices", true),
    ] {
        if path == prefix {
            return collection_request(store, collection, keyed, method, req, env);
        }
        if let Some(id) = path.strip_prefix(prefix).and_then(|p| p.strip_prefix('/'))
            && !id.is_empty()
            && !id.contains('/')
        {
            return record_request(store, collection, id, method, req, env);
        }
    }
    Err(Error::Missing("route not found"))
}

fn system<S: Storage>(store: &Store<S>) -> Result<Value> {
    let mut system = copy(json::get(store.document().root(), "system"))?;
    json::remove(&mut system, "attachSecret")?;
    let units = stulp_core::units::filled(json::get(&system, "units").unwrap_or(&json::object()))?;
    json::set(&mut system, "units", units)?;
    json::set(&mut system, "unitsOffer", stulp_core::units::offer()?)?;
    json::set(
        &mut system,
        "statistics",
        Value::Bool(store.statistics_enabled()),
    )?;
    json::set(
        &mut system,
        "statisticsRunning",
        Value::Bool(store.statistics_enabled()),
    )?;
    json::set(
        &mut system,
        "statisticsBytes",
        Value::uint(store.statistics().bytes() as u64),
    )?;
    Ok(system)
}

fn collection_request<S: Storage>(
    store: &mut Store<S>,
    collection: &str,
    keyed: bool,
    method: &str,
    req: &Request,
    env: &mut impl Environment,
) -> Result<Response> {
    match method {
        "GET" => {
            if keyed {
                let mut out = json::Object::new();
                for record in store.document().records(collection) {
                    out.push(
                        json::text(record, "id"),
                        if collection == "flows" {
                            super::flow_units::convert(store, record, false, None)?
                        } else {
                            record.try_clone()?
                        },
                    )?;
                }
                Response::json(200, &Value::Object(out))
            } else if collection == "scenes" {
                let mut out = Vec::new();
                for s in store.document().records(collection) {
                    json::push(&mut out, super::scenes::output(store, s)?, 4096)?;
                }
                Response::json(200, &Value::Array(out))
            } else {
                Response::json(200, &copy(json::get(store.document().root(), collection))?)
            }
        }
        "POST" if matches!(collection, "deviceGroups" | "scenes" | "flows") => {
            let mut body = json::parse(&req.body)?;
            let id = env.id()?;
            json::set(&mut body, "id", json::string(&id)?)?;
            if collection == "scenes" && json::get(&body, "states").is_some() {
                super::scenes::incoming(store, &mut body, None)?;
            }
            if collection == "flows" {
                body = super::flow_units::convert(store, &body, true, None)?;
            }
            normalize_flow_ids(collection, &mut body, env)?;
            store.put(collection, body, true, None, &env.now()?)?;
            if collection == "scenes" {
                Response::json(
                    201,
                    &super::scenes::output(store, store.document().record(collection, &id)?)?,
                )
            } else {
                let record = store.document().record(collection, &id)?;
                Response::json(
                    201,
                    &if collection == "flows" {
                        super::flow_units::convert(store, record, false, None)?
                    } else {
                        record.try_clone()?
                    },
                )
            }
        }
        _ => Response::error(405, "method not allowed"),
    }
}

fn record_request<S: Storage>(
    store: &mut Store<S>,
    collection: &str,
    id: &str,
    method: &str,
    req: &Request,
    env: &mut impl Environment,
) -> Result<Response> {
    match method {
        "GET" if collection == "devices" => Response::json(200, &objects::device(store, id)?),
        "GET" if collection == "scenes" => Response::json(
            200,
            &super::scenes::output(store, store.document().record(collection, id)?)?,
        ),
        "GET" if collection == "flows" => Response::json(
            200,
            &super::flow_units::convert(
                store,
                store.document().record(collection, id)?,
                false,
                None,
            )?,
        ),
        "GET" => Response::json(200, store.document().record(collection, id)?),
        // Ontkoppelen vraagt eerst de app om externe middelen op te ruimen.
        "DELETE" if collection == "devices" => {
            Err(Error::Missing("device removal requires app callback"))
        }
        "DELETE" => {
            store.delete(collection, id)?;
            Response::json(200, &Value::Bool(true))
        }
        "PUT" if collection != "notifications" => {
            let body = json::parse(&req.body)?;
            if collection == "devices" {
                let d = store.document().record("devices", id)?;
                if json::text(d, "appId") == "com.stulp.scene"
                    && let Some(name) = json::get(&body, "name")
                    && !json::get(d, "name").is_some_and(|v| json::equal(v, name))
                {
                    let sid = json::copy(json::text(
                        json::get(d, "data").unwrap_or(&Value::Null),
                        "sceneId",
                    ))?;
                    let mut scene = store.document().record("scenes", &sid)?.try_clone()?;
                    json::set(&mut scene, "name", name.try_clone()?)?;
                    store.put("scenes", scene, false, None, &env.now()?)?;
                }
            }
            let mut record = store.document().record(collection, id)?.try_clone()?;
            let fields: &[&str] = match collection {
                "devices" => &["name", "groupId"],
                "deviceGroups" => &["name", "parentId", "sortOrder"],
                "flows" => &["name", "enabled", "nodes", "edges"],
                "scenes" => &["name", "kind", "states"],
                _ => &[],
            };
            for key in fields {
                if let Some(value) = json::get(&body, key) {
                    json::set(&mut record, key, value.try_clone()?)?;
                }
            }
            if collection == "devices" {
                super::manager::metadata(&mut record, &body)?;
            }
            if collection == "scenes" && json::get(&body, "states").is_some() {
                super::scenes::incoming(
                    store,
                    &mut record,
                    Some(store.document().record(collection, id)?),
                )?;
            }
            if collection == "flows" && json::get(&body, "nodes").is_some() {
                record = super::flow_units::convert(
                    store,
                    &record,
                    true,
                    Some(store.document().record(collection, id)?),
                )?;
            }
            normalize_flow_ids(collection, &mut record, env)?;
            let revision = json::get(&body, "revision")
                .map(|v| v.as_u64().ok_or(Error::Invalid("invalid revision")))
                .transpose()?;
            store.put(collection, record, false, revision, &env.now()?)?;
            if collection == "devices" {
                Response::json(200, &objects::device(store, id)?)
            } else if collection == "scenes" {
                Response::json(
                    200,
                    &super::scenes::output(store, store.document().record(collection, id)?)?,
                )
            } else {
                let record = store.document().record(collection, id)?;
                Response::json(
                    200,
                    &if collection == "flows" {
                        super::flow_units::convert(store, record, false, None)?
                    } else {
                        record.try_clone()?
                    },
                )
            }
        }
        _ => Response::error(405, "method not allowed"),
    }
}

fn normalize_flow_ids(collection: &str, value: &mut Value, env: &mut impl Environment) -> Result {
    if collection != "flows" {
        return Ok(());
    }
    for field in ["nodes", "edges"] {
        let mut records = Vec::new();
        for item in json::array(value, field) {
            let mut item = item.try_clone()?;
            if json::text(&item, "id").trim().is_empty() {
                json::set(&mut item, "id", json::string(&env.id()?)?)?;
            }
            json::push(&mut records, item, 256)?;
        }
        json::set(value, field, Value::Array(records))?;
    }
    Ok(())
}

fn app_read<S: Storage>(store: &Store<S>, path: &str) -> Result<Response> {
    let (id, tail) = path.split_once('/').unwrap_or((path, ""));
    let app = objects::app(store, id)?;
    let empty = json::object();
    let manifest = store.manifest(id).unwrap_or(&empty);
    let settings = json::get(store.document().root(), "appSettings")
        .and_then(|s| json::get(s, id))
        .unwrap_or(&empty);
    let value = match tail {
        "" => app,
        "setting" => settings.try_clone()?,
        "locale" => json::fields(&[
            ("language", json::string(store.language())?),
            ("name", copy(json::get(&app, "name"))?),
            ("manifest", manifest.try_clone()?),
        ])?,
        "update" => json::fields(&[
            ("version", copy(json::get(&app, "version"))?),
            ("updateAvailable", Value::Bool(false)),
            (
                "reason",
                json::string(
                    "an app is updated by placing a new image; it announces its version when it attaches",
                )?,
            ),
        ])?,
        _ => {
            let key = tail
                .strip_prefix("setting/")
                .filter(|k| !k.is_empty())
                .ok_or(Error::Missing("route not found"))?;
            json::get(settings, key)
                .ok_or(Error::Missing("setting does not exist"))?
                .try_clone()?
        }
    };
    Response::json(200, &value)
}
