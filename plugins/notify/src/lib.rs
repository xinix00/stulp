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
/// De toestand die de iOS-app van de telefoon meldt, als booleans.
const REPORTED: [&str; 2] = ["charging", "home"];
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
        if json::text(field(device, "data"), "kind") == "app" {
            return Err(Error::Invalid(
                "Dit toestel is via de Stulp-app gekoppeld; meldingen via de app komen nog.",
            ));
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
                // Zonder ingesteld adres gaat er géén sub mee: Apple's pushdienst
                // weigert een JWT met "mailto:stulp@localhost" als BadJwtToken
                // (14-10, gemeten), terwijl een JWT zonder sub gewoon aankomt.
                .unwrap_or(""),
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
            status => {
                // De statuscode in de melding en op de console: 403 is vrijwel
                // altijd een VAPID-sleutel die niet bij het abonnement hoort
                // (14-10: een telefoon gekoppeld met een sleutel uit een leeg
                // document, daarna een restore met de oude sleutel).
                use core::fmt::Write as _;
                let mut text = alloc::string::String::new();
                if text.try_reserve(160).is_err() {
                    return Err(Error::Invalid("De pushdienst heeft het bericht geweigerd."));
                }
                let _ = write!(
                    text,
                    "De pushdienst heeft het bericht geweigerd ({status})."
                );
                if status == 403 {
                    text.push_str(
                        " Koppel dit toestel opnieuw: het abonnement hoort bij een andere sleutel.",
                    );
                }
                let mut line = alloc::string::String::new();
                if line.try_reserve(200).is_ok() {
                    let _ = write!(
                        line,
                        "NOTIFY_REJECTED status={status} bytes={}",
                        response.body.len()
                    );
                    c.log("warn", &line)?;
                }
                return Err(Error::Remote(text));
            }
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
            "app.init" => {
                // Een herstart of restore kan een andere sleutel in het document
                // zetten; de gecachete sleutel mag dan niet blijven hangen.
                self.private = None;
                Ok(Value::Null)
            }
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
                // Een telefoon uit de Stulp-app heeft (nog) geen browser-push-
                // abonnement: hij meldt lader en thuis, en krijgt meldingen
                // straks via APNs. Een browser moet wel een abonnement hebben.
                if json::text(field(d, "data"), "kind") != "app" {
                    Subscription::read(field(d, "data"))?;
                }
                let initialize = json::get(field(d, "state"), "onoff").is_none();
                // Telefoons van vóór 25-10 hebben alleen onoff; de toestandsmeldingen
                // van de app (lader, thuis) komen er los bij.
                let missing: alloc::vec::Vec<&str> = REPORTED
                    .iter()
                    .copied()
                    .filter(|cap| {
                        !json::array(d, "capabilities")
                            .iter()
                            .any(|v| v.as_str() == Some(cap))
                    })
                    .collect();
                for cap in missing {
                    c.call(
                        "capability.add",
                        &json::fields(&[
                            ("deviceId", json::string(id)?),
                            ("capability", json::string(cap)?),
                        ])?,
                    )
                    .await?;
                }
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
            // Eén knop in Manage: stuurt een vaste melding naar dit toestel,
            // zodat een koppeling direct te controleren is.
            // De iOS-app meldt hier wat de telefoon weet: aan de lader (via een
            // Opdrachten-automatisering) en thuis (via de geofence).
            "api.invoke" if json::text(p, "handler") == "report" => {
                let body = field(p, "body");
                let id = json::text(body, "deviceId");
                if !self.phones.iter().any(|s| s == id) {
                    return Err(Error::Invalid("Kies in de app eerst dit toestel."));
                }
                let values = field(body, "values");
                let Some(fields) = values.as_object() else {
                    return Err(Error::Invalid("values moet een object zijn."));
                };
                let mut patch = json::object();
                for (key, value) in fields.iter() {
                    if !REPORTED.contains(&key) || value.as_bool().is_none() {
                        return Err(Error::Invalid("Alleen charging en home, als aan of uit."));
                    }
                    json::set(&mut patch, key, stulp_sdk::clone(value)?)?;
                }
                if patch.as_object().is_none_or(|o| o.is_empty()) {
                    return Err(Error::Invalid("Geen waarden om te melden."));
                }
                c.values(id, patch).await?;
                c.available(id, true).await?;
                Ok(Value::Null)
            }
            "api.invoke" if json::text(p, "handler") == "test" => {
                let device = json::text(field(p, "body"), "deviceId");
                let args = json::fields(&[
                    (
                        "device",
                        json::fields(&[("$device", json::string(device)?)])?,
                    ),
                    ("title", json::string("Testmelding")?),
                    (
                        "message",
                        json::string("Stulp kan je op dit toestel bereiken.")?,
                    ),
                ])?;
                self.send(c, &args).await
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
