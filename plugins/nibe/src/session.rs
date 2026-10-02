//! OAuth2 met PKCE, roterende refresh-tokens en duurzame bevestiging vóór gebruik.
use alloc::string::String;
use stulp_core::json::{self, Value};
use stulp_protocol::token::base64;
use stulp_sdk::{
    Client, Error, HttpRequest, Result, Transport, clone,
    util::{field, form, join},
};
pub(super) const BASE: &str = "https://api.myuplink.com";
pub(super) struct Pending {
    state: String,
    verifier: String,
}
#[derive(Default)]
pub(super) struct Session {
    pub(super) pending: Option<Pending>,
    pub(super) error: String,
    pub(super) next: u64,
}
fn setting<'a, T: Transport>(c: &'a Client<T>, key: &str) -> &'a str {
    c.state().setting(key).and_then(Value::as_str).unwrap_or("")
}
pub(super) fn tokens<T: Transport>(c: &Client<T>) -> &Value {
    field(field(c.state().root(), "appState"), "tokens")
}
fn random<T: Transport>(c: &mut Client<T>) -> Result<String> {
    let mut bytes = [0; 48];
    bytes[..32].copy_from_slice(&c.random()?);
    bytes[32..].copy_from_slice(&c.random()?[..16]);
    Ok(base64(&bytes)?)
}
fn check<T: Transport>(c: &Client<T>, redirect: bool) -> Result {
    if setting(c, "clientId").is_empty()
        || setting(c, "clientSecret").is_empty()
        || (redirect && setting(c, "redirectUri").is_empty())
    {
        Err(Error::Invalid(
            "Vul de myUplink-registratie in bij de instellingen van de app.",
        ))
    } else {
        Ok(())
    }
}
impl Session {
    pub(super) fn authorize<T: Transport>(&mut self, c: &mut Client<T>) -> Result<Value> {
        check(c, true)?;
        let verifier = random(c)?;
        let state = random(c)?;
        let challenge = base64(&auth::sha256(verifier.as_bytes()))?;
        let query = form(&[
            ("response_type", "code"),
            ("client_id", setting(c, "clientId")),
            ("redirect_uri", setting(c, "redirectUri")),
            ("scope", "READSYSTEM WRITESYSTEM offline_access"),
            ("state", &state),
            ("code_challenge", &challenge),
            ("code_challenge_method", "S256"),
        ])?;
        let answer = json::fields(&[(
            "url",
            json::string(&join(&[BASE, "/oauth/authorize?", &query])?)?,
        )])?;
        self.pending = Some(Pending { state, verifier });
        Ok(answer)
    }
    pub(super) async fn exchange<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        pasted: &str,
    ) -> Result {
        check(c, true)?;
        let p = self
            .pending
            .as_ref()
            .ok_or(Error::Invalid("Begin eerst met Autoriseren."))?;
        let code = code_from_redirect(pasted, &p.state)?;
        let encoded = form(&[
            ("grant_type", "authorization_code"),
            ("code", &code),
            ("redirect_uri", setting(c, "redirectUri")),
            ("code_verifier", &p.verifier),
        ])?;
        self.obtain(c, &encoded, "", true, false).await?;
        self.pending = None;
        Ok(())
    }
    pub(super) async fn connect<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        check(c, false)?;
        self.obtain(
            c,
            "grant_type=client_credentials&scope=READSYSTEM%20WRITESYSTEM",
            "",
            false,
            false,
        )
        .await?;
        self.pending = None;
        Ok(())
    }
    pub(super) async fn access<T: Transport>(&mut self, c: &mut Client<T>) -> Result<String> {
        if tokens(c).is_null() {
            return Err(Error::Invalid(
                "Koppel deze app eerst met myUplink via de instellingen.",
            ));
        }
        let expiry = json::unix_seconds(json::text(tokens(c), "expiry")).unwrap_or(0);
        if c.wall_time()?.saturating_add(300) > expiry {
            let refresh = json::copy(json::text(tokens(c), "refreshToken"))?;
            let encoded = if refresh.is_empty() {
                json::copy("grant_type=client_credentials&scope=READSYSTEM%20WRITESYSTEM")?
            } else {
                form(&[("grant_type", "refresh_token"), ("refresh_token", &refresh)])?
            };
            self.obtain(c, &encoded, &refresh, !refresh.is_empty(), true)
                .await?;
        }
        let access = json::text(tokens(c), "accessToken");
        if access.is_empty() {
            return Err(Error::Invalid(
                "De bewaarde koppeling heeft geen access-token.",
            ));
        }
        Ok(json::copy(access)?)
    }
    async fn obtain<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        encoded: &str,
        keep: &str,
        require_refresh: bool,
        refreshing: bool,
    ) -> Result {
        check(c, false)?;
        let creds = form(&[
            ("client_id", setting(c, "clientId")),
            ("client_secret", setting(c, "clientSecret")),
        ])?;
        let mut request = HttpRequest::get(&join(&[BASE, "/oauth/token"])?)?;
        request.method = json::copy("POST")?;
        request.body = join(&[encoded, "&", &creds])?.into_bytes();
        headers(&mut request, "application/x-www-form-urlencoded")?;
        let response = c.http(request).await?;
        let answer = json::parse(&response.body).map_err(stulp_core::Error::from)?;
        if response.status >= 400 {
            self.error = join(&["myUplink weigert het token: ", json::text(&answer, "error")])?;
            if refreshing
                && matches!(
                    json::text(&answer, "error"),
                    "invalid_grant"
                        | "invalid_client"
                        | "unauthorized_client"
                        | "invalid_scope"
                        | "unsupported_grant_type"
                )
            {
                c.app_state(Value::Null).await?;
                c.call("notification",&json::fields(&[("excerpt",json::string("Nibe: myUplink weigert de koppeling. Controleer de registratie en koppel de app opnieuw.")?)])?).await?;
            }
            return Err(Error::Remote(json::copy(&self.error)?));
        }
        let access = json::text(&answer, "access_token");
        let refresh = match json::text(&answer, "refresh_token") {
            "" => keep,
            s => s,
        };
        let seconds = json::uint(&answer, "expires_in");
        if access.is_empty() || seconds == 0 || (require_refresh && refresh.is_empty()) {
            return Err(Error::Invalid(
                "myUplink stuurde een onvolledig tokenantwoord.",
            ));
        }
        let expiry = c
            .wall_time()?
            .checked_add(seconds)
            .and_then(|v| v.checked_mul(1_000_000_000))
            .ok_or(Error::Invalid("token expiry out of range"))?;
        let saved = json::fields(&[
            ("accessToken", json::string(access)?),
            ("refreshToken", json::string(refresh)?),
            ("scope", clone(field(&answer, "scope"))?),
            ("expiry", json::string(&json::timestamp(expiry)?)?),
        ])?;
        c.app_state(json::fields(&[("tokens", saved)])?).await?;
        self.error.clear();
        Ok(())
    }
    pub(super) async fn request<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        path: &str,
        body: &Value,
    ) -> Result<Value> {
        let token = self.access(c).await?;
        let mut req = HttpRequest::get(&join(&[BASE, path])?)?;
        req.method = json::copy(method)?;
        headers(&mut req, "application/json; charset=utf-8")?;
        json::push(
            &mut req.headers,
            (json::copy("Authorization")?, join(&["Bearer ", &token])?),
            8,
        )?;
        if !body.is_null() {
            req.body = json::to_string(body)
                .map_err(stulp_core::Error::from)?
                .into_bytes();
        }
        let response = c.http(req).await?;
        match response.status {
            401 => {
                return Err(Error::Invalid(
                    "myUplink accepteert het token niet; koppel opnieuw.",
                ));
            }
            403 => {
                return Err(Error::Invalid(
                    "myUplink weigert deze bediening; controleer Premium en WRITESYSTEM.",
                ));
            }
            400..=599 => return Err(Error::Invalid("myUplink heeft het verzoek geweigerd.")),
            _ => (),
        }
        if response.body.is_empty() {
            return Ok(Value::Null);
        }
        json::parse(&response.body).map_err(|e| Error::Core(e.into()))
    }
}
fn headers(req: &mut HttpRequest, content: &str) -> Result {
    for (k, v) in [
        ("Accept", "application/json"),
        ("User-Agent", "com.stulp.nibe/1.0.0"),
        ("Content-Type", content),
    ] {
        json::push(&mut req.headers, (json::copy(k)?, json::copy(v)?), 8)?;
    }
    Ok(())
}
/// Een geplakte code of redirect wordt aan de huidige autorisatie gebonden.
pub fn code_from_redirect(pasted: &str, expected: &str) -> Result<String> {
    let s = pasted.trim();
    if s.is_empty() {
        return Err(Error::Invalid(
            "Plak het adres uit de adresbalk na het inloggen.",
        ));
    }
    let Some((_, query)) = s.split_once('?') else {
        return if s.contains("://") {
            Err(Error::Invalid("Dit adres bevat geen code."))
        } else {
            Ok(json::copy(s)?)
        };
    };
    let mut code = None;
    let mut state = None;
    let mut failure = false;
    for part in query.split('&') {
        let (k, v) = part.split_once('=').unwrap_or((part, ""));
        let k = stulp_sdk::util::unquery(k)?;
        let v = stulp_sdk::util::unquery(v)?;
        match k.as_str() {
            "code" => {
                if code.is_some() {
                    return Err(Error::Invalid("duplicate OAuth code"));
                }
                code = Some(v)
            }
            "state" => {
                if state.is_some() {
                    return Err(Error::Invalid("duplicate OAuth state"));
                }
                state = Some(v)
            }
            "error" => failure = !v.is_empty(),
            _ => (),
        }
    }
    if failure {
        return Err(Error::Invalid("myUplink heeft de autorisatie geweigerd."));
    }
    if state.as_ref().is_some_and(|s| {
        !s.is_empty()
            && !expected.is_empty()
            && !auth::constant_time_eq(s.as_bytes(), expected.as_bytes())
    }) {
        return Err(Error::Invalid(
            "Dit adres hoort bij een andere autorisatie.",
        ));
    }
    code.filter(|s| !s.is_empty())
        .ok_or(Error::Invalid("Dit adres bevat geen code."))
}
