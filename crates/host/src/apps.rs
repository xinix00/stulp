//! Nonblocking appverbindingen blijven eigendom van dezelfde controller-taak.
use std::{
    io::{Read, Write},
    net::TcpListener,
    time::Instant,
};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_protocol::{
    Decoder, Frame, MAX_FRAME, MAX_GREETING,
    token::{self, Direction},
};
use stulp_runtime::{App, Completion};
use stulp_web::Environment as _;

const MAX_APPS: usize = 32;
const MAX_OUTPUT: usize = 4 << 20;
const MAX_PACKETS: usize = 64;
const MAX_COMPLETIONS: usize = MAX_APPS * stulp_protocol::session::MAX_PENDING;

struct Connection<'a> {
    stream: stulp_transport::Stream<'a>,
    decoder: Decoder,
    nonce: String,
    app: Option<App>,
    outgoing: Vec<Vec<u8>>,
    offset: usize,
    queued: usize,
    accepted: u64,
    last_ping: Option<u64>,
    manual: Option<String>,
}

/// Een listener bestaat uitsluitend als --attach-port expliciet is opgegeven.
pub struct Apps<'a> {
    identity: Option<&'a stulp_transport::KeyPair>,
    listener: Option<TcpListener>,
    connections: Vec<Connection<'a>>,
    started: Instant,
    local: Option<crate::processes::Local>,
    attached: Option<crate::processes::Local>,
    manual: Option<String>,
}

impl<'a> Apps<'a> {
    /// Neemt de listener over; caller heeft transportvertrouwelijkheid gekozen.
    pub fn new(listener: Option<TcpListener>) -> std::io::Result<Self> {
        if let Some(listener) = &listener {
            listener.set_nonblocking(true)?;
        }
        let mut connections = Vec::new();
        connections
            .try_reserve_exact(MAX_APPS)
            .map_err(std::io::Error::other)?;
        Ok(Self {
            listener,
            identity: None,
            connections,
            started: Instant::now(),
            local: None,
            attached: None,
            manual: None,
        })
    }
    /// Beveiligde remote listener; lokale proceskanalen blijven Unix-sockets.
    pub fn secure(
        listener: Option<TcpListener>,
        identity: &'a stulp_transport::KeyPair,
    ) -> std::io::Result<Self> {
        let mut apps = Self::new(listener)?;
        apps.identity = Some(identity);
        Ok(apps)
    }
    pub(crate) fn for_app(id: &str) -> Result<Self> {
        let mut apps =
            Self::new(None).map_err(|_| Error::Invalid("cannot initialize app transport"))?;
        apps.manual = Some(json::copy(id)?);
        Ok(apps)
    }

    /// Biedt een expliciete Unix-attachsocket naast de private processocket aan.
    pub fn attach_local(&mut self, path: &std::path::Path) -> Result {
        if self.attached.is_some() {
            return Err(Error::Conflict("Unix attach listener already configured"));
        }
        self.attached = Some(crate::processes::Local::at(path)?);
        Ok(())
    }

    pub(crate) fn reset(&mut self) {
        self.connections.clear();
    }

    /// Monotone klok voor callbacks en begroetingen.
    pub fn now(&self) -> u64 {
        u64::try_from(self.started.elapsed().as_millis()).unwrap_or(u64::MAX)
    }

    /// Eén begrensde ronde; een zwijgende app kan de HTTP-eigenaar niet ophouden.
    pub fn poll<S: Storage>(&mut self, store: &mut Store<S>) -> Result<Vec<Completion>> {
        self.accept()?;
        let now = self.now();
        let mut completed = Vec::new();
        let count = self.connections.len();
        for _ in 0..count {
            let mut connection = self.connections.remove(0);
            let result = connection.poll(store, &self.connections, now, &mut completed);
            match result {
                Ok(()) => {
                    if let Some(app) = &connection.app
                        && app.is_running()
                    {
                        store.set_app_status(app.id(), "running")?;
                    }
                    self.connections.push(connection);
                }
                Err(e) => {
                    eprintln!(
                        "[stulp:app-disconnected] app={} error={e}",
                        connection
                            .app
                            .as_ref()
                            .map(App::id)
                            .unwrap_or("unidentified")
                    );
                    if let Some(mut app) = connection.app {
                        for owner in app.disconnect().filter(|owner| *owner != 0) {
                            json::push(
                                &mut completed,
                                Completion {
                                    owner,
                                    failed: true,
                                    value: json::fields(&[(
                                        "message",
                                        json::string("app disconnected")?,
                                    )])?,
                                },
                                MAX_COMPLETIONS,
                            )?;
                        }
                        let enabled = store
                            .document()
                            .record("apps", app.id())
                            .is_ok_and(|a| json::boolean(a, "enabled"));
                        if store.document().record("apps", app.id()).is_ok() {
                            store.set_app_status(
                                app.id(),
                                if enabled { "waiting" } else { "stopped" },
                            )?;
                        }
                        let mut ids = Vec::new();
                        for device in store
                            .document()
                            .records("devices")
                            .iter()
                            .filter(|d| json::text(d, "appId") == app.id())
                        {
                            json::push(&mut ids, json::copy(json::text(device, "id"))?, 4096)?;
                        }
                        for id in ids {
                            let device = store.device(&id)?;
                            let state = json::get(&device, "state")
                                .unwrap_or(&Value::Null)
                                .try_clone()?;
                            store.observe(app.id(), &id, state, false, "app disconnected")?;
                        }
                    }
                }
            }
        }
        Ok(completed)
    }

