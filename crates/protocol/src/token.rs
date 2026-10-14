//! Bestaande Go-HMAC-domeinen en base64url, zonder geheimen over de verbinding.
//! HMAC-SHA256 en de vergelijking komen uit `leancrypto`, base64 uit `leanbase64`.
use alloc::{string::String, vec::Vec};
use stulp_core::{Error, Result, json};

/// Base64url zonder padding, onder meer voor 32 willekeurige noncebytes.
pub fn base64(bytes: &[u8]) -> Result<String> {
    leanbase64::URL.encode(bytes).map_err(|_| Error::Memory)
}

fn mac(key: &str, fields: &[&str]) -> Result<String> {
    let mut bytes = Vec::new();
    for field in fields {
        for b in field.as_bytes() {
            json::push(&mut bytes, *b, 16384)?;
        }
        json::push(&mut bytes, 0, 16384)?;
    }
    base64(&leancrypto::hmac::HmacSha256::mac(key.as_bytes(), &bytes))
}

/// Het token is gebonden aan precies één app-id; de nulbyte hoort bij het wirecontract.
pub fn token(secret: &str, app_id: &str) -> Result<String> {
    mac(secret, &["token", app_id])
}

/// De richting voorkomt reflectie van een appbewijs als controllerbewijs.
#[derive(Clone, Copy)]
pub enum Direction {
    /// App bewijst zich aan Stulp.
    App,
    /// Stulp bewijst zich aan de app.
    Stulp,
}

/// Antwoord op een verse nonce, met afgebakende velden.
pub fn proof(token: &str, direction: Direction, nonce: &str, app_id: &str) -> Result<String> {
    mac(
        token,
        &[
            match direction {
                Direction::App => "app",
                Direction::Stulp => "stulp",
            },
            nonce,
            app_id,
        ],
    )
}

/// Een leeg geheim of nonce authenticeert niemand.
pub fn check(secret: &str, app_id: &str, nonce: &str, offered: &str) -> Result<bool> {
    if secret.is_empty() || nonce.is_empty() || offered.is_empty() {
        return Ok(false);
    }
    let expected = proof(&token(secret, app_id)?, Direction::App, nonce, app_id)?;
    Ok(leancrypto::ct::eq(expected.as_bytes(), offered.as_bytes()))
}

/// Constante-tijd vergelijking voor browsercookies en wederzijdse bewijzen.
pub fn equal(a: &str, b: &str) -> bool {
    leancrypto::ct::eq(a.as_bytes(), b.as_bytes())
}

/// Begrensde base64-decoder voor binaire app-assets, beide gebruikelijke alfabetten.
///
/// Ruimer dan `leanbase64` met opzet, zoals het Go-koppelpad: witruimte
/// eromheen, `+/` en `-_` door elkaar, en hoogstens twee `=` aan het eind
/// waar die ook ontbreken mogen. De rest (lengte, ongebruikte bits) toetst
/// `leanbase64::URL` strikt.
pub fn decode(raw: &str) -> Result<Vec<u8>> {
    let raw = raw.trim();
    let s = raw.trim_end_matches('=');
    if s.len() > super::MAX_FRAME || raw.len() - s.len() > 2 {
        return Err(Error::Invalid("invalid base64"));
    }
    let mut url = Vec::new();
    url.try_reserve_exact(s.len()).map_err(|_| Error::Memory)?;
    url.extend(s.bytes().map(|b| match b {
        b'+' => b'-',
        b'/' => b'_',
        b => b,
    }));
    leanbase64::URL.decode(&url).map_err(|e| match e {
        leanbase64::Error::Trailing => Error::Invalid("invalid base64 tail"),
        leanbase64::Error::OutOfMemory => Error::Memory,
        _ => Error::Invalid("invalid base64"),
    })
}
