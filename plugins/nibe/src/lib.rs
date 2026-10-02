//! Nibe/myUplink: de S- en F-serie delen een cloud, niet hun parameternummers.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Plugin, Result, Transport, clone,
    util::{device_arg, field, float, join},
};
/// Verdeling van de meterstand over verwarmen en warm water.
pub mod energy;
mod points;
mod session;
pub use session::code_from_redirect;
use session::{Session, tokens};
struct Pump {
    id: String,
    cloud: String,
    present: Vec<String>,
    allowed: Option<Value>,
    energy: energy::Split,
    next: u64,
    power: u64,
    features: bool,
}
/// Eén sessie-eigenaar serialiseert tokenrotatie, polls en bediening.
#[derive(Default)]
pub struct Nibe {
    session: Session,
    pumps: Vec<Pump>,
    pairs: Vec<String>,
}
impl Nibe {
    fn refresh(&mut self) {
        for p in &mut self.pumps {
            p.next = 0;
            p.power = 0;
            p.features = true;
        }
    }
    async fn systems<T: Transport>(&mut self, c: &mut Client<T>, pair: bool) -> Result<Value> {
        let answer = self
            .session
            .request(c, "GET", "/v2/systems/me", &Value::Null)
            .await?;
        let mut out = Vec::new();
        for system in json::array(&answer, "systems") {
            for device in json::array(system, "devices") {
                let product = field(device, "product");
                let name = json::text(product, "name");
                let item = if pair {
                    if json::text(device, "id").is_empty() {
                        continue;
                    }
                    json::fields(&[
                        (
                            "name",
                            json::string(if name.is_empty() {
                                "Nibe-warmtepomp"
                            } else {
                                name
                            })?,
                        ),
                        (
                            "data",
                            json::fields(&[("id", clone(field(device, "id"))?)])?,
                        ),
                        (
                            "store",
                            json::fields(&[
                                ("systemId", clone(field(system, "systemId"))?),
                                ("system", clone(field(system, "name"))?),
                                ("model", json::string(name)?),
                                ("serial", clone(field(product, "serialNumber"))?),
                            ])?,
                        ),
                    ])?
                } else {
                    json::fields(&[
                        ("system", clone(field(system, "name"))?),
                        ("name", json::string(name)?),
                        ("serial", clone(field(product, "serialNumber"))?),
                        (
                            "connected",
                            Value::Bool(json::text(device, "connectionState") == "Connected"),
                        ),
                    ])?
                };
                json::push(&mut out, item, 4096)?;
            }
        }
        if pair {
            Ok(Value::Array(out))
        } else {
            Ok(json::fields(&[
                ("linked", Value::Bool(true)),
                ("pumps", Value::Array(out)),
            ])?)
        }
    }
    async fn api<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        match json::text(p, "handler") {
            "status" => {
                let mut answer = json::fields(&[
                    (
                        "clientId",
                        clone(c.state().setting("clientId").unwrap_or(&Value::Null))?,
                    ),
                    (
                        "redirectUri",
                        clone(c.state().setting("redirectUri").unwrap_or(&Value::Null))?,
                    ),
                    (
                        "hasSecret",
                        Value::Bool(
                            c.state()
                                .setting("clientSecret")
                                .and_then(Value::as_str)
                                .is_some_and(|s| !s.is_empty()),
                        ),
                    ),
                    ("linked", Value::Bool(!tokens(c).is_null())),
                    ("waiting", Value::Bool(self.session.pending.is_some())),
                    ("devices", Value::uint(self.pumps.len() as u64)),
                    ("error", json::string(&self.session.error)?),
                ])?;
                if !tokens(c).is_null() {
                    json::set(&mut answer, "expiresAt", clone(field(tokens(c), "expiry"))?)?;
                    json::set(
                        &mut answer,
                        "machine",
                        Value::Bool(json::text(tokens(c), "refreshToken").is_empty()),
                    )?;
                }
                Ok(answer)
            }
            "authorize" => self.session.authorize(c),
            "exchange" => {
                self.session
                    .exchange(c, json::text(field(p, "body"), "redirect"))
                    .await?;
                self.refresh();
                self.systems(c, false).await
            }
            "connect" => {
                self.session.connect(c).await?;
                self.refresh();
                self.systems(c, false).await
            }
            "check" => self.systems(c, false).await,
            "disconnect" => {
                c.app_state(Value::Null).await?;
                self.session.pending = None;
                self.session.error.clear();
                Ok(json::fields(&[("linked", Value::Bool(false))])?)
            }
            _ => Err(Error::Invalid("unknown Nibe API handler")),
        }
    }
    async fn write<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        cap: &str,
        value: &Value,
        hours: Option<i64>,
    ) -> Result<Value> {
        let pump = self
            .pumps
            .iter_mut()
            .find(|p| p.id == id)
            .ok_or(Error::Invalid("Deze warmtepomp draait niet in deze app."))?;
        let (parameter, amount) = if let Some(hours) = hours {
            let (p, v) = points::boost(&pump.present, hours)?;
            (p, Value::uint(v))
        } else {
            let point = points::POINTS
                .iter()
                .find(|p| {
                    p.capability == cap && p.writable && pump.present.iter().any(|id| id == p.id)
                })
                .ok_or(Error::Invalid(
                    "Deze bediening is niet beschikbaar voor deze warmtepomp.",
                ))?;
            (point.id, float(points::writable(point, value)?)?)
        };
        let path = join(&["/v2/devices/", &stulp_sdk::query(&pump.cloud)?, "/points"])?;
        self.session
            .request(c, "PATCH", &path, &json::fields(&[(parameter, amount)])?)
            .await?;
        pump.next = c.now();
        pump.power = c.now();
        pump.features = true;
        Ok(Value::Null)
    }
}
impl Plugin for Nibe {
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
                self.session.next = c.now().saturating_add(60_000);
                Ok(Value::Null)
            }
            "driver.init" => {
                if json::text(p, "driverId") == "heatpump" {
                    Ok(Value::Null)
                } else {
                    Err(Error::Invalid("unknown Nibe driver"))
                }
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                let cloud = json::text(field(d, "data"), "id");
                if cloud.is_empty() || json::text(d, "driverId") != "heatpump" {
                    return Err(Error::Invalid(
                        "Dit apparaat heeft geen myUplink-id; koppel het opnieuw.",
                    ));
                }
                let pump = Pump {
                    id: json::copy(id)?,
                    cloud: json::copy(cloud)?,
                    present: Vec::new(),
                    allowed: None,
                    energy: energy::Split::restore(field(d, "store")),
                    next: c.now().saturating_add(1000),
                    power: 0,
                    features: true,
                };
                self.pumps.retain(|p| p.id != id);
                json::push(&mut self.pumps, pump, 4096)?;
                Ok(Value::Null)
            }
            "device.delete" => {
                self.pumps.retain(|d| d.id != json::text(p, "deviceId"));
                Ok(Value::Null)
            }
            "api.invoke" => self.api(c, p).await,
            "capability.invoke" => {
                self.write(
                    c,
                    json::text(p, "deviceId"),
                    json::text(p, "capability"),
                    field(p, "value"),
                    None,
                )
                .await
            }
            "capabilities.invoke" => {
                let mut failed = json::object();
                for cmd in json::array(p, "commands") {
                    let cap = json::text(cmd, "capability");
                    if let Err(e) = self
                        .write(c, json::text(p, "deviceId"), cap, field(cmd, "value"), None)
                        .await
                    {
                        json::set(&mut failed, cap, json::string(&stulp_sdk::message(&e)?)?)?;
                    }
                }
                Ok(failed)
            }
            "flow.run" => {
                if json::text(p, "kind") != "action" || json::text(p, "id") != "boost_hot_water" {
                    return Err(Error::Invalid("unknown Nibe card"));
                }
                let args = field(p, "args");
                let hours = json::text(args, "duration")
                    .trim()
                    .parse()
                    .map_err(|_| Error::Invalid("Onbekende duur voor extra warm water."))?;
                self.write(c, device_arg(args), "", &Value::Null, Some(hours))
                    .await
            }
            "pair.list" => {
                if json::text(p, "driverId") != "heatpump" {
                    return Err(Error::Invalid("unknown Nibe driver"));
                }
                self.systems(c, true).await
            }
            "pair.start" => {
                let id = json::text(p, "sessionId");
                if id.is_empty()
                    || json::text(p, "driverId") != "heatpump"
                    || self.pairs.iter().any(|s| s == id)
                {
                    return Err(Error::Invalid("invalid pairing session"));
                }
                json::push(&mut self.pairs, json::copy(id)?, 32)?;
                json::parse(br#"["list_devices"]"#).map_err(|e| Error::Core(e.into()))
            }
            "pair.emit" => {
                if json::text(p, "event") != "list_devices"
                    || !self.pairs.iter().any(|s| s == json::text(p, "sessionId"))
                {
                    return Err(Error::Invalid("invalid pairing event"));
                }
                self.systems(c, true).await
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
                _ => Ok(json::fields(&[("found", Value::Bool(false))])?),
            },
            _ => Err(Error::Invalid("unknown Nibe method")),
        }
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        if self.session.next <= c.now() {
            self.session.next = c.now().saturating_add(60_000);
            if !tokens(c).is_null() {
                if let Err(e) = self.session.access(c).await {
                    self.session.error = stulp_sdk::message(&e)?;
                }
                return Ok(());
            }
        }
        let Some(pump) = self
            .pumps
            .iter_mut()
            .find(|p| p.next <= c.now() || p.power <= c.now())
        else {
            return Ok(());
        };
        let path = join(&["/v2/devices/", &stulp_sdk::query(&pump.cloud)?])?;
        if pump.next <= c.now() {
            pump.next = c.now().saturating_add(300_000);
            if pump.features {
                pump.features = false;
                if let Ok(device) = self.session.request(c, "GET", &path, &Value::Null).await {
                    pump.allowed = Some(clone(field(&device, "availableFeatures"))?);
                }
            }
            match self
                .session
                .request(c, "GET", &join(&[&path, "/points"])?, &Value::Null)
                .await
            {
                Ok(points) => {
                    pump.apply(c, &points).await?;
                    c.available(&pump.id, true).await?;
                }
                Err(e) => c.unavailable(&pump.id, &stulp_sdk::message(&e)?).await?,
            }
            return Ok(());
        }
        pump.power = c.now().saturating_add(60_000);
        if !pump.meters() {
            return Ok(());
        }
        if let Ok(points) = self
            .session
            .request(
                c,
                "GET",
                &join(&[&path, "/points?parameters=22130%2C14950"])?,
                &Value::Null,
            )
            .await
            && let Some(watt) = points::reading(&points, "22130")
        {
            let priority = points::reading(&points, "14950").map_or(-1, |v| v as i64);
            let (h, w) = pump.energy.power(c.now(), watt, priority);
            c.values(
                &pump.id,
                json::fields(&[
                    ("measure_power", float(watt)?),
                    ("measure_power.heating", float(h)?),
                    ("measure_power.hotwater", float(w)?),
                ])?,
            )
            .await?;
        }
        Ok(())
    }
}
impl Pump {
    fn meters(&self) -> bool {
        self.present.iter().any(|id| id == "22130" || id == "28393")
    }
    async fn apply<T: Transport>(&mut self, c: &mut Client<T>, points: &Value) -> Result {
        let present = points::present(points)?;
        if points::POINTS
            .iter()
            .any(|p| present.iter().any(|id| id == p.id))
        {
            self.present = present;
            let mut backed = json::object();
            for p in points::POINTS {
                let yes = json::boolean(&backed, p.capability)
                    || self.present.iter().any(|id| id == p.id);
                json::set(&mut backed, p.capability, Value::Bool(yes))?;
            }
            for cap in points::ENERGY {
                json::set(&mut backed, cap, Value::Bool(self.meters()))?;
            }
            if let Some(entries) = backed.as_object() {
                for (cap, value) in entries.iter() {
                    let mut desired = value.as_bool().unwrap_or(false);
                    if let (Some(feature), Some(allowed)) =
                        (points::feature(cap), self.allowed.as_ref())
                    {
                        desired &= json::boolean(allowed, feature);
                    }
                    let has = json::array(c.state().device(&self.id)?, "capabilities")
                        .iter()
                        .any(|v| v.as_str() == Some(cap));
                    if has != desired {
                        c.call(
                            if desired {
                                "capability.add"
                            } else {
                                "capability.remove"
                            },
                            &json::fields(&[
                                ("deviceId", json::string(&self.id)?),
                                ("capability", json::string(cap)?),
                            ])?,
                        )
                        .await?;
                    }
                }
            }
        }
        let mut values = json::object();
        let device = c.state().device(&self.id)?;
        for p in points::POINTS {
            if json::array(device, "capabilities")
                .iter()
                .any(|v| v.as_str() == Some(p.capability))
                && let Some(n) = points::reading(points, p.id)
            {
                json::set(&mut values, p.capability, points::capability(p, n)?)?;
            }
        }
        if self.meters()
            && let Some(total) = points::reading(points, "28393")
        {
            let mut candidate = self.energy;
            candidate.anchor(total);
            // Ook de eerste ijking bewaren: een herstart verliest dan geen meterdelta.
            c.store(&self.id, candidate.store()?).await?;
            self.energy = candidate;
            let (h, w) = self.energy.meters();
            for (k, v) in [
                ("meter_power", total),
                ("meter_power.heating", h),
                ("meter_power.hotwater", w),
            ] {
                json::set(&mut values, k, float(v)?)?;
            }
        }
        c.values(&self.id, values).await
    }
}