    pub(crate) fn connected(&self, id: &str) -> bool {
        self.connections
            .iter()
            .any(|c| c.app.as_ref().is_some_and(|a| a.id() == id))
    }
    pub(crate) fn running(&self, id: &str) -> bool {
        self.connections.iter().any(|c| {
            c.app
                .as_ref()
                .is_some_and(|a| a.id() == id && a.is_running())
        })
    }
    pub(crate) fn disconnect(&mut self, id: &str) {
        for c in &mut self.connections {
            if c.app.as_ref().is_some_and(|a| a.id() == id) {
                c.stream.shutdown();
            }
        }
    }
    pub(crate) fn local_path(&mut self) -> Result<&std::path::Path> {
        if self.local.is_none() {
            self.local = Some(crate::processes::Local::new()?);
        }
        self.local
            .as_ref()
            .map(|l| l.path.as_path())
            .ok_or(Error::Missing("local app listener"))
    }
    fn accept(&mut self) -> Result {
        for source in 0..3 {
            for _ in 0..4 {
                let accepted = if source != 0 {
                    match if source == 1 {
                        &self.local
                    } else {
                        &self.attached
                    } {
                        Some(l) => l
                            .listener
                            .accept()
                            .map(|(s, _)| stulp_platform::socket::Socket::Unix(s)),
                        None => break,
                    }
                } else {
                    match &self.listener {
                        Some(l) => l
                            .accept()
                            .map(|(s, _)| stulp_platform::socket::Socket::Tcp(s)),
                        None => break,
                    }
                };
                match accepted {
                    Ok(stream) => {
                        if self.connections.len() == MAX_APPS {
                            continue;
                        }
                        stream
                            .nonblocking()
                            .map_err(|_| Error::Invalid("app socket mode failed"))?;
                        let nonce = if let stulp_platform::socket::Socket::Unix(s) = &stream {
                            if !stulp_platform::peer::same_user(s)
                                .map_err(|_| Error::Invalid("cannot identify Unix peer"))?
                            {
                                continue;
                            }
                            String::new()
                        } else {
                            token::base64(
                                &hostnet::entropy()
                                    .map_err(|_| Error::Invalid("OS entropy unavailable"))?[..32],
                            )?
                        };
                        let hello = json::fields(&[
                            ("protocol", Value::uint(1)),
                            ("nonce", json::string(&nonce)?),
                        ])?;
                        let stream = match (source, self.identity) {
                            (0, Some(identity)) => {
                                stulp_transport::Stream::server(stream, identity, false)
                            }
                            _ => stulp_transport::Stream::plain(stream),
                        }
                        .map_err(|_| Error::Invalid("cannot initialize app transport"))?;
                        let mut c = Connection {
                            stream,
                            decoder: Decoder::new(MAX_GREETING),
                            nonce,
                            app: None,
                            outgoing: Vec::new(),
                            offset: 0,
                            queued: 0,
                            accepted: self.now(),
                            last_ping: None,
                            manual: self.manual.as_deref().map(json::copy).transpose()?,
                        };
                        c.queue(stulp_protocol::encode(&hello)?)?;
                        self.connections.push(c);
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                    Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                    Err(_) => return Err(Error::Invalid("app listener failed")),
                }
            }
        }
        Ok(())
    }

    /// Publiceert een adoptie vóór driver.init/device.init, zonder automatische dubbele init.
    pub fn adopt(&mut self, device: &Value) -> Result {
        let connection = self
            .connections
            .iter_mut()
            .find(|c| {
                c.app
                    .as_ref()
                    .is_some_and(|a| a.id() == json::text(device, "appId"))
            })
            .ok_or(Error::Missing("app is not connected"))?;
        let frame = connection
            .app
            .as_mut()
            .ok_or(Error::Missing("app is not connected"))?
            .adopt(device)?;
        connection.queue(stulp_protocol::encode(&frame)?)
    }

    /// Verstuurt een controllercallback; het resultaat komt terug uit een volgende poll.
    pub fn call(&mut self, app_id: &str, owner: u64, method: &str, params: &Value) -> Result {
        let now = self.now();
        let connection = self
            .connections
            .iter_mut()
            .find(|c| c.app.as_ref().is_some_and(|a| a.id() == app_id))
            .ok_or(Error::Missing("app is not connected"))?;
        let app = connection
            .app
            .as_mut()
            .ok_or(Error::Missing("app is not connected"))?;
        let frame = app.call(owner, method, params, now)?;
        let id = json::uint(&frame, "id");
        let result = stulp_protocol::encode(&frame).and_then(|bytes| connection.queue(bytes));
        if result.is_err()
            && let Some(app) = &mut connection.app
        {
            app.cancel(id);
        }
        result
    }
}

impl Connection<'_> {
    fn queue(&mut self, bytes: Vec<u8>) -> Result {
        if bytes.len() > MAX_OUTPUT.saturating_sub(self.queued) {
            return Err(Error::Full);
        }
        let len = bytes.len();
        json::push(&mut self.outgoing, bytes, MAX_PACKETS)?;
        self.queued += len;
        Ok(())
    }

