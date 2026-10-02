//! Sigenergy heeft één lokale Modbus-verbinding en een onafhankelijke mySigen-koppeling.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
#[cfg(test)]
extern crate std;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, Plugin, Result, Transport, clone, util::field};
mod cloud;
mod devices;
mod gateway;
mod map;
mod modbus;
mod register;
mod scan;
use devices::Meter;
use gateway::Gateway;
struct Pair {
    id: String,
    driver: String,
}
/// Pluginstaat en beide protocolroutes hebben één eigenaar.
#[derive(Default)]
pub struct Sigenergy {
    bus: modbus::Modbus,
    cloud: cloud::Cloud,
    meters: Vec<Meter>,
    gateways: Vec<Gateway>,
    pairs: Vec<Pair>,
    address: String,
    error: String,
    timeout: u64,
}
fn decimal(n: u64) -> Result<String> {
    json::to_string(&Value::uint(n)).map_err(|e| Error::Core(e.into()))
}
fn setting_number<T: Transport>(c: &Client<T>, key: &str, default: u64) -> u64 {
    c.state()
        .setting(key)
        .and_then(Value::as_u64)
        .filter(|v| *v > 0)
        .unwrap_or(default)
}
fn configured_address<T: Transport>(c: &Client<T>) -> Result<String> {
    let host = c
        .state()
        .setting("host")
        .and_then(Value::as_str)
        .unwrap_or("");
    let port = u16::try_from(setting_number(c, "port", 502))
        .map_err(|_| Error::Invalid("Sigenergy-poort moet 1..65535 zijn."))?;
    modbus::address(host, port)
}
impl Sigenergy {
    fn sync_local<T: Transport>(&mut self, c: &Client<T>) -> Result {
        let address = configured_address(c)?;
        let timeout = setting_number(c, "timeout", 5).clamp(1, 60) * 1000;
        if self.address != address || self.timeout != timeout {
            self.bus.reset();
            self.address = address;
            self.timeout = timeout;
            self.error.clear();
            for m in &mut self.meters {
                m.next = 0;
            }
        }
        Ok(())
    }
    async fn pair<T: Transport>(&mut self, c: &mut Client<T>, driver: &str) -> Result<Value> {
        if driver == "gateway" {
            let stations = gateway::stations(c, &mut self.cloud).await?;
            return gateway::describe(c, &mut self.cloud, &stations, true).await;
        }
        self.sync_local(c)?;
        scan::pair(c, &mut self.bus, &self.address, driver).await
    }
    async fn api<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        match json::text(p, "handler") {
            "status" => {
                let local = configured_address(c);
                let mut v = self.cloud.status()?;
                for (k, d) in [("port", 502), ("interval", 10), ("timeout", 5)] {
                    json::set(&mut v, k, Value::uint(setting_number(c, k, d)))?;
                }
                for k in ["host", "chargerUnit"] {
                    json::set(
                        &mut v,
                        k,
                        json::string(c.state().setting(k).and_then(Value::as_str).unwrap_or(""))?,
                    )?;
                }
                json::set(
                    &mut v,
                    "units",
                    json::string(
                        c.state()
                            .setting("units")
                            .and_then(Value::as_str)
                            .filter(|s| !s.is_empty())
                            .unwrap_or("1-32,247"),
                    )?,
                )?;
                json::set(
                    &mut v,
                    "connected",
                    Value::Bool(local.is_ok() && self.error.is_empty()),
                )?;
                let error = match local {
                    Err(e) => stulp_sdk::message(&e)?,
                    _ => json::copy(&self.error)?,
                };
                json::set(&mut v, "error", Value::String(error))?;
                json::set(&mut v, "devices", Value::uint(self.meters.len() as u64))?;
                Ok(v)
            }
            "test" => {
                let result = scan::test(c, &mut self.bus, field(p, "body")).await;
                self.bus.reset();
                result
            }
            "cloud_connect" => {
                let stations = self.cloud.connect(c, field(p, "body")).await?;
                for g in &mut self.gateways {
                    g.cancel();
                }
                gateway::describe(c, &mut self.cloud, &stations, false).await
            }
            "cloud_check" => {
                let stations = gateway::stations(c, &mut self.cloud).await?;
                gateway::describe(c, &mut self.cloud, &stations, false).await
            }
            "cloud_disconnect" => {
                let result = self.cloud.disconnect(c).await;
                for g in &mut self.gateways {
                    g.cancel();
                }
                result?;
                Ok(json::fields(&[("linked", Value::Bool(false))])?)
            }
            _ => Err(Error::Invalid("unknown Sigenergy API")),
        }
    }
    async fn capability<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let id = json::text(p, "deviceId");
        let cap = json::text(p, "capability");
        let on = field(p, "value")
            .as_bool()
            .ok_or(Error::Invalid("Deze bediening verwacht aan of uit."))?;
        if let Some(g) = self.gateways.iter_mut().find(|g| g.id == id) {
            if cap != "off_grid" {
                return Err(Error::Invalid("unknown Gateway capability"));
            }
            g.prepare(c, &mut self.cloud, on).await?;
            return Ok(Value::Null);
        }
        if cap != "evcharger_charging"
            || !self
                .meters
                .iter()
                .any(|m| m.id == id && m.driver == "evaccharger")
        {
            return Err(Error::Invalid("unknown Sigenergy capability"));
        }
        let unit = devices::unit(c.state().device(id)?)?;
        self.sync_local(c)?;
        self.bus
            .write_single(
                c,
                &self.address,
                self.timeout,
                unit,
                42000,
                if on { 0 } else { 1 },
            )
            .await?;
        Ok(Value::Null)
    }
}
impl Plugin for Sigenergy {
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
                self.cloud.restore(field(c.state().root(), "appState"))?;
                Ok(Value::Null)
            }
            "driver.init" => {
                let driver = json::text(p, "driverId");
                if driver != "gateway" {
                    register::card(driver)?;
                }
                Ok(Value::Null)
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                let driver = json::text(d, "driverId");
                if driver == "gateway" {
                    let station = cloud::station_id(field(field(d, "data"), "stationId"))?;
                    self.gateways.retain(|g| g.id != id);
                    json::push(
                        &mut self.gateways,
                        Gateway::new(id, station, c.now() + 100)?,
                        4096,
                    )?;
                } else {
                    let unit = devices::unit(d)?;
                    self.meters.retain(|m| m.id != id);
                    json::push(
                        &mut self.meters,
                        Meter::new(id, driver, unit, c.now() + 100)?,
                        4096,
                    )?;
                }
                Ok(Value::Null)
            }
            "device.delete" => {
                let id = json::text(p, "deviceId");
                self.meters.retain(|m| m.id != id);
                self.gateways.retain(|g| g.id != id);
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
            "api.invoke" => self.api(c, p).await,
            "pair.list" => self.pair(c, json::text(p, "driverId")).await,
            "pair.start" => {
                let id = json::text(p, "sessionId");
                let driver = json::text(p, "driverId");
                if id.is_empty() || self.pairs.iter().any(|s| s.id == id) {
                    return Err(Error::Invalid("invalid pair session"));
                }
                if driver != "gateway" {
                    register::card(driver)?;
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
                let driver = json::copy(
                    &self
                        .pairs
                        .iter()
                        .find(|s| s.id == json::text(p, "sessionId"))
                        .ok_or(Error::Invalid("pair session missing"))?
                        .driver,
                )?;
                self.pair(c, &driver).await
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
            _ => Err(Error::Invalid("unknown Sigenergy callback")),
        }
    }
    async fn settings<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        patch: &Value,
    ) -> Result<Value> {
        let d = c.state().device(id)?;
        if json::text(d, "driverId") != "gateway"
            && let Some(unit) = json::get(patch, "modbus_unitId")
        {
            devices::unit(&json::fields(&[(
                "settings",
                json::fields(&[("modbus_unitId", clone(unit)?)])?,
            )])?)?;
            if let Some(meter) = self.meters.iter_mut().find(|m| m.id == id) {
                meter.next = 0;
            }
        }
        Ok(Value::Null)
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.meters.retain(|m| c.state().device(&m.id).is_ok());
        self.gateways.retain(|g| c.state().device(&g.id).is_ok());
        if let Some(g) = self.gateways.iter_mut().find(|g| g.next <= c.now())
            && let Err(e) = g.poll(c, &mut self.cloud).await
        {
            c.unavailable(&g.id, &stulp_sdk::message(&e)?).await?;
        }
        if let Some(i) = self.meters.iter().position(|m| m.next <= c.now()) {
            let configured = self.sync_local(c);
            let power = self
                .meters
                .iter()
                .filter(|m| m.driver == "evaccharger")
                .map(|m| m.power)
                .sum();
            let m = &mut self.meters[i];
            m.next = c
                .now()
                .saturating_add(setting_number(c, "interval", 10).clamp(5, 3600) * 1000);
            let result = match configured {
                Ok(()) => {
                    m.refresh(c, &mut self.bus, &self.address, self.timeout, power)
                        .await
                }
                Err(e) => Err(e),
            };
            if let Err(e) = result {
                self.error = stulp_sdk::message(&e)?;
                c.unavailable(&m.id, &self.error).await?;
            } else {
                self.error.clear();
            }
        }
        Ok(())
    }
}
#[cfg(test)]
mod tests;
