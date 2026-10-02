//! MCP-opdrachten delen de echte app-, scene-, Flow- en pairing-eigenaren.
use crate::{Apps, catalog::Catalog, flows::Flows, pairing::Pairing, scenes::Scenes};
use crate::{Inbox, Reply};
use alloc::{
    format,
    string::{String, ToString},
    vec::Vec,
};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_runtime::Completion;
use stulp_web::{
    Environment, Request, Response,
    mcp::{self, Call, Projection, Work},
};
struct Job<R: Reply> {
    call: Call,
    reply: R,
    deadline: u64,
    phase: Phase<R>,
}
enum Phase<R: Reply> {
    Catalog(R::Inbox),
    Callback(u64, Projection),
    Scene(u64, Value),
    Flow(R::Inbox, bool),
    Pair {
        receive: R::Inbox,
        step: u8,
        name: String,
        session: String,
    },
    Done(Response),
}
/// Owns bounded MCP jobs, rate limits and chained lifecycle operations.
pub struct Mcp<R: Reply> {
    jobs: Vec<Job<R>>,
    credits: u64,
    refilled: u64,
}
impl<R: Reply> Mcp<R> {
    /// Create an idle MCP owner with its initial rate-limit budget.
    pub fn new() -> Self {
        Self {
            jobs: Vec::new(),
            credits: 30_000,
            refilled: 0,
        }
    }
    /// Decode an MCP request and start its platform-independent operation.
    pub fn route<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        req: &Request,
        reply: &R,
        services: &mut Services<'_, R>,
        env: &mut impl Environment,
    ) -> Result {
        let call = match mcp::decode(req)? {
            mcp::Dispatch::Reply(r) => {
                let _ = reply.send(r);
                return Ok(());
            }
            mcp::Dispatch::Tool(c) => c,
        };
        let now = services.apps.now();
        self.credits = self
            .credits
            .saturating_add(now.saturating_sub(self.refilled).saturating_mul(2))
            .min(30_000);
        self.refilled = now;
        if self.credits < 1000 || self.jobs.len() >= 4 {
            let mut r = mcp::rpc_error(429, &call.id, -32000, "MCP tool rate limit exceeded")?;
            json::push(&mut r.headers, ("Retry-After", json::copy("1")?), 8)?;
            let _ = reply.send(r);
            return Ok(());
        }
        self.credits -= 1000;
        self.jobs.try_reserve(1).map_err(|_| Error::Memory)?;
        let phase = if matches!(
            call.name.as_str(),
            "system_context"
                | "devices_list"
                | "devices_write"
                | "devices_create"
                | "flows_run"
                | "flows_delete"
                | "flows_update"
                | "flows_remove_card"
                | "flows_disconnect_cards"
                | "flows_connect_cards"
        ) || (call.name == "flows_list"
            && json::text(&call.args, "flowId").is_empty())
        {
            prepare(store, &call, &Value::Null, services, env)
        } else {
            let (send, receive) = R::channel()?;
            let req = request("GET", "/api/stulp/flow/cards", Value::Null)?;
            services
                .catalog
                .route(store, services.apps, services.owner, &req, &send)?;
            Ok(Phase::Catalog(receive))
        };
        let phase = match phase {
            Ok(p) => p,
            Err(e) => Phase::Done(mcp::tool_error(&call.id, &e.to_string())?),
        };
        self.jobs.push(Job {
            call,
            reply: reply.clone(),
            deadline: now.saturating_add(45_000),
            phase,
        });
        Ok(())
    }
    /// Consume a matching app or scene completion.
    pub fn complete(&mut self, c: &Completion) -> Result<bool> {
        let Some(job) = self
            .jobs
            .iter_mut()
            .find(|j| matches!(&j.phase,Phase::Callback(id,_)|Phase::Scene(id,_) if *id==c.owner))
        else {
            return Ok(false);
        };
        let response = match &job.phase {
            Phase::Scene(_, result) if !json::text(&c.value, "sceneId").is_empty() => {
                mcp::tool_result(
                    &job.call.id,
                    mcp::scene(result.try_clone()?, &c.value)?,
                    "Scene execution finished.",
                    c.failed,
                )?
            }
            _ if c.failed => mcp::tool_error(
                &job.call.id,
                c.value
                    .as_str()
                    .unwrap_or_else(|| json::text(&c.value, "message")),
            )?,
            Phase::Callback(_, projection) => {
                let projection = match projection {
                    Projection::Device(v) => Projection::Device(v.try_clone()?),
                    Projection::Autocomplete => Projection::Autocomplete,
                };
                mcp::tool_result(
                    &job.call.id,
                    mcp::projected(projection, &c.value)?,
                    "Request accepted; the last reported state may precede the change.",
                    false,
                )?
            }
            _ => mcp::tool_error(&job.call.id, "invalid callback result")?,
        };
        job.phase = Phase::Done(response);
        Ok(true)
    }
    /// Advance pending MCP phases and expire requests at their deadline.
    pub fn poll<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        services: &mut Services<'_, R>,
        env: &mut impl Environment,
    ) {
        let mut i = 0;
        while i < self.jobs.len() {
            let job = &mut self.jobs[i];
            let result = if services.apps.now() >= job.deadline {
                Err(Error::Invalid("MCP tool timed out"))
            } else {
                advance(store, job, services, env)
            };
            if let Err(e) = result {
                cleanup(&job.phase, store, services, env);
                match mcp::tool_error(&job.call.id, &e.to_string()) {
                    Ok(r) => job.phase = Phase::Done(r),
                    Err(e) => {
                        services.apps.log(format_args!("[stulp:mcp-result] {e}"));
                        self.jobs.remove(i);
                        continue;
                    }
                }
            }
            if matches!(job.phase, Phase::Done(_)) {
                let job = self.jobs.remove(i);
                if let Phase::Done(r) = job.phase {
                    let _ = job.reply.send(r);
                }
            } else {
                i += 1;
            }
        }
    }
}
/// Borrow the controller owners used by one MCP dispatch round.
pub struct Services<'a, R: Reply> {
    /// Authenticated callback transport.
    pub apps: &'a mut dyn Apps,
    /// Flow-card registration owner.
    pub catalog: &'a mut Catalog<R>,
    /// Flow execution owner.
    pub flows: &'a mut Flows<R>,
    /// Scene execution and restore owner.
    pub scenes: &'a mut Scenes<R>,
    /// Pairing and device lifecycle owner.
    pub pairing: &'a mut Pairing<R>,
    /// Monotonic callback identity allocator.
    pub owner: &'a mut u64,
}
fn next<R: Reply>(services: &mut Services<'_, R>) -> Result<u64> {
    *services.owner = services.owner.checked_add(1).ok_or(Error::Full)?;
    Ok(*services.owner)
}
fn prepare<S: Storage, R: Reply>(
    store: &mut Store<S>,
    call: &Call,
    cards: &Value,
    s: &mut Services<'_, R>,
    env: &mut impl Environment,
) -> Result<Phase<R>> {
    match mcp::prepare(store, call, cards, env)? {
        Work::Done(v) => Ok(Phase::Done(mcp::tool_result(&call.id, v, "", false)?)),
        Work::Callback {
            app,
            method,
            params,
            projection,
        } => {
            let owner = next(s)?;
            s.apps.call(&app, owner, method, &params)?;
            Ok(Phase::Callback(owner, projection))
        }
        Work::Scene { id, on, result } => {
            let owner = next(s)?;
            s.scenes.submit(&id, on, s.apps.now(), None, Some(owner))?;
            Ok(Phase::Scene(owner, result))
        }
        Work::Flow {
            definition,
            persist,
        } => {
            let owner = next(s)?;
            let (reply, receive) = R::channel()?;
            s.flows
                .definition(&definition, owner, s.apps.now(), reply, env)?;
            Ok(Phase::Flow(receive, !persist))
        }
        Work::CreateVirtual(name) => {
            let receive = pair(
                store,
                s,
                env,
                "POST",
                "/api/stulp/pair",
                json::parse(br#"{"appId":"com.stulp.virtualdevices","driverId":"switch"}"#)?,
            )?;
            Ok(Phase::Pair {
                receive,
                step: 0,
                name,
                session: String::new(),
            })
        }
    }
}
fn request(method: &str, path: &str, body: Value) -> Result<Request> {
    Ok(Request {
        method: json::copy(method)?,
        path: json::copy(path)?,
        query: String::new(),
        cookie: String::new(),
        host: String::new(),
        origin: String::new(),
        headers: json::object(),
        body: json::to_string(&body)?.into_bytes(),
    })
}
fn pair<S: Storage, R: Reply>(
    store: &mut Store<S>,
    s: &mut Services<'_, R>,
    env: &mut impl Environment,
    method: &str,
    path: &str,
    body: Value,
) -> Result<R::Inbox> {
    let (reply, receive) = R::channel()?;
    if !s.pairing.route(
        store,
        s.apps,
        env,
        s.owner,
        &request(method, path, body)?,
        &reply,
    )? {
        return Err(Error::Missing("pairing route unavailable"));
    }
    Ok(receive)
}
fn received<I: Inbox>(receive: &I) -> Result<Option<Value>> {
    let Some(r) = receive.receive()? else {
        return Ok(None);
    };
    if r.status >= 400 {
        return Err(Error::Invalid("controller callback failed"));
    }
    Ok(Some(json::parse(r.body.bytes())?))
}
fn advance<S: Storage, R: Reply>(
    store: &mut Store<S>,
    job: &mut Job<R>,
    s: &mut Services<'_, R>,
    env: &mut impl Environment,
) -> Result {
    let phase = match &mut job.phase {
        Phase::Catalog(receive) => {
            let Some(cards) = received(receive)? else {
                return Ok(());
            };
            prepare(store, &job.call, &cards, s, env)?
        }
        Phase::Flow(receive, action) => {
            let Some(raw) = received(receive)? else {
                return Ok(());
            };
            Phase::Done(mcp::tool_result(
                &job.call.id,
                mcp::execution(&raw, *action)?,
                json::text(&raw, "error"),
                *action && !json::text(&raw, "error").is_empty(),
            )?)
        }
        Phase::Pair {
            receive,
            step,
            name,
            session,
        } => {
            let Some(v) = received(receive)? else {
                return Ok(());
            };
            match *step {
                0 => {
                    *session = json::copy(json::text(&v, "id"))?;
                    if session.is_empty()
                        || !["create", "list_devices"].iter().all(|h| {
                            json::array(&v, "handlers")
                                .iter()
                                .any(|v| v.as_str() == Some(h))
                        })
                    {
                        return Err(Error::Invalid(
                            "Virtual devices app does not support switch creation",
                        ));
                    }
                    let receive = pair(
                        store,
                        s,
                        env,
                        "POST",
                        &format!("/api/stulp/pair/{session}/emit/create"),
                        json::fields(&[("name", json::string(name)?)])?,
                    )?;
                    Phase::Pair {
                        receive,
                        step: 1,
                        name: json::copy(name)?,
                        session: json::copy(session)?,
                    }
                }
                1 => {
                    let receive = pair(
                        store,
                        s,
                        env,
                        "POST",
                        &format!("/api/stulp/pair/{session}/emit/list_devices"),
                        Value::Null,
                    )?;
                    Phase::Pair {
                        receive,
                        step: 2,
                        name: json::copy(name)?,
                        session: json::copy(session)?,
                    }
                }
                2 => {
                    let list = v
                        .as_array()
                        .ok_or(Error::Invalid("pairing candidates must be an array"))?;
                    if list.len() != 1 {
                        return Err(Error::Invalid("expected one virtual switch candidate"));
                    }
                    let receive = pair(
                        store,
                        s,
                        env,
                        "POST",
                        "/api/stulp/apps/com.stulp.virtualdevices/drivers/switch/pair/devices",
                        list.first().ok_or(Error::Full)?.try_clone()?,
                    )?;
                    Phase::Pair {
                        receive,
                        step: 3,
                        name: json::copy(name)?,
                        session: json::copy(session)?,
                    }
                }
                3 => {
                    let id = json::text(&v, "id");
                    let result = mcp::created(store, id)?;
                    // Na adoptie is sluiten best-effort; geen koppelresultaat verdwijnt door een late close-fout.
                    let _ = pair(
                        store,
                        s,
                        env,
                        "DELETE",
                        &format!("/api/stulp/pair/{session}"),
                        Value::Null,
                    );
                    Phase::Done(mcp::tool_result(
                        &job.call.id,
                        result,
                        "Created a persistent virtual switch, initially off.",
                        false,
                    )?)
                }
                _ => return Err(Error::Invalid("unknown pairing stage")),
            }
        }
        _ => return Ok(()),
    };
    job.phase = phase;
    Ok(())
}
fn cleanup<S: Storage, R: Reply>(
    phase: &Phase<R>,
    store: &mut Store<S>,
    s: &mut Services<'_, R>,
    env: &mut impl Environment,
) {
    if let Phase::Pair { session, .. } = phase
        && !session.is_empty()
    {
        let _ = pair(
            store,
            s,
            env,
            "DELETE",
            &format!("/api/stulp/pair/{session}"),
            Value::Null,
        );
    }
}

impl<R: Reply> Default for Mcp<R> {
    fn default() -> Self {
        Self::new()
    }
}
