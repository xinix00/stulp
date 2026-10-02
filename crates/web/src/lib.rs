//! De bestaande Stulp-interface en transportvrije HTTP-contracten.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};

mod api;
mod capability;
mod cards;
mod filter;
mod flow_units;
mod titles;
pub use cards::catalog as flow_cards;
mod manager;
mod measures;
pub use measures::show as show_measures;
pub mod mcp;
mod objects;
mod scenes;
mod views;
pub use capability::input as capability_input;

/// Hetzelfde browserobject na een geslaagde asynchrone koppeling.
pub fn device_object<S: Storage>(store: &Store<S>, id: &str) -> Result<Value> {
    objects::device(store, id)
}

/// Gebeurtenissen voor een SSE-abonnee; verlies vraagt expliciet om herladen.
pub fn events<S: Storage>(store: &Store<S>, cursor: u64) -> Result<String> {
    events_view(store, cursor, "", false)
}

/// Managerfilters behouden reloadsignalen; de tegelstream stuurt waarden zonder alle metadata.
pub fn events_view<S: Storage>(
    store: &Store<S>,
    cursor: u64,
    manager: &str,
    overview: bool,
) -> Result<String> {
    use core::fmt::Write;
    let mut out = String::new();
    let events = match store.events_after(cursor) {
        Ok(events) => events,
        Err(Error::Changed) => {
            return json::copy(
                "event: store.reload\ndata: {\"manager\":\"store\",\"type\":\"store.reload\"}\n\n",
            );
        }
        Err(e) => return Err(e),
    };
    for event in events {
        if !manager.is_empty() && manager != event.manager && event.manager != "store" {
            continue;
        }
        let data = if event.manager == "devices" && event.kind == "device.update" {
            match views::device(store, &event.id, if overview { "overview" } else { "" }) {
                Ok(mut device) => {
                    if overview {
                        json::set(
                            &mut device,
                            "capabilityValues",
                            copy(json::get(&store.device(&event.id)?, "state"))?,
                        )?;
                    }
                    device
                }
                Err(_) => Value::Null,
            }
        } else if event.kind == "notification.create" {
            copy(store.document().record("notifications", &event.id).ok())?
        } else {
            Value::Null
        };
        let value = json::fields(&[
            ("manager", json::string(event.manager)?),
            ("type", json::string(event.kind)?),
            ("id", json::string(&event.id)?),
            ("data", data),
        ])?;
        let encoded = json::to_string(&value)?;
        let needed = encoded.len() + event.kind.len() + 64;
        if out.len().saturating_add(needed) > json::MAX_DOCUMENT {
            return json::copy(
                "event: store.reload\ndata: {\"manager\":\"store\",\"type\":\"store.reload\"}\n\n",
            );
        }
        out.try_reserve(needed).map_err(|_| Error::Memory)?;
        write!(
            out,
            "id: {}\nevent: {}\ndata: {}\n\n",
            event.sequence, event.kind, encoded
        )
        .map_err(|_| Error::Full)?;
    }
    Ok(out)
}

/// Eerste waarde uit een formulierquery, inclusief percentcodering en plustekens.
pub fn query_value(query: &str, key: &str) -> Result<String> {
    fn decode(s: &str) -> Result<String> {
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(s.len())
            .map_err(|_| Error::Memory)?;
        let mut source = s.as_bytes().iter().copied();
        while let Some(b) = source.next() {
            bytes.push(match b {
                b'+' => b' ',
                b'%' => {
                    let mut hex = || {
                        source
                            .next()
                            .and_then(|n| char::from(n).to_digit(16))
                            .ok_or(Error::Invalid("invalid query escape"))
                    };
                    (hex()? * 16 + hex()?) as u8
                }
                n => n,
            });
        }
        String::from_utf8(bytes).map_err(|_| Error::Invalid("query is not UTF-8"))
    }
    for part in query.split('&') {
        let (k, v) = part.split_once('=').unwrap_or((part, ""));
        if decode(k)? == key {
            return decode(v);
        }
    }
    Ok(String::new())
}

/// Verzoekgegevens gaan als één eigendomsoverdracht naar de controller.
pub struct Request {
    /// HTTP-methode.
    pub method: String,
    /// Gedecodeerd pad zonder query.
    pub path: String,
    /// Rauwe query voor app-API's en koppelsessies.
    pub query: String,
    /// Browsercookie, nooit een Bearer-token.
    pub cookie: String,
    /// Host-header voor origincontrole.
    pub host: String,
    /// Eventuele Origin-header.
    pub origin: String,
    /// Selectieve protocolheaders, met kleine letters als sleutel.
    pub headers: Value,
    /// Begrensde body.
    pub body: Vec<u8>,
}

