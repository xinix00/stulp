//! Fixed HTTP workers communicate with the single parked controller owner.
use crate::replies::Sender;
use alloc::{
    string::{String, ToString},
    vec::Vec,
};
use applib::{
    EXEC,
    appnet::{TcpListener, TcpStream},
    tcp::TcpConn,
};
use core::future::Future;
use core::time::Duration;
use hop_sync::{Either, Local, mpsc::Mailbox, select};
use stulp_controller::{
    Reply,
    callbacks::callback,
    catalog::Catalog,
    flows::Flows,
    mcp::{Mcp, Services},
    pairing::Pairing,
    scenes::Scenes,
    timezone::Timezone,
};
use stulp_core::{
    json,
    store::{Storage, Store},
};
use stulp_web::{Request, Response, Web};
/// At most six long streams leave two workers for interactive requests.
pub const WORKERS: usize = 8;
#[allow(clippy::large_enum_variant)]
pub(crate) enum Work {
    AuthorizeRestore(Call),
    Request(Call),
    MediaDone(u64),
}
pub(crate) struct Call {
    pub(crate) request: Request,
    pub(crate) reply: Sender,
}
// Only this executor core accesses these queues. Local is required because reply handles are !Send.
pub(crate) static WORK: Local<Mailbox<Work, 8>> = Local::new(Mailbox::new());
static SOCKETS: [Local<Mailbox<TcpStream, 1>>; WORKERS] =
    [const { Local::new(Mailbox::new()) }; WORKERS];
macro_rules! log {($($arg:tt)*)=>{applib::log!($($arg)*)}}
/// Start a fixed number of workers and a bounded acceptor, once per slot.
pub fn start_http(
    app: &'static applib::App,
    port: u16,
    env: &mut crate::environment::Environment,
) -> stulp_core::Result {
    let listener =
        TcpListener::bind(port).map_err(|_| stulp_core::Error::Invalid("HTTP bind failed"))?;
    for queue in &SOCKETS {
        let dial = crate::network::Dial::new(app, &env.random());
        EXEC.get()
            .spawn(worker(queue, dial))
            .map_err(|_| stulp_core::Error::Memory)?;
    }
    EXEC.get()
        .spawn(async move {
            let mut next = 0;
            loop {
                let stream = match listener.accept().await {
                    Ok(s) => s,
                    Err(e) => {
                        log!("[stulp:http-accept] {e:?}");
                        EXEC.get().after(Duration::from_millis(10)).await;
                        continue;
                    }
                };
                let mut available = Some(stream);
                for _ in 0..WORKERS {
                    let Some(stream) = available.take() else {
                        break;
                    };
                    available = SOCKETS[next].try_send(stream).err().map(|e| e.0);
                    next = (next + 1) % WORKERS;
                }
                // A saturated fixed pool closes excess connections without allocating a task.
                drop(available);
            }
        })
        .map_err(|_| stulp_core::Error::Memory)
}
struct Pending {
    owner: u64,
    reply: Sender,
    deadline: u64,
    asset: Option<stulp_controller::app_ui::Asset>,
    media: bool,
    image: bool,
    measures: bool,
}
struct Subscriber {
    reply: Sender,
    cursor: u64,
    last: u64,
    manager: String,
    overview: bool,
}

