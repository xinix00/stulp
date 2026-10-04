//! Vier uitvoeringen delen de controller; geen thread per Flow of slapende delay.
use crate::Reply;
use alloc::{
    collections::VecDeque,
    string::{String, ToString},
    vec::Vec,
};
use stulp_core::{
    Error, Result,
    json::{self, Value},
    store::{Storage, Store},
};
use stulp_runtime::{
    Completion,
    flows::{Effect, Run},
};
use stulp_web::{Environment, Response};

const MAX_RUNS: usize = 4;
/// Vanaf deze rijdiepte meldt de eigenaar zich, hoogstens eens per [`QUEUE_LOG_MS`].
const QUEUE_LOG_DEPTH: usize = 32;
const QUEUE_LOG_MS: u64 = 5000;
struct Job<R: Reply> {
    owner: u64,
    run: Run,
    reply: Option<R>,
    capability: bool,
    timing: Option<Timing>,
}
/// De meting van een getriggerde run: hoe lang het event in de rij stond,
/// en per kaart de tijd van verzenden tot antwoord (03-10: "beweging naar
/// lamp lijkt 5 s"; zonder dit was niet te zien waar die tijd zat).
struct Timing {
    lag: u64,
    started: u64,
    trigger: String,
    device: String,
    card: Option<(String, u64)>,
    cards: Vec<(String, u64)>,
}
impl Timing {
    fn finished(&mut self, now: u64) {
        if let Some((label, at)) = self.card.take() {
            let _ = json::push(&mut self.cards, (label, now.saturating_sub(at)), 64);
        }
    }
}

struct Trigger {
    event: Value,
    definitions: Vec<Value>,
    lag: u64,
}
/// Owns bounded Flow runs, scheduled triggers and stability timers.
pub struct Flows<R: Reply> {
    jobs: Vec<Job<R>>,
    trigger: Option<Trigger>,
    stability: stulp_runtime::stability::Stability,
    schedule: stulp_runtime::schedule::Schedule,
    clock: crate::timezone::Timezone,
    observed: Option<u64>,
    checked: Option<(i64, u64)>,
    /// Wanneer de eigenaar elk wachtend triggerevent voor het eerst zag, in
    /// de volgorde van de rij in de store.
    seen: VecDeque<u64>,
    queue_logged: u64,
}
impl<R: Reply> Flows<R> {
    /// Create an idle owner using an already decoded time zone.
    pub fn new(clock: crate::timezone::Timezone) -> Self {
        Self {
            jobs: Vec::new(),
            trigger: None,
            stability: Default::default(),
            schedule: Default::default(),
            clock,
            observed: None,
            checked: None,
            seen: VecDeque::new(),
            queue_logged: 0,
        }
    }

    /// Start a persisted Flow and route its eventual result to the request.
    pub fn start<S: Storage>(
        &mut self,
        store: &Store<S>,
        id: &str,
        owner: u64,
        now: u64,
        reply: R,
        env: &impl Environment,
    ) -> Result {
        if self.jobs.len() >= MAX_RUNS {
            return Err(Error::Full);
        }
        let run = Run::manual(store.document().record("flows", id)?, now, &env.now()?)?;
        json::push(
            &mut self.jobs,
            Job {
                owner,
                run,
                reply: Some(reply),
                capability: false,
                timing: None,
            },
            MAX_RUNS,
        )
    }

    /// Run a supplied Flow definition, including a one-off MCP action.
    pub fn definition(
        &mut self,
        definition: &Value,
        owner: u64,
        now: u64,
        reply: R,
        env: &impl Environment,
    ) -> Result {
        let run = Run::manual(definition, now, &env.now()?)?;
        json::push(
            &mut self.jobs,
            Job {
                owner,
                run,
                reply: Some(reply),
                capability: false,
                timing: None,
            },
            MAX_RUNS,
        )
    }

