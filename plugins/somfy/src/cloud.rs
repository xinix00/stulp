//! Eén account bezit één cookiejar; een 401 wordt precies één keer hersteld.
use crate::join;
use alloc::{string::String, vec::Vec};
use leancookie::{Jar, Url};
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, HttpRequest, HttpResponse, Result, Transport};
const BASE: &str = "https://www.tahomalink.com/enduser-mobile-web/enduserAPI";
const THROTTLED: &str =
    "TaHoma beperkt tijdelijk het aantal verzoeken. De app wacht en probeert automatisch opnieuw.";
const LOGIN_WAIT: &str = "TaHoma-inloggen is nog niet gelukt. De app wacht voor een nieuwe poging.";
struct Domains;
impl leancookie::DomainPolicy for Domains {
    fn allow(&self, _: &str, domain: &str) -> bool {
        matches!(domain, "tahomalink.com" | "www.tahomalink.com")
    }
}
pub(super) struct Cloud {
    username: String,
    password: String,
    jar: Jar<Domains>,
    active: bool,
    retry_at: u64,
    login_delay: u64,
    wait_message: &'static str,
}
impl Cloud {
    pub(super) fn new(account: &Value) -> Result<Self> {
        let username = json::text(account, "username");
        let password = json::text(account, "password");
        if username.is_empty() || password.is_empty() {
            return Err(Error::Invalid(
                "Vul je TaHoma-gebruikersnaam en wachtwoord in op de instellingenpagina.",
            ));
        }
        Ok(Self {
            username: json::copy(username)?,
            password: json::copy(password)?,
            jar: Jar::with_policy(32, Domains),
            active: false,
            retry_at: 0,
            login_delay: 60_000,
            wait_message: LOGIN_WAIT,
        })
    }
    pub(super) fn retry_after(&self, now: u64) -> u64 {
        self.retry_at.saturating_sub(now).div_ceil(1000)
    }
    pub(super) fn ready(&self, now: u64) -> Result {
        if now < self.retry_at {
            return Err(Error::Invalid(self.wait_message));
        }
        Ok(())
    }
    pub(super) fn defer_from(&mut self, other: &Self) {
        if other.retry_at > self.retry_at {
            self.retry_at = other.retry_at;
            self.wait_message = other.wait_message;
        }
    }
    async fn send<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: &[u8],
        form: bool,
    ) -> Result<HttpResponse> {
        let url = join(&[BASE, path])?;
        let parsed = Url::parse(&url).map_err(|_| Error::Invalid("invalid TaHoma URL"))?;
        let cookie = self
            .jar
            .header(
                &parsed,
                i64::try_from(c.wall_time()?)
                    .map_err(|_| Error::Invalid("wall clock out of range"))?,
            )
            .map_err(|_| Error::Invalid("cookie encoding failed"))?;
        let mut req = HttpRequest::get(&url)?;
        req.method = json::copy(method)?;
        // Dezelfde JSON-bovengrens als de gedeelde parser, vóór het inlezen.
        req.limit = json::MAX_DOCUMENT;
        req.body
            .try_reserve(body.len())
            .map_err(|_| stulp_core::Error::Memory)?;
        req.body.extend_from_slice(body);
        json::push(
            &mut req.headers,
            (json::copy("Accept")?, json::copy("application/json")?),
            8,
        )?;
        if !cookie.is_empty() {
            json::push(&mut req.headers, (json::copy("Cookie")?, cookie), 8)?;
        }
        if !body.is_empty() {
            json::push(
                &mut req.headers,
                (
                    json::copy("Content-Type")?,
                    json::copy(if form {
                        "application/x-www-form-urlencoded"
                    } else {
                        "application/json; charset=utf-8"
                    })?,
                ),
                8,
            )?;
        }
        let answer = c.http(req).await?;
        if throttled(&answer) {
            // TaHoma meldt de limiet ook als 401 AUTHENTICATION_ERROR.
            // Een nieuwe login verlengt de blokkade; wacht minstens een kwartier.
            let seconds = answer
                .headers
                .iter()
                .find(|(k, _)| k.eq_ignore_ascii_case("retry-after"))
                .and_then(|(_, v)| v.trim().parse::<u64>().ok())
                .unwrap_or(900)
                .max(900);
            self.retry_at = c.now().saturating_add(seconds.saturating_mul(1000));
            self.wait_message = THROTTLED;
        }
        self.jar
            .set_from(
                &parsed,
                answer
                    .headers
                    .iter()
                    .filter(|(k, _)| k.eq_ignore_ascii_case("set-cookie"))
                    .map(|(_, v)| v.as_str()),
                i64::try_from(c.wall_time()?)
                    .map_err(|_| Error::Invalid("wall clock out of range"))?,
            )
            .map_err(|_| Error::Invalid("cookie storage failed"))?;
        Ok(answer)
    }
    pub(super) async fn login<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.ready(c.now())?;
        let result = self.login_once(c).await;
        if let Err(error) = &result {
            if self.retry_at <= c.now() {
                self.wait_message = match error {
                    Error::Invalid(message) => message,
                    _ => LOGIN_WAIT,
                };
            }
            self.retry_at = self.retry_at.max(c.now().saturating_add(self.login_delay));
            self.login_delay = self.login_delay.saturating_mul(2).min(900_000);
        } else {
            self.retry_at = 0;
            self.login_delay = 60_000;
            self.wait_message = LOGIN_WAIT;
        }
        result
    }
    async fn login_once<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.active = false;
        let form = join(&[
            "userId=",
            &stulp_sdk::query(&self.username)?,
            "&userPassword=",
            &stulp_sdk::query(&self.password)?,
        ])?;
        let answer = self
            .send(c, "POST", "/login", form.as_bytes(), true)
            .await?;
        status(&answer, true)?;
        let value = json::parse(&answer.body).map_err(stulp_core::Error::from)?;
        if json::get(&value, "success").and_then(Value::as_bool) == Some(false) {
            return Err(Error::Invalid("TaHoma weigert de inloggegevens."));
        }
        if value.as_object().is_none() {
            return Err(Error::Invalid("invalid TaHoma login response"));
        }
        self.active = true;
        Ok(())
    }
    pub(super) async fn request<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: &Value,
    ) -> Result<Value> {
        self.ready(c.now())?;
        if !self.active {
            self.login(c).await?;
        }
        let encoded = if body.is_null() {
            String::new()
        } else {
            json::to_string(body).map_err(stulp_core::Error::from)?
        };
        let mut answer = self
            .send(c, method, path, encoded.as_bytes(), false)
            .await?;
        if answer.status == 401 && !throttled(&answer) {
            self.login(c).await?;
            answer = self
                .send(c, method, path, encoded.as_bytes(), false)
                .await?;
        }
        if answer.status == 401 && !throttled(&answer) {
            self.active = false;
            self.retry_at = c.now().saturating_add(60_000);
        }
        status(&answer, false)?;
        if answer.body.is_empty() {
            return Ok(Value::Null);
        }
        json::parse(&answer.body).map_err(|e| Error::Core(e.into()))
    }
    pub(super) async fn logout<T: Transport>(&mut self, c: &mut Client<T>) {
        if self.active {
            let _ = self.send(c, "POST", "/logout", &[], false).await;
        }
        self.active = false;
        self.jar = Jar::with_policy(32, Domains);
    }
}
fn throttled(answer: &HttpResponse) -> bool {
    if answer.status == 429 {
        return true;
    }
    if answer.status != 401 {
        return false;
    }
    let Ok(body) = json::parse(&answer.body) else {
        return false;
    };
    let message = json::text(&body, "error");
    message.contains("Too many requests")
        || message.contains("too many requests")
        || json::text(&body, "errorCode") == "TOO_MANY_REQUESTS"
}
fn status(answer: &HttpResponse, login: bool) -> Result {
    if throttled(answer) {
        return Err(Error::Invalid(THROTTLED));
    }
    if answer.status == 401 {
        return Err(Error::Invalid(if login {
            "TaHoma weigert het inloggen; controleer het account."
        } else {
            "TaHoma weigert de sessie, ook na opnieuw inloggen."
        }));
    }
    if !(200..300).contains(&answer.status) {
        return Err(Error::Invalid("TaHoma heeft het verzoek geweigerd."));
    }
    Ok(())
}
pub(super) fn execution(
    label: &str,
    url: &str,
    name: &str,
    parameters: Vec<Value>,
) -> Result<Value> {
    let command = json::fields(&[
        ("name", json::string(name)?),
        ("parameters", Value::Array(parameters)),
    ])?;
    let mut commands = Vec::new();
    json::push(&mut commands, command, 1)?;
    let action = json::fields(&[
        ("deviceURL", json::string(url)?),
        ("commands", Value::Array(commands)),
    ])?;
    let mut actions = Vec::new();
    json::push(&mut actions, action, 1)?;
    Ok(json::fields(&[
        (
            "label",
            json::string(&join(&[label.trim(), " - ", name, " - Stulp"])?)?,
        ),
        ("actions", Value::Array(actions)),
    ])?)
}
