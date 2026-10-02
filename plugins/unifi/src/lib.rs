//! UniFi Protect: apparaatbediening en eventstromen hebben één eigenaar.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
#[cfg(test)]
extern crate std;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Plugin, Result, Transport, clone,
    util::{device_arg, field, join, number},
};
mod camera;
mod events;
mod observe;
mod protect;
pub mod rtsp;
mod ws;
struct Device {
    id: String,
    driver: String,
    protect: String,
    output: String,
    next: u64,
    retry: u64,
    motion_off: u64,
}
struct Pair {
    id: String,
    driver: String,
}
/// De consoleconfiguratie, gekoppelde apparaten en twee subscriptions van één app.
pub struct Unifi {
    config: protect::Config,
    devices: Vec<Device>,
    pairs: Vec<Pair>,
    subscriptions: [events::Subscription; 2],
    serial: u64,
    cameras: Vec<camera::Live>,
    error: String,
}
impl Default for Unifi {
    fn default() -> Self {
        Self {
            config: protect::Config::default(),
            devices: Vec::new(),
            pairs: Vec::new(),
            subscriptions: [
                events::Subscription::new("subscribe/devices"),
                events::Subscription::new("subscribe/events"),
            ],
            serial: 0,
            cameras: Vec::new(),
            error: String::new(),
        }
    }
}
impl Unifi {
    fn owned(&self, id: &str) -> Result<&Device> {
        self.devices
            .iter()
            .find(|d| d.id == id)
            .ok_or(Error::Invalid(
                "Dit apparaat hoort niet bij deze Protect-app.",
            ))
    }
    async fn list<T: Transport>(&self, c: &mut Client<T>, driver: &str) -> Result<Value> {
        let cfg = protect::Config::read(field(c.state().root(), "settings"))?;
        let devices = cfg
            .call(c, "GET", protect::resource(driver)?, None, 20_000)
            .await?;
        protect::pairs(driver, &devices)
    }
    async fn pulse<T: Transport>(&self, c: &mut Client<T>, id: &str, ms: f64) -> Result<Value> {
        let d = self.owned(id)?;
        if d.driver != "relay" {
            return Err(Error::Invalid("Dit is geen relaisuitgang."));
        }
        let path = join(&[
            &protect::path("relay", &d.protect)?,
            "/outputs/",
            &stulp_sdk::query(&d.output)?,
            "/activate",
        ])?;
        self.config
            .call(
                c,
                "POST",
                &path,
                Some(&json::fields(&[(
                    "pulseDuration",
                    Value::uint(ms.clamp(100., 10_000.) as u64),
                )])?),
                20_000,
            )
            .await?;
        Ok(Value::Null)
    }
    async fn capability<T: Transport>(&self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let id = json::text(p, "deviceId");
        let d = self.owned(id)?;
        let cap = json::text(p, "capability");
        let value = field(p, "value");
        let flag = || {
            value
                .as_bool()
                .ok_or(Error::Invalid("Deze bediening vraagt true of false."))
        };
        let numeric = || number(value).ok_or(Error::Invalid("Deze bediening vraagt een getal."));
        let settings = field(c.state().device(id)?, "settings");
        let path = protect::path(&d.driver, &d.protect)?;
        let body = match (d.driver.as_str(), cap) {
            ("light", "onoff") => json::fields(&[("isLightForceEnabled", Value::Bool(flag()?))])?,
            ("light", "dim") => json::fields(&[(
                "lightDeviceSettings",
                json::fields(&[(
                    "ledLevel",
                    Value::uint(((numeric()?.clamp(0., 1.) * 6.) as u64 + 1).min(6)),
                )])?,
            )])?,
            ("chime", "onoff" | "volume_set") => {
                let volume = if cap == "volume_set" {
                    (numeric()?.clamp(0., 1.) * 100.) as u64
                } else if flag()? {
                    number(field(field(c.state().device(id)?, "store"), "lastVolume"))
                        .filter(|v| *v > 0.)
                        .unwrap_or(100.)
                        .clamp(0., 100.) as u64
                } else {
                    0
                };
                json::fields(&[("volume", Value::uint(volume))])?
            }
            ("relay", "onoff") => {
                let on = flag()?;
                if json::boolean(settings, "pulse") {
                    let ms = number(field(settings, "pulse_ms"))
                        .filter(|v| *v > 0.)
                        .unwrap_or(1000.);
                    return self.pulse(c, id, ms).await;
                }
                let relay = self.config.call(c, "GET", &path, None, 10_000).await?;
                let mut outputs = Vec::new();
                let mut found = false;
                for output in json::array(&relay, "outputs") {
                    let selected = json::text(output, "id") == d.output;
                    found |= selected;
                    json::push(
                        &mut outputs,
                        json::fields(&[
                            ("id", clone(field(output, "id"))?),
                            (
                                "state",
                                if selected {
                                    json::string(if on { "on" } else { "off" })?
                                } else {
                                    clone(field(output, "state"))?
                                },
                            ),
                        ])?,
                        256,
                    )?;
                }
                if !found {
                    return Err(Error::Invalid(
                        "Deze uitgang bestaat niet meer op het relais.",
                    ));
                }
                json::fields(&[("outputs", Value::Array(outputs))])?
            }
            _ => return Err(Error::Invalid("Onbekende Protect-bediening.")),
        };
        self.config
            .call(c, "PATCH", &path, Some(&body), 10_000)
            .await?;
        if d.driver == "chime" {
            observe::apply(c, id, &body, false).await?;
        }
        Ok(Value::Null)
    }
    async fn api<T: Transport>(&self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        match json::text(p, "handler") {
            "status" => {
                let cfg = protect::Config::read(field(c.state().root(), "settings"))?;
                let error = if self.error.is_empty() {
                    self.subscriptions
                        .iter()
                        .find(|s| !s.error.is_empty())
                        .map(|s| s.error.as_str())
                        .unwrap_or("")
                } else {
                    &self.error
                };
                Ok(json::fields(&[
                    ("host", json::string(&cfg.host)?),
                    ("port", Value::uint(u64::from(cfg.port))),
                    ("hasKey", Value::Bool(!cfg.key.is_empty())),
                    ("connected", Value::Bool(cfg.ready() && error.is_empty())),
                    ("error", json::string(error)?),
                    ("devices", Value::uint(self.devices.len() as u64)),
                ])?)
            }
            "test" => {
                let cfg = protect::Config::read(field(p, "body"))?;
                let deadline = c.now().saturating_add(20_000);
                let consoles = cfg
                    .call(c, "GET", "nvrs", None, deadline.saturating_sub(c.now()))
                    .await?;
                let mut answer = json::object();
                let console = consoles
                    .as_array()
                    .and_then(|v| v.first())
                    .unwrap_or(&consoles);
                json::set(&mut answer, "console", clone(field(console, "name"))?)?;
                for resource in ["cameras", "lights", "sensors", "chimes", "relays"] {
                    let result = cfg
                        .call(c, "GET", resource, None, deadline.saturating_sub(c.now()))
                        .await;
                    let n = match result {
                        Ok(v) => v
                            .as_array()
                            .ok_or(Error::Invalid("Ongeldige Protect-apparatenlijst."))?
                            .len(),
                        Err(e) if resource == "cameras" => return Err(e),
                        Err(_) => 0,
                    };
                    json::set(&mut answer, resource, Value::uint(n as u64))?;
                }
                Ok(answer)
            }
            _ => Err(Error::Invalid("Onbekende Protect-API.")),
        }
    }
    async fn flow<T: Transport>(&self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let args = field(p, "args");
        match json::text(p, "id") {
            "pulse_relay" => {
                self.pulse(
                    c,
                    device_arg(args),
                    number(field(args, "milliseconds"))
                        .filter(|v| *v > 0.)
                        .unwrap_or(1000.),
                )
                .await
            }
            "smart_detection" | "audio_detection" => {
                let wanted = json::text(args, "kind");
                Ok(Value::Bool(
                    wanted.is_empty()
                        || wanted == "any"
                        || wanted == json::text(field(p, "state"), "kind"),
                ))
            }
            "doorbell_pressed" => Ok(Value::Bool(true)),
            _ => Err(Error::Invalid("Onbekende Protect-flowkaart.")),
        }
    }
    async fn message<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        channel: usize,
        p: &Value,
    ) -> Result {
        let item = field(p, "item");
        for d in &mut self.devices {
            if channel == 0 {
                if json::text(item, "id") == d.protect && json::text(item, "modelKey") == d.driver {
                    observe::apply(c, &d.id, item, false).await?;
                }
                continue;
            }
            if d.driver != "camera" || json::text(item, "device") != d.protect {
                continue;
            }
            let kind = json::text(item, "type");
            if kind == "motion" {
                let active = if json::text(p, "type") == "add" {
                    Some(true)
                } else if number(field(item, "end")).is_some_and(|v| v != 0.) {
                    Some(false)
                } else {
                    None
                };
                if let Some(active) = active {
                    c.values(
                        &d.id,
                        json::fields(&[("alarm_motion", Value::Bool(active))])?,
                    )
                    .await?;
                    d.motion_off = if active {
                        c.now().saturating_add(30_000)
                    } else {
                        0
                    };
                }
            } else {
                let card = match kind {
                    "ring" if json::text(p, "type") == "add" => "doorbell_pressed",
                    "smartDetectZone" => "smart_detection",
                    "smartAudioDetect" => "audio_detection",
                    _ => continue,
                };
                if card == "doorbell_pressed" {
                    trigger(c, &d.id, card, json::object(), json::object()).await?;
                } else {
                    for raw in json::array(item, "smartDetectTypes") {
                        let Some(raw) = raw.as_str() else {
                            continue;
                        };
                        let kind = if card == "audio_detection" {
                            observe::audio(raw)
                        } else {
                            raw
                        };
                        let tokens = json::fields(&[("kind", json::string(kind)?)])?;
                        let mut state = clone(&tokens)?;
                        if card == "audio_detection" {
                            json::set(&mut state, "raw", json::string(raw)?)?;
                        }
                        trigger(c, &d.id, card, tokens, state).await?;
                    }
                }
            }
        }
        Ok(())
    }
}
async fn trigger<T: Transport>(
    c: &mut Client<T>,
    device: &str,
    card: &str,
    tokens: Value,
    mut state: Value,
) -> Result {
    json::set(&mut state, "deviceId", json::string(device)?)?;
    c.call(
        "flow.trigger",
        &json::fields(&[
            ("kind", json::string("device-trigger")?),
            ("id", json::string(card)?),
            ("tokens", tokens),
            ("state", state),
        ])?,
    )
    .await?;
    Ok(())
}
impl Plugin for Unifi {
    fn manifest(&self) -> &'static [u8] {
        include_bytes!("../app.json")
    }
    fn assets(&self) -> &'static [&'static str] {
        &["settings/index.html", "settings/page.js"]
    }
    async fn handle<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        p: &Value,
    ) -> Result<Value> {
        match method {
            "app.init" => {
                self.config = protect::Config::read(field(c.state().root(), "settings"))?;
                Ok(Value::Null)
            }
            "driver.init" => {
                protect::resource(json::text(p, "driverId"))?;
                Ok(Value::Null)
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                let driver = json::text(d, "driverId");
                protect::resource(driver)?;
                let data = field(d, "data");
                let protect = json::text(data, "id");
                let output = json::text(data, "output");
                if protect.is_empty() || (driver == "relay" && output.is_empty()) {
                    return Err(Error::Invalid(
                        "Dit apparaat heeft geen Protect-id; koppel opnieuw.",
                    ));
                }
                let entry = Device {
                    id: json::copy(id)?,
                    driver: json::copy(driver)?,
                    protect: json::copy(protect)?,
                    output: json::copy(output)?,
                    next: c.now().saturating_add(2000),
                    retry: 2000,
                    motion_off: 0,
                };
                self.devices.retain(|d| d.id != id);
                let is_camera = entry.driver == "camera";
                json::push(&mut self.devices, entry, 4096)?;
                if is_camera {
                    camera::register(c, id).await?;
                }
                Ok(Value::Null)
            }
            "device.delete" => {
                self.close_cameras(c, Some(json::text(p, "deviceId")))?;
                self.devices.retain(|d| d.id != json::text(p, "deviceId"));
                Ok(Value::Null)
            }
            "video.resolve" => self.video(c, p).await,
            "capability.invoke" => self.capability(c, p).await,
            "capabilities.invoke" => {
                let mut errors = json::object();
                for cmd in json::array(p, "commands") {
                    let mut p2 = clone(cmd)?;
                    json::set(&mut p2, "deviceId", clone(field(p, "deviceId"))?)?;
                    if let Err(e) = self.capability(c, &p2).await {
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
            "api.invoke" => self.api(c, p).await,
            "pair.list" => self.list(c, json::text(p, "driverId")).await,
            "pair.start" => {
                let id = json::text(p, "sessionId");
                let driver = json::text(p, "driverId");
                protect::resource(driver)?;
                if id.is_empty() || self.pairs.iter().any(|s| s.id == id) {
                    return Err(Error::Invalid("invalid pair session"));
                }
                json::push(
                    &mut self.pairs,
                    Pair {
                        id: json::copy(id)?,
                        driver: json::copy(driver)?,
                    },
                    32,
                )?;
                Ok(json::parse(br#"["list_devices"]"#).map_err(stulp_core::Error::from)?)
            }
            "pair.emit" => {
                let pair = self
                    .pairs
                    .iter()
                    .find(|s| s.id == json::text(p, "sessionId"))
                    .ok_or(Error::Invalid("pair session missing"))?;
                if json::text(p, "event") != "list_devices" {
                    return Err(Error::Invalid("unknown pair event"));
                }
                self.list(c, &pair.driver).await
            }
            "pair.close" => {
                self.pairs.retain(|s| s.id != json::text(p, "sessionId"));
                Ok(Value::Null)
            }
            "registrations" => stulp_sdk::registrations(
                &json::parse(self.manifest()).map_err(stulp_core::Error::from)?,
            ),
            "ui.asset" => match json::text(p, "path") {
                "settings/index.html" => stulp_sdk::asset(include_bytes!("../settings/index.html")),
                "settings/page.js" => stulp_sdk::asset(include_bytes!("../settings/page.js")),
                _ => Ok(json::fields(&[("found", Value::Bool(false))])?),
            },
            _ => Err(Error::Invalid("unknown UniFi method")),
        }
    }
    async fn settings<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        patch: &Value,
    ) -> Result<Value> {
        let d = self.owned(id)?;
        if d.driver == "camera"
            && let Some(value) = json::get(patch, "mic_volume")
        {
            let mic = value.as_u64().filter(|n| *n <= 100).ok_or(Error::Invalid(
                "Het microfoonvolume moet een geheel getal van 0 tot 100 zijn.",
            ))?;
            let config = protect::Config::read(field(c.state().root(), "settings"))?;
            config
                .call(
                    c,
                    "PATCH",
                    &protect::path("camera", &d.protect)?,
                    Some(&json::fields(&[("micVolume", Value::uint(mic))])?),
                    20_000,
                )
                .await?;
        }
        Ok(Value::Null)
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.devices.retain(|d| c.state().device(&d.id).is_ok());
        let cfg = protect::Config::read(field(c.state().root(), "settings"))?;
        if cfg != self.config {
            self.close_cameras(c, None)?;
            for s in &mut self.subscriptions {
                s.reset(c)?;
            }
            self.config = cfg;
            self.error.clear();
            for d in &mut self.devices {
                d.next = c.now().saturating_add(2000);
                d.retry = 2000;
            }
        }
        // Eerst complete berichten verwerken, pas daarna nieuwe bytes aannemen.
        for i in 0..2 {
            match self.subscriptions[i].tick(c, &self.config, &mut self.serial) {
                Ok(Some(p)) => self.message(c, i, &p).await?,
                Ok(None) => (),
                Err(e) => self.subscriptions[i].failed(c, e)?,
            }
            if self.subscriptions[i].refresh {
                self.subscriptions[i].refresh = false;
                for d in &mut self.devices {
                    d.next = c.now();
                }
            }
        }
        self.camera_io(c)?;
        for d in &mut self.devices {
            if d.motion_off != 0 && c.now() >= d.motion_off {
                c.values(
                    &d.id,
                    json::fields(&[("alarm_motion", Value::Bool(false))])?,
                )
                .await?;
                d.motion_off = 0;
            }
        }
        let Some(d) = self.devices.iter_mut().find(|d| c.now() >= d.next) else {
            return Ok(());
        };
        match self
            .config
            .call(
                c,
                "GET",
                &protect::path(&d.driver, &d.protect)?,
                None,
                10_000,
            )
            .await
        {
            Ok(item) => {
                observe::apply(c, &d.id, &item, true).await?;
                d.next = u64::MAX;
                d.retry = 2000;
                self.error.clear();
            }
            Err(e) => {
                self.error = stulp_sdk::message(&e)?;
                c.unavailable(&d.id, &self.error).await?;
                d.next = c.now().saturating_add(d.retry);
                d.retry = (d.retry * 2).min(300_000);
            }
        }
        Ok(())
    }
}
#[cfg(test)]
mod tests;
