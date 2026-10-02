//! De controllerkant van één geïsoleerde app, onafhankelijk van zijn transport.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_protocol::{Frame, Kind, session::Session};

pub mod flows;
mod groups;
mod mutations;
pub mod supervisor;

/// Hoogstens 64 frames per dispatch; payloadbytes worden door de transportqueue begrensd.
const MAX_OUTBOX: usize = 64;
/// Initialisatie bestaat uit app.init en twee calls per device.
const MAX_STARTUP: usize = 8193;

/// De controller bezit zowel deze runtime als de configuratiestore.
pub struct App {
    id: String,
    manifest: Value,
    welcomed: bool,
    running: bool,
    startup: Vec<(&'static str, Value)>,
    pending: Session,
    cursor: u64,
    known: Vec<String>,
    initializing: bool,
    groups: Vec<groups::Group>,
    manual: bool,
}

/// Een antwoord op een door de controller gestart verzoek.
pub struct Completion {
    /// Identiteit van de wachtende HTTP-, Flow- of scene-opdracht.
    pub owner: u64,
    /// Alleen de app bepaalt of zijn callback slaagde.
    pub failed: bool,
    /// Resultaat of foutobject uit het frame.
    pub value: Value,
}

/// Resultaat van één ontvangen frame, in verplichte wirevolgorde.
pub struct Received {
    /// State-events staan vóór de bevestiging van een appmutatie.
    pub outgoing: Vec<Value>,
    /// Eventueel afgeronde controlleropdracht.
    pub completed: Option<Completion>,
}

impl App {
    /// De geauthenticeerde attach/spawn-grens heeft deze identiteit al vastgesteld.
    pub fn new(id: &str, manifest: Value) -> Result<Self> {
        stulp_core::manifest::validate(&manifest)?;
        if json::text(&manifest, "id") != id {
            return Err(Error::Invalid("manifest belongs to another app"));
        }
        Ok(Self {
            id: json::copy(id)?,
            manifest,
            welcomed: false,
            running: false,
            startup: Vec::new(),
            pending: Session::new(),
            cursor: 0,
            known: Vec::new(),
            initializing: false,
            groups: Vec::new(),
            manual: false,
        })
    }
    /// Een expliciet CLI-commando mag een geïnstalleerde, uitgeschakelde app eenmalig starten.
    /// De duurzame enabled-instelling en de normale attachrechten blijven behouden.
    pub fn manual(id: &str, manifest: Value) -> Result<Self> {
        let mut app = Self::new(id, manifest)?;
        app.manual = true;
        Ok(app)
    }
    /// De identiteit komt nooit opnieuw uit een mutatiepayload.
    pub fn id(&self) -> &str {
        &self.id
    }
    /// App, drivers en devices zijn allemaal bevestigd geïnitialiseerd.
    pub fn is_running(&self) -> bool {
        self.running
    }
    /// Alleen het actuele proces levert dit manifest.
    pub fn manifest(&self) -> &Value {
        &self.manifest
    }

    /// Verwerkt één frame. Een mislukte mutatie krijgt een error-frame, geen valse ack.
    pub fn receive<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        frame: Frame,
        now_ms: u64,
        now: &str,
        new_id: &str,
    ) -> Result<Received> {
        let mut outgoing = Vec::new();
        if matches!(frame.kind, Kind::Response | Kind::Error) {
            let completed = self.reply(frame, now_ms, &mut outgoing)?;
            return Ok(Received {
                outgoing,
                completed,
            });
        }
        if frame.kind != Kind::Request {
            return Err(Error::Invalid("app must send requests"));
        }
        let first_hello = frame.method() == "hello" && !self.welcomed;
        let outcome = self.request(store, &frame, now_ms, now, new_id, &mut outgoing);
        let start = first_hello && outcome.is_ok();
        let response = match outcome {
            Ok(value) => Frame::response(frame.id, Ok(value))?,
            Err(e) => {
                use core::fmt::Write;
                let mut message = String::new();
                message.try_reserve(512).map_err(|_| Error::Memory)?;
                write!(message, "{e}").map_err(|_| Error::Full)?;
                Frame::response(frame.id, Err(&message))?
            }
        };
        json::push(&mut outgoing, response, MAX_OUTBOX)?;
        if start {
            self.start_next(now_ms, &mut outgoing)?;
        }
        self.cursor = store.sequence();
        Ok(Received {
            outgoing,
            completed: None,
        })
    }

