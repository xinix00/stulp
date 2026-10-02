//! Somfy TaHoma: privaat account, zeven zonweringsdrivers en scenario's.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, Plugin, Result, Transport, clone};
mod cloud;
/// Asconversie en driverherkenning zonder I/O.
pub mod covering;
use cloud::Cloud;

struct Device {
    id: String,
    url: String,
    driver: String,
}
struct Pair {
    id: String,
    driver: String,
}
/// Eén eigenaar voor het account, de cookiejar en alle pollers.
#[derive(Default)]
pub struct Somfy {
    cloud: Option<Cloud>,
    devices: Vec<Device>,
    pairs: Vec<Pair>,
    latest: Vec<Value>,
    next: u64,
    last_ok: Option<u64>,
    error: String,
}
fn join(parts: &[&str]) -> Result<String> {
    let size = parts.iter().try_fold(0usize, |n, p| {
        n.checked_add(p.len()).ok_or(stulp_core::Error::Full)
    })?;
    let mut s = String::new();
    s.try_reserve(size).map_err(|_| stulp_core::Error::Memory)?;
    for part in parts {
        s.push_str(part);
    }
    Ok(s)
}
fn field<'a>(v: &'a Value, key: &str) -> &'a Value {
    json::get(v, key).unwrap_or(&Value::Null)
}
fn account<T: Transport>(c: &Client<T>) -> &Value {
    field(c.state().root(), "appState")
}
fn interval(a: &Value) -> u64 {
    match json::uint(a, "intervalSeconds") {
        0 => 10,
        n => n,
    }
}
impl Somfy {
    fn connect<T: Transport>(&mut self, c: &Client<T>) -> Result {
        self.cloud = None;
        self.latest.clear();
        self.last_ok = None;
        self.next = c.now().saturating_add(1000);
        match Cloud::new(account(c)) {
            Ok(cloud) => {
                self.cloud = Some(cloud);
                self.error.clear();
            }
            Err(error) => self.error = stulp_sdk::message(&error)?,
        }
        Ok(())
    }
    async fn request<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: &Value,
    ) -> Result<Value> {
        self.cloud
            .as_mut()
            .ok_or(Error::Invalid(
                "Vul eerst het TaHoma-account in op de instellingenpagina.",
            ))?
            .request(c, method, path, body)
            .await
    }
    async fn list<T: Transport>(&mut self, c: &mut Client<T>, driver: &str) -> Result<Value> {
        if !covering::valid(driver) {
            return Err(Error::Invalid("unknown covering driver"));
        }
        let setup = self.request(c, "GET", "/setup", &Value::Null).await?;
        let mut found = Vec::new();
        for device in json::array(&setup, "devices") {
            if covering::driver(json::text(device, "controllableName")) != Some(driver) {
                continue;
            }
            let data = json::fields(&[
                ("deviceURL", clone(field(device, "deviceURL"))?),
                ("oid", clone(field(device, "oid"))?),
                ("label", clone(field(device, "label"))?),
            ])?;
            json::push(
                &mut found,
                json::fields(&[
                    ("name", clone(field(device, "label"))?),
                    ("data", data),
                    (
                        "store",
                        json::fields(&[(
                            "controllableName",
                            clone(field(device, "controllableName"))?,
                        )])?,
                    ),
                ])?,
                4096,
            )?;
        }
        Ok(Value::Array(found))
    }
    async fn capability<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let id = json::text(p, "deviceId");
        let owned = self
            .devices
            .iter()
            .find(|d| d.id == id)
            .ok_or(Error::Invalid("covering is not initialized"))?;
        let device = c.state().device(id)?;
        let data = field(device, "data");
        let label = match json::text(data, "label") {
            "" => json::text(device, "name"),
            s => s,
        };
        let mut parameters = Vec::new();
        let name =
            match json::text(p, "capability") {
                "windowcoverings_set" => {
                    let p = number(field(p, "value")).filter(|n| n.is_finite()).ok_or(
                        Error::Invalid("Een stand moet een getal tussen 0 en 1 zijn."),
                    )?;
                    json::push(&mut parameters, Value::uint(covering::closure(p)), 1)?;
                    "setClosure"
                }
                "windowcoverings_state" => {
                    let state = json::text(p, "value");
                    if state == "idle" {
                        let execution = json::text(field(device, "store"), "executionId");
                        if execution.is_empty() {
                            return Err(Error::Invalid(
                                "Er loopt geen beweging die deze app gestart is.",
                            ));
                        }
                        let path = join(&["/exec/current/setup/", &stulp_sdk::query(execution)?])?;
                        self.request(c, "DELETE", &path, &Value::Null).await?;
                        c.store(id, json::fields(&[("executionId", json::string("")?)])?)
                            .await?;
                        c.values(
                            id,
                            json::fields(&[("windowcoverings_state", json::string("idle")?)])?,
                        )
                        .await?;
                        self.next = c.now();
                        return Ok(Value::Null);
                    }
                    covering::command(&owned.driver, state)?
                }
                _ => return Err(Error::Invalid("unknown covering capability")),
            };
        let body = cloud::execution(label, &owned.url, name, parameters)?;
        let result = self.request(c, "POST", "/exec/apply", &body).await?;
        let execution = json::text(&result, "execId");
        self.next = c.now();
        if execution.is_empty() {
            return Err(Error::Invalid("TaHoma gaf geen uitvoer-id terug."));
        }
        c.store(
            id,
            json::fields(&[("executionId", json::string(execution)?)])?,
        )
        .await?;
        // De opdracht is aangenomen; uitsluitend de poll meldt de bereikte stand.
        Ok(Value::Null)
    }
    async fn api<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let body = field(p, "body");
        match json::text(p, "handler") {
            "status" => {
                let a = account(c);
                let mut answer = json::fields(&[
                    ("username", json::string(json::text(a, "username"))?),
                    (
                        "hasPassword",
                        Value::Bool(!json::text(a, "password").is_empty()),
                    ),
                    ("interval", Value::uint(interval(a))),
                    (
                        "connected",
                        Value::Bool(self.last_ok.is_some() && self.error.is_empty()),
                    ),
                    (
                        "retryAfter",
                        Value::uint(
                            self.cloud
                                .as_ref()
                                .map_or(0, |cloud| cloud.retry_after(c.now())),
                        ),
                    ),
                    ("error", json::string(&self.error)?),
                    ("devices", Value::uint(self.devices.len() as u64)),
                ])?;
                if let Some(last) = self.last_ok {
                    json::set(
                        &mut answer,
                        "secondsAgo",
                        Value::uint(c.now().saturating_sub(last) / 1000),
                    )?;
                }
                Ok(answer)
            }
            "save" => {
                let old = account(c);
                let user = match json::text(body, "username") {
                    "" => json::text(old, "username"),
                    s => s,
                };
                let pass = match json::text(body, "password") {
                    "" => json::text(old, "password"),
                    s => s,
                };
                let seconds = number(field(body, "interval"))
                    .filter(|v| v.is_finite() && *v >= 1.0)
                    .map(|v| v as u64)
                    .unwrap_or(interval(old));
                let saved = json::fields(&[
                    ("username", json::string(user)?),
                    ("password", json::string(pass)?),
                    ("intervalSeconds", Value::uint(seconds)),
                ])?;
                Cloud::new(&saved)?;
                c.app_state(saved).await?;
                self.connect(c)?;
                Ok(json::fields(&[("interval", Value::uint(seconds))])?)
            }
            "forget" => {
                if let Some(cloud) = self.cloud.as_mut() {
                    cloud.logout(c).await;
                }
                c.app_state(json::object()).await?;
                self.connect(c)?;
                Ok(json::fields(&[("ok", Value::Bool(true))])?)
            }
            "test" => {
                if let Some(cloud) = &self.cloud {
                    cloud.ready(c.now())?;
                }
                let password = match json::text(body, "password") {
                    "" => json::text(account(c), "password"),
                    s => s,
                };
                let trial = json::fields(&[
                    ("username", json::string(json::text(body, "username"))?),
                    ("password", json::string(password)?),
                ])?;
                let mut cloud = Cloud::new(&trial)?;
                let result = cloud.request(c, "GET", "/setup", &Value::Null).await;
                if let Some(active) = &mut self.cloud {
                    active.defer_from(&cloud);
                }
                let setup = result?;
                let scenarios = cloud
                    .request(c, "GET", "/actionGroups", &Value::Null)
                    .await
                    .unwrap_or(Value::Null);
                let mut supported = json::object();
                let mut unknown = Vec::new();
                for device in json::array(&setup, "devices") {
                    let name = json::text(device, "controllableName");
                    if let Some(driver) = covering::driver(name) {
                        let count = json::uint(&supported, driver);
                        json::set(&mut supported, driver, Value::uint(count + 1))?;
                    } else if !name.is_empty() && !unknown.iter().any(|s: &String| s == name) {
                        json::push(&mut unknown, json::copy(name)?, 4096)?;
                    }
                }
                unknown.sort_unstable();
                let mut names = Vec::new();
                for s in unknown {
                    json::push(&mut names, json::string(&s)?, 4096)?;
                }
                Ok(json::fields(&[
                    (
                        "devices",
                        Value::uint(json::array(&setup, "devices").len() as u64),
                    ),
                    (
                        "scenarios",
                        Value::uint(scenarios.as_array().map_or(0, |s| s.len()) as u64),
                    ),
                    ("supported", supported),
                    ("unknown", Value::Array(names)),
                ])?)
            }
            _ => Err(Error::Invalid("unknown Somfy API handler")),
        }
    }
    async fn apply<T: Transport>(&self, c: &mut Client<T>, device: &Device) -> Result {
        let existing = c.state().device(&device.id)?;
        if let Some(latest) = self
            .latest
            .iter()
            .find(|v| json::text(v, "deviceURL") == device.url)
        {
            let available = json::boolean(existing, "available");
            let values = covering::values(latest, &device.driver)?;
            let mut changed = json::object();
            if let Some(fields) = values.as_object() {
                for (key, value) in fields.iter() {
                    if !json::equal(value, field(field(existing, "state"), key)) {
                        json::set(&mut changed, key, clone(value)?)?;
                    }
                }
            }
            if changed.as_object().is_some_and(|o| !o.is_empty()) {
                c.values(&device.id, changed).await?;
            }
            if !available {
                c.available(&device.id, true).await?;
            }
        } else if json::boolean(existing, "available") {
            c.unavailable(
                &device.id,
                "TaHoma kent dit apparaat niet meer. Staat het nog in de Somfy-app?",
            )
            .await?;
        }
        Ok(())
    }
}
impl Plugin for Somfy {
    fn assets(&self) -> &'static [&'static str] {
        &["settings/index.html", "settings/page.js"]
    }
    fn manifest(&self) -> &'static [u8] {
        include_bytes!("../app.json")
    }
    async fn handle<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        p: &Value,
    ) -> Result<Value> {
        match method {
            "app.init" => {
                self.connect(c)?;
                Ok(Value::Null)
            }
            "driver.init" => {
                if covering::valid(json::text(p, "driverId")) {
                    Ok(Value::Null)
                } else {
                    Err(Error::Invalid("unknown covering driver"))
                }
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                let driver = json::text(d, "driverId");
                let url = json::text(field(d, "data"), "deviceURL");
                if !covering::valid(driver) || url.is_empty() {
                    return Err(Error::Invalid(
                        "Dit apparaat heeft geen TaHoma-adres; koppel het opnieuw.",
                    ));
                }
                let device = Device {
                    id: json::copy(id)?,
                    url: json::copy(url)?,
                    driver: json::copy(driver)?,
                };
                if self.last_ok.is_some() {
                    self.apply(c, &device).await?;
                }
                self.devices.retain(|d| d.id != id);
                json::push(&mut self.devices, device, 4096)?;
                self.next = c.now().saturating_add(1000);
                Ok(Value::Null)
            }
            "device.delete" => {
                self.devices.retain(|d| d.id != json::text(p, "deviceId"));
                Ok(Value::Null)
            }
            "capability.invoke" => self.capability(c, p).await,
            "capabilities.invoke" => {
                let mut failed = json::object();
                for command in json::array(p, "commands") {
                    let mut request = clone(command)?;
                    json::set(&mut request, "deviceId", clone(field(p, "deviceId"))?)?;
                    if let Err(e) = self.capability(c, &request).await {
                        json::set(
                            &mut failed,
                            json::text(command, "capability"),
                            json::string(&stulp_sdk::message(&e)?)?,
                        )?;
                    }
                }
                Ok(failed)
            }
            "api.invoke" => self.api(c, p).await,
            "pair.list" => self.list(c, json::text(p, "driverId")).await,
            "pair.start" => {
                let id = json::text(p, "sessionId");
                let driver = json::text(p, "driverId");
                if id.is_empty()
                    || !covering::valid(driver)
                    || self.pairs.iter().any(|v| v.id == id)
                {
                    return Err(Error::Invalid("invalid pairing session"));
                }
                json::push(
                    &mut self.pairs,
                    Pair {
                        id: json::copy(id)?,
                        driver: json::copy(driver)?,
                    },
                    32,
                )?;
                json::parse(br#"["list_devices"]"#).map_err(|e| Error::Core(e.into()))
            }
            "pair.emit" => {
                if json::text(p, "event") != "list_devices" {
                    return Err(Error::Invalid("unknown pair event"));
                }
                let pair = self
                    .pairs
                    .iter()
                    .find(|v| v.id == json::text(p, "sessionId"))
                    .ok_or(Error::Invalid("pair session missing"))?;
                let driver = json::copy(&pair.driver)?;
                self.list(c, &driver).await
            }
            "pair.close" => {
                self.pairs.retain(|v| v.id != json::text(p, "sessionId"));
                Ok(Value::Null)
            }
            "flow.run" => {
                if json::text(p, "kind") != "action" || json::text(p, "id") != "activate_scenario" {
                    return Err(Error::Invalid("unknown Somfy card"));
                }
                let v = field(field(p, "args"), "scenario");
                let id = v.as_str().unwrap_or_else(|| json::text(v, "id"));
                if id.is_empty() {
                    return Err(Error::Invalid("Deze kaart heeft geen scenario gekozen."));
                }
                self.request(
                    c,
                    "POST",
                    &join(&["/exec/", &stulp_sdk::query(id)?])?,
                    &Value::Null,
                )
                .await?;
                self.next = c.now();
                Ok(Value::Null)
            }
            "flow.autocomplete" => {
                if json::text(p, "kind") != "action"
                    || json::text(p, "id") != "activate_scenario"
                    || json::text(p, "argument") != "scenario"
                {
                    return Err(Error::Invalid("unknown autocomplete"));
                }
                let scenarios = self
                    .request(c, "GET", "/actionGroups", &Value::Null)
                    .await?;
                let query = lower(json::text(p, "query").trim())?;
                let mut items = Vec::new();
                for s in scenarios.as_array().unwrap_or(&[]) {
                    if lower(json::text(s, "label"))?.contains(&query) {
                        json::push(
                            &mut items,
                            json::fields(&[
                                ("id", clone(field(s, "oid"))?),
                                ("name", clone(field(s, "label"))?),
                            ])?,
                            4096,
                        )?;
                    }
                }
                Ok(Value::Array(items))
            }
            "registrations" => {
                let mut registrations = stulp_sdk::registrations(
                    &json::parse(self.manifest()).map_err(stulp_core::Error::from)?,
                )?;
                let mut cards = Vec::new();
                for card in json::array(&registrations, "flows") {
                    let mut card = clone(card)?;
                    json::set(
                        &mut card,
                        "autocomplete",
                        json::parse(br#"["scenario"]"#).map_err(stulp_core::Error::from)?,
                    )?;
                    json::push(&mut cards, card, 1)?;
                }
                json::set(&mut registrations, "flows", Value::Array(cards))?;
                Ok(registrations)
            }
            "ui.asset" => match json::text(p, "path") {
                "settings/index.html" => stulp_sdk::asset(include_bytes!("../settings/index.html")),
                "settings/page.js" => stulp_sdk::asset(include_bytes!("../settings/page.js")),
                _ => Ok(json::fields(&[("found", Value::Bool(false))])?),
            },
            _ => Err(Error::Invalid("unknown Somfy method")),
        }
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        if self.cloud.is_none() || c.now() < self.next {
            return Ok(());
        }
        self.next = c
            .now()
            .saturating_add(interval(account(c)).saturating_mul(1000));
        match self.request(c, "GET", "/setup", &Value::Null).await {
            Ok(setup) => {
                let Some(devices) = json::get(&setup, "devices").and_then(Value::as_array) else {
                    self.error = json::copy("TaHoma stuurde een ongeldige apparatenlijst.")?;
                    return Ok(());
                };
                let mut latest = Vec::new();
                for device in devices {
                    json::push(&mut latest, clone(device)?, 4096)?;
                }
                self.latest = latest;
                self.last_ok = Some(c.now());
                self.error.clear();
                for device in &self.devices {
                    self.apply(c, device).await?;
                }
            }
            Err(error) => {
                let message = stulp_sdk::message(&error)?;
                if message != self.error {
                    c.log("warn", &message)?;
                    for device in &self.devices {
                        c.unavailable(&device.id, &message).await?;
                    }
                }
                self.error = message;
            }
        }
        Ok(())
    }
}
fn lower(s: &str) -> Result<String> {
    let mut out = String::new();
    out.try_reserve(s.len().checked_mul(3).ok_or(stulp_core::Error::Full)?)
        .map_err(|_| stulp_core::Error::Memory)?;
    for c in s.chars() {
        for lower in c.to_lowercase() {
            out.push(lower);
        }
    }
    Ok(out)
}

fn number(v: &Value) -> Option<f64> {
    match v {
        Value::Number(n) => Some(n.as_f64()),
        _ => None,
    }
}
