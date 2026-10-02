//! WiiM-spelers: lokale ontdekking, UPnP-waarnemingen en expliciete bediening.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Plugin, Result, Transport, clone,
    util::{device_arg, field, join, number},
};
mod wire;
struct Player {
    id: String,
    address: String,
    port: u16,
    control: String,
    next: u64,
    failures: u64,
    error: String,
    last_ok: Option<u64>,
}
/// Eén eigenaar voor de spelers, hun pollers en de gevonden koppelkandidaten.
#[derive(Default)]
pub struct Wiim {
    players: Vec<Player>,
    found: Vec<wire::Description>,
    pairs: Vec<String>,
}
fn decimal(n: u64) -> Result<String> {
    json::to_string(&Value::uint(n)).map_err(|e| Error::Core(e.into()))
}
impl Wiim {
    async fn search<T: Transport>(&mut self, c: &mut Client<T>) -> Result<Value> {
        let deadline = c.now().saturating_add(12_000);
        let datagrams=c.datagrams(stulp_sdk::DatagramRequest{target:stulp_sdk::DatagramTarget::Ssdp,payload:json::copy("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n")?.into_bytes(),timeout_ms:3000}).await?;
        let mut locations: Vec<&str> = Vec::new();
        for d in &datagrams {
            if let Some(url) = wire::location(&d.payload)
                && !locations.contains(&url)
            {
                json::push(&mut locations, url, 256)?;
            }
        }
        let mut found = Vec::new();
        for url in locations {
            let remaining = deadline.saturating_sub(c.now());
            if remaining == 0 {
                break;
            }
            if let Ok(d) = wire::describe(c, url, remaining.min(2000)).await
                && d.player
            {
                json::push(&mut found, d, 256)?;
            }
        }
        found.sort_unstable_by(|a, b| a.address.cmp(&b.address));
        let mut unique: Vec<wire::Description> = Vec::new();
        for d in found {
            if !unique.iter().any(|v| v.uuid == d.uuid) {
                json::push(&mut unique, d, 256)?;
            }
        }
        unique.sort_unstable_by(|a, b| a.name.cmp(&b.name));
        self.found = unique;
        self.answer()
    }
    async fn manual<T: Transport>(&mut self, c: &mut Client<T>, address: &str) -> Result<Value> {
        let address = address.trim();
        wire::address(address)?;
        let mut d = wire::describe(c, &wire::description_url(address, 49152)?, 12_000).await?;
        if !d.player {
            return Err(Error::Invalid("Op dit adres staat geen WiiM-speler."));
        }
        d.address = json::copy(address)?;
        let mut found = Vec::new();
        json::push(&mut found, d, 1)?;
        self.found = found;
        self.answer()
    }
    fn answer(&self) -> Result<Value> {
        let mut list = Vec::new();
        for d in &self.found {
            json::push(&mut list, d.summary()?, 256)?;
        }
        Ok(json::fields(&[
            ("found", Value::uint(list.len() as u64)),
            ("players", Value::Array(list)),
        ])?)
    }
    fn paired(&self) -> Result<Value> {
        if self.found.is_empty() {
            return Err(Error::Invalid(
                "Zoek eerst een speler of voeg hem toe op adres.",
            ));
        }
        let mut list = Vec::new();
        for d in &self.found {
            json::push(&mut list, d.paired()?, 256)?;
        }
        Ok(Value::Array(list))
    }
    fn report<T: Transport>(&self, c: &Client<T>) -> Result<Value> {
        let mut players = Vec::new();
        for p in &self.players {
            let d = c.state().device(&p.id)?;
            let mut report = json::fields(&[
                ("name", clone(field(d, "name"))?),
                ("uuid", clone(field(field(d, "data"), "id"))?),
                ("address", clone(field(field(d, "settings"), "address"))?),
                (
                    "answers",
                    Value::Bool(p.failures < 3 && p.last_ok.is_some()),
                ),
                ("failures", Value::uint(p.failures)),
            ])?;
            if !p.error.is_empty() {
                json::set(&mut report, "error", json::string(&p.error)?)?;
            }
            if let Some(last) = p.last_ok {
                json::set(
                    &mut report,
                    "secondsAgo",
                    Value::uint(c.now().saturating_sub(last) / 1000),
                )?;
            }
            json::push(&mut players, report, 4096)?;
        }
        Ok(json::fields(&[("players", Value::Array(players))])?)
    }
    async fn send<T: Transport>(&self, c: &mut Client<T>, id: &str, cmd: &str) -> Result<Value> {
        if !self.players.iter().any(|p| p.id == id) {
            return Err(Error::Invalid("Deze speler draait niet."));
        }
        let address = json::copy(json::text(
            field(c.state().device(id)?, "settings"),
            "address",
        ))?;
        wire::command(c, &address, cmd).await?;
        Ok(Value::Null)
    }
    async fn capability<T: Transport>(&self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let id = json::text(p, "deviceId");
        let cap = json::text(p, "capability");
        let value = field(p, "value");
        let state = field(c.state().device(id)?, "state");
        let flag = || {
            value
                .as_bool()
                .ok_or(Error::Invalid("Deze bediening vraagt true of false."))
        };
        let cmd = match cap {
            "speaker_playing" => json::copy(if flag()? {
                "setPlayerCmd:resume"
            } else {
                "setPlayerCmd:pause"
            })?,
            "speaker_next" => json::copy("setPlayerCmd:next")?,
            "speaker_prev" => json::copy("setPlayerCmd:prev")?,
            "button.off" => json::copy("setPlayerCmd:stop")?,
            "volume_mute" => join(&["setPlayerCmd:mute:", if flag()? { "1" } else { "0" }])?,
            "volume_set" => {
                let n = number(value).ok_or(Error::Invalid("Volume moet een getal zijn."))?;
                join(&[
                    "setPlayerCmd:vol:",
                    &decimal((n.clamp(0., 1.) * 100. + 0.5) as u64)?,
                ])?
            }
            "speaker_shuffle" | "speaker_repeat" => {
                let shuffle = if cap == "speaker_shuffle" {
                    flag()?
                } else {
                    field(state, "speaker_shuffle")
                        .as_bool()
                        .ok_or(Error::Invalid(
                            "De speler heeft zijn shuffle-stand nog niet gemeld.",
                        ))?
                };
                let repeat = if cap == "speaker_repeat" {
                    value.as_str()
                } else {
                    field(state, "speaker_repeat").as_str()
                }
                .ok_or(Error::Invalid(
                    "De speler heeft zijn herhaalstand nog niet gemeld.",
                ))?;
                join(&[
                    "setPlayerCmd:loopmode:",
                    &wire::encode_loop(shuffle, repeat)?,
                ])?
            }
            "button.preset1" | "button.preset2" | "button.preset3" | "button.preset4" => {
                join(&["MCUKeyShortClick:", &cap[cap.len() - 1..]])?
            }
            _ => return Err(Error::Invalid("Onbekende WiiM-bediening.")),
        };
        self.send(c, id, &cmd).await
    }
    async fn flow<T: Transport>(&self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let args = field(p, "args");
        let id = device_arg(args);
        let cmd = match json::text(p, "id") {
            "switch_off" => json::copy("setPlayerCmd:stop")?,
            "call_preset" => {
                let v = field(args, "preset_number");
                let n = v
                    .as_str()
                    .and_then(|s| s.parse::<u64>().ok())
                    .or_else(|| {
                        number(v)
                            .filter(|n| *n >= 1. && *n <= 12.)
                            .map(|n| n as u64)
                    })
                    .filter(|n| (1..=12).contains(n))
                    .ok_or(Error::Invalid("Kies voorkeur 1 tot en met 12."))?;
                join(&["MCUKeyShortClick:", &decimal(n)?])?
            }
            _ => return Err(Error::Invalid("Onbekende WiiM-flowkaart.")),
        };
        self.send(c, id, &cmd).await
    }
}
impl Plugin for Wiim {
    fn manifest(&self) -> &'static [u8] {
        include_bytes!("../app.json")
    }
    fn assets(&self) -> &'static [&'static str] {
        &[
            "settings/index.html",
            "settings/page.js",
            "drivers/player/pair/search.html",
        ]
    }
    async fn handle<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        p: &Value,
    ) -> Result<Value> {
        match method {
            "app.init" => Ok(Value::Null),
            "driver.init" => {
                if json::text(p, "driverId") != "player" {
                    return Err(Error::Invalid("unknown WiiM driver"));
                }
                Ok(Value::Null)
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                if json::text(d, "driverId") != "player"
                    || json::text(field(d, "data"), "id").is_empty()
                {
                    return Err(Error::Invalid(
                        "Dit apparaat heeft geen WiiM-uuid; koppel opnieuw.",
                    ));
                }
                self.players.retain(|p| p.id != id);
                json::push(
                    &mut self.players,
                    Player {
                        id: json::copy(id)?,
                        address: String::new(),
                        port: 49152,
                        control: String::new(),
                        next: c.now().saturating_add(100),
                        failures: 0,
                        error: String::new(),
                        last_ok: None,
                    },
                    4096,
                )?;
                Ok(Value::Null)
            }
            "device.delete" => {
                self.players.retain(|v| v.id != json::text(p, "deviceId"));
                Ok(Value::Null)
            }
            "capability.invoke" => self.capability(c, p).await,
            "capabilities.invoke" => {
                let mut errors = json::object();
                for cmd in json::array(p, "commands") {
                    let mut params = clone(cmd)?;
                    json::set(&mut params, "deviceId", clone(field(p, "deviceId"))?)?;
                    if let Err(e) = self.capability(c, &params).await {
                        json::set(
                            &mut errors,
                            json::text(cmd, "capability"),
                            json::string(&stulp_sdk::message(&e)?)?,
                        )?;
                    }
                }
                Ok(errors)
            }
            "flow.run" => self.flow(c, p).await,
            "api.invoke" => match json::text(p, "handler") {
                "status" => self.report(c),
                "search" => self.search(c).await,
                _ => Err(Error::Invalid("unknown WiiM API")),
            },
            "pair.start" => {
                let id = json::text(p, "sessionId");
                if id.is_empty()
                    || json::text(p, "driverId") != "player"
                    || self.pairs.iter().any(|v| v == id)
                {
                    return Err(Error::Invalid("invalid pairing session"));
                }
                json::push(&mut self.pairs, json::copy(id)?, 32)?;
                json::parse(br#"["search","manual","list_devices"]"#)
                    .map_err(|e| Error::Core(e.into()))
            }
            "pair.list" => {
                if json::text(p, "driverId") != "player" {
                    return Err(Error::Invalid("unknown WiiM driver"));
                }
                self.paired()
            }
            "pair.emit" => {
                if !self.pairs.iter().any(|s| s == json::text(p, "sessionId")) {
                    return Err(Error::Invalid("pair session missing"));
                }
                match json::text(p, "event") {
                    "list_devices" => self.paired(),
                    "search" => self.search(c).await,
                    "manual" => {
                        self.manual(c, json::text(field(p, "data"), "address"))
                            .await
                    }
                    _ => Err(Error::Invalid("unknown WiiM pair event")),
                }
            }
            "pair.close" => {
                self.pairs.retain(|s| s != json::text(p, "sessionId"));
                Ok(Value::Null)
            }
            "registrations" => stulp_sdk::registrations(
                &json::parse(self.manifest()).map_err(stulp_core::Error::from)?,
            ),
            "ui.asset" => match json::text(p, "path") {
                "settings/index.html" => stulp_sdk::asset(include_bytes!("../settings/index.html")),
                "settings/page.js" => stulp_sdk::asset(include_bytes!("../settings/page.js")),
                "drivers/player/pair/search.html" => {
                    stulp_sdk::asset(include_bytes!("../drivers/player/pair/search.html"))
                }
                _ => Ok(json::fields(&[("found", Value::Bool(false))])?),
            },
            _ => Err(Error::Invalid("unknown WiiM method")),
        }
    }
    async fn settings<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        patch: &Value,
    ) -> Result<Value> {
        c.state().device(id)?;
        if let Some(address) = json::get(patch, "address") {
            wire::address(
                address
                    .as_str()
                    .ok_or(Error::Invalid("Het adres moet tekst zijn."))?,
            )?;
            if let Some(player) = self.players.iter_mut().find(|p| p.id == id) {
                player.next = 0;
            }
        }
        Ok(Value::Null)
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.players.retain(|p| c.state().device(&p.id).is_ok());
        let Some(i) = self.players.iter().position(|p| p.next <= c.now()) else {
            return Ok(());
        };
        let p = &mut self.players[i];
        let d = c.state().device(&p.id)?;
        let address = json::text(field(d, "settings"), "address");
        let port = u16::try_from(json::uint(field(d, "store"), "port"))
            .ok()
            .filter(|p| *p > 0)
            .unwrap_or(49152);
        if p.address != address || p.port != port {
            p.address = json::copy(address)?;
            p.port = port;
            p.control.clear();
            p.failures = 0;
            p.last_ok = None;
        }
        p.next = c.now().saturating_add(5000);
        if let Err(e) = wire::address(&p.address) {
            p.error = stulp_sdk::message(&e)?;
            c.unavailable(&p.id, &p.error).await?;
            return Ok(());
        }
        let deadline = c.now().saturating_add(4000);
        let result = async {
            if p.control.is_empty() {
                let d =
                    wire::describe(c, &wire::description_url(&p.address, p.port)?, 4000).await?;
                if d.control.is_empty() {
                    return Err(Error::Invalid("De speler biedt geen AVTransport aan."));
                }
                p.control = d.control;
            }
            let left = deadline.saturating_sub(c.now());
            if left == 0 {
                return Err(Error::Timeout);
            }
            wire::status(c, &p.control, left).await
        }
        .await;
        match result {
            Ok(values) => {
                let state = field(c.state().device(&p.id)?, "state");
                let mut changed = json::object();
                if let Some(values) = values.as_object() {
                    for (k, v) in values.iter() {
                        if !json::equal(v, field(state, k)) {
                            json::set(&mut changed, k, clone(v)?)?;
                        }
                    }
                }
                if changed.as_object().is_some_and(|o| !o.is_empty()) {
                    c.values(&p.id, changed).await?;
                }
                if !json::boolean(c.state().device(&p.id)?, "available") {
                    c.available(&p.id, true).await?;
                }
                p.failures = 0;
                p.error.clear();
                p.last_ok = Some(c.now());
            }
            Err(e) => {
                p.control.clear();
                p.failures = p.failures.saturating_add(1);
                p.error = stulp_sdk::message(&e)?;
                if p.failures == 3 {
                    c.unavailable(&p.id, &p.error).await?;
                }
            }
        }
        Ok(())
    }
}
#[cfg(test)]
mod tests;