/// De tik van de eigenaar: de korrel van alle termijnen die hij zelf bewaakt
/// (hartslag 15 s, callbacks 31 s, SSE-keepalive 15 s, Flow- en scènetimers).
/// Werk komt eerder, via de wek van een app-verbinding of de werkmailbox; dit
/// is alleen de vloer, dezelfde maat als de hartslag van applib.
const OWNER_TICK: Duration = Duration::from_millis(50);
/// Runs the sole controller owner on its parked stack. HTTP workers are fixed executor tasks.
pub fn run<S: Storage>(
    mut store: Store<S>,
    web: Web,
    mut apps: crate::apps::Apps,
    mut env: crate::environment::Environment,
    wait: &impl crate::storage::Wait,
    timezone: Timezone,
) -> stulp_core::Result {
    let mut pending: Vec<Pending> = Vec::new();
    let mut owner = 0_u64;
    let mut flows = Flows::<Sender>::new(timezone);
    let mut catalog = Catalog::<Sender>::new();
    let mut mcp = Mcp::<Sender>::new();
    let mut scenes = Scenes::<Sender>::new();
    let mut pairing = Pairing::<Sender>::new();
    let mut subscribers: Vec<Subscriber> = Vec::new();
    let mut media: Vec<(u64, Sender)> = Vec::new();
    loop {
        // Slapen op gebeurtenissen (HopOS docs/apps.md): bytes van een app,
        // een nieuwe attach, werk van een HTTP-werker, of de tik waarop de
        // termijnen van hartslag, Flows, scènes en SSE-keepalive lopen.
        let (first, mut incoming) = match wait.wait(select(apps.wait(OWNER_TICK), WORK.recv()))? {
            Either::Left(first) => (first, None),
            Either::Right(work) => (None, Some(work)),
        };
        media.retain(|(_, reply)| !reply.closed());
        if let Ok(now) = env.unix_ms() {
            store.tick(now);
        }
        if let Some(e) = store.statistics_fault() {
            log!("[stulp:statistics] {e}");
        }
        match apps.poll(&mut store, &mut env, first) {
            Ok(completed) => {
                for completion in completed {
                    match mcp.complete(&completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            log!("[stulp:mcp-callback] {e}");
                            continue;
                        }
                    }
                    match catalog.complete(&store, &completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            log!("[stulp:catalog] {e}");
                            continue;
                        }
                    }
                    match scenes.complete(&completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            log!("[stulp:scene-callback] {e}");
                            continue;
                        }
                    }
                    if pairing.complete(&mut store, &mut apps, &completion) {
                        continue;
                    }
                    match flows.complete(&completion, apps.now()) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            log!("[stulp:flow] callback failed: {e}");
                            continue;
                        }
                    }
                    if let Some(index) = pending.iter().position(|p| p.owner == completion.owner) {
                        let mut p = pending.remove(index);
                        if let Some(asset) = &mut p.asset {
                            match asset.advance(&completion.value, completion.failed) {
                                Ok(Some(params)) => {
                                    match apps.call(&asset.app, p.owner, "ui.asset", &params) {
                                        Ok(()) => {
                                            p.deadline = apps.now().saturating_add(31_000);
                                            pending.push(p);
                                        }
                                        Err(e) => {
                                            if let Ok(response) =
                                                Response::error(502, &e.to_string())
                                            {
                                                let _ = p.reply.send(response);
                                            }
                                        }
                                    }
                                    continue;
                                }
                                Ok(None) => (),
                                Err(e) => {
                                    if let Ok(response) = Response::error(502, &e.to_string()) {
                                        let _ = p.reply.send(response);
                                    }
                                    continue;
                                }
                            }
                        }
                        let response = if let Some(asset) = p.asset {
                            asset.response(&completion.value, completion.failed)
                        } else if completion.failed {
                            Response::error(502, json::text(&completion.value, "message"))
                        } else if p.media {
                            crate::media::response(&completion.value, p.owner, p.image)
                        } else if p.measures {
                            stulp_web::show_measures(&store, &completion.value)
                                .and_then(|v| Response::json(200, &v))
                        } else {
                            Response::json(200, &completion.value)
                        };
                        match response {
                            Ok(response) => {
                                let is_stream =
                                    matches!(response.body, stulp_web::Body::Proxy { .. });
                                let sent = p.reply.send(response).is_ok();
                                if !is_stream || !sent {
                                    media.retain(|(id, _)| *id != p.owner);
                                }
                            }
                            Err(_) => {
                                media.retain(|(id, _)| *id != p.owner);
                                if let Ok(response) =
                                    Response::error(502, "app returned invalid media")
                                {
                                    let _ = p.reply.send(response);
                                }
                            }
                        }
                    }
                }
            }
            Err(e) => log!("[stulp:app-poll] {e}"),
        }
        let now = apps.now();
        pairing.poll(&mut store, &mut apps);
        flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
        match scenes.poll(&mut store, &mut apps, &mut owner) {
            Ok(done) => {
                for completion in done {
                    match mcp.complete(&completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            log!("[stulp:mcp-scene] {e}");
                            continue;
                        }
                    }
                    if let Err(e) = flows.complete(&completion, apps.now()) {
                        log!("[stulp:scene-flow] {e}");
                    }
                }
            }
            Err(e) => log!("[stulp:scene] {e}"),
        }
        mcp.poll(
            &mut store,
            &mut Services {
                apps: &mut apps,
                catalog: &mut catalog,
                flows: &mut flows,
                scenes: &mut scenes,
                pairing: &mut pairing,
                owner: &mut owner,
            },
            &mut env,
        );
        subscribers.retain_mut(|s| {
            if store.sequence() == s.cursor && now.saturating_sub(s.last) < 15_000 {
                return true;
            }
            let body = if store.sequence() == s.cursor {
                Ok(String::from(": keepalive\n\n"))
            } else {
                stulp_web::events_view(&store, s.cursor, &s.manager, s.overview)
            };
            let Ok(body) = body else {
                return false;
            };
            let response = Response {
                status: 200,
                content_type: "text/event-stream",
                body: stulp_web::Body::Owned(body),
                cookie: None,
                headers: Vec::new(),
            };
            match s.reply.send(response) {
                Ok(()) => {
                    s.cursor = store.sequence();
                    s.last = now;
                    true
                }
                Err(stulp_core::Error::Full) => true,
                Err(_) => false,
            }
        });
        let mut index = 0;
        while index < pending.len() {
            if pending.get(index).is_some_and(|p| p.deadline <= now) {
                let p = pending.remove(index);
                media.retain(|(id, _)| *id != p.owner);
                if let Ok(response) = Response::error(504, "app callback timed out") {
                    let _ = p.reply.send(response);
                }
            } else {
                index += 1;
            }
        }
        let call = match incoming.take().or_else(|| WORK.try_recv()) {
            Some(Work::AuthorizeRestore(call)) => {
                let response = web
                    .handle(&mut store, &call.request, &mut env)
                    .and_then(|r| {
                        if r.status == 404 && web.authenticated(&call.request) {
                            Response::json(200, &json::Value::Bool(true))
                        } else {
                            Ok(r)
                        }
                    })
                    .or_else(|e| Response::error(400, &e.to_string()));
                if let Ok(r) = response {
                    let _ = call.reply.send(r);
                }
                continue;
            }
            Some(Work::Request(call)) => call,
            Some(Work::MediaDone(owner)) => {
                media.retain(|(id, _)| *id != owner);
                continue;
            }
            None => continue,
        };
        let handle0 = applib::clock::now_ns();
        let handled = web.handle(&mut store, &call.request, &mut env);
        let handle_ms = applib::clock::now_ns().saturating_sub(handle0) / 1_000_000;
        if handle_ms >= SLOW_MS {
            log!(
                "[stulp:slow-handle] {} {} ms={handle_ms}",
                call.request.method,
                call.request.path
            );
        }
        match handled {
            Ok(response) => {
                if response.status == 200 && response.content_type == "text/event-stream" {
                    if subscribers.len() + media.len() >= WORKERS - 2 {
                        if let Ok(response) = Response::error(503, "event stream capacity reached")
                        {
                            let _ = call.reply.send(response);
                        }
                        continue;
                    }
                    if json::push(
                        &mut subscribers,
                        Subscriber {
                            reply: call.reply.clone(),
                            cursor: store.sequence(),
                            last: now,
                            manager: stulp_web::query_value(&call.request.query, "manager")
                                .unwrap_or_default(),
                            overview: stulp_web::query_value(&call.request.query, "view")
                                .is_ok_and(|s| s == "overview"),
                        },
                        WORKERS,
                    )
                    .is_err()
                    {
                        continue;
                    }
                }
                if response.status == 404 && web.mcp_authenticated(&call.request) {
                    if let Err(e) = mcp.route(
                        &mut store,
                        &call.request,
                        &call.reply,
                        &mut Services {
                            apps: &mut apps,
                            catalog: &mut catalog,
                            flows: &mut flows,
                            scenes: &mut scenes,
                            pairing: &mut pairing,
                            owner: &mut owner,
                        },
                        &mut env,
                    ) && let Ok(r) = Response::error(500, &e.to_string())
                    {
                        let _ = call.reply.send(r);
                    }
                    continue;
                }
                if response.status == 404
                    && (web.authenticated(&call.request)
                        || (call.request.method == "GET"
                            && call.request.path.starts_with("/image/")))
                    && (call.request.path.starts_with("/api/")
                        || call.request.path.starts_with("/app-ui/")
                        || call.request.path.starts_with("/image/"))
                {
                    if matches!(
                        (call.request.method.as_str(), call.request.path.as_str()),
                        ("GET", "/api/stulp/backup") | ("POST", "/api/stulp/restore")
                    ) {
                        let restoring = call.request.method == "POST";
                        let archive0 = applib::clock::now_ns();
                        let response = crate::archive::route(&mut store, call.request, &env);
                        log!(
                            "[stulp:archive] restore={restoring} ms={}",
                            applib::clock::now_ns().saturating_sub(archive0) / 1_000_000
                        );
                        if restoring && response.as_ref().is_ok_and(|r| r.status == 200) {
                            apps.reset();
                            pending.clear();
                            subscribers.clear();
                            media.clear();
                            flows = Flows::new(crate::files::timezone(store.timezone(), wait)?);
                            catalog = Catalog::new();
                            mcp = Mcp::new();
                            scenes = Scenes::new();
                            pairing = Pairing::new();
                        }
                        let response = response.or_else(|e| Response::error(422, &e.to_string()));
                        if let Ok(response) = response {
                            let _ = call.reply.send(response);
                        }
                        continue;
                    }
                    match catalog.route(&store, &mut apps, &mut owner, &call.request, &call.reply) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            if let Ok(r) = Response::error(503, &e.to_string()) {
                                let _ = call.reply.send(r);
                            }
                            continue;
                        }
                    }
                    match apps.route(&mut store, &call.request) {
                        Ok(Some(response)) => {
                            let _ = call.reply.send(response);
                            continue;
                        }
                        Ok(None) => (),
                        Err(e) => {
                            if let Ok(response) = Response::error(400, &e.to_string()) {
                                let _ = call.reply.send(response);
                            }
                            continue;
                        }
                    }
                    match scenes.route(&store, &call.request, now, &call.reply) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            if let Ok(response) = Response::error(400, &e.to_string()) {
                                let _ = call.reply.send(response);
                            }
                            continue;
                        }
                    }
                    match pairing.route(
                        &mut store,
                        &mut apps,
                        &mut env,
                        &mut owner,
                        &call.request,
                        &call.reply,
                    ) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(error) => {
                            let status = match error {
                                stulp_core::Error::Missing(_) => 404,
                                stulp_core::Error::Conflict(_) => 409,
                                stulp_core::Error::Storage | stulp_core::Error::Full => 503,
                                _ => 400,
                            };
                            if let Ok(response) = Response::error(status, &error.to_string()) {
                                let _ = call.reply.send(response);
                            }
                            continue;
                        }
                    }
                    if call.request.method == "POST"
                        && let Some(id) = call
                            .request
                            .path
                            .strip_prefix("/api/stulp/flows/")
                            .and_then(|p| p.strip_suffix("/run"))
                    {
                        let result = owner
                            .checked_add(1)
                            .ok_or(stulp_core::Error::Full)
                            .and_then(|next| {
                                flows.start(&store, id, next, now, call.reply.clone(), &env)?;
                                owner = next;
                                Ok(())
                            });
                        if let Err(error) = result {
                            let status = match error {
                                stulp_core::Error::Full => 503,
                                stulp_core::Error::Missing(_) => 404,
                                _ => 400,
                            };
                            if let Ok(response) = Response::error(status, &error.to_string()) {
                                let _ = call.reply.send(response);
                            }
                        }
                        continue;
                    }
                    let prepared = stulp_controller::app_ui::prepare(&store, &call.request);
                    let routed = match prepared {
                        Ok(Some((_, _, asset))) if asset.is_local() => {
                            let response = asset
                                .local_response(|root, name| crate::files::asset(root, name, wait))
                                .or_else(|e| Response::error(400, &e.to_string()));
                            if let Ok(response) = response {
                                let _ = call.reply.send(response);
                            }
                            continue;
                        }
                        Ok(Some((app, params, asset))) => {
                            Ok(Some((app, "ui.asset", params, Some(asset))))
                        }
                        Ok(None) => callback(&store, &call.request, now)
                            .map(|v| v.map(|(app, method, params)| (app, method, params, None))),
                        Err(error) => Err(error),
                    };
                    match routed {
                        Ok(Some((app, method, params, asset))) => {
                            let result = owner
                                .checked_add(1)
                                .ok_or(stulp_core::Error::Full)
                                .and_then(|next| {
                                    if pending.len() >= 64 {
                                        return Err(stulp_core::Error::Full);
                                    }
                                    pending
                                        .try_reserve(1)
                                        .map_err(|_| stulp_core::Error::Memory)?;
                                    if method == "video.resolve" {
                                        if media.len() >= 4
                                            || media.len() + subscribers.len() >= WORKERS - 2
                                        {
                                            return Err(stulp_core::Error::Full);
                                        }
                                        media
                                            .try_reserve(1)
                                            .map_err(|_| stulp_core::Error::Memory)?;
                                    }
                                    apps.call(&app, next, method, &params)?;
                                    if method == "video.resolve" {
                                        media.push((next, call.reply.clone()));
                                    }
                                    owner = next;
                                    Ok(())
                                });
                            match result {
                                Ok(()) => {
                                    pending.push(Pending {
                                        owner,
                                        reply: call.reply,
                                        deadline: now.saturating_add(
                                            stulp_runtime::callback_timeout_for(
                                                &app, method, &params,
                                            ) + 1000,
                                        ),
                                        asset,
                                        measures: method == "api.invoke",
                                        media: method == "video.resolve",
                                        image: method == "video.resolve"
                                            && json::text(&params, "kind") == "image",
                                    });
                                    continue;
                                }
                                Err(e) => {
                                    if let Ok(response) = Response::error(502, &e.to_string()) {
                                        let _ = call.reply.send(response);
                                    }
                                    continue;
                                }
                            }
                        }
                        Err(e) => {
                            let status = if matches!(e, stulp_core::Error::Missing(_)) {
                                404
                            } else {
                                400
                            };
                            if let Ok(response) = Response::error(status, &e.to_string()) {
                                let _ = call.reply.send(response);
                            }
                            continue;
                        }
                        Ok(None) => (),
                    }
                }
                let _ = call.reply.send(response);
            }
            Err(error) => {
                log!("[stulp:http] response construction failed: {error}");
                if let Ok(response) = Response::error(500, "response construction failed") {
                    let _ = call.reply.send(response);
                }
            }
        }
    }
}

