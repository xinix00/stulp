//! Virtuele schakelaars bewaren eerst hun waarheid, en publiceren daarna de live-stand.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, Plugin, Result, Transport, clone};
const MAX_SESSIONS: usize = 32;
struct Pair {
    id: String,
    candidate: Option<Value>,
}
/// Iedere plugininstantie bezit zijn eigen koppelsessies.
#[derive(Default)]
pub struct VirtualDevices {
    pairs: Vec<Pair>,
}
impl VirtualDevices {
    async fn initialize<T: Transport>(
        &mut self,
        client: &mut Client<T>,
        id: &str,
    ) -> Result<Value> {
        let stored = json::get(client.state().device(id)?, "store")
            .and_then(|s| json::get(s, "onoff"))
            .and_then(Value::as_bool);
        let on = stored.unwrap_or(false);
        if stored.is_none() {
            client
                .store(id, json::fields(&[("onoff", Value::Bool(on))])?)
                .await?;
        }
        client
            .values(id, json::fields(&[("onoff", Value::Bool(on))])?)
            .await?;
        client.available(id, true).await?;
        Ok(Value::Null)
    }
    async fn set<T: Transport>(
        &mut self,
        client: &mut Client<T>,
        id: &str,
        capability: &str,
        value: &Value,
    ) -> Result<Value> {
        if capability != "onoff" {
            return Err(Error::Invalid(
                "virtuele schakelaar kent deze capability niet",
            ));
        }
        let on = value
            .as_bool()
            .ok_or(Error::Invalid("de aan/uit-stand moet true of false zijn"))?;
        client.state().device(id)?;
        client
            .store(id, json::fields(&[("onoff", Value::Bool(on))])?)
            .await?;
        client
            .values(id, json::fields(&[("onoff", Value::Bool(on))])?)
            .await?;
        Ok(Value::Null)
    }
    fn pair<T: Transport>(&mut self, client: &mut Client<T>, params: &Value) -> Result<Value> {
        let id = json::text(params, "sessionId");
        let pair = self
            .pairs
            .iter_mut()
            .find(|s| s.id == id)
            .ok_or(Error::Invalid("pair session does not exist"))?;
        match json::text(params, "event") {
            "create" => {
                let data = json::get(params, "data").unwrap_or(&Value::Null);
                let name = json::text(data, "name").trim();
                if name.is_empty() {
                    return Err(Error::Invalid("geef de virtuele schakelaar een naam"));
                }
                if name.chars().count() > 160 {
                    return Err(Error::Invalid("de naam mag hoogstens 160 tekens lang zijn"));
                }
                use core::fmt::Write;
                let bytes = client.random()?;
                let mut id = json::copy("virtual-")?;
                id.try_reserve(32).map_err(|_| stulp_core::Error::Memory)?;
                for b in bytes.iter().take(16) {
                    write!(&mut id, "{b:02x}")
                        .map_err(|_| Error::Invalid("identity formatting failed"))?;
                }
                let candidate = json::fields(&[
                    ("name", json::string(name)?),
                    ("data", json::fields(&[("id", json::string(&id)?)])?),
                    ("store", json::fields(&[("onoff", Value::Bool(false))])?),
                ])?;
                pair.candidate = Some(clone(&candidate)?);
                Ok(candidate)
            }
            "list_devices" => {
                let candidate = pair
                    .candidate
                    .as_ref()
                    .ok_or(Error::Invalid("geef de virtuele schakelaar eerst een naam"))?;
                let mut values = Vec::new();
                json::push(&mut values, clone(candidate)?, 1)?;
                Ok(Value::Array(values))
            }
            _ => Err(Error::Invalid("unknown pair event")),
        }
    }
}
impl Plugin for VirtualDevices {
    fn assets(&self) -> &'static [&'static str] {
        &["drivers/switch/pair/name.html"]
    }
    fn manifest(&self) -> &'static [u8] {
        include_bytes!("../app.json")
    }
    async fn handle<T: Transport>(
        &mut self,
        client: &mut Client<T>,
        method: &str,
        params: &Value,
    ) -> Result<Value> {
        match method {
            "app.init" | "device.delete" => Ok(Value::Null),
            "driver.init" => {
                if json::text(params, "driverId") != "switch" {
                    return Err(Error::Invalid("unknown virtual driver"));
                }
                Ok(Value::Null)
            }
            "device.init" => {
                if json::text(params, "driverId") != "switch" {
                    return Err(Error::Invalid("unknown virtual driver"));
                }
                self.initialize(client, json::text(params, "deviceId"))
                    .await
            }
            "capability.invoke" => {
                self.set(
                    client,
                    json::text(params, "deviceId"),
                    json::text(params, "capability"),
                    json::get(params, "value").unwrap_or(&Value::Null),
                )
                .await
            }
            "capabilities.invoke" => {
                let mut errors = json::object();
                for command in json::array(params, "commands") {
                    let cap = json::text(command, "capability");
                    if let Err(error) = self
                        .set(
                            client,
                            json::text(params, "deviceId"),
                            cap,
                            json::get(command, "value").unwrap_or(&Value::Null),
                        )
                        .await
                    {
                        json::set(
                            &mut errors,
                            cap,
                            json::string(&stulp_sdk::message(&error)?)?,
                        )?;
                    }
                }
                Ok(errors)
            }
            "pair.start" => {
                if json::text(params, "driverId") != "switch" {
                    return Err(Error::Invalid("unknown virtual driver"));
                }
                let id = json::text(params, "sessionId");
                if id.is_empty() {
                    return Err(Error::Invalid("pair session id is required"));
                }
                if self.pairs.iter().any(|s| s.id == id) {
                    return Err(Error::Invalid("pair session already exists"));
                }
                json::push(
                    &mut self.pairs,
                    Pair {
                        id: json::copy(id)?,
                        candidate: None,
                    },
                    MAX_SESSIONS,
                )?;
                json::parse(br#"["create","list_devices"]"#).map_err(|e| Error::Core(e.into()))
            }
            "pair.emit" => self.pair(client, params),
            "pair.close" => {
                self.pairs
                    .retain(|s| s.id != json::text(params, "sessionId"));
                Ok(Value::Null)
            }
            "registrations" => json::parse(br#"{"drivers":["switch"],"flows":[]}"#)
                .map_err(|e| Error::Core(e.into())),
            "ui.asset" => {
                let path = json::text(params, "path");
                if path != "drivers/switch/pair/name.html" {
                    return Ok(json::fields(&[("found", Value::Bool(false))])?);
                }
                stulp_sdk::asset(include_bytes!("../drivers/switch/pair/name.html"))
            }
            _ => Err(Error::Invalid("unknown virtual devices method")),
        }
    }
}