    fn request<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        frame: &Frame,
        now_ms: u64,
        now: &str,
        new_id: &str,
        outgoing: &mut Vec<Value>,
    ) -> Result<Value> {
        let params = json::get(&frame.value, "p").unwrap_or(&Value::Null);
        if frame.method() == "$appproto.ping" {
            return Ok(Value::Null);
        }
        if frame.method() == "hello" {
            if self.welcomed {
                return Err(Error::Conflict("app already said hello"));
            }
            if json::uint(params, "protocol") != 1 {
                return Err(Error::Invalid("app protocol version mismatch"));
            }
            let welcome = self.welcome(store)?;
            self.prepare_startup(store)?;
            self.welcomed = true;
            return Ok(welcome);
        }
        if !self.welcomed {
            return Err(Error::Invalid("app must say hello first"));
        }
        mutations::dispatch(
            &self.id,
            store,
            frame.method(),
            params,
            (now, now_ms),
            new_id,
            outgoing,
        )
    }

    fn welcome<S: Storage>(&self, store: &Store<S>) -> Result<Value> {
        let app = store.document().record("apps", &self.id)?;
        if (!self.manual && !json::boolean(app, "enabled")) || json::boolean(app, "offered") {
            return Err(Error::Invalid("app is not enabled"));
        }
        let mut devices = json::Object::new();
        for device in store
            .document()
            .records("devices")
            .iter()
            .filter(|d| json::text(d, "appId") == self.id)
        {
            let id = json::text(device, "id");
            devices.push(id, store.device(id)?)?;
        }
        json::fields(&[
            ("protocol", Value::uint(1)),
            ("appId", json::string(&self.id)?),
            ("stulpId", json::string("stulp")?),
            ("stulpVersion", json::string(env!("CARGO_PKG_VERSION"))?),
            ("language", json::string(store.language())?),
            ("timezone", json::string(store.timezone())?),
            ("manifest", self.manifest.try_clone()?),
            ("env", json::object()),
            ("locale", json::object()),
            ("devices", Value::Object(devices)),
            ("settings", app_field(store, "appSettings", &self.id)?),
            ("appState", app_field(store, "appState", &self.id)?),
        ])
    }

    fn prepare_startup<S: Storage>(&mut self, store: &Store<S>) -> Result {
        let mut startup = Vec::new();
        json::push(&mut startup, ("app.init", json::object()), MAX_STARTUP)?;
        for device in store
            .document()
            .records("devices")
            .iter()
            .filter(|d| json::text(d, "appId") == self.id)
        {
            json::push(&mut self.known, json::copy(json::text(device, "id"))?, 4096)?;
            let driver = json::string(json::text(device, "driverId"))?;
            json::push(
                &mut startup,
                (
                    "driver.init",
                    json::fields(&[("driverId", driver.try_clone()?)])?,
                ),
                MAX_STARTUP,
            )?;
            let params = json::fields(&[
                ("driverId", driver),
                ("deviceId", json::string(json::text(device, "id"))?),
            ])?;
            json::push(&mut startup, ("device.init", params), MAX_STARTUP)?;
        }
        startup.reverse();
        self.startup = startup;
        Ok(())
    }

    fn start_next(&mut self, now: u64, outgoing: &mut Vec<Value>) -> Result {
        if let Some((method, params)) = self.startup.pop() {
            self.initializing = true;
            let id = self.pending.begin(0, now, 30_000)?;
            json::push(outgoing, Frame::request(id, method, &params)?, MAX_OUTBOX)?;
        } else {
            self.running = true;
            self.initializing = false;
        }
        Ok(())
    }

    fn reply(
        &mut self,
        frame: Frame,
        now: u64,
        outgoing: &mut Vec<Value>,
    ) -> Result<Option<Completion>> {
        let Some(owner) = self.pending.complete(frame.id) else {
            return Ok(None);
        };
        if owner == 0 {
            if frame.kind == Kind::Error {
                return Err(Error::Invalid("app initialization callback failed"));
            }
            self.start_next(now, outgoing)?;
            return Ok(None);
        }
        if let Some(at) = self.groups.iter().position(|g| g.owner == owner) {
            let mut group = self.groups.remove(at);
            match group.reply(&frame)? {
                groups::Step::Original => (),
                groups::Step::Done(value) => {
                    return Ok(Some(Completion {
                        owner,
                        failed: false,
                        value,
                    }));
                }
                groups::Step::Call(params) => {
                    if now >= group.deadline {
                        return Ok(Some(Completion {
                            owner,
                            failed: true,
                            value: json::fields(&[(
                                "message",
                                json::string("grouped capability callback timed out")?,
                            )])?,
                        }));
                    }
                    let id = self.pending.begin(owner, now, group.deadline - now)?;
                    let packet = Frame::request(id, "capability.invoke", &params);
                    match packet.and_then(|f| json::push(outgoing, f, MAX_OUTBOX)) {
                        Ok(()) => self.groups.push(group),
                        Err(e) => {
                            self.pending.complete(id);
                            return Err(e);
                        }
                    }
                    return Ok(None);
                }
            }
        }
        let value = json::get(
            &frame.value,
            if frame.kind == Kind::Error { "e" } else { "r" },
        )
        .unwrap_or(&Value::Null)
        .try_clone()?;
        Ok(Some(Completion {
            owner,
            failed: frame.kind == Kind::Error,
            value,
        }))
    }

    /// Begint een callback zonder de eigenaar op het appantwoord te laten blokkeren.
    pub fn call(&mut self, owner: u64, method: &str, params: &Value, now: u64) -> Result<Value> {
        if !self.running {
            return Err(Error::Invalid("app is not running"));
        }
        if owner == 0 {
            return Err(Error::Invalid("request owner zero is reserved"));
        }
        let group = if method == "capabilities.invoke" {
            if self.groups.len() >= stulp_protocol::session::MAX_PENDING
                || self.groups.iter().any(|g| g.owner == owner)
            {
                return Err(Error::Full);
            }
            self.groups.try_reserve(1).map_err(|_| Error::Memory)?;
            Some(groups::Group::new(owner, now, params)?)
        } else {
            None
        };
        let id = self
            .pending
            .begin(owner, now, callback_timeout_for(&self.id, method, params))?;
        match Frame::request(id, method, params) {
            Ok(frame) => {
                if let Some(group) = group {
                    self.groups.push(group);
                }
                Ok(frame)
            }
            Err(e) => {
                self.pending.complete(id);
                Err(e)
            }
        }
    }

    /// Reserveert een browseradoptie. De caller verzorgt init en rollback zelf.
    /// Het snapshot moet vóór de initcallback in hetzelfde transport worden gezet.
    pub fn adopt(&mut self, device: &Value) -> Result<Value> {
        let id = json::text(device, "id");
        if !self.running || json::text(device, "appId") != self.id || id.is_empty() {
            return Err(Error::Invalid("device cannot be adopted by this app"));
        }
        let frame = Frame::request(
            0,
            "state.device",
            &json::fields(&[
                ("deviceId", json::string(id)?),
                ("device", device.try_clone()?),
            ])?,
        )?;
        if !self.known.iter().any(|known| known == id) {
            json::push(&mut self.known, json::copy(id)?, 4096)?;
        }
        Ok(frame)
    }

    /// Browsermutaties komen vóór het volgende appverzoek in zijn lokale kopie.
    pub fn follow<S: Storage>(&mut self, store: &Store<S>, now: u64) -> Result<Vec<Value>> {
        let mut outgoing = Vec::new();
        if !self.welcomed {
            return Ok(outgoing);
        }
        for event in store.events_after(self.cursor)? {
            if event.manager == "apps" && event.kind == "app.settings" && event.id == self.id {
                json::push(
                    &mut outgoing,
                    Frame::request(
                        0,
                        "state.settings",
                        &app_field(store, "appSettings", &self.id)?,
                    )?,
                    MAX_OUTBOX,
                )?;
            }
            if event.manager != "devices" {
                continue;
            }
            let device = match store.device(&event.id) {
                Ok(device) if json::text(&device, "appId") == self.id => device,
                Ok(_) => continue,
                Err(Error::Missing(_)) if self.known.iter().any(|id| id == &event.id) => {
                    Value::Null
                }
                Err(Error::Missing(_)) => continue,
                Err(error) => return Err(error),
            };
            let params = json::fields(&[
                ("deviceId", json::string(&event.id)?),
                ("device", device.try_clone()?),
            ])?;
            json::push(
                &mut outgoing,
                Frame::request(0, "state.device", &params)?,
                MAX_OUTBOX,
            )?;
            if device == Value::Null {
                self.known.retain(|id| id != &event.id);
            } else if !self.known.iter().any(|id| id == &event.id) {
                json::push(&mut self.known, json::copy(&event.id)?, 4096)?;
                // De startupstack is omgekeerd; nieuwe adopties komen achter bestaand werk.
                if self.startup.len() + 2 > MAX_STARTUP {
                    return Err(Error::Full);
                }
                self.startup.try_reserve(2).map_err(|_| Error::Memory)?;
                let driver =
                    json::fields(&[("driverId", json::string(json::text(&device, "driverId"))?)])?;
                let mut device_params = driver.try_clone()?;
                json::set(&mut device_params, "deviceId", json::string(&event.id)?)?;
                self.startup.insert(0, ("driver.init", driver));
                self.startup.insert(0, ("device.init", device_params));
            }
        }
        if !self.initializing && !self.startup.is_empty() {
            self.start_next(now, &mut outgoing)?;
        }
        self.cursor = store.sequence();
        Ok(outgoing)
    }

    /// Een frame dat niet in het transport paste houdt geen callbackslot bezet.
    pub fn cancel(&mut self, id: u64) {
        if let Some(owner) = self.pending.complete(id) {
            self.groups.retain(|g| g.owner != owner);
        }
    }

    /// Alle wachtende eigenaren krijgen meteen een fout zodra de verbinding verdwijnt.
    pub fn disconnect(&mut self) -> impl Iterator<Item = u64> + '_ {
        self.groups.clear();
        self.pending.disconnect()
    }

    /// Verlopen callbacks blokkeren geen nieuwe calls en hebben geen late eigenaar meer.
    pub fn expire(&mut self, now: u64) -> Option<u64> {
        let (_, owner) = self.pending.expire(now)?;
        self.groups.retain(|g| g.owner != owner);
        Some(owner)
    }
}

