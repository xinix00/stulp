//! Registraties worden tegelijk opgehaald; een trage app blokkeert de controller niet.
use crate::Apps;
use crate::Reply;
use alloc::{
    string::{String, ToString},
    vec::Vec,
};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_runtime::Completion;
use stulp_web::{Request, Response};
struct Call {
    owner: u64,
    app: String,
}
struct Job<R: Reply> {
    calls: Vec<Call>,
    registrations: Value,
    reply: R,
}
/// Collects active plugin card registrations without blocking other requests.
pub struct Catalog<R: Reply> {
    jobs: Vec<Job<R>>,
}
impl<R: Reply> Catalog<R> {
    /// Create an empty registration-job owner.
    pub fn new() -> Self {
        Self { jobs: Vec::new() }
    }
    /// Start a card-catalog request if this is its HTTP route.
    pub fn route<S: Storage>(
        &mut self,
        store: &Store<S>,
        apps: &mut dyn Apps,
        owner: &mut u64,
        req: &Request,
        reply: &R,
    ) -> Result<bool> {
        if req.path != "/api/stulp/flow/cards" || !matches!(req.method.as_str(), "GET" | "HEAD") {
            return Ok(false);
        }
        if self.jobs.len() >= 8 {
            return Err(Error::Full);
        }
        self.jobs.try_reserve(1).map_err(|_| Error::Memory)?;
        let mut job = Job {
            calls: Vec::new(),
            registrations: json::object(),
            reply: reply.clone(),
        };
        job.calls.try_reserve_exact(32).map_err(|_| Error::Memory)?;
        for app in store.document().records("apps") {
            let id = json::text(app, "id");
            if !apps.running(id)
                || store
                    .manifest(id)
                    .and_then(|m| json::get(m, "flow"))
                    .is_none()
            {
                continue;
            }
            if job.calls.len() == 32 {
                return Err(Error::Full);
            }
            let app = json::copy(id)?;
            let next = owner.checked_add(1).ok_or(Error::Full)?;
            if apps.call(id, next, "registrations", &Value::Null).is_ok() {
                *owner = next;
                job.calls.push(Call { owner: next, app });
            }
        }
        if job.calls.is_empty() {
            let _ = job.reply.send(Response::json(
                200,
                &stulp_web::flow_cards(store, &job.registrations)?,
            )?);
        } else {
            self.jobs.push(job);
        }
        Ok(true)
    }
    /// Merge a plugin registration response and finish a complete catalog.
    pub fn complete<S: Storage>(&mut self, store: &Store<S>, c: &Completion) -> Result<bool> {
        let Some((i, j)) = self.jobs.iter().enumerate().find_map(|(i, job)| {
            job.calls
                .iter()
                .position(|p| p.owner == c.owner)
                .map(|j| (i, j))
        }) else {
            return Ok(false);
        };
        let job = self.jobs.get_mut(i).ok_or(Error::Full)?;
        let call = job.calls.remove(j);
        if !c.failed {
            json::set(&mut job.registrations, &call.app, c.value.try_clone()?)?;
        }
        if job.calls.is_empty() {
            let job = self.jobs.remove(i);
            let response = stulp_web::flow_cards(store, &job.registrations)
                .and_then(|v| Response::json(200, &v));
            let response = match response {
                Ok(r) => r,
                Err(e) => Response::error(500, &e.to_string())?,
            };
            let _ = job.reply.send(response);
        }
        Ok(true)
    }
}

impl<R: Reply> Default for Catalog<R> {
    fn default() -> Self {
        Self::new()
    }
}
