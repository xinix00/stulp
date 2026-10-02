//! Vaste HTTP-pool; één controller-taak bezit alle huisstaat.
use std::{
    net::TcpListener,
    sync::mpsc::{self, Receiver, SyncSender},
    time::Duration,
};
use stulp_core::{
    json,
    store::{Storage, Store},
};
use stulp_web::{Request, Response, Web};

/// Acht verbindingen, acht wachtende opdrachten; geen thread per verbinding.
pub const WORKERS: usize = 8;
#[path = "upload.rs"]
mod upload;

// Acht inline opdrachten blijven onder 2 KiB; boxing zou een extra allocatie per request toevoegen.
#[allow(clippy::large_enum_variant)]
enum Work {
    Request(Call),
    MediaDone(u64),
    Restore(crate::archive::Prepared, SyncSender<Response>),
}
/// De bron van een SSE-stroom voor `leanhttp::Exchange::stream`: het eerste
/// antwoord van de controller, dan elk volgend; een seconde stilte is
/// "niets" (de stroom kijkt dan of de lezer er nog is), vijftien seconden
/// stilte is een keepalive, een gesloten kanaal het einde.
struct Events<'a> {
    answers: &'a Receiver<Response>,
    first: Option<Vec<u8>>,
    /// Seconden stilte sinds de laatste bytes.
    quiet: u32,
}