/// De bron van een SSE-stroom voor `leanhttp::Exchange::stream`: het eerste
/// antwoord van de controller, dan elk volgend antwoord zodra het er is;
/// tussendoor een kort dutje, zodat de stroom zijn lezer blijft peilen.
struct Events<'a> {
    answers: &'a crate::replies::Receiver,
    first: Option<Vec<u8>>,
    /// De eigenaar sloot het kanaal: de stroom eindigt na wat nog klaarstaat.
    ended: bool,
}

/// Het plafond op het wachten van de stroom: zonder antwoord in deze tijd
/// kijkt leanhttp één keer of de lezer er nog is, en wacht hij weer. Het
/// echte wachten is op het antwoordkanaal zelf (docs/apps.md van HopOS).
const EVENTS_NAP: Duration = Duration::from_secs(1);

impl leanhttp::Source for Events<'_> {
    async fn next(&mut self) -> leanhttp::Next {
        if let Some(first) = self.first.take() {
            return leanhttp::Next::Data(first);
        }
        if self.ended {
            return leanhttp::Next::End;
        }
        // Eén poll: ligt er nu een antwoord? Zo niet, dan "niets"; de
        // stroom kijkt naar de lezer en wacht `EVENTS_NAP`.
        let mut wait = core::pin::pin!(self.answers.wait());
        match core::future::poll_fn(|cx| core::task::Poll::Ready(wait.as_mut().poll(cx))).await {
            core::task::Poll::Ready(Ok(response)) => {
                leanhttp::Next::Data(response.body.bytes().to_vec())
            }
            core::task::Poll::Ready(Err(_)) => leanhttp::Next::End,
            core::task::Poll::Pending => leanhttp::Next::Nothing,
        }
    }

    async fn nap(&mut self) {
        match select(self.answers.wait(), EXEC.get().after(EVENTS_NAP)).await {
            Either::Left(Ok(response)) => self.first = Some(response.body.bytes().to_vec()),
            Either::Left(Err(_)) => self.ended = true,
            Either::Right(()) => (),
        }
    }
}