    fn poll<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        others: &[Connection<'_>],
        now: u64,
        completed: &mut Vec<Completion>,
    ) -> Result {
        if self.app.is_none() && now.saturating_sub(self.accepted) > 10_000 {
            return Err(Error::Invalid("attach greeting timed out"));
        }
        if self.app.as_ref().is_some_and(|a| !a.is_running())
            && now.saturating_sub(self.accepted) > 60_000
        {
            return Err(Error::Invalid("app initialization timed out"));
        }
        if self
            .last_ping
            .is_some_and(|last| now.saturating_sub(last) > 15_000)
        {
            return Err(Error::Invalid("app heartbeat timed out"));
        }
        if let Some(app) = &mut self.app {
            let record = store.document().record("apps", app.id())?;
            if (!json::boolean(record, "enabled") && self.manual.as_deref() != Some(app.id()))
                || json::boolean(record, "offered")
            {
                return Err(Error::Invalid("app disabled"));
            }
            let frames = app.follow(store, now)?;
            for frame in frames {
                self.queue(stulp_protocol::encode(&frame)?)?;
            }
        }
        let mut buf = [0_u8; 8192];
        for _ in 0..16 {
            let read = match self.stream.read(&mut buf) {
                Ok(0) => return Err(Error::Invalid("app stream closed")),
                Ok(read) => read,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => return Err(Error::Invalid("app read failed")),
            };
            let mut used = 0;
            while used < read {
                used += self.decoder.feed(buf.get(used..read).ok_or(Error::Full)?)?;
                if let Some(bytes) = self.decoder.take() {
                    self.receive(store, others, &bytes, now, completed)?;
                }
            }
        }
        if let Some(app) = &mut self.app {
            while let Some(owner) = app.expire(now) {
                if owner == 0 {
                    return Err(Error::Invalid("app initialization callback timed out"));
                }
                json::push(
                    completed,
                    Completion {
                        owner,
                        failed: true,
                        value: json::fields(&[(
                            "message",
                            json::string("app callback timed out")?,
                        )])?,
                    },
                    MAX_COMPLETIONS,
                )?;
            }
        }
        self.flush()
    }

