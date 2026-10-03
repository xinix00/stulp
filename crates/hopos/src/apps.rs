//! Authenticated external plugin slots, owned and polled by the controller.
use alloc::{
    string::{String, ToString},
    vec::Vec,
};
use applib::{
    EXEC,
    appnet::{TcpListener, TcpStream},
};
use core::time::Duration;
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
struct Connection {
    stream: TcpStream,
    decoder: Decoder,
    nonce: String,
    app: Option<App>,
    outgoing: Vec<Vec<u8>>,
    offset: usize,
    queued: usize,
    accepted: u64,
    last_ping: Option<u64>,
}
/// Bounded external plugin listener; Hop starts each plugin in its own slot.
pub struct Apps {
    platform: &'static applib::App,
    listener: TcpListener,
    connections: Vec<Connection>,
}
impl Apps {
    /// Bind the private attach port. The deployment must keep this port within its trusted network.
    pub fn bind(platform: &'static applib::App, port: u16) -> Result<Self> {
        let listener =
            TcpListener::bind(port).map_err(|_| Error::Invalid("attach listener failed"))?;
        let mut connections = Vec::new();
        connections
            .try_reserve_exact(MAX_APPS)
            .map_err(|_| Error::Memory)?;
        Ok(Self {
            platform,
            listener,
            connections,
        })
    }
    /// Monotonic callback clock in milliseconds.
    pub fn now(&self) -> u64 {
        applib::clock::now_ns() / 1_000_000
    }
    /// De app van dit slot, voor de meetlat.
    pub fn platform(&self) -> &'static applib::App {
        self.platform
    }
    /// Whether the plugin completed initialization.
    pub fn running(&self, id: &str) -> bool {
        self.connections.iter().any(|c| {
            c.app
                .as_ref()
                .is_some_and(|a| a.id() == id && a.is_running())
        })
    }
    /// Close all plugin routes before restoring the controller document.
    pub fn reset(&mut self) {
        self.connections.clear();
    }
    /// Slaapt tot er werk is (HopOS docs/apps.md): een nieuwe attach (die
    /// komt mee terug), bytes op een app-verbinding, of de tik. Met uitgaand
    /// werk in een rij korter, zodat een volle zendbuffer binnen 10 ms
    /// opnieuw geprobeerd wordt; de zendkant heeft geen eigen bel.
    pub async fn wait(&mut self, tick: Duration) -> Option<TcpStream> {
        let flushing = self.connections.iter().any(|c| !c.outgoing.is_empty());
        let nap = if flushing {
            Duration::from_millis(10)
        } else {
            tick
        };
        let mut timer = core::pin::pin!(EXEC.get().after(nap));
        let room = self.connections.len() < MAX_APPS;
        let Self {
            listener,
            connections,
            ..
        } = self;
        core::future::poll_fn(|cx| {
            use core::task::Poll;
            if timer.as_mut().poll(cx).is_ready() {
                return Poll::Ready(None);
            }
            if room {
                match core::pin::pin!(listener.accept()).poll(cx) {
                    Poll::Ready(Ok(stream)) => return Poll::Ready(Some(stream)),
                    Poll::Ready(Err(_)) => return Poll::Ready(None),
                    Poll::Pending => (),
                }
            }
            for c in connections.iter_mut() {
                if core::pin::pin!(c.stream.readable()).poll(cx).is_ready() {
                    return Poll::Ready(None);
                }
            }
            Poll::Pending
        })
        .await
    }
    fn accept(
        &mut self,
        env: &mut crate::environment::Environment,
        first: Option<TcpStream>,
    ) -> Result {
        if let Some(stream) = first {
            self.admit(stream, env)?;
        }
        for _ in 0..4 {
            if self.connections.len() == MAX_APPS {
                break;
            }
            let stream = match crate::poll::once(self.listener.accept()) {
                Some(Ok(stream)) => stream,
                None => break,
                Some(Err(_)) => return Err(Error::Invalid("attach accept failed")),
            };
            self.admit(stream, env)?;
        }
        Ok(())
    }
    /// Neemt één aangenomen verbinding op en stuurt de uitdaging; vol is
    /// stil weigeren (de peer ziet een gesloten verbinding en komt terug).
    fn admit(&mut self, stream: TcpStream, env: &mut crate::environment::Environment) -> Result {
        if self.connections.len() >= MAX_APPS {
            return Ok(());
        }
        {
            let nonce = token::base64(&env.random())?;
            let mut connection = Connection {
                stream,
                decoder: Decoder::new(MAX_GREETING),
                nonce,
                app: None,
                outgoing: Vec::new(),
                offset: 0,
                queued: 0,
                accepted: self.now(),
                last_ping: None,
            };
            let challenge = json::fields(&[
                ("protocol", Value::uint(1)),
                ("nonce", json::string(&connection.nonce)?),
            ])?;
            connection.queue(stulp_protocol::encode(&challenge)?)?;
            self.connections.push(connection);
        }
        Ok(())
    }
    pub(crate) fn route<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        request: &stulp_web::Request,
    ) -> Result<Option<stulp_web::Response>> {
        if request.method == "POST"
            && let Some(id) = request
                .path
                .strip_prefix("/api/manager/apps/app/")
                .and_then(|v| v.strip_suffix("/restart"))
                .filter(|id| !id.is_empty() && !id.contains('/'))
        {
            let record = store.document().record("apps", id)?;
            if !json::boolean(record, "enabled") || json::boolean(record, "offered") {
                return Err(Error::Invalid("app is disabled"));
            }
            // External slots reconnect and initialize on loss of their attach connection.
            self.connections
                .retain(|c| c.app.as_ref().is_none_or(|a| a.id() != id));
            store.set_app_status(id, "waiting")?;
            return Ok(Some(stulp_web::Response::json(200, &Value::Bool(true))?));
        }
        Ok(None)
    }
    /// Eén begrensde ronde; een zwijgende app kan de HTTP-eigenaar niet ophouden.
    pub fn poll<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        env: &mut crate::environment::Environment,
        first: Option<TcpStream>,
    ) -> Result<Vec<Completion>> {
        self.accept(env, first)?;
        let now = self.now();
        let mut completed = Vec::new();
        let count = self.connections.len();
        for _ in 0..count {
            let mut connection = self.connections.remove(0);
            let result = connection.poll(store, &self.connections, now, &mut completed, env);
            match result {
                Ok(()) => {
                    if let Some(app) = &connection.app
                        && app.is_running()
                    {
                        store.set_app_status(app.id(), "running")?;
                    }
                    self.connections.push(connection);
                }
                Err(e) => self.closed(store, connection, &e, &mut completed)?,
            }
        }
        // Een verse welkomst van een app die al een verbinding had: de oude
        // gaat dicht, met dezelfde opruiming als bij een verbroken verbinding.
        let mut fresh = Vec::new();
        for c in &self.connections {
            if let Some(app) = &c.app
                && self.connections.iter().any(|o| {
                    o.accepted < c.accepted && o.app.as_ref().is_some_and(|a| a.id() == app.id())
                })
            {
                json::push(&mut fresh, (json::copy(app.id())?, c.accepted), MAX_APPS)?;
            }
        }
        for (id, accepted) in fresh {
            while let Some(at) = self
                .connections
                .iter()
                .position(|o| o.accepted < accepted && o.app.as_ref().is_some_and(|a| a.id() == id))
            {
                let stale = self.connections.remove(at);
                self.closed(
                    store,
                    stale,
                    &Error::Conflict("superseded by a fresh attach"),
                    &mut completed,
                )?;
            }
        }
        Ok(completed)
    }

    /// Een verbinding die wegvalt: melden, lopende callbacks laten falen, de
    /// app op wachtend of gestopt zetten en zijn apparaten onbereikbaar melden.
    fn closed<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        connection: Connection,
        e: &dyn core::fmt::Display,
        completed: &mut Vec<Completion>,
    ) -> Result {
        self.platform.log(format_args!(
            "[stulp:app-disconnected] app={} error={e}",
            connection
                .app
                .as_ref()
                .map(App::id)
                .unwrap_or("unidentified")
        ));
        if let Some(mut app) = connection.app {
            for owner in app.disconnect().filter(|owner| *owner != 0) {
                json::push(
                    completed,
                    Completion {
                        owner,
                        failed: true,
                        value: json::fields(&[("message", json::string("app disconnected")?)])?,
                    },
                    MAX_COMPLETIONS,
                )?;
            }
            let enabled = store
                .document()
                .record("apps", app.id())
                .is_ok_and(|a| json::boolean(a, "enabled"));
            if store.document().record("apps", app.id()).is_ok() {
                store.set_app_status(app.id(), if enabled { "waiting" } else { "stopped" })?;
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
        Ok(())
    }

    /// Announce an adopted device to its plugin.
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
impl Connection {
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
        others: &[Connection],
        now: u64,
        completed: &mut Vec<Completion>,
        env: &mut crate::environment::Environment,
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
            .is_some_and(|last| now.saturating_sub(last) > 30_000)
        {
            return Err(Error::Invalid("app heartbeat timed out"));
        }
        if let Some(app) = &mut self.app {
            let record = store.document().record("apps", app.id())?;
            if !json::boolean(record, "enabled") || json::boolean(record, "offered") {
                return Err(Error::Invalid("app disabled"));
            }
            let frames = app.follow(store, now)?;
            for frame in frames {
                self.queue(stulp_protocol::encode(&frame)?)?;
            }
        }
        let mut buf = [0_u8; 8192];
        for _ in 0..16 {
            let read = match crate::poll::once(self.stream.read(&mut buf)) {
                Some(Ok(0)) => return Err(Error::Invalid("app stream closed")),
                Some(Ok(read)) => read,
                None => break,
                Some(Err(_)) => return Err(Error::Invalid("app read failed")),
            };
            let mut used = 0;
            while used < read {
                used += self.decoder.feed(buf.get(used..read).ok_or(Error::Full)?)?;
                if let Some(bytes) = self.decoder.take() {
                    self.receive(store, others, &bytes, now, completed, env)?;
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
            match crate::poll::once(
                self.stream
                    .write(bytes.get(self.offset..).ok_or(Error::Full)?),
            ) {
                Some(Ok(0)) => return Err(Error::Invalid("app write closed")),
                Some(Ok(written)) => {
                    self.offset += written;
                    if self.offset == bytes.len() {
                        self.queued -= bytes.len();
                        self.offset = 0;
                        self.outgoing.remove(0);
                    }
                }
                None => break,
                Some(Err(_)) => return Err(Error::Invalid("app write failed")),
            }
        }
        Ok(())
    }

    fn receive<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        others: &[Connection],
        bytes: &[u8],
        now: u64,
        completed: &mut Vec<Completion>,
        env: &mut crate::environment::Environment,
    ) -> Result {
        if self.app.is_none() {
            let result = self.greeting(store, others, bytes, env);
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
        others: &[Connection],
        bytes: &[u8],
        env: &mut crate::environment::Environment,
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
        // Een tweede attach van dezelfde app wint: de vorige verbinding is
        // vrijwel altijd een dode die de hartslag nog niet heeft afgeschreven
        // (15 s), en zolang hij er zat kwam de verse elke 1, 2, 4, 8 s terug
        // met "already connected" (LicheeRV, 02-10). `Apps::poll` ruimt de
        // oude op zodra deze is verwelkomd.
        let _ = others;
        let manifest = match json::get(&greeting, "manifest").filter(|v| !v.is_null()) {
            Some(m) => m.try_clone()?,
            None => match store.manifest(id) {
                Some(m) => m.try_clone()?,
                None => return Err(Error::Missing("external app must announce its manifest")),
            },
        };
        let runtime = App::new(id, manifest)?;
        let secret = json::copy(secret)?;
        if store.document().record("apps", id).is_err() {
            let record = json::fields(&[
                ("id", json::string(id)?),
                ("root", json::string("")?),
                ("offered", Value::Bool(true)),
                ("enabled", Value::Bool(false)),
            ])?;
            store.put("apps", record, true, None, &env.now()?)?;
        }
        store.announce(id, runtime.manifest().try_clone()?)?;
        let app = store.document().record("apps", id)?;
        if !json::boolean(app, "enabled") || json::boolean(app, "offered") {
            store.set_app_status(id, "stopped")?;
            return Err(Error::Invalid("app awaits installation or is disabled"));
        }
        if id == "com.stulp.matter" {
            stulp_matter::devices::reconcile(store, &env.now()?).map_err(|e| match e {
                stulp_sdk::Error::Core(e) => e,
                stulp_sdk::Error::Invalid(message) => Error::Invalid(message),
                _ => Error::Invalid("Matter endpoint migration failed"),
            })?;
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

impl stulp_controller::Apps for Apps {
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
        self.platform.log(message);
    }
}
