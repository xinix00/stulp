//! Protect REST: minimale patches, expliciete lokale TLS en geen write-retries.
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, HttpRequest, Result, Transport, clone,
    util::{field, join, number},
};
pub(super) const BASE: &str = "/proxy/protect/integration/v1/";
#[derive(Default, PartialEq, Eq)]
pub(super) struct Config {
    pub host: String,
    pub port: u16,
    pub key: String,
}
impl Config {
    pub(super) fn read(v: &Value) -> Result<Self> {
        let port = number(field(v, "port")).unwrap_or(443.);
        if !(1. ..=65535.).contains(&port) || port != f64::from(port as u16) {
            return Err(Error::Invalid("Ongeldige consolepoort."));
        }
        let c = Self {
            host: json::copy(json::text(v, "host").trim())?,
            port: port as u16,
            key: json::copy(json::text(v, "apiKey").trim())?,
        };
        if c.host.len() > 253
            || c.host
                .bytes()
                .any(|b| b <= 32 || b >= 127 || b"/@\\?#".contains(&b))
            || c.key.bytes().any(|b| b < 32 || b == 127)
        {
            return Err(Error::Invalid("Ongeldig consoleadres of API-key."));
        }
        Ok(c)
    }
    pub(super) fn ready(&self) -> bool {
        !self.host.is_empty() && !self.key.is_empty()
    }
    pub(super) fn authority(&self) -> Result<String> {
        let port =
            json::to_string(&Value::uint(u64::from(self.port))).map_err(stulp_core::Error::from)?;
        if self.host.contains(':') {
            join(&["[", self.host.trim_matches(['[', ']']), "]:", &port])
        } else {
            join(&[&self.host, ":", &port])
        }
    }
    pub(super) async fn raw<T: Transport>(
        &self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: Option<&Value>,
        timeout: u64,
    ) -> Result<stulp_sdk::HttpResponse> {
        if !self.ready() {
            return Err(Error::Invalid(
                "Vul het consoleadres en de API-key in bij de appinstellingen.",
            ));
        }
        if timeout == 0 {
            return Err(Error::Timeout);
        }
        let mut req = HttpRequest::get(&join(&["https://", &self.authority()?, BASE, path])?)?;
        req.device_certificate = true;
        req.method = json::copy(method)?;
        req.timeout_ms = timeout.min(20_000);
        let image = path.contains("/snapshot?");
        if image {
            req.limit = 4 << 20;
        }
        json::push(
            &mut req.headers,
            (json::copy("X-API-KEY")?, json::copy(&self.key)?),
            8,
        )?;
        json::push(
            &mut req.headers,
            (
                json::copy("Accept")?,
                json::copy(if image { "image/*" } else { "application/json" })?,
            ),
            8,
        )?;
        if let Some(body) = body {
            req.body = json::to_string(body)
                .map_err(stulp_core::Error::from)?
                .into_bytes();
            json::push(
                &mut req.headers,
                (json::copy("Content-Type")?, json::copy("application/json")?),
                8,
            )?;
        }
        let reply = c.http(req).await?;
        match reply.status {
            200..=299 => Ok(reply),
            401 => Err(Error::Invalid("De console weigert de API-key.")),
            403 => Err(Error::Invalid("Deze API-key heeft onvoldoende rechten.")),
            404 => Err(Error::Invalid(
                "Dit apparaat of deze Protect API bestaat niet op de console.",
            )),
            _ => Err(Error::Remote(join(&[
                "Protect antwoordt met HTTP ",
                &json::to_string(&Value::uint(u64::from(reply.status)))
                    .map_err(stulp_core::Error::from)?,
            ])?)),
        }
    }
    pub(super) async fn call<T: Transport>(
        &self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: Option<&Value>,
        timeout: u64,
    ) -> Result<Value> {
        let r = self.raw(c, method, path, body, timeout).await?;
        if r.body.is_empty() {
            Ok(Value::Null)
        } else {
            json::parse(&r.body).map_err(|e| Error::Core(e.into()))
        }
    }
}
pub(super) fn resource(driver: &str) -> Result<&'static str> {
    match driver {
        "camera" => Ok("cameras"),
        "light" => Ok("lights"),
        "sensor" => Ok("sensors"),
        "chime" => Ok("chimes"),
        "relay" => Ok("relays"),
        _ => Err(Error::Invalid("Onbekend Protect-apparaat.")),
    }
}
pub(super) fn path(driver: &str, id: &str) -> Result<String> {
    if id.is_empty() {
        return Err(Error::Invalid("Protect-id ontbreekt."));
    }
    join(&[resource(driver)?, "/", &stulp_sdk::query(id)?])
}
pub(super) fn pairs(driver: &str, devices: &Value) -> Result<Value> {
    let items = devices
        .as_array()
        .ok_or(Error::Invalid("De console stuurde geen apparatenlijst."))?;
    let mut result = Vec::new();
    for d in items {
        if json::text(d, "id").is_empty() {
            continue;
        }
        let mut store = json::fields(&[("model", clone(field(d, "modelKey"))?)])?;
        if driver == "camera" {
            json::set(&mut store, "mac", clone(field(d, "mac"))?)?;
        }
        if driver == "light" {
            json::set(&mut store, "host", clone(field(d, "host"))?)?;
        }
        if driver == "relay" {
            json::set(&mut store, "relay", clone(field(d, "name"))?)?;
            for o in json::array(d, "outputs") {
                if json::text(o, "id").is_empty() {
                    continue;
                }
                let name = if json::text(o, "name").is_empty() {
                    join(&[json::text(d, "name"), " ", json::text(o, "id")])?
                } else {
                    json::copy(json::text(o, "name"))?
                };
                json::push(
                    &mut result,
                    json::fields(&[
                        ("name", json::string(&name)?),
                        (
                            "data",
                            json::fields(&[
                                ("id", clone(field(d, "id"))?),
                                ("output", clone(field(o, "id"))?),
                            ])?,
                        ),
                        ("store", clone(&store)?),
                    ])?,
                    4096,
                )?;
            }
        } else {
            json::push(
                &mut result,
                json::fields(&[
                    ("name", clone(field(d, "name"))?),
                    ("data", json::fields(&[("id", clone(field(d, "id"))?)])?),
                    ("store", store),
                ])?,
                4096,
            )?;
        }
    }
    Ok(Value::Array(result))
}
