//! Een telefoon met de Stulp-app is een apparaat; meldingen gaan via APNs.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_apns as apns;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Plugin, Result, Transport,
    util::{device_arg, field, join},
};
/// De telefoons met de Stulp-app, en het APNs-provider-token zolang het geldt.
#[derive(Default)]
pub struct Notify {
    /// Provider-token voor APNs en het moment van uitgifte (wandklok, seconden).
    apns: Option<(String, u64)>,
    phones: Vec<String>,
}
/// De toestand die de iOS-app van de telefoon meldt, als booleans.
const REPORTED: [&str; 2] = ["charging", "home"];
impl Notify {
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
    /// Het provider-token voor APNs, hergebruikt tot het half verlopen is.
    fn apns_token<T: Transport>(&mut self, c: &mut Client<T>) -> Result<String> {
        let now = c.wall_time()?;
        if let Some((token, issued)) = &self.apns
            && now.saturating_sub(*issued) < apns::TOKEN_SECONDS
        {
            return Ok(json::copy(token)?);
        }
        let setting = |key: &str| {
            c.state()
                .setting(key)
                .and_then(Value::as_str)
                .map(str::trim)
                .unwrap_or("")
        };
        let pem = setting("apnsKey");
        let key_id = setting("apnsKeyId");
        let team_id = match setting("apnsTeamId") {
            "" => "QVY3KR44QS",
            team => team,
        };
        if pem.is_empty() || key_id.is_empty() {
            return Err(Error::Invalid(
                "Meldingen via de app: stel apnsKey en apnsKeyId in bij de Meldingen-app.",
            ));
        }
        let key = apns::key_from_p8(pem)?;
        let token = apns::token(&key, key_id, team_id, now)?;
        self.apns = Some((json::copy(&token)?, now));
        Ok(token)
    }
    async fn send_apns<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        (device_token, environment): (&str, &str),
        (title, body, level): (&str, &str, apns::Level),
        extras: apns::Extras<'_>,
        result: Value,
    ) -> Result<Value> {
        let payload = apns::payload(title, body, level, &extras)?;
        let provider = self.apns_token(c)?;
        let req = apns::request(device_token, environment, &provider, level, payload)?;
        let response = c.http(req).await?;
        if (200..=299).contains(&response.status) {
            c.available(id, true).await?;
            return Ok(result);
        }
        let reason = apns::reason(&response.body)?;
        use core::fmt::Write as _;
        let mut line = alloc::string::String::new();
        if line.try_reserve(160).is_ok() {
            let _ = write!(
                line,
                "NOTIFY_APNS status={} reason={reason}",
                response.status
            );
            c.log("warn", &line)?;
        }
        match reason.as_str() {
            // Een verlopen of geweigerd provider-token: de volgende keer vers.
            "ExpiredProviderToken" | "InvalidProviderToken" => self.apns = None,
            // De app is verwijderd of het token hoort bij de andere omgeving.
            "Unregistered" | "BadDeviceToken" | "DeviceTokenNotForTopic" => {
                c.unavailable(
                    id,
                    "Open de Stulp-app op dit toestel opnieuw voor meldingen.",
                )
                .await?;
            }
            _ => {}
        }
        let mut text = alloc::string::String::new();
        text.try_reserve(120)
            .map_err(|_| stulp_core::Error::Memory)?;
        let _ = write!(text, "Apple weigerde de melding ({}", response.status);
        if !reason.is_empty() {
            let _ = write!(text, ", {reason}");
        }
        text.push_str(").");
        Err(Error::Remote(text))
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
        let store = field(device, "store");
        let apns_token = json::copy(json::text(store, "apns.token"))?;
        let apns_environment = json::copy(json::text(store, "apns.environment"))?;
        if apns_token.is_empty() {
            return Err(Error::Invalid(
                "Open de Stulp-app op dit toestel en sta meldingen toe.",
            ));
        }
        let body = text(field(args, "message"))?;
        let body = body.trim();
        if body.is_empty() {
            return Err(Error::Invalid("Een melding heeft tekst nodig."));
        }
        let title = text(field(args, "title"))?;
        let title = if title.trim().is_empty() {
            "Stulp"
        } else {
            title.trim()
        };
        let mut result = json::fields(&[("device", json::string(json::text(device, "name"))?)])?;
        let source = choice(field(args, "image"));
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
        let level = apns::Level::parse(choice(field(args, "level")));
        let camera = (!source.is_empty()).then_some(source);
        self.send_apns(
            c,
            id,
            (&apns_token, &apns_environment),
            (title, body, level),
            apns::Extras {
                image: image.as_deref(),
                camera,
            },
            result,
        )
        .await
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
                // Een restore kan een andere APNs-sleutel in de instellingen
                // zetten; het provider-token van de oude mag niet blijven hangen.
                self.apns = None;
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
            "pair.list" | "pair.start" => Err(Error::Invalid(
                "Een telefoon meldt zich aan vanuit de Stulp-app.",
            )),
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
            // De app meldt zijn APNs-token na toestemming; het hoort bij dit
            // toestel en is plugin-privé, dus in store, niet in data.
            "api.invoke" if json::text(p, "handler") == "register_push" => {
                let body = field(p, "body");
                let id = json::text(body, "deviceId");
                if !self.phones.iter().any(|s| s == id) {
                    return Err(Error::Invalid("Kies in de app eerst dit toestel."));
                }
                let token = json::text(body, "token");
                let environment = json::text(body, "environment");
                if token.is_empty()
                    || token.len() > 200
                    || !token.bytes().all(|b| b.is_ascii_hexdigit())
                {
                    return Err(Error::Invalid("Ongeldig push-token."));
                }
                if !matches!(environment, "development" | "production") {
                    return Err(Error::Invalid(
                        "Omgeving moet development of production zijn.",
                    ));
                }
                c.store(
                    id,
                    json::fields(&[
                        ("apns.token", json::string(token)?),
                        ("apns.environment", json::string(environment)?),
                    ])?,
                )
                .await?;
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
/// Accepteert dezelfde tekstvormen als de Flow-kaart, zonder debug-JSON voor strings.
fn text(value: &Value) -> Result<String> {
    match value {
        Value::Null => Ok(String::new()),
        Value::String(s) => Ok(json::copy(s)?),
        _ => json::to_string(value).map_err(|e| Error::Core(e.into())),
    }
}
/// Autocomplete-keuzes bewaren een id; een apparaatargument is een apart type.
fn choice(value: &Value) -> &str {
    value
        .as_str()
        .unwrap_or_else(|| field(value, "id").as_str().unwrap_or(""))
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
