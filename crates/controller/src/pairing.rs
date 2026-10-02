//! Browserkoppelingen hebben één eigenaar, een deadline en expliciete rollback.
use crate::Apps;
use crate::Reply;
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_runtime::Completion;
use stulp_web::{Environment, Request, Response};

const MAX_SESSIONS: usize = 32;
const MAX_PENDING: usize = 64;
struct Session {
    id: String,
    app: String,
    driver: String,
}
enum Stage {
    Start(Session),
    Emit,
    Close,
    Settings {
        id: String,
        patch: Value,
        now: String,
    },
    Delete {
        id: String,
    },
    Driver {
        app: String,
        id: String,
        params: Value,
    },
    Device {
        id: String,
    },
}
struct Pending<R: Reply> {
    owner: u64,
    deadline: u64,
    reply: R,
    stage: Stage,
}
/// Owns pairing sessions and pending device lifecycle operations.
pub struct Pairing<R: Reply> {
    sessions: Vec<Session>,
    pending: Vec<Pending<R>>,
}
impl<R: Reply> Pairing<R> {
    /// Create an owner with no pairing sessions.
    pub fn new() -> Self {
        Self {
            sessions: Vec::new(),
            pending: Vec::new(),
        }
    }
    fn reserve(&mut self) -> Result {
        if self.pending.len() == MAX_PENDING {
            return Err(Error::Full);
        }
        self.pending.try_reserve(1).map_err(|_| Error::Memory)
    }
    fn send(
        &mut self,
        apps: &mut dyn Apps,
        owner: &mut u64,
        reply: R,
        call: (&str, &str, &Value),
        stage: Stage,
    ) -> Result {
        let (app, method, params) = call;
        self.reserve()?;
        let next = owner.checked_add(1).ok_or(Error::Full)?;
        apps.call(app, next, method, params)?;
        *owner = next;
        self.pending.push(Pending {
            owner: next,
            deadline: apps
                .now()
                .saturating_add(stulp_runtime::callback_timeout_for(app, method, params) + 1000),
            reply,
            stage,
        });
        Ok(())
    }
    fn device_busy(&self, id: &str) -> bool {
        self.pending.iter().any(|p| match &p.stage {
            Stage::Driver { id: pending, .. }
            | Stage::Device { id: pending }
            | Stage::Delete { id: pending }
            | Stage::Settings { id: pending, .. } => pending == id,
            _ => false,
        })
    }
    /// Alleen aangeroepen nadat Web sessie en Origin heeft gecontroleerd.
    pub fn route<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        apps: &mut dyn Apps,
        env: &mut impl Environment,
        owner: &mut u64,
        r: &Request,
        reply: &R,
    ) -> Result<bool> {
        if r.method == "PUT"
            && let Some(id) = r
                .path
                .strip_prefix("/api/manager/devices/device/")
                .and_then(|p| p.strip_suffix("/settings"))
                .filter(|id| !id.is_empty() && !id.contains('/'))
        {
            let d = store.document().record("devices", id)?;
            if self.device_busy(id) {
                return Err(Error::Conflict("device lifecycle operation in progress"));
            }
            let input = json::parse(&r.body)?;
            let entries = input
                .as_object()
                .ok_or(Error::Invalid("settings must be an object"))?;
            let previous = json::get(d, "settings").unwrap_or(&Value::Null);
            let mut patch = json::object();
            for (key, value) in entries.iter() {
                if !json::get(previous, key).is_some_and(|old| json::equal(old, value)) {
                    json::set(&mut patch, key, value.try_clone()?)?;
                }
            }
            if patch.as_object().is_some_and(|p| p.is_empty()) {
                let _ = reply.send(Response::json(200, &stulp_web::device_object(store, id)?)?);
                return Ok(true);
            }
            let params = json::fields(&[
                ("deviceId", json::string(id)?),
                ("settings", patch.try_clone()?),
            ])?;
            self.send(
                apps,
                owner,
                reply.clone(),
                (json::text(d, "appId"), "device.settings", &params),
                Stage::Settings {
                    id: json::copy(id)?,
                    patch,
                    now: env.now()?,
                },
            )?;
            return Ok(true);
        }
        if r.method == "DELETE"
            && let Some(id) = r
                .path
                .strip_prefix("/api/manager/devices/device/")
                .filter(|v| !v.is_empty() && !v.contains('/'))
        {
            let d = store.document().record("devices", id)?;
            let app = json::copy(json::text(d, "appId"))?;
            if app == "com.stulp.scene" {
                let scene = json::copy(json::text(
                    json::get(d, "data").unwrap_or(&Value::Null),
                    "sceneId",
                ))?;
                store.delete("scenes", &scene)?;
            } else {
                if self.device_busy(id) {
                    return Err(Error::Conflict("device lifecycle operation in progress"));
                }
                let params = json::fields(&[("deviceId", json::string(id)?)])?;
                let stage = Stage::Delete {
                    id: json::copy(id)?,
                };
                self.reserve()?;
                match self.send(
                    apps,
                    owner,
                    reply.clone(),
                    (&app, "device.delete", &params),
                    stage,
                ) {
                    Ok(()) => return Ok(true),
                    Err(Error::Missing(_) | Error::Invalid(_)) => (),
                    Err(e) => return Err(e),
                }
                store.delete("devices", id)?;
            }
            let _ = reply.send(Response::json(200, &Value::Bool(true))?);
            return Ok(true);
        }
        if r.method == "POST" && r.path == "/api/stulp/pair" {
            let body = json::parse(&r.body)?;
            let app = json::text(&body, "appId");
            let driver = json::text(&body, "driverId");
            definition(store, app, driver)?;
            let starting = self
                .pending
                .iter()
                .filter(|p| matches!(p.stage, Stage::Start(_)))
                .count();
            if self.sessions.len() + starting >= MAX_SESSIONS {
                return Err(Error::Full);
            }
            self.sessions
                .try_reserve(starting + 1)
                .map_err(|_| Error::Memory)?;
            let session = Session {
                id: env.id()?,
                app: json::copy(app)?,
                driver: json::copy(driver)?,
            };
            let params = json::fields(&[
                ("sessionId", json::string(&session.id)?),
                ("driverId", json::string(driver)?),
            ])?;
            self.send(
                apps,
                owner,
                reply.clone(),
                (app, "pair.start", &params),
                Stage::Start(session),
            )?;
            return Ok(true);
        }
        if let Some(rest) = r.path.strip_prefix("/api/stulp/pair/") {
            if r.method == "DELETE" && !rest.is_empty() && !rest.contains('/') {
                if let Some(i) = self.sessions.iter().position(|s| s.id == rest) {
                    let params = json::fields(&[("sessionId", json::string(rest)?)])?;
                    let session = self.sessions.remove(i);
                    // Sluiten is idempotent, ook wanneer de plugin intussen is verdwenen.
                    if self
                        .send(
                            apps,
                            owner,
                            reply.clone(),
                            (&session.app, "pair.close", &params),
                            Stage::Close,
                        )
                        .is_ok()
                    {
                        return Ok(true);
                    }
                }
                let _ = reply.send(Response::json(200, &Value::Bool(true))?);
                return Ok(true);
            }
            if r.method == "POST"
                && let Some((id, event)) = rest.split_once("/emit/")
            {
                if event.is_empty() || event.contains('/') {
                    return Err(Error::Invalid("invalid pair event"));
                }
                let session = self
                    .sessions
                    .iter()
                    .find(|s| s.id == id)
                    .ok_or(Error::Missing("pair session does not exist"))?;
                let app = json::copy(&session.app)?;
                let data = if r.body.is_empty() {
                    Value::Null
                } else {
                    json::parse(&r.body)?
                };
                let params = json::fields(&[
                    ("sessionId", json::string(id)?),
                    ("event", json::string(event)?),
                    ("data", data),
                ])?;
                self.send(
                    apps,
                    owner,
                    reply.clone(),
                    (&app, "pair.emit", &params),
                    Stage::Emit,
                )?;
                return Ok(true);
            }
        }
        if r.method == "POST"
            && let Some(rest) = r.path.strip_prefix("/api/stulp/apps/")
            && let Some((app, rest)) = rest.split_once("/drivers/")
            && let Some(driver) = rest
                .strip_suffix("/pair/devices")
                .filter(|v| !v.is_empty() && !v.contains('/'))
        {
            let input = json::parse(&r.body)?;
            let id = env.id()?;
            let record = candidate(definition(store, app, driver)?, app, driver, &id, &input)?;
            if let Some(existing) = store.document().records("devices").iter().find(|d| {
                json::text(d, "appId") == app
                    && json::text(d, "driverId") == driver
                    && json::equal(
                        json::get(d, "data").unwrap_or(&Value::Null),
                        json::get(&record, "data").unwrap_or(&Value::Null),
                    )
            }) {
                let id = json::text(existing, "id");
                if self.pending.iter().any(|p| match &p.stage {
                    Stage::Driver { id: pending, .. } | Stage::Device { id: pending } => {
                        pending == id
                    }
                    _ => false,
                }) {
                    return Err(Error::Conflict("device is still being initialized"));
                }
                let _ = reply.send(Response::json(201, &stulp_web::device_object(store, id)?)?);
                return Ok(true);
            }
            self.reserve()?;
            let params = json::fields(&[
                ("driverId", json::string(driver)?),
                ("deviceId", json::string(&id)?),
            ])?;
            let stage = Stage::Driver {
                app: json::copy(app)?,
                id: json::copy(&id)?,
                params: params.try_clone()?,
            };
            store.put("devices", record, true, None, &env.now()?)?;
            let result = store
                .device(&id)
                .and_then(|d| apps.adopt(&d))
                .and_then(|()| {
                    self.send(
                        apps,
                        owner,
                        reply.clone(),
                        (app, "driver.init", &params),
                        stage,
                    )
                });
            if let Err(error) = result {
                store.delete("devices", &id)?;
                return Err(error);
            }
            return Ok(true);
        }
        Ok(false)
    }
    /// Finish a lifecycle callback, rolling back failed device initialization.
    pub fn complete<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        apps: &mut dyn Apps,
        c: &Completion,
    ) -> bool {
        let Some(i) = self.pending.iter().position(|p| p.owner == c.owner) else {
            return false;
        };
        let p = self.pending.remove(i);
        let result = self.finish(store, apps, c, &p.stage);
        match result {
            Ok(None) => self.pending.push(Pending {
                stage: match p.stage {
                    Stage::Driver { id, .. } => Stage::Device { id },
                    _ => return true,
                },
                deadline: apps
                    .now()
                    .saturating_add(stulp_runtime::callback_timeout("device.init") + 1000),
                ..p
            }),
            Ok(Some(response)) => {
                let _ = p.reply.send(response);
            }
            Err(error) => {
                let mut error = error;
                if let Stage::Driver { id, .. } | Stage::Device { id } = &p.stage
                    && store.document().record("devices", id).is_ok()
                    && let Err(rollback) = store.delete("devices", id)
                {
                    error = rollback;
                }
                if let Stage::Start(s) = &p.stage {
                    // De start kan na de deadline alsnog voltooid zijn; ruim de pluginzijde ook op.
                    if let Ok(params) =
                        json::fields(&[("sessionId", json::string(&s.id).unwrap_or(Value::Null))])
                    {
                        let _ = apps.call(&s.app, p.owner, "pair.close", &params);
                    }
                }
                let detail = c
                    .value
                    .as_str()
                    .unwrap_or_else(|| json::text(&c.value, "message"));
                let message = if c.failed && !detail.is_empty() && !matches!(error, Error::Storage)
                {
                    json::copy(detail)
                } else {
                    stulp_runtime::flows::error_text(&error)
                };
                if let Ok(message) = message
                    && let Ok(response) = Response::error(
                        if matches!(error, Error::Storage) {
                            503
                        } else {
                            502
                        },
                        &message,
                    )
                {
                    let _ = p.reply.send(response);
                }
            }
        }
        true
    }
    fn finish<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        apps: &mut dyn Apps,
        c: &Completion,
        stage: &Stage,
    ) -> Result<Option<Response>> {
        if let Stage::Delete { id } = stage {
            if c.failed {
                apps.log(format_args!(
                    "[stulp:unpair] plugin could not clean up device {id}"
                ));
            }
            store.delete("devices", id)?;
            return Ok(Some(Response::json(200, &Value::Bool(true))?));
        }
        if matches!(stage, Stage::Close) {
            return Ok(Some(Response::json(200, &Value::Bool(true))?));
        }
        if c.failed {
            return Err(Error::Invalid("plugin lifecycle callback failed"));
        }
        Ok(Some(match stage {
            Stage::Start(s) => {
                let handlers = c
                    .value
                    .as_array()
                    .ok_or(Error::Invalid("pair handlers must be an array"))?;
                if handlers.iter().any(|v| v.as_str().is_none()) {
                    return Err(Error::Invalid("pair handler must be a string"));
                }
                let response = Response::json(
                    201,
                    &json::fields(&[
                        ("id", json::string(&s.id)?),
                        ("appId", json::string(&s.app)?),
                        ("driverId", json::string(&s.driver)?),
                        ("handlers", c.value.try_clone()?),
                    ])?,
                )?;
                let session = Session {
                    id: json::copy(&s.id)?,
                    app: json::copy(&s.app)?,
                    driver: json::copy(&s.driver)?,
                };
                self.sessions.push(session);
                response
            }
            Stage::Settings { id, patch, now } => {
                // Neem het nieuwste record: een naam of pluginrapport kan intussen veranderen.
                let mut d = store.device(id)?;
                let mut settings = json::get(&d, "settings")
                    .unwrap_or(&json::object())
                    .try_clone()?;
                for (key, value) in patch
                    .as_object()
                    .ok_or(Error::Invalid("invalid settings patch"))?
                    .iter()
                {
                    json::set(&mut settings, key, value.try_clone()?)?;
                }
                json::set(&mut d, "settings", settings)?;
                store.put("devices", d, false, None, now)?;
                Response::json(200, &stulp_web::device_object(store, id)?)?
            }
            Stage::Emit => Response::json(200, &c.value)?,
            Stage::Driver { app, params, .. } => {
                apps.call(app, c.owner, "device.init", params)?;
                return Ok(None);
            }
            Stage::Device { id } => Response::json(201, &stulp_web::device_object(store, id)?)?,
            Stage::Close | Stage::Delete { .. } => {
                return Err(Error::Invalid("unexpected pair stage"));
            }
        }))
    }
    /// Expire lifecycle operations and close abandoned pairing sessions.
    pub fn poll<S: Storage>(&mut self, store: &mut Store<S>, apps: &mut dyn Apps) {
        while let Some(owner) = self
            .pending
            .iter()
            .find(|p| p.deadline <= apps.now())
            .map(|p| p.owner)
        {
            self.complete(
                store,
                apps,
                &Completion {
                    owner,
                    failed: true,
                    value: Value::Null,
                },
            );
        }
    }
}
fn definition<'a, S: Storage>(store: &'a Store<S>, app: &str, driver: &str) -> Result<&'a Value> {
    if !json::boolean(store.document().record("apps", app)?, "enabled") {
        return Err(Error::Invalid("app is disabled"));
    }
    let manifest = store
        .manifest(app)
        .ok_or(Error::Missing("app manifest unavailable"))?;
    json::array(manifest, "drivers")
        .iter()
        .find(|d| json::text(d, "id") == driver)
        .ok_or(Error::Missing("driver does not exist"))
}
/// Apply driver defaults to a candidate while preserving controller identity.
pub fn candidate(
    driver: &Value,
    app: &str,
    driver_id: &str,
    id: &str,
    input: &Value,
) -> Result<Value> {
    let name = json::text(input, "name");
    if name.trim().is_empty() {
        return Err(Error::Invalid("paired device needs a name"));
    }
    let class = json::text(input, "class");
    let mut out = json::fields(&[
        ("id", json::string(id)?),
        ("appId", json::string(app)?),
        ("driverId", json::string(driver_id)?),
        ("name", json::string(name)?),
        (
            "class",
            json::string(if class.is_empty() {
                json::text(driver, "class")
            } else {
                class
            })?,
        ),
    ])?;
    let mut settings = json::object();
    for setting in json::array(driver, "settings") {
        let key = json::text(setting, "id");
        if !key.is_empty()
            && let Some(v) = json::get(setting, "value").filter(|v| **v != Value::Null)
        {
            json::set(&mut settings, key, v.try_clone()?)?;
        }
    }
    for key in ["data", "store", "settings"] {
        let v = json::get(input, key).unwrap_or(&Value::Null);
        if key == "settings" {
            if let Some(fields) = v.as_object() {
                for (k, v) in fields.iter() {
                    json::set(&mut settings, k, v.try_clone()?)?;
                }
            } else if *v != Value::Null {
                return Err(Error::Invalid("candidate settings must be an object"));
            }
        } else {
            if *v != Value::Null && v.as_object().is_none() {
                return Err(Error::Invalid("candidate data/store must be an object"));
            }
            json::set(
                &mut out,
                key,
                if *v == Value::Null {
                    json::object()
                } else {
                    v.try_clone()?
                },
            )?;
        }
    }
    json::set(&mut out, "settings", settings)?;
    let caps = json::get(input, "capabilities").or_else(|| json::get(driver, "capabilities"));
    let caps = match caps {
        Some(Value::Array(a)) if a.iter().all(|v| v.as_str().is_some()) => {
            caps.ok_or(Error::Invalid("capabilities"))?.try_clone()?
        }
        None | Some(Value::Null) => Value::Array(Vec::new()),
        _ => return Err(Error::Invalid("candidate capabilities must be strings")),
    };
    json::set(&mut out, "capabilities", caps)?;
    Ok(out)
}

impl<R: Reply> Default for Pairing<R> {
    fn default() -> Self {
        Self::new()
    }
}
