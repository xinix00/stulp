//! Een browser is een apparaat; abonnement en meldingsschakelaar volgen zijn lifecycle.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_protocol::token::base64;
use stulp_sdk::{
    Client, Error, HttpRequest, Plugin, Result, Transport,
    util::{device_arg, field, join},
};
use stulp_webpush::{self as push, Subscription};
/// De VAPID-identiteit wordt pas gebruikt nadat Stulp hem duurzaam bevestigd heeft.
#[derive(Default)]
pub struct Notify {
    private: Option<[u8; 32]>,
    phones: Vec<String>,
    pairs: Vec<String>,
}
impl Drop for Notify {
    fn drop(&mut self) {
        if let Some(private) = self.private.as_mut() {
            push::erase(private);
        }
    }
}
impl Notify {
    async fn key<T: Transport>(&mut self, c: &mut Client<T>) -> Result<&[u8; 32]> {
        if self.private.is_none() {
            if let Some(s) = c
                .state()
                .setting("vapidPrivateKey")
                .and_then(Value::as_str)
                .filter(|s| !s.is_empty())
            {
                let mut decoded = push::decode(s)?;
                let parsed: Result<[u8; 32]> = decoded.as_slice().try_into().map_err(|_| {
                    Error::Invalid("De bewaarde VAPID-sleutel heeft een ongeldige lengte.")
                });
                push::erase(&mut decoded);
                let mut key = parsed?;
                if let Err(error) = push::public(&key) {
                    push::erase(&mut key);
                    return Err(error);
                }
                self.private = Some(key);
                push::erase(&mut key);
            } else {
                let mut key = c.random()?;
                push::public(&key)?;
                let encoded = base64(&key)?;
                if let Err(e) = c.setting("vapidPrivateKey", json::string(&encoded)?).await {
                    push::erase(&mut key);
                    return Err(e);
                }
                self.private = Some(key);
                push::erase(&mut key);
            }
        }
        self.private
            .as_ref()
            .ok_or(Error::Invalid("VAPID key missing"))
    }
    async fn capability<T: Transport>(&self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let id = json::text(p, "deviceId");
        if !self.phones.iter().any(|v| v == id) {
            return Err(Error::Invalid("Dit toestel is niet gestart."));
        }
        if json::text(p, "capability") != "onoff" || field(p, "value").as_bool().is_none() {
            return Err(Error::Invalid("Meldingen verwachten aan of uit."));
        }
        c.values(
            id,
            json::fields(&[("onoff", stulp_sdk::clone(field(p, "value"))?)])?,
        )
        .await?;
        Ok(Value::Null)
    }
    async fn send<T: Transport>(&mut self, c: &mut Client<T>, args: &Value) -> Result<Value> {
        let id = device_arg(args);
        if !self.phones.iter().any(|s| s == id) {
            return Err(Error::Invalid("Kies eerst een toestel."));
        }
        let device = c.state().device(id)?;
        if json::get(field(device, "state"), "onoff").is_some_and(|v| v.as_bool() != Some(true)) {
            return Err(Error::Invalid("Meldingen staan uit voor dit toestel."));
        }
        let subscription = Subscription::read(field(device, "data"))?;
        let body = push::text(field(args, "message"))?;
        let body = body.trim();
        if body.is_empty() {
            return Err(Error::Invalid("Een melding heeft tekst nodig."));
        }
        let title = push::text(field(args, "title"))?;
        let title = if title.trim().is_empty() {
            "Stulp"
        } else {
            title.trim()
        };
        let mut result = json::fields(&[("device", json::string(json::text(device, "name"))?)])?;
        let source = push::choice(field(args, "image"));
        let mut image = None;
        if !source.is_empty() {
            match c
                .call(
                    "image.url",
                    &json::fields(&[
                        ("deviceId", json::string(source)?),
                        ("slot", json::string("")?),
                    ])?,
                )
                .await
            {
                Ok(v) => image = Some(json::copy(json::text(&v, "url"))?),
                Err(e) => json::set(
                    &mut result,
                    "image",
                    json::string(&join(&["zonder foto: ", &stulp_sdk::message(&e)?])?)?,
                )?,
            }
        }
        let message = push::message(title, body, image.as_deref())?;
        let subject = json::copy(
            c.state()
                .setting("contact")
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .unwrap_or("mailto:stulp@localhost"),
        )?;
        let now = c.wall_time()?;
        let authorization =
            push::authorization(self.key(c).await?, subscription.endpoint(), &subject, now)?;
        let mut ephemeral = c.random()?;
        let mut random = c.random()?;
        let mut salt = [0u8; 16];
        salt.copy_from_slice(&random[..16]);
        push::erase(&mut random);
        let encrypted = push::encrypt(&subscription, &ephemeral, &salt, &message);
        push::erase(&mut ephemeral);
        let encrypted = encrypted?;
        let mut req = HttpRequest::get(subscription.endpoint())?;
        req.method = json::copy("POST")?;
        req.body = encrypted;
        req.limit = 4096;
        for (k, v) in [
            ("Authorization", authorization.as_str()),
            ("Content-Encoding", "aes128gcm"),
            ("Content-Type", "application/octet-stream"),
            ("TTL", "600"),
            ("Urgency", "high"),
        ] {
            json::push(&mut req.headers, (json::copy(k)?, json::copy(v)?), 8)?;
        }
        let response = c.http(req).await?;
        match response.status {
            200..=299 => c.available(id, true).await?,
            404 | 410 => {
                c.unavailable(id, "Deze browser is afgemeld. Koppel dit toestel opnieuw.")
                    .await?;
                return Err(Error::Invalid(
                    "De pushdienst kent dit abonnement niet meer.",
                ));
            }
            _ => return Err(Error::Invalid("De pushdienst heeft het bericht geweigerd.")),
        }
        Ok(result)
    }
}
impl Plugin for Notify {
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
            "app.init" => Ok(Value::Null),
            "driver.init" => {
                if json::text(p, "driverId") == "phone" {
                    Ok(Value::Null)
                } else {
                    Err(Error::Invalid("unknown notification driver"))
                }
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                if json::text(d, "driverId") != "phone" {
                    return Err(Error::Invalid("unknown notification driver"));
                }
                Subscription::read(field(d, "data"))?;
                let initialize = json::get(field(d, "state"), "onoff").is_none();
                self.phones.retain(|s| s != id);
                json::push(&mut self.phones, json::copy(id)?, 4096)?;
                if initialize {
                    c.values(id, json::fields(&[("onoff", Value::Bool(true))])?)
                        .await?;
                }
                c.available(id, true).await?;
                Ok(Value::Null)
            }
            "device.delete" => {
                self.phones.retain(|s| s != json::text(p, "deviceId"));
                Ok(Value::Null)
            }
            "capability.invoke" => self.capability(c, p).await,
            "capabilities.invoke" => {
                let mut failed = json::object();
                for command in json::array(p, "commands") {
                    let mut command = stulp_sdk::clone(command)?;
                    json::set(
                        &mut command,
                        "deviceId",
                        stulp_sdk::clone(field(p, "deviceId"))?,
                    )?;
                    if let Err(e) = self.capability(c, &command).await {
                        json::set(
                            &mut failed,
                            json::text(&command, "capability"),
                            json::string(&stulp_sdk::message(&e)?)?,
                        )?;
                    }
                }
                Ok(failed)
            }
            "pair.list" => Err(Error::Invalid(
                "Een telefoon meldt zichzelf aan; er valt niets te zoeken.",
            )),
            "pair.start" => {
                let id = json::text(p, "sessionId");
                if id.is_empty()
                    || json::text(p, "driverId") != "phone"
                    || self.pairs.iter().any(|s| s == id)
                {
                    return Err(Error::Invalid("invalid pairing session"));
                }
                json::push(&mut self.pairs, json::copy(id)?, 32)?;
                json::parse(br#"["publicKey"]"#).map_err(|e| Error::Core(e.into()))
            }
            "pair.emit" => {
                if json::text(p, "event") != "publicKey"
                    || !self.pairs.iter().any(|s| s == json::text(p, "sessionId"))
                {
                    return Err(Error::Invalid("invalid pairing event"));
                }
                let key = base64(&push::public(self.key(c).await?)?)?;
                Ok(json::fields(&[("publicKey", json::string(&key)?)])?)
            }
            "pair.close" => {
                self.pairs.retain(|s| s != json::text(p, "sessionId"));
                Ok(Value::Null)
            }
            "flow.run" => {
                if json::text(p, "kind") != "action" || json::text(p, "id") != "send" {
                    return Err(Error::Invalid("unknown notification card"));
                }
                self.send(c, field(p, "args")).await
            }
            "flow.autocomplete" => {
                if json::text(p, "kind") != "action"
                    || json::text(p, "id") != "send"
                    || json::text(p, "argument") != "image"
                {
                    return Err(Error::Invalid("unknown notification autocomplete"));
                }
                let sources = c.call("images.list", &json::object()).await?;
                let mut out = Vec::new();
                let needle = lower(json::text(p, "query"))?;
                for s in sources.as_array().unwrap_or(&[]) {
                    let name = json::text(s, "deviceName");
                    let title = json::text(s, "title");
                    if !lower(&join(&[name, " ", title])?)?.contains(&needle) {
                        continue;
                    }
                    json::push(
                        &mut out,
                        json::fields(&[
                            ("id", json::string(json::text(s, "deviceId"))?),
                            ("name", json::string(name)?),
                            ("description", json::string(title)?),
                        ])?,
                        4096,
                    )?;
                }
                Ok(Value::Array(out))
            }
            "registrations" => {
                let mut r = stulp_sdk::registrations(
                    &json::parse(self.manifest()).map_err(stulp_core::Error::from)?,
                )?;
                let mut cards = Vec::new();
                for card in json::array(&r, "flows") {
                    let mut card = stulp_sdk::clone(card)?;
                    json::set(
                        &mut card,
                        "autocomplete",
                        json::parse(br#"["image"]"#).map_err(stulp_core::Error::from)?,
                    )?;
                    json::push(&mut cards, card, 1)?;
                }
                json::set(&mut r, "flows", Value::Array(cards))?;
                Ok(r)
            }
            "ui.asset" => Ok(json::fields(&[("found", Value::Bool(false))])?),
            _ => Err(Error::Invalid("unknown notification method")),
        }
    }
}
fn lower(s: &str) -> Result<String> {
    let mut out = String::new();
    out.try_reserve(s.len().checked_mul(3).ok_or(stulp_core::Error::Full)?)
        .map_err(|_| stulp_core::Error::Memory)?;
    for c in s.chars() {
        for v in c.to_lowercase() {
            out.push(v);
        }
    }
    Ok(out)
}