impl leanhttp::Source for Events<'_> {
    async fn next(&mut self) -> leanhttp::Next {
        if let Some(first) = self.first.take() {
            return leanhttp::Next::Data(first);
        }
        match self.answers.recv_timeout(Duration::from_secs(1)) {
            Ok(response) => {
                self.quiet = 0;
                leanhttp::Next::Data(response.body.bytes().to_vec())
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {
                self.quiet += 1;
                if self.quiet >= 15 {
                    self.quiet = 0;
                    leanhttp::Next::Data(b": keepalive\n\n".to_vec())
                } else {
                    leanhttp::Next::Nothing
                }
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => leanhttp::Next::End,
        }
    }

    // Het wachten zit al in `recv_timeout`.
    async fn nap(&mut self) {}
}

struct MediaGuard {
    sender: SyncSender<Work>,
    owner: u64,
}
impl Drop for MediaGuard {
    fn drop(&mut self) {
        let _ = self.sender.send(Work::MediaDone(self.owner));
    }
}
struct Call {
    request: Request,
    reply: SyncSender<Response>,
}

/// Draait op een reeds gebonden listener, zodat tests een vrije poort kunnen kiezen.
pub fn serve<S: Storage>(listener: TcpListener, store: Store<S>, web: Web) -> std::io::Result<()> {
    serve_with_apps(listener, store, web, crate::apps::Apps::new(None)?)
}

/// De apptransporten worden samen met de store aan de controller overgedragen.
pub fn serve_with_apps<S: Storage>(
    listener: TcpListener,
    store: Store<S>,
    web: Web,
    apps: crate::apps::Apps<'_>,
) -> std::io::Result<()> {
    serve_with_level(listener, store, web, apps, crate::logging::Level::Info)
}
/// Dezelfde server met een expliciet minimumniveau voor lokale pluginlogs.
pub fn serve_with_level<S: Storage>(
    listener: TcpListener,
    store: Store<S>,
    web: Web,
    apps: crate::apps::Apps<'_>,
    level: crate::logging::Level,
) -> std::io::Result<()> {
    serve_secure(listener, store, web, apps, level, None)
}
/// HTTPS deelt dezelfde vaste HTTP-pool, routes en streaminggrenzen.
pub fn serve_secure<S: Storage>(
    listener: TcpListener,
    store: Store<S>,
    web: Web,
    apps: crate::apps::Apps<'_>,
    level: crate::logging::Level,
    identity: Option<&stulp_transport::KeyPair>,
) -> std::io::Result<()> {
    listener.set_nonblocking(true)?;
    let (sender, receiver) = mpsc::sync_channel(WORKERS);
    let mut listeners = Vec::new();
    listeners
        .try_reserve_exact(WORKERS)
        .map_err(std::io::Error::other)?;
    for _ in 0..WORKERS {
        listeners.push(listener.try_clone()?);
    }
    std::thread::scope(|scope| {
        for listener in listeners {
            let sender = sender.clone();
            scope.spawn(move || hostnet::block_on(worker(listener, sender, identity)));
        }
        drop(sender);
        hostnet::block_on(controller(store, web, receiver, apps, level));
    });
    Ok(())
}

struct Pending {
    owner: u64,
    reply: SyncSender<Response>,
    deadline: u64,
    asset: Option<crate::app_ui::Asset>,
    media: bool,
    image: bool,
    measures: bool,
}
struct Subscriber {
    reply: SyncSender<Response>,
    cursor: u64,
    last: u64,
    manager: String,
    overview: bool,
}

async fn controller<S: Storage>(
    mut store: Store<S>,
    web: Web,
    receiver: Receiver<Work>,
    mut apps: crate::apps::Apps<'_>,
    level: crate::logging::Level,
) {
    let mut env = crate::Environment;
    let mut pending: Vec<Pending> = Vec::new();
    let mut owner = 0_u64;
    let mut flows =
        crate::flows::Flows::new(crate::timezone::load(store.timezone()).unwrap_or_else(|e| {
            eprintln!("[stulp:timezone] {e}; using UTC");
            Default::default()
        }));
    let mut catalog = crate::catalog::Catalog::new();
    let mut mcp = crate::mcp::Mcp::new();
    let mut scenes = crate::scenes::Scenes::new();
    let mut pairing = crate::pairing::Pairing::new();
    let mut processes = crate::processes::Processes::with_level(level);
    let mut subscribers: Vec<Subscriber> = Vec::new();
    let mut media: Vec<u64> = Vec::new();
    loop {
        if stulp_platform::signals::requested() {
            return;
        }
        if let Ok(now) = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH) {
            store.tick(u64::try_from(now.as_millis()).unwrap_or(u64::MAX));
        }
        if let Some(e) = store.statistics_fault() {
            eprintln!("[stulp:statistics] {e}");
        }
        match apps.poll(&mut store) {
            Ok(completed) => {
                for completion in completed {
                    match mcp.complete(&completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            eprintln!("[stulp:mcp-callback] {e}");
                            continue;
                        }
                    }
                    match catalog.complete(&store, &completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            eprintln!("[stulp:catalog] {e}");
                            continue;
                        }
                    }
                    match scenes.complete(&completion) {
                        Ok(true) => continue,
                        Ok(false) => (),
                        Err(e) => {
                            eprintln!("[stulp:scene-callback] {e}");
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
                            eprintln!("[stulp:flow] callback failed: {e}");
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
                                    media.retain(|id| *id != p.owner);
                                }
                            }
                            Err(_) => {
                                media.retain(|id| *id != p.owner);
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
            Err(e) => eprintln!("[stulp:app-poll] {e}"),
        }
        if let Err(e) = processes.poll(&mut store, &mut apps) {
            eprintln!("[stulp:process-poll] {e}");
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
                            eprintln!("[stulp:mcp-scene] {e}");
                            continue;
                        }
                    }
                    if let Err(e) = flows.complete(&completion, apps.now()) {
                        eprintln!("[stulp:scene-flow] {e}");
                    }
                }
            }
            Err(e) => eprintln!("[stulp:scene] {e}"),
        }
        mcp.poll(
            &mut store,
            &mut crate::mcp::Services {
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
            match s.reply.try_send(response) {
                Ok(()) => {
                    s.cursor = store.sequence();
                    s.last = now;
                    true
                }
                Err(mpsc::TrySendError::Full(_)) => true,
                Err(mpsc::TrySendError::Disconnected(_)) => false,
            }
        });
        let mut index = 0;
        while index < pending.len() {
            if pending.get(index).is_some_and(|p| p.deadline <= now) {
                let p = pending.remove(index);
                media.retain(|id| *id != p.owner);
                if let Ok(response) = Response::error(504, "app callback timed out") {
                    let _ = p.reply.send(response);
                }
            } else {
                index += 1;
            }
        }
        let call = match receiver.recv_timeout(Duration::from_millis(10)) {
            Ok(Work::Request(call)) => call,
            Ok(Work::Restore(prepared, reply)) => {
                // Drop bevestigt het einde van ieder lokaal kind vóór bundels veranderen.
                processes = crate::processes::Processes::new();
                apps.reset();
                pending.clear();
                subscribers.clear();
                media.clear();
                flows = crate::flows::Flows::new(
                    crate::timezone::load(store.timezone()).unwrap_or_else(|e| {
                        eprintln!("[stulp:timezone] {e}; using UTC");
                        Default::default()
                    }),
                );
                catalog = crate::catalog::Catalog::new();
                mcp = crate::mcp::Mcp::new();
                scenes = crate::scenes::Scenes::new();
                pairing = crate::pairing::Pairing::new();
                let response = match prepared.apply(&mut store) {
                    Ok(value) => Response::json(200, &value),
                    Err(error) => Response::error(422, &error.to_string()),
                };
                if let Ok(response) = response {
                    let _ = reply.send(response);
                }
                continue;
            }
            Ok(Work::MediaDone(owner)) => {
                media.retain(|id| *id != owner);
                continue;
            }
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => return,
        };
        match web.handle(&mut store, &call.request, &mut env) {
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
                        &mut crate::mcp::Services {
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
                        let response = crate::archive::route(&store, call.request);
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
                    match processes.route(&mut store, &mut apps, &call.request) {
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
                    let prepared = crate::app_ui::prepare(&store, &call.request);
                    let routed = match prepared {
                        Ok(Some((_, _, asset))) if asset.is_local() => {
                            let response = asset
                                .local_response(crate::app_ui::read_local)
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
                                        media.push(next);
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
                eprintln!("[stulp:http] response construction failed: {error}");
                if let Ok(response) = Response::error(500, "response construction failed") {
                    let _ = call.reply.send(response);
                }
            }
        }
    }
}

use stulp_controller::callbacks::callback;

async fn worker(
    listener: TcpListener,
    sender: SyncSender<Work>,
    identity: Option<&stulp_transport::KeyPair>,
) {
    loop {
        if stulp_platform::signals::requested() {
            return;
        }
        let stream = match listener.accept() {
            Ok((stream, _)) => stream,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(10));
                continue;
            }
            Err(e) => {
                eprintln!("[stulp:accept] {e}");
                return;
            }
        };
        let socket = stulp_platform::socket::Socket::Tcp(stream);
        let stream = match identity {
            Some(identity) => stulp_transport::Stream::server(socket, identity, true),
            None => stulp_transport::Stream::plain(socket),
        };
        let mut stream = match stream {
            Ok(s) => s,
            Err(e) => {
                eprintln!("[stulp:transport] {e}");
                continue;
            }
        };
        stream.blocking(Duration::from_secs(15));
        match upload::matches(&mut stream) {
            Ok(true) => {
                if let Err(e) = upload::serve(stream, &sender) {
                    eprintln!("[stulp:restore-upload] {e}");
                }
                continue;
            }
            Ok(false) => (),
            Err(e) => {
                eprintln!("[stulp:request-head] {e}");
                continue;
            }
        }
        // Elke verbinding krijgt eigen antwoordhandvatten. Een oude SSE-zender
        // mag nooit in de inbox van de volgende browserverbinding belanden.
        let (reply, answers) = mpsc::sync_channel(1);
        let conn = stulp_transport::Http::new(stream);
        let result = leanhttp::serve(
            conn,
            async |exchange: &mut leanhttp::Exchange<'_, stulp_transport::Http<'_>>| {
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
                if sender
                    .send(Work::Request(Call {
                        request,
                        reply: reply.clone(),
                    }))
                    .is_err()
                {
                    return exchange.error(503, "controller stopped").await;
                }
                let mut response = match answers.recv() {
                    Ok(response) => response,
                    Err(_) => return exchange.error(503, "controller stopped").await,
                };
                if matches!(&response.body, stulp_web::Body::Restore { .. }) {
                    let stulp_web::Body::Restore {
                        destination,
                        archive,
                    } = std::mem::replace(&mut response.body, stulp_web::Body::Static(b""))
                    else {
                        return Err(leanhttp::Error::Connect);
                    };
                    let prepared = crate::archive::Prepared::read(
                        &mut std::io::Cursor::new(archive),
                        std::path::Path::new(&destination),
                    );
                    match prepared {
                        Ok(prepared) => {
                            if sender.send(Work::Restore(prepared, reply.clone())).is_err() {
                                return exchange.error(503, "controller stopped").await;
                            }
                            response = match answers.recv() {
                                Ok(r) => r,
                                Err(_) => return exchange.error(503, "controller stopped").await,
                            };
                        }
                        Err(e) => {
                            response = Response::error(422, &e.to_string())
                                .map_err(|_| leanhttp::Error::Alloc { bytes: 4096 })?
                        }
                    }
                }
                if let stulp_web::Body::Backup(document) = &response.body {
                    return crate::archive::download(exchange, document).await;
                }
                if let stulp_web::Body::Proxy { url, mime, owner } = &response.body {
                    let _guard = MediaGuard {
                        sender: sender.clone(),
                        owner: *owner,
                    };
                    return crate::media::pipe(exchange, url, mime).await;
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
                    // nog is, zodat een gesloten tabblad zijn werker binnen een
                    // seconde teruggeeft in plaats van na de tweede keepalive.
                    let mut events = Events {
                        answers: &answers,
                        first: Some(response.body.bytes().to_vec()),
                        quiet: 0,
                    };
                    return exchange.stream(response.status, &mut events).await;
                }
                exchange.write(response.body.bytes()).await?;
                Ok(())
            },
        )
        .await;
        if let Err(e) = result {
            eprintln!("[stulp:connection] {e}");
        }
    }
}
