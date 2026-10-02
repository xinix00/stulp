//! Sceneverzoeken delen één rij; iedere apparaatgroep krijgt een eigen callbackhandvat.
use crate::Apps;
use crate::Reply;
use alloc::{
    string::{String, ToString},
    vec::Vec,
};
use stulp_core::{
    Error, Result,
    json::{self, Value},
    store::{Storage, Store},
};
use stulp_runtime::{Completion, scenes::Scene};
use stulp_web::{Request, Response};
const MAX_JOBS: usize = 32;
const MAX_CALLS: usize = 16;
struct Job<R: Reply> {
    id: String,
    on: bool,
    run: Option<Scene>,
    reply: Option<R>,
    owner: Option<u64>,
    deadline: u64,
}
struct Call {
    owner: u64,
    scene: String,
    device: String,
}
/// Owns scene requests and individual device-group callbacks.
pub struct Scenes<R: Reply> {
    jobs: Vec<Job<R>>,
    calls: Vec<Call>,
    trips: Vec<Value>,
}
impl<R: Reply> Scenes<R> {
    /// Create an idle scene owner.
    pub fn new() -> Self {
        Self {
            jobs: Vec::new(),
            calls: Vec::new(),
            trips: Vec::new(),
        }
    }
    /// Queue activation or restoration and retain its response route.
    pub fn submit(
        &mut self,
        id: &str,
        on: bool,
        now: u64,
        reply: Option<R>,
        owner: Option<u64>,
    ) -> Result {
        json::push(
            &mut self.jobs,
            Job {
                id: json::copy(id.trim())?,
                on,
                run: None,
                reply,
                owner,
                deadline: now.saturating_add(45_000),
            },
            MAX_JOBS,
        )
    }
    /// Handle a scene HTTP route; false leaves the request to other services.
    pub fn route<S: Storage>(
        &mut self,
        store: &Store<S>,
        r: &Request,
        now: u64,
        reply: &R,
    ) -> Result<bool> {
        if r.method != "PUT" {
            return Ok(false);
        }
        let Some((id, cap)) = r
            .path
            .strip_prefix("/api/manager/devices/device/")
            .and_then(|p| p.split_once("/capability/"))
        else {
            return Ok(false);
        };
        let d = store.device(id)?;
        if json::text(&d, "appId") != "com.stulp.scene" {
            return Ok(false);
        }
        if !json::array(&d, "capabilities")
            .iter()
            .any(|v| v.as_str() == Some(cap))
        {
            return Err(Error::Invalid("scene capability does not exist"));
        }
        let body = json::parse(&r.body)?;
        let on = json::get(&body, "value")
            .and_then(Value::as_bool)
            .ok_or(Error::Invalid("scene value must be boolean"))?;
        let scene = json::get(&d, "data")
            .map(|v| json::text(v, "sceneId"))
            .unwrap_or("");
        self.submit(scene, on, now, Some(reply.clone()), None)?;
        Ok(true)
    }
    /// Accept a matching app callback without repeating a physical write.
    pub fn complete(&mut self, c: &Completion) -> Result<bool> {
        let Some(i) = self.calls.iter().position(|p| p.owner == c.owner) else {
            return Ok(false);
        };
        let call = self.calls.remove(i);
        if let Some(run) = self
            .jobs
            .iter_mut()
            .find(|j| j.id == call.scene && j.run.is_some())
            .and_then(|j| j.run.as_mut())
        {
            let message = c
                .value
                .as_str()
                .unwrap_or_else(|| json::text(&c.value, "message"));
            let fail = if c.failed {
                Some(if message.is_empty() {
                    "app callback failed"
                } else {
                    message
                })
            } else if !c.value.is_null() && c.value.as_object().is_none() {
                Some("invalid grouped capability response")
            } else {
                None
            };
            run.complete(&call.device, &c.value, fail)?;
        }
        Ok(true)
    }
    /// Advance scene jobs and retain restore state for partial failures.
    pub fn poll<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        apps: &mut dyn Apps,
        owner: &mut u64,
    ) -> Result<Vec<Completion>> {
        let now = apps.now();
        let mut out = Vec::new();
        let mut i = 0;
        while i < self.jobs.len() {
            if self.jobs[i].run.is_none()
                && self.jobs.iter().take(i).any(|j| j.id == self.jobs[i].id)
            {
                i += 1;
                continue;
            }
            let job = &mut self.jobs[i];
            let result = (|| -> Result<Option<Value>> {
                if job.run.is_none() {
                    if now >= job.deadline {
                        return Err(Error::Invalid("scene execution timed out"));
                    }
                    job.run = Some(Scene::start(store, &job.id, job.on)?);
                }
                let run = job
                    .run
                    .as_mut()
                    .ok_or(Error::Missing("scene execution missing"))?;
                if now >= job.deadline {
                    run.cancel()?;
                }
                while self.calls.len() < MAX_CALLS && !run.done() {
                    let Some((device, params)) = run.next_group()? else {
                        break;
                    };
                    let next = owner.checked_add(1).ok_or(Error::Full)?;
                    self.calls.try_reserve(1).map_err(|_| Error::Memory)?;
                    let result = store.document().record("devices", &device).and_then(|d| {
                        if json::text(d, "appId") == "com.stulp.scene" {
                            return Err(Error::Invalid("scene cannot control another scene"));
                        }
                        apps.call(json::text(d, "appId"), next, "capabilities.invoke", &params)
                    });
                    *owner = next;
                    match result {
                        Ok(()) => self.calls.push(Call {
                            owner: next,
                            scene: json::copy(run.id())?,
                            device,
                        }),
                        Err(e) => run.complete(&device, &Value::Null, Some(&e.to_string()))?,
                    }
                }
                if run.done() {
                    Ok(Some(run.finish(store)?))
                } else {
                    Ok(None)
                }
            })();
            let result = match result {
                Ok(None) => {
                    i += 1;
                    continue;
                }
                Ok(Some(v)) => Ok(v),
                Err(e) => Err(e),
            };
            let job = self.jobs.remove(i);
            self.calls.retain(|c| c.scene != job.id);
            let (value, failed) = match result {
                Ok(value) => {
                    let failed = !json::boolean(&value, "success");
                    (value, failed)
                }
                Err(e) => (
                    json::fields(&[("message", json::string(&e.to_string())?)])?,
                    true,
                ),
            };
            if let Some(reply) = job.reply {
                let response = if failed {
                    Response::error(
                        502,
                        if json::text(&value, "message").is_empty() {
                            "Niet alle scene-opdrachten zijn uitgevoerd; herstel blijft beschikbaar."
                        } else {
                            json::text(&value, "message")
                        },
                    )?
                } else {
                    Response::json(200, &Value::Bool(true))?
                };
                let _ = reply.send(response);
            }
            if let Some(owner) = job.owner {
                json::push(
                    &mut out,
                    Completion {
                        owner,
                        failed,
                        value,
                    },
                    MAX_JOBS,
                )?;
            }
        }
        if let Err(e) = self.follow(store) {
            apps.log(format_args!("[stulp:scene-follow] {e}"));
        }
        Ok(out)
    }
    fn follow<S: Storage>(&mut self, store: &mut Store<S>) -> Result {
        while self.trips.len() < 256
            && let Some(trip) = store.take_scene_trip()
        {
            if !self.trips.iter().any(|v| json::equal(v, &trip)) {
                json::push(&mut self.trips, trip, 256)?;
            }
        }
        let mut i = 0;
        while i < self.trips.len() {
            let trip = &self.trips[i];
            let id = json::text(trip, "sceneId");
            if self.jobs.iter().any(|j| j.id == id) {
                i += 1;
                continue;
            }
            let device = json::text(trip, "deviceId");
            let cap = json::text(trip, "capabilityId");
            if let Ok(scene) = store.document().record("scenes", id)
                && json::boolean(scene, "active")
                && json::array(scene, "previous").iter().any(|v| {
                    json::text(v, "deviceId") == device && json::text(v, "capabilityId") == cap
                })
                && let Some(target) = json::array(scene, "states").iter().find(|v| {
                    json::text(v, "deviceId") == device && json::text(v, "capabilityId") == cap
                })
                && let Ok(d) = store.device(device)
                && let Some(current) = json::get(&d, "state")
                    .and_then(|v| json::get(v, cap))
                    .filter(|v| !v.is_null())
                && !stulp_runtime::scenes::same(
                    current,
                    json::get(target, "value").unwrap_or(&Value::Null),
                )
            {
                store.scene_remaining(id, Vec::new())?;
            }
            self.trips.remove(i);
        }
        Ok(())
    }
}

impl<R: Reply> Default for Scenes<R> {
    fn default() -> Self {
        Self::new()
    }
}