/// Statische assets worden niet per verzoek gekopieerd.
pub enum Body {
    /// Ingebakken frontend.
    Static(&'static [u8]),
    /// Binaire app-assets.
    Bytes(Vec<u8>),
    /// Geserialiseerde gegevens.
    Owned(String),
    /// Een geautoriseerde backup streamt een vaste documentkopie en zijn bundels.
    Backup(String),
    /// De host valideert een upload buiten de controllertaak, voor publicatie.
    Restore {
        /// Het vastgestelde documentpad.
        destination: String,
        /// Het nog niet vertrouwde archief.
        archive: Vec<u8>,
    },
    /// Door de adapter te streamen bron; bytes() is leeg en mag hiervoor niet worden gebruikt.
    Proxy {
        /// Private HTTP(S)-bron.
        url: String,
        /// Gevalideerd type van de app.
        mime: String,
        /// Capaciteitshandvat van de controller.
        owner: u64,
    },
}
impl Body {
    /// Bytes voor iedere HTTP-adapter.
    pub fn bytes(&self) -> &[u8] {
        match self {
            Self::Static(b) => b,
            Self::Bytes(b) => b,
            Self::Owned(s) => s.as_bytes(),
            Self::Proxy { .. } | Self::Backup(_) | Self::Restore { .. } => &[],
        }
    }
}

/// Het antwoord bevat alleen transportmetadata en bytes.
pub struct Response {
    /// HTTP-statuscode.
    pub status: u16,
    /// MIME-type.
    pub content_type: &'static str,
    /// Payload.
    pub body: Body,
    /// Alleen aanwezig na het bezoeken van de sleutel-URL.
    pub cookie: Option<String>,
    /// Aanvullende protocolheaders met vaste namen.
    pub headers: Vec<(&'static str, String)>,
}

impl Response {
    /// Encodeert één JSON-antwoord.
    pub fn json(status: u16, value: &Value) -> Result<Self> {
        Ok(Self {
            status,
            content_type: "application/json; charset=utf-8",
            body: Body::Owned(json::to_string(value)?),
            cookie: None,
            headers: Vec::new(),
        })
    }
    /// Fouten behouden de browser-API-vorm.
    pub fn error(status: u16, message: &str) -> Result<Self> {
        Self::json(
            status,
            &json::fields(&[
                ("error", json::string(message)?),
                ("error_description", json::string(message)?),
            ])?,
        )
    }
}

/// Door de adapter geleverde klok en entropy; de kern roept geen besturingssysteem aan.
pub trait Environment {
    /// Verse UUID per nieuw object.
    fn id(&mut self) -> Result<String>;
    /// RFC3339 UTC-tijd voor configuratierevisies.
    fn now(&self) -> Result<String>;
}

/// Bevat geen huisstaat; alleen de browsertoegangsconfiguratie.
pub struct Web {
    token: String,
    proof: String,
}

impl Web {
    /// Bouwt dezelfde HttpOnly-sessie als de Go-versie.
    pub fn new(token: &str) -> Result<Self> {
        let mut input = json::copy("stulp web session\0")?;
        input.try_reserve(token.len()).map_err(|_| Error::Memory)?;
        input.push_str(token);
        let mut proof = String::new();
        proof.try_reserve_exact(64).map_err(|_| Error::Memory)?;
        const HEX: &[u8; 16] = b"0123456789abcdef";
        for b in auth::sha256(input.as_bytes()) {
            proof.push(char::from(
                *HEX.get(usize::from(b >> 4)).ok_or(Error::Full)?,
            ));
            proof.push(char::from(
                *HEX.get(usize::from(b & 15)).ok_or(Error::Full)?,
            ));
        }
        Ok(Self {
            token: json::copy(token)?,
            proof,
        })
    }