    fn flush(&mut self) -> Result {
        for _ in 0..MAX_PACKETS {
            let Some(bytes) = self.outgoing.first() else {
                return Ok(());
            };
            match self
                .stream
                .write(bytes.get(self.offset..).ok_or(Error::Full)?)
            {
                Ok(0) => return Err(Error::Invalid("app write closed")),
                Ok(written) => {
                    self.offset += written;
                    if self.offset == bytes.len() {
                        self.queued -= bytes.len();
                        self.offset = 0;
                        self.outgoing.remove(0);
                    }
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => return Err(Error::Invalid("app write failed")),
            }
        }
        Ok(())
    }

    fn receive<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        others: &[Connection<'_>],
        bytes: &[u8],
        now: u64,
        completed: &mut Vec<Completion>,
    ) -> Result {
        if self.app.is_none() {
            let result = self.greeting(store, others, bytes);
            if let Err(e) = &result {
                // Een korte weigering geeft een losse container een bruikbare reden.
                if let Ok(reply) = json::fields(&[
                    ("ok", Value::Bool(false)),
                    ("error", json::string(&e.to_string())?),
                ]) {
                    let _ = self.queue(stulp_protocol::encode(&reply)?);
                    let _ = self.flush();
                }
            }
            return result;
        }
        let frame = Frame::decode(bytes)?;
        if frame.method() == "$appproto.ping" {
            self.last_ping = Some(now);
        }
        let mut env = crate::Environment;
        let new_id = if matches!(frame.method(), "notification" | "image.url") {
            env.id()?
        } else {
            String::new()
        };
        let app = self.app.as_mut().ok_or(Error::Missing("app runtime"))?;
        let received = app.receive(store, frame, now, &env.now()?, &new_id)?;
        for frame in received.outgoing {
            self.queue(stulp_protocol::encode(&frame)?)?;
        }
        if let Some(completion) = received.completed {
            json::push(completed, completion, MAX_COMPLETIONS)?;
        }
        Ok(())
    }

    fn greeting<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        others: &[Connection<'_>],
        bytes: &[u8],
    ) -> Result {
        let greeting = json::parse(bytes)?;
        let id = json::text(&greeting, "appId");
        let secret = json::get(store.document().root(), "system")
            .map(|s| json::text(s, "attachSecret"))
            .unwrap_or("");
        if id.is_empty()
            || (!self.nonce.is_empty()
                && !token::check(secret, id, &self.nonce, json::text(&greeting, "proof"))?)
        {
            return Err(Error::Invalid("unknown app or wrong token"));
        }
        if json::uint(&greeting, "protocol") != 1
            || (!self.nonce.is_empty() && json::text(&greeting, "nonce").is_empty())
        {
            return Err(Error::Invalid("invalid attach protocol or nonce"));
        }
        if others
            .iter()
            .any(|c| c.app.as_ref().is_some_and(|app| app.id() == id))
        {
            return Err(Error::Conflict("app is already connected"));
        }
        let manifest = match json::get(&greeting, "manifest").filter(|v| !v.is_null()) {
            Some(m) => m.try_clone()?,
            None => match store.manifest(id) {
                Some(m) => m.try_clone()?,
                None => {
                    let installed = store.document().record("apps", id)?;
                    crate::processes::read_manifest(json::text(installed, "root"))?
                }
            },
        };
        let runtime = if self.manual.as_deref() == Some(id) {
            App::manual(id, manifest)?
        } else {
            App::new(id, manifest)?
        };
        let secret = json::copy(secret)?;
        if store.document().record("apps", id).is_err() {
            let record = json::fields(&[
                ("id", json::string(id)?),
                ("root", json::string("")?),
                ("offered", Value::Bool(true)),
                ("enabled", Value::Bool(false)),
            ])?;
            store.put("apps", record, true, None, &crate::Environment.now()?)?;
        }
        store.announce(id, runtime.manifest().try_clone()?)?;
        let app = store.document().record("apps", id)?;
        if (!json::boolean(app, "enabled") && self.manual.as_deref() != Some(id))
            || json::boolean(app, "offered")
        {
            store.set_app_status(id, "stopped")?;
            return Err(Error::Invalid("app awaits installation or is disabled"));
        }
        if id == "com.stulp.matter" {
            stulp_matter::devices::reconcile(store, &crate::Environment.now()?).map_err(
                |e| match e {
                    stulp_sdk::Error::Core(e) => e,
                    stulp_sdk::Error::Invalid(message) => Error::Invalid(message),
                    _ => Error::Invalid("Matter endpoint migration failed"),
                },
            )?;
        }
        let proof = if self.nonce.is_empty() {
            String::new()
        } else {
            token::proof(
                &token::token(&secret, id)?,
                Direction::Stulp,
                json::text(&greeting, "nonce"),
                id,
            )?
        };
        let reply = json::fields(&[("ok", Value::Bool(true)), ("proof", json::string(&proof)?)])?;
        self.queue(stulp_protocol::encode(&reply)?)?;
        self.decoder = Decoder::new(MAX_FRAME);
        self.app = Some(runtime);
        Ok(())
    }
}

impl stulp_controller::Apps for Apps<'_> {
    fn now(&self) -> u64 {
        self.now()
    }
    fn running(&self, id: &str) -> bool {
        self.running(id)
    }
    fn call(&mut self, app: &str, owner: u64, method: &str, params: &Value) -> Result {
        self.call(app, owner, method, params)
    }
    fn adopt(&mut self, device: &Value) -> Result {
        self.adopt(device)
    }
    fn log(&self, message: core::fmt::Arguments<'_>) {
        eprintln!("{message}");
    }
}
