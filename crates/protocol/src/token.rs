//! Bestaande Go-HMAC-domeinen en base64url, zonder geheimen over de verbinding.
use alloc::{string::String, vec::Vec};
use stulp_core::{Error, Result, json};

/// Base64url zonder padding, onder meer voor 32 willekeurige noncebytes.
pub fn base64(bytes: &[u8]) -> Result<String> {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::new();
    out.try_reserve(bytes.len().checked_mul(4).ok_or(Error::Full)?.div_ceil(3))
        .map_err(|_| Error::Memory)?;
    for chunk in bytes.chunks(3) {
        let a = *chunk.first().ok_or(Error::Full)?;
        let b = chunk.get(1).copied().unwrap_or(0);
        let c = chunk.get(2).copied().unwrap_or(0);
        for (index, n) in [
            a >> 2,
            ((a & 3) << 4) | (b >> 4),
            ((b & 15) << 2) | (c >> 6),
            c & 63,
        ]
        .iter()
        .enumerate()
        {
            if index <= chunk.len() {
                out.push(char::from(*TABLE.get(usize::from(*n)).ok_or(Error::Full)?));
            }
        }
    }
    Ok(out)
}

fn mac(key: &str, fields: &[&str]) -> Result<String> {
    let mut bytes = Vec::new();
    for field in fields {
        for b in field.as_bytes() {
            json::push(&mut bytes, *b, 16384)?;
        }
        json::push(&mut bytes, 0, 16384)?;
    }
    base64(&auth::hmac_sha256(key.as_bytes(), &bytes))
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
    Ok(auth::constant_time_eq(
        expected.as_bytes(),
        offered.as_bytes(),
    ))
}

/// Constante-tijd vergelijking voor browsercookies en wederzijdse bewijzen.
pub fn equal(a: &str, b: &str) -> bool {
    auth::constant_time_eq(a.as_bytes(), b.as_bytes())
}

/// Begrensde base64-decoder voor binaire app-assets, beide gebruikelijke alfabetten.
pub fn decode(raw: &str) -> Result<Vec<u8>> {
    let raw = raw.trim();
    let s = raw.trim_end_matches('=');
    if s.len() > super::MAX_FRAME || s.len() % 4 == 1 || raw.len() - s.len() > 2 {
        return Err(Error::Invalid("invalid base64"));
    }
    let mut out = Vec::new();
    out.try_reserve(s.len() * 3 / 4)
        .map_err(|_| Error::Memory)?;
    let mut bits = 0u32;
    let mut count = 0;
    for b in s.bytes() {
        let v = match b {
            b'A'..=b'Z' => b - b'A',
            b'a'..=b'z' => b - b'a' + 26,
            b'0'..=b'9' => b - b'0' + 52,
            b'+' | b'-' => 62,
            b'/' | b'_' => 63,
            _ => return Err(Error::Invalid("invalid base64")),
        };
        bits = (bits << 6) | u32::from(v);
        count += 6;
        if count >= 8 {
            count -= 8;
            out.push((bits >> count) as u8);
            bits &= (1 << count) - 1;
        }
    }
    if bits != 0 {
        return Err(Error::Invalid("invalid base64 tail"));
    }
    Ok(out)
}
