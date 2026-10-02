//! mySigen bewaart alleen regio, accountnaam en tokens in private app-state.
use aes::{
    Aes128,
    cipher::{Block, BlockEncrypt, KeyInit},
};
use alloc::string::String;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, HttpRequest, Result, Transport, clone,
    util::{field, form, join, number},
};
use zeroize::Zeroize;
#[derive(Default)]
pub(super) struct Cloud {
    identity: Value,
    pub(super) generation: u64,
}
pub(super) fn region(text: &str) -> Result<(&'static str, &'static str)> {
    match text.trim() {
        "" | "eu" => Ok(("https://api-eu.sigencloud.com", "eu")),
        "apac" => Ok(("https://api-apac.sigencloud.com", "apac")),
        "cn" => Ok(("https://api-cn.sigenergy.com", "cn")),
        "us" => Ok(("https://api-us.sigencloud.com", "us")),
        "aus" => Ok(("https://api-aus.sigencloud.com", "aus")),
        "jp" => Ok(("https://api-jp.sigencloud.com", "jp")),
        _ => Err(Error::Invalid("Onbekende mySigen-regio.")),
    }
}
fn integer(v: &Value) -> Option<u64> {
    v.as_u64()
        .or_else(|| v.as_str().and_then(|s| s.parse().ok()))
}
pub(super) fn station_id(v: &Value) -> Result<u64> {
    integer(v)
        .filter(|v| *v > 0 && *v <= i64::MAX as u64)
        .ok_or(Error::Invalid("Ongeldig mySigen-station-id."))
}
fn headers(req: &mut HttpRequest, region: &str) -> Result {
    for (k, v) in [
        ("Accept", "application/json"),
        ("User-Agent", "Stulp-Sigenergy/1 mySigen-gateway"),
        ("lang", "en"),
        ("client-server", region),
        ("AUTH-CLIENT-ID", "sigen"),
        ("sg-platform", "web"),
    ] {
        json::push(&mut req.headers, (json::copy(k)?, json::copy(v)?), 16)?;
    }
    Ok(())
}
fn envelope(status: u16, body: &[u8]) -> Result<Value> {
    let v = json::parse(body).map_err(stulp_core::Error::from)?;
    let code = field(&v, "code");
    if !(200..300).contains(&status)
        || !(code.is_null()
            || code.as_str() == Some("")
            || code.as_str() == Some("0")
            || code.as_u64() == Some(0))
    {
        let msg = match json::text(&v, "msg") {
            "" => json::text(&v, "message"),
            s => s,
        };
        return Err(Error::Remote(join(&[
            "mySigen weigert de vraag: ",
            if msg.is_empty() {
                "onbekende fout"
            } else {
                msg
            },
        ])?));
    }
    if json::get(&v, "data").is_some() {
        clone(field(&v, "data"))
    } else if json::get(&v, "access_token").is_some() {
        Ok(v)
    } else {
        Ok(Value::Null)
    }
}
pub(super) fn password(text: &str) -> Result<String> {
    if text.len() > 4096 {
        return Err(Error::Invalid("mySigen-wachtwoord is te lang."));
    }
    let cipher = Aes128::new_from_slice(b"sigensigensigenp")
        .map_err(|_| Error::Invalid("mySigen cipher key"))?;
    let padding = 16 - text.len() % 16;
    let mut bytes = alloc::vec::Vec::new();
    bytes
        .try_reserve_exact(text.len() + padding)
        .map_err(|_| stulp_core::Error::Memory)?;
    bytes.extend_from_slice(text.as_bytes());
    bytes.resize(text.len() + padding, padding as u8);
    let mut previous = *b"sigensigensigenp";
    for chunk in bytes.chunks_exact_mut(16) {
        let mut block = Block::<Aes128>::default();
        for i in 0..16 {
            block[i] = chunk[i] ^ previous[i];
        }
        cipher.encrypt_block(&mut block);
        chunk.copy_from_slice(&block);
        previous.copy_from_slice(&block);
    }
    let encoded = stulp_sdk::asset(&bytes)?;
    bytes.zeroize();
    Ok(json::copy(json::text(&encoded, "data"))?)
}
async fn obtain<T: Transport>(
    c: &mut Client<T>,
    reg: &str,
    encoded: &str,
    old_refresh: &str,
) -> Result<Value> {
    let (base, reg) = region(reg)?;
    let mut req = HttpRequest::get(&join(&[base, "/auth/oauth/token"])?)?;
    req.method = json::copy("POST")?;
    req.timeout_ms = 10_000;
    req.body = json::copy(encoded)?.into_bytes();
    headers(&mut req, reg)?;
    json::push(
        &mut req.headers,
        (
            json::copy("Authorization")?,
            json::copy("Basic c2lnZW46c2lnZW4=")?,
        ),
        16,
    )?;
    json::push(
        &mut req.headers,
        (
            json::copy("Content-Type")?,
            json::copy("application/x-www-form-urlencoded")?,
        ),
        16,
    )?;
    let r = c.http(req).await?;
    let answer = envelope(r.status, &r.body)?;
    let seconds = integer(field(&answer, "expires_in"))
        .filter(|v| *v > 0)
        .ok_or(Error::Invalid("mySigen stuurde een onvolledig token."))?;
    let access = json::text(&answer, "access_token");
    if access.is_empty() {
        return Err(Error::Invalid("mySigen access-token ontbreekt."));
    }
    let refresh = match json::text(&answer, "refresh_token") {
        "" => old_refresh,
        s => s,
    };
    let expiry = c
        .wall_time()?
        .checked_add(seconds.saturating_sub(60))
        .and_then(|v| v.checked_mul(1_000_000_000))
        .ok_or(Error::Invalid("mySigen token expiry out of range"))?;
    Ok(json::fields(&[
        ("accessToken", json::string(access)?),
        ("refreshToken", json::string(refresh)?),
        ("expiresAt", json::string(&json::timestamp(expiry)?)?),
    ])?)
}
impl Cloud {
    pub(super) fn restore(&mut self, state: &Value) -> Result {
        let version = json::uint(state, "version");
        if version > 1 {
            return Err(Error::Invalid("Onbekende versie van de mySigen-koppeling."));
        }
        region(json::text(state, "region"))?;
        self.identity = if state.as_object().is_some() {
            clone(state)?
        } else {
            json::object()
        };
        Ok(())
    }
    pub(super) fn linked(&self) -> bool {
        !json::text(field(&self.identity, "tokens"), "accessToken").is_empty()
    }
    pub(super) fn status(&self) -> Result<Value> {
        Ok(json::fields(&[
            ("cloudLinked", Value::Bool(self.linked())),
            (
                "cloudRegion",
                json::string(region(json::text(&self.identity, "region"))?.1)?,
            ),
            ("cloudUsername", clone(field(&self.identity, "username"))?),
        ])?)
    }
    pub(super) async fn disconnect<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        let mut state = clone(&self.identity)?;
        json::set(&mut state, "tokens", json::object())?;
        json::set(&mut state, "version", Value::uint(1))?;
        self.identity = clone(&state)?;
        self.generation = self.generation.wrapping_add(1);
        c.app_state(state).await
    }
    pub(super) async fn connect<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        body: &Value,
    ) -> Result<Value> {
        let (_, reg) = region(json::text(body, "region"))?;
        let user = json::text(body, "username").trim();
        let secret = json::text(body, "password");
        if user.is_empty() || secret.is_empty() {
            return Err(Error::Invalid("Vul het mySigen-account en wachtwoord in."));
        }
        let mut uuid = c.random()?;
        uuid[6] = uuid[6] & 15 | 64;
        uuid[8] = uuid[8] & 63 | 128;
        let mut id = String::new();
        id.try_reserve(36).map_err(|_| stulp_core::Error::Memory)?;
        use core::fmt::Write;
        for (i, b) in uuid[..16].iter().enumerate() {
            if matches!(i, 4 | 6 | 8 | 10) {
                id.push('-');
            }
            write!(&mut id, "{b:02x}").map_err(|_| Error::Invalid("UUID formatting failed"))?;
        }
        let mut encrypted = password(secret)?;
        let mut encoded = form(&[
            ("scope", "server"),
            ("grant_type", "password"),
            ("userDeviceId", &id),
            ("username", user),
            ("password", &encrypted),
        ])?;
        encrypted.zeroize();
        let token = obtain(c, reg, &encoded, "").await;
        encoded.zeroize();
        let token = token?;
        let mut candidate = Cloud {
            identity: json::fields(&[
                ("version", Value::uint(1)),
                ("region", json::string(reg)?),
                ("username", json::string(user)?),
                ("tokens", token),
            ])?,
            generation: self.generation.wrapping_add(1),
        };
        // Een geldige login moet de eigenaarslijst kunnen lezen vóór bewaren.
        let stations = candidate
            .request(c, "GET", "/device/owner/station/list", &Value::Null, false)
            .await?;
        c.app_state(clone(&candidate.identity)?).await?;
        *self = candidate;
        Ok(stations)
    }
    async fn access<T: Transport>(&mut self, c: &mut Client<T>, persist: bool) -> Result<String> {
        if !self.linked() {
            return Err(Error::Invalid(
                "Koppel eerst het mySigen-account bij de app-instellingen.",
            ));
        }
        let tokens = field(&self.identity, "tokens");
        let expiry = json::unix_seconds(json::text(tokens, "expiresAt")).unwrap_or(0);
        if c.wall_time()? >= expiry {
            let refresh = json::copy(json::text(tokens, "refreshToken"))?;
            if refresh.is_empty() {
                return Err(Error::Invalid("mySigen-token verlopen; koppel opnieuw."));
            }
            let reg = json::copy(json::text(&self.identity, "region"))?;
            let encoded = form(&[
                ("scope", "server"),
                ("grant_type", "refresh_token"),
                ("refresh_token", &refresh),
            ])?;
            let token = obtain(c, &reg, &encoded, &refresh).await?;
            let mut state = clone(&self.identity)?;
            json::set(&mut state, "tokens", token)?;
            if persist {
                c.app_state(clone(&state)?).await?;
            }
            self.identity = state;
        }
        Ok(json::copy(json::text(
            field(&self.identity, "tokens"),
            "accessToken",
        ))?)
    }
    pub(super) async fn request<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: &Value,
        persist: bool,
    ) -> Result<Value> {
        let token = self.access(c, persist).await?;
        let (base, reg) = region(json::text(&self.identity, "region"))?;
        let mut req = HttpRequest::get(&join(&[base, path])?)?;
        req.method = json::copy(method)?;
        req.timeout_ms = 10_000;
        headers(&mut req, reg)?;
        json::push(
            &mut req.headers,
            (json::copy("Authorization")?, join(&["Bearer ", &token])?),
            16,
        )?;
        if !body.is_null() {
            req.body = json::to_string(body)
                .map_err(stulp_core::Error::from)?
                .into_bytes();
            json::push(
                &mut req.headers,
                (json::copy("Content-Type")?, json::copy("application/json")?),
                16,
            )?;
        }
        let r = c.http(req).await?;
        envelope(r.status, &r.body)
    }
    pub(super) async fn gateway<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: u64,
    ) -> Result<Value> {
        let v = self
            .request(
                c,
                "GET",
                &join(&[
                    "/device/gateway/gateway-status?stationId=",
                    &super::decimal(id)?,
                ])?,
                &Value::Null,
                true,
            )
            .await?;
        if v.as_object().is_none() {
            return Err(Error::Invalid("mySigen Gateway-status ontbreekt."));
        }
        Ok(v)
    }
    pub(super) async fn preflight<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: u64,
        target: bool,
    ) -> Result<Value> {
        let settings = self
            .request(
                c,
                "GET",
                &join(&["/device/gateway/settings/", &super::decimal(id)?])?,
                &Value::Null,
                true,
            )
            .await?;
        let status = self.gateway(c, id).await?;
        if !reached(&status, target) {
            validate(&settings, &status, id, target)?;
        }
        Ok(status)
    }
}
pub(super) fn flag(value: &Value) -> Result<bool> {
    match value {
        Value::Bool(b) => Ok(*b),
        _ => match value.as_str() {
            Some("1" | "true") => Ok(true),
            Some("0" | "false") => Ok(false),
            _ => match number(value) {
                Some(1.) => Ok(true),
                Some(0.) => Ok(false),
                _ => Err(Error::Invalid("mySigen boolean ontbreekt of is ongeldig.")),
            },
        },
    }
}
pub(super) fn grid(v: &Value) -> i64 {
    field(v, "onOffGridStatus").as_i64().unwrap_or(-1)
}
pub(super) fn manual(v: &Value) -> i64 {
    field(v, "manualOffGridStatus").as_i64().unwrap_or(-1)
}
pub(super) fn reached(v: &Value, off: bool) -> bool {
    if off {
        matches!(grid(v), 1 | 2)
    } else {
        grid(v) == 0
    }
}
pub(super) fn known(v: &Value) -> bool {
    (0..=3).contains(&grid(v))
}
pub(super) fn validate(settings: &Value, status: &Value, id: u64, target: bool) -> Result {
    if let Some(station) = integer(field(settings, "stationId"))
        && station > 0
        && station != id
    {
        return Err(Error::Invalid("mySigen antwoordde voor een ander station."));
    }
    if !flag(field(settings, "offGridEnable"))? {
        return Err(Error::Invalid(
            "Off-gridbediening staat niet aan in de Gateway-instellingen.",
        ));
    }
    if !flag(field(status, "showButton"))? {
        return Err(Error::Invalid(
            "mySigen biedt de Go-Off-Grid-knop niet aan voor dit station.",
        ));
    }
    if !(0..=2).contains(&manual(status)) {
        return Err(Error::Invalid(
            "Gateway meldt een fout of onbekende handmatige status.",
        ));
    }
    if !target && grid(status) == 1 {
        return Err(Error::Invalid(
            "De Gateway staat automatisch off-grid; wacht tot het net terug is.",
        ));
    }
    if (target && !matches!(grid(status), 0 | 3)) || (!target && grid(status) != 2) {
        return Err(Error::Invalid("Gateway meldt een onbekende netstand."));
    }
    Ok(())
}
pub(super) fn values(status: &Value) -> Result<Value> {
    let name = match grid(status) {
        0 => "on_grid",
        1 => "off_grid_automatic",
        2 => "off_grid_manual",
        3 => "generator_grid",
        _ => "unknown",
    };
    Ok(json::fields(&[
        ("off_grid", Value::Bool(reached(status, true))),
        ("grid_status", json::string(name)?),
    ])?)
}
