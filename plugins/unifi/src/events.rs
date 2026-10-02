//! Twee afzonderlijke subscriptions; iedere nieuwe verbinding vraagt een snapshot.
use crate::{
    protect::{BASE, Config},
    ws,
};
use alloc::string::String;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, StreamCommand as Command, StreamEvent as Event, Transport, util::join,
};
pub(super) struct Subscription {
    pub id: u64,
    decoder: Option<ws::Decoder>,
    path: &'static str,
    next: u64,
    backoff: u64,
    deadline: u64,
    ping: u64,
    ready: bool,
    pub refresh: bool,
    pub error: String,
}
impl Subscription {
    pub(super) fn new(path: &'static str) -> Self {
        Self {
            id: 0,
            decoder: None,
            path,
            next: 0,
            backoff: 1000,
            deadline: 0,
            ping: 0,
            ready: false,
            refresh: false,
            error: String::new(),
        }
    }
    pub(super) fn close<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        if self.id != 0 {
            c.stream(Command::Close { id: self.id })?;
        }
        self.id = 0;
        self.decoder = None;
        self.ready = false;
        self.deadline = 0;
        Ok(())
    }
    pub(super) fn reset<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.close(c)?;
        self.next = c.now().saturating_add(2000);
        self.backoff = 1000;
        self.error.clear();
        Ok(())
    }
    pub(super) fn failed<T: Transport>(&mut self, c: &mut Client<T>, error: Error) -> Result {
        self.close(c)?;
        self.error = stulp_sdk::message(&error)?;
        self.next = c.now().saturating_add(self.backoff);
        self.backoff = (self.backoff * 2).min(60_000);
        Ok(())
    }
    fn control<T: Transport>(&self, c: &mut Client<T>, op: u8, body: &[u8]) -> Result {
        let random = c.random()?;
        c.stream(Command::Write {
            id: self.id,
            bytes: ws::control(op, body, [random[0], random[1], random[2], random[3]])?,
        })
    }
    pub(super) fn tick<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        cfg: &Config,
        serial: &mut u64,
    ) -> Result<Option<Value>> {
        if !cfg.ready() {
            return Ok(None);
        }
        if self.id == 0 && c.now() >= self.next {
            *serial = serial
                .checked_add(1)
                .ok_or(Error::Invalid("stream ids exhausted"))?;
            c.stream(Command::Open {
                id: *serial,
                host: json::copy(cfg.host.trim_matches(['[', ']']))?,
                port: cfg.port,
                tls: true,
                device_certificate: true,
            })?;
            self.id = *serial;
            self.deadline = c.now().saturating_add(35_000);
        }
        if self.deadline != 0 && c.now() >= self.deadline {
            return Err(Error::Timeout);
        }
        if self.ready && c.now() >= self.ping {
            self.control(c, 9, b"stulp")?;
            self.ping = c.now().saturating_add(30_000);
            self.deadline = c.now().saturating_add(15_000);
        }
        let message = match self.decoder.as_mut() {
            Some(d) => d.next()?,
            None => None,
        };
        match message {
            Some(ws::Message::Open) => {
                self.ready = true;
                self.refresh = true;
                self.error.clear();
                self.backoff = 1000;
                self.deadline = 0;
                self.ping = c.now().saturating_add(30_000);
            }
            Some(ws::Message::Text(bytes)) => {
                return json::parse(&bytes)
                    .map(Some)
                    .map_err(|e| Error::Core(e.into()));
            }
            Some(ws::Message::Ping(bytes)) => self.control(c, 10, &bytes)?,
            Some(ws::Message::Pong) => self.deadline = 0,
            Some(ws::Message::Close(bytes)) => {
                self.control(c, 8, &bytes)?;
                return Err(Error::Transport("console closed websocket"));
            }
            None => (),
        }
        Ok(None)
    }
    pub(super) fn event<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        cfg: &Config,
        event: Event,
    ) -> Result {
        match event {
            Event::Opened(_) => {
                let random = c.random()?;
                let (expected, bytes) = ws::request(
                    &cfg.authority()?,
                    &join(&[BASE, self.path])?,
                    &cfg.key,
                    &random[..16],
                )?;
                self.decoder = Some(ws::Decoder::new(expected));
                c.stream(Command::Write { id: self.id, bytes })?;
            }
            Event::Data(_, bytes) => self
                .decoder
                .as_mut()
                .ok_or(Error::Invalid("websocket data before open"))?
                .feed(&bytes)?,
            Event::Closed(_, e) => return Err(e),
        }
        Ok(())
    }
}
// De eigenaar neemt de buffer over zonder de payload te klonen.
pub(super) fn id(event: &Event) -> u64 {
    match event {
        Event::Opened(id) | Event::Data(id, _) | Event::Closed(id, _) => *id,
    }
}