fn app_field<S: Storage>(store: &Store<S>, field: &str, id: &str) -> Result<Value> {
    Ok(
        match json::get(store.document().root(), field).and_then(|v| json::get(v, id)) {
            Some(v) => v.try_clone()?,
            None => json::object(),
        },
    )
}

/// Ontdekking en accountconfiguratie mogen meerdere begrensde netwerkvragen doen.
/// Bediening en lifecycle behouden hun korte termijn; elke wachtende call blijft begrensd.
pub fn callback_timeout(method: &str) -> u64 {
    if method == "video.resolve" {
        return 60_000;
    }
    if matches!(method, "pair.list" | "pair.emit" | "api.invoke") {
        120_000
    } else {
        30_000
    }
}

/// De volledige Sigenergy-scan omvat maximaal 255 × 5 probes van 500 ms.
/// Alleen deze ontdekkingscalls krijgen vijftien minuten; bediening blijft kort.
pub fn callback_timeout_for(app: &str, method: &str, params: &Value) -> u64 {
    if app == "com.stulp.sigenergy"
        && (method == "pair.list"
            || (method == "pair.emit" && json::text(params, "event") == "list_devices")
            || (method == "api.invoke" && json::text(params, "handler") == "test"))
    {
        900_000
    } else {
        callback_timeout(method)
    }
}

/// Duurzame scene-activering en herstel zonder platform-I/O.
pub mod scenes;

pub mod stability;

pub mod calendar;
pub mod schedule;