    fn triggers<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        env: &impl Environment,
        now: u64,
        owner: &mut u64,
    ) -> Result {
        if self.observed != Some(store.sequence()) {
            self.stability.reconcile(store, now)?;
            self.observed = Some(store.sequence());
        }
        while self.jobs.len() < MAX_RUNS {
            self.jobs.try_reserve(1).map_err(|_| Error::Memory)?;
            let next = owner.checked_add(1).ok_or(Error::Full)?;
            let Some(run) = self.stability.due(store, now, &env.now()?)? else {
                break;
            };
            *owner = next;
            self.jobs.push(Job {
                owner: next,
                run,
                reply: None,
                capability: false,
                timing: None,
            });
        }
        let unix = json::unix_seconds(&env.now()?).ok_or(Error::Invalid("invalid wall clock"))?;
        let unix = i64::try_from(unix).map_err(|_| Error::Full)?;
        let check = (unix.div_euclid(60), store.sequence());
        while self.jobs.len() < MAX_RUNS && self.checked != Some(check) {
            self.jobs.try_reserve(1).map_err(|_| Error::Memory)?;
            let next = owner.checked_add(1).ok_or(Error::Full)?;
            let Some(run) = self
                .schedule
                .due(store, &self.clock, unix, now, &env.now()?)?
            else {
                self.checked = Some(check);
                break;
            };
            *owner = next;
            self.jobs.push(Job {
                owner: next,
                run,
                reply: None,
                capability: false,
                timing: None,
            });
        }
        use json::TryClone;
        // Elk nieuw event in de rij krijgt nu zijn tijdstempel; de eigenaar
        // kijkt elke beurt (hoogstens 50 ms), dat is de fout van de meting.
        let pending = store.pending_triggers();
        while self.seen.len() > pending {
            self.seen.pop_front();
        }
        while self.seen.len() < pending {
            self.seen.try_reserve(1).map_err(|_| Error::Memory)?;
            self.seen.push_back(now);
        }
        for _ in 0..128 {
            if self.jobs.len() >= MAX_RUNS {
                break;
            }
            if self.trigger.is_none() {
                let Some(event) = store.take_trigger() else {
                    break;
                };
                let lag = now.saturating_sub(self.seen.pop_front().unwrap_or(now));
                let mut definitions = Vec::new();
                for d in store.document().records("flows").iter().rev() {
                    json::push(&mut definitions, d.try_clone()?, 4096)?;
                }
                self.trigger = Some(Trigger {
                    event,
                    definitions,
                    lag,
                });
            }
            let Some(trigger) = &mut self.trigger else {
                break;
            };
            let Some(definition) = trigger.definitions.pop() else {
                self.trigger = None;
                continue;
            };
            let Some(run) = Run::triggered(&definition, &trigger.event, now, &env.now()?)? else {
                continue;
            };
            let next = owner.checked_add(1).ok_or(Error::Full)?;
            let tokens = json::get(&trigger.event, "tokens").unwrap_or(&Value::Null);
            let state = json::get(&trigger.event, "state").unwrap_or(&Value::Null);
            let device = match json::text(tokens, "device") {
                "" => json::text(state, "deviceId"),
                name => name,
            };
            let timing = Timing {
                lag: trigger.lag,
                started: now,
                trigger: json::copy(json::text(&trigger.event, "id"))?,
                device: json::copy(device)?,
                card: None,
                cards: Vec::new(),
            };
            json::push(
                &mut self.jobs,
                Job {
                    owner: next,
                    run,
                    reply: None,
                    capability: false,
                    timing: Some(timing),
                },
                MAX_RUNS,
            )?;
            *owner = next;
        }
        Ok(())
    }

    /// Consume a matching app completion; false leaves it for another owner.
    pub fn complete(&mut self, completion: &Completion, now: u64) -> Result<bool> {
        use json::TryClone;
        let Some(job) = self.jobs.iter_mut().find(|j| j.owner == completion.owner) else {
            return Ok(false);
        };
        if let Some(timing) = &mut job.timing {
            timing.finished(now);
        }
        if completion.failed {
            let message = completion
                .value
                .as_str()
                .unwrap_or_else(|| json::text(&completion.value, "message"));
            job.run.fail(if message.is_empty() {
                "app callback failed"
            } else {
                message
            })?;
        } else {
            job.run.complete(
                if job.capability {
                    Value::Bool(true)
                } else {
                    completion.value.try_clone()?
                },
                now,
            )?;
        }
        Ok(true)
    }

    /// Advance timers and runs without waiting on a device or delay.
    pub fn poll<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        apps: &mut dyn crate::Apps,
        env: &mut impl Environment,
        owner: &mut u64,
        scenes: &mut crate::scenes::Scenes<R>,
    ) {
        let now = apps.now();
        if let Err(e) = self.triggers(store, env, now, owner) {
            apps.log(format_args!("[stulp:flow-trigger] {e}"));
        }
        let depth = store.pending_triggers();
        if depth >= QUEUE_LOG_DEPTH && now.saturating_sub(self.queue_logged) >= QUEUE_LOG_MS {
            self.queue_logged = now;
            let oldest = now.saturating_sub(self.seen.front().copied().unwrap_or(now));
            apps.log(format_args!(
                "[stulp:flow-queue] depth={depth} oldest_ms={oldest} runs={}",
                self.jobs.len()
            ));
        }
        let mut index = 0;
        while index < self.jobs.len() {
            let Some(job) = self.jobs.get_mut(index) else {
                break;
            };
            let result = advance(job, store, apps, env, now, scenes);
            match result {
                Ok(false) => {
                    index += 1;
                    continue;
                }
                Ok(true) => (),
                Err(error) => {
                    let message = stulp_runtime::flows::error_text(&error);
                    if let Ok(message) = message {
                        let _ = job.run.fail(&message);
                    }
                    apps.log(format_args!("[stulp:flow] execution failed: {error}"));
                }
            }
            let mut job = self.jobs.remove(index);
            if let Some(timing) = &mut job.timing {
                timing.finished(now);
                let run_ms = now.saturating_sub(timing.started);
                if !timing.cards.is_empty() || timing.lag >= 200 {
                    let mut cards = String::new();
                    for (label, ms) in &timing.cards {
                        use core::fmt::Write;
                        if cards.try_reserve(label.len() + 12).is_ok() {
                            let _ = write!(
                                cards,
                                "{}{label}:{ms}",
                                if cards.is_empty() { "" } else { "," }
                            );
                        }
                    }
                    apps.log(format_args!(
                        "[stulp:flow-time] flow=\"{}\" trigger={} device=\"{}\" lag_ms={} run_ms={run_ms} cards={cards}{}",
                        job.run.name(),
                        timing.trigger,
                        timing.device,
                        timing.lag,
                        if job.run.error().is_empty() { "" } else { " failed" }
                    ));
                }
            }
            let response = if job.run.id().is_empty() {
                job.run.result().and_then(|r| Response::json(200, &r))
            } else {
                finish(&job.run, store, env)
            };
            match response {
                Ok(response) => {
                    if let Some(reply) = job.reply {
                        let _ = reply.send(response);
                    }
                }
                Err(error) => {
                    apps.log(format_args!(
                        "[stulp:flow] result persistence failed: {error}"
                    ));
                    if let Ok(response) = Response::error(500, &error.to_string())
                        && let Some(reply) = job.reply
                    {
                        let _ = reply.send(response);
                    }
                }
            }
        }
    }
}