    /// Eén request wordt synchroon door de staatseigenaar uitgevoerd.
    pub fn handle<S: Storage>(
        &self,
        store: &mut Store<S>,
        request: &Request,
        env: &mut impl Environment,
    ) -> Result<Response> {
        if request.path.starts_with("/mcp/") {
            if self.token.is_empty()
                || !stulp_protocol::token::equal(
                    request.path.trim_start_matches("/mcp/"),
                    &self.token,
                )
            {
                return Response::error(404, "not found");
            }
            return match mcp::decode(request)? {
                mcp::Dispatch::Reply(reply) => Ok(reply),
                mcp::Dispatch::Tool(_) => {
                    Response::error(404, "MCP tool requires controller callback")
                }
            };
        }
        if !self.token.is_empty()
            && request.method == "GET"
            && stulp_protocol::token::equal(
                request.path.strip_prefix('/').unwrap_or(""),
                &self.token,
            )
        {
            let mut response = asset("/").ok_or(Error::Missing("UI"))?;
            let mut cookie = json::copy("stulp-session=")?;
            let suffix = "; Path=/; HttpOnly; SameSite=Strict; Max-Age=31536000";
            cookie
                .try_reserve(self.proof.len() + suffix.len())
                .map_err(|_| Error::Memory)?;
            cookie.push_str(&self.proof);
            cookie.push_str(suffix);
            response.cookie = Some(cookie);
            return Ok(response);
        }
        let public = request.path.starts_with("/assets/")
            || (request.method == "GET" && request.path.starts_with("/image/"))
            || matches!(
                request.path.as_str(),
                "/sw.js" | "/manifest.webmanifest" | "/stulp.js"
            );
        if !public && !self.token.is_empty() && !self.has_cookie(&request.cookie) {
            return Response::error(
                if request.path.starts_with("/api/") {
                    401
                } else {
                    404
                },
                "open /<key> before using Stulp",
            );
        }
        if !matches!(request.method.as_str(), "GET" | "HEAD" | "OPTIONS")
            && !request.origin.is_empty()
        {
            let origin_host = request
                .origin
                .strip_prefix("https://")
                .or_else(|| request.origin.strip_prefix("http://"));
            if origin_host != Some(request.host.as_str()) {
                return Response::error(403, "cross-origin write refused");
            }
        }
        if matches!(request.method.as_str(), "GET" | "HEAD")
            && let Some(asset) = asset(&request.path)
        {
            return Ok(asset);
        }
        match api::dispatch(store, request, env) {
            Ok(response) => Ok(response),
            Err(e) => {
                let status = match e {
                    Error::Missing(_) => 404,
                    Error::Changed | Error::Conflict(_) => 409,
                    Error::Storage => 503,
                    _ => 400,
                };
                use core::fmt::Write;
                let mut message = String::new();
                message.try_reserve(512).map_err(|_| Error::Memory)?;
                write!(message, "{e}").map_err(|_| Error::Full)?;
                Response::error(status, &message)
            }
        }
    }

    /// MCP gebruikt de URL-sleutel en heeft geen browsersessie nodig.
    pub fn mcp_authenticated(&self, request: &Request) -> bool {
        !self.token.is_empty()
            && request
                .path
                .strip_prefix("/mcp/")
                .is_some_and(|key| stulp_protocol::token::equal(key, &self.token))
    }

    /// Extra hostroutes gebruiken precies dezelfde sessiecontrole als de basis-API.
    pub fn authenticated(&self, request: &Request) -> bool {
        self.token.is_empty() || self.has_cookie(&request.cookie)
    }
    fn has_cookie(&self, cookie: &str) -> bool {
        cookie
            .split(';')
            .filter_map(|part| part.trim().split_once('='))
            .any(|(name, value)| {
                name == "stulp-session" && stulp_protocol::token::equal(value, &self.proof)
            })
    }
}

fn asset(path: &str) -> Option<Response> {
    let (content_type, bytes): (&str, &[u8]) = match path {
        "/" | "/app" => (
            "text/html; charset=utf-8",
            include_bytes!("../ui/index.html"),
        ),
        "/assets/app.js" => (
            "text/javascript; charset=utf-8",
            include_bytes!("../ui/app.js"),
        ),
        "/assets/style.css" => ("text/css; charset=utf-8", include_bytes!("../ui/style.css")),
        "/assets/app-frame.css" => (
            "text/css; charset=utf-8",
            include_bytes!("../ui/app-frame.css"),
        ),
        "/stulp.js" | "/assets/stulp.js" => (
            "text/javascript; charset=utf-8",
            include_bytes!("../ui/stulp.js"),
        ),
        "/sw.js" => (
            "text/javascript; charset=utf-8",
            include_bytes!("../ui/sw.js"),
        ),
        "/manifest.webmanifest" => (
            "application/manifest+json",
            include_bytes!("../ui/manifest.webmanifest"),
        ),
        "/assets/icon-192.png" => ("image/png", include_bytes!("../ui/icon-192.png")),
        "/assets/icon-512.png" => ("image/png", include_bytes!("../ui/icon-512.png")),
        "/assets/stulp.svg" => ("image/svg+xml", include_bytes!("../ui/stulp.svg")),
        _ => return None,
    };
    Some(Response {
        status: 200,
        content_type,
        body: Body::Static(bytes),
        cookie: None,
        headers: Vec::new(),
    })
}

fn copy(value: Option<&Value>) -> Result<Value> {
    Ok(match value {
        Some(v) => v.try_clone()?,
        None => Value::Null,
    })
}