struct MediaGuard {
    owner: u64,
}
impl Drop for MediaGuard {
    fn drop(&mut self) {
        let _ = WORK.try_send(Work::MediaDone(self.owner));
    }
}
async fn worker(queue: &'static Local<Mailbox<TcpStream, 1>>, mut dial: crate::network::Dial) {
    loop {
        let stream = queue.recv().await;
        let mut stream = match crate::upload::Prefix::open(stream).await {
            Ok(s) => s,
            Err(e) => {
                log!("[stulp:request-prefix] {e}");
                continue;
            }
        };
        match stream.restore().await {
            Ok(true) => {
                if let Err(e) = stream.serve().await {
                    log!("[stulp:restore-upload] {e}");
                }
                continue;
            }
            Ok(false) => (),
            Err(e) => {
                log!("[stulp:request-prefix] {e}");
                continue;
            }
        }
        let stream = stream.normal();
        // Elke verbinding krijgt eigen antwoordhandvatten. Een oude SSE-zender
        // mag nooit in de inbox van de volgende browserverbinding belanden.
        let (reply, answers) = match Sender::channel() {
            Ok(pair) => pair,
            Err(e) => {
                log!("[stulp:reply] {e}");
                continue;
            }
        };
        let conn = TcpConn::new(stream, EXEC.get());
        let result = leanhttp::serve(
            conn,
            async |exchange: &mut leanhttp::Exchange<'_, TcpConn<crate::upload::Prefix>>| {
                // Eén verzoek per werkerbeurt houdt de pool vrij en laat grote uploads
                // iedere keer door dezelfde aparte, begrensde transportgrens lopen.
                exchange.header_mut().set("Connection", "close")?;
                let body = exchange.read_body_to_end().await?;
                let copy =
                    |s: &str| json::copy(s).map_err(|_| leanhttp::Error::Alloc { bytes: s.len() });
                let mut protocol_headers = json::object();
                for (name, key) in [
                    ("Content-Type", "content-type"),
                    ("Accept", "accept"),
                    ("MCP-Protocol-Version", "mcp-protocol-version"),
                ] {
                    json::set(
                        &mut protocol_headers,
                        key,
                        json::string(exchange.req.header.get(name).unwrap_or(""))
                            .map_err(|_| leanhttp::Error::Alloc { bytes: 256 })?,
                    )
                    .map_err(|_| leanhttp::Error::Alloc { bytes: 256 })?;
                }
                let request = Request {
                    headers: protocol_headers,
                    method: copy(&exchange.req.method)?,
                    path: copy(&exchange.req.path)?,
                    query: copy(&exchange.req.raw_query)?,
                    cookie: copy(exchange.req.header.get("Cookie").unwrap_or(""))?,
                    host: copy(exchange.req.header.get("Host").unwrap_or(""))?,
                    origin: copy(exchange.req.header.get("Origin").unwrap_or(""))?,
                    body,
                };
                // De meetlat van de aanvraag: hoe lang de eigenaar erover deed en hoe
                // lang het hele antwoord duurde. Alles boven SLOW_MS komt op de console,
                // zodat "traag" op de node een pad en een getal heeft.
                let t0 = applib::clock::now_ns();
                if WORK
                    .try_send(Work::Request(Call {
                        request,
                        reply: reply.clone(),
                    }))
                    .is_err()
                {
                    return exchange.error(503, "controller stopped").await;
                }
                let response = match answers.wait().await {
                    Ok(response) => response,
                    Err(_) => return exchange.error(503, "controller stopped").await,
                };
                let owner_ms = applib::clock::now_ns().saturating_sub(t0) / 1_000_000;
                if let stulp_web::Body::Proxy { url, mime, owner } = &response.body {
                    let _guard = MediaGuard { owner: *owner };
                    return crate::media::pipe(exchange, &mut dial, url, mime).await;
                }
                let header = exchange.header_mut();
                header.set("Content-Type", response.content_type)?;
                header.set("Cache-Control", "no-store")?;
                header.set("Referrer-Policy", "no-referrer")?;
                header.set("X-Content-Type-Options", "nosniff")?;
                header.set("X-Stulp-Version", env!("CARGO_PKG_VERSION"))?;
                if let Some(cookie) = response.cookie {
                    header.set("Set-Cookie", &cookie)?;
                }
                for (name, value) in &response.headers {
                    header.set(name, value)?;
                }
                exchange.write_header(response.status)?;
                if response.content_type == "text/event-stream" && response.status == 200 {
                    // De SSE-stroom via leanhttp's `stream`: die claimt de
                    // leeskant vóór de kop en kijkt elke beurt of de lezer er
                    // nog is. Op de netstack van HopOS faalt een schrijf naar
                    // een weggevallen lezer lang niet, dus zonder dit hield
                    // een gesloten tabblad zijn werker en zijn abonnement vast.
                    let mut events = Events {
                        answers: &answers,
                        first: Some(response.body.bytes().to_vec()),
                        ended: false,
                    };
                    return exchange.stream(response.status, &mut events).await;
                }
                let status = response.status;
                let bytes = response.body.bytes().len();
                exchange.write(response.body.bytes()).await?;
                let total_ms = applib::clock::now_ns().saturating_sub(t0) / 1_000_000;
                if total_ms >= SLOW_MS {
                    log!(
                        "[stulp:slow] {} {} status={status} bytes={bytes} owner_ms={owner_ms} total_ms={total_ms}",
                        exchange.req.method,
                        exchange.req.path
                    );
                }
                Ok(())
            },
        )
        .await;
        if let Err(e) = result {
            log!("[stulp:connection] {e}");
        }
    }
}
/// Vanaf deze duur is een aanvraag of een eigenaarsstap een consoleregel waard.
const SLOW_MS: u64 = 200;