fn advance<S: Storage, R: Reply>(
    job: &mut Job<R>,
    store: &mut Store<S>,
    apps: &mut dyn crate::Apps,
    env: &mut impl Environment,
    now: u64,
    scenes: &mut crate::scenes::Scenes<R>,
) -> Result<bool> {
    match job.run.advance(store, now)? {
        Effect::Finished => return Ok(true),
        Effect::Waiting => (),
        Effect::Call {
            app,
            method,
            params,
        } => {
            job.capability = method == "capability.invoke";
            if let Some(timing) = &mut job.timing {
                timing.finished(now);
                let what = match json::text(&params, "capability") {
                    "" => json::text(&params, "id"),
                    capability => capability,
                };
                let mut label = json::copy(&app)?;
                label
                    .try_reserve(what.len() + 1)
                    .map_err(|_| Error::Memory)?;
                label.push('/');
                label.push_str(what);
                timing.card = Some((label, now));
            }
            // Een actie die niet te versturen is mislukt als kaart: de andere takken lopen door.
            if let Err(error) = call(&app, method, &params, job.owner, apps, now, scenes) {
                apps.log(format_args!(
                    "[stulp:flow] card failed app={app} method={method}: {error}"
                ));
                job.run.fail(&stulp_runtime::flows::error_text(&error)?)?;
            }
        }
        Effect::Notification(excerpt) => {
            let id = env.id()?;
            let record = json::fields(&[
                ("id", json::string(&id)?),
                ("appId", json::string("stulp")?),
                ("excerpt", json::string(&excerpt)?),
            ])?;
            store.put("notifications", record, true, None, &env.now()?)?;
            use json::TryClone;
            job.run.complete(
                store.document().record("notifications", &id)?.try_clone()?,
                now,
            )?;
        }
    }
    Ok(false)
}

fn call<R: Reply>(
    app: &str,
    method: &str,
    params: &Value,
    owner: u64,
    apps: &mut dyn crate::Apps,
    now: u64,
    scenes: &mut crate::scenes::Scenes<R>,
) -> Result {
    if app == "com.stulp.scene" && method == "capability.invoke" {
        let id = json::text(params, "deviceId")
            .strip_prefix("scene:")
            .ok_or(Error::Invalid("invalid scene device"))?;
        let on = json::get(params, "value")
            .and_then(Value::as_bool)
            .ok_or(Error::Invalid("scene value must be boolean"))?;
        scenes.submit(id, on, now, None, Some(owner))
    } else {
        apps.call(app, owner, method, params)
    }
}

fn finish<S: Storage>(run: &Run, store: &mut Store<S>, env: &impl Environment) -> Result<Response> {
    // Een verwijderde Flow wordt niet heraangemaakt door zijn late resultaat.
    if store.document().record("flows", run.id()).is_ok() {
        store.flow_result(run.id(), run.ran_at(), run.error(), &env.now()?)?;
    }
    if run.error().is_empty() {
        Response::json(200, &run.result()?)
    } else {
        Response::error(502, run.error())
    }
}

impl<R: Reply> Default for Flows<R> {
    fn default() -> Self {
        Self::new(Default::default())
    }
}
