//! Apple Push Notification service: de pushdienst van een eigen iOS-app.
//!
//! Web Push bereikt een webapp op het beginscherm, maar zonder dringende of
//! kritieke meldingen en zonder beeld op iOS. APNs bereikt de Stulp-app zelf.
//! Wat hier staat is wat Stulp er zonder netwerk van kan maken: de sleutel uit
//! de .p8 van Apple, het ondertekende token (JWT, ES256), de JSON-payload en
//! het HTTP/2-verzoek. Verzenden doet de plugin via [`stulp_sdk::Client::http`].
use alloc::{string::String, vec::Vec};
use p256::ecdsa::{Signature, SigningKey, signature::Signer};
use stulp_core::json::{self, Value};
use stulp_sdk::{Error, HttpRequest, Result, util::join};
use zeroize::Zeroizing;

/// De bundle-id van de app; APNs heet dat het topic.
pub const TOPIC: &str = "software.haas.stulp";
/// APNs weigert payloads boven 4 KiB.
pub const MAX_PAYLOAD: usize = 4096;
/// Apple wil een token niet vaker dan eens per 20 minuten vernieuwd zien, en
/// weigert het na een uur; halverwege vernieuwen houdt beide grenzen.
pub const TOKEN_SECONDS: u64 = 40 * 60;

/// Hoe hard een melding binnenkomt.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Level {
    /// Gewoon: valt weg in een Focus die Stulp niet doorlaat.
    Active,
    /// Dringend: breekt door Focus heen, niet door stil.
    TimeSensitive,
    /// Kritiek: ook door stil en Focus, met geluid. Vereist Apple's goedkeuring
    /// van de app; zonder die entitlement toont iOS hem als gewoon.
    Critical,
}
impl Level {
    /// Uit de keuzelijst van de flowkaart; onbekend is gewoon.
    pub fn parse(id: &str) -> Self {
        match id {
            "time-sensitive" | "urgent" => Self::TimeSensitive,
            "critical" => Self::Critical,
            _ => Self::Active,
        }
    }
    fn name(self) -> &'static str {
        match self {
            Self::Active => "active",
            Self::TimeSensitive => "time-sensitive",
            Self::Critical => "critical",
        }
    }
}

/// De ruwe P-256-sleutel uit Apple's .p8: PEM rond PKCS#8 rond een SEC1
/// ECPrivateKey. Alleen die vorm; al het andere is een fout, geen gok.
pub fn key_from_p8(pem: &str) -> Result<Zeroizing<[u8; 32]>> {
    let mut armored = String::new();
    armored
        .try_reserve(pem.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    let mut inside = false;
    for line in pem.lines() {
        let line = line.trim();
        if line == "-----BEGIN PRIVATE KEY-----" {
            inside = true;
        } else if line == "-----END PRIVATE KEY-----" {
            break;
        } else if inside {
            armored.push_str(line);
        }
    }
    if armored.is_empty() {
        return Err(Error::Invalid("APNs-sleutel: geen PRIVATE KEY in de .p8"));
    }
    let der = Wipe(
        leanbase64::STANDARD
            .decode(armored.as_bytes())
            .map_err(|_| Error::Invalid("APNs-sleutel: ongeldige base64"))?,
    );
    let bad = || Error::Invalid("APNs-sleutel: geen PKCS#8 P-256-sleutel");
    // PrivateKeyInfo ::= SEQUENCE { version 0, AlgorithmIdentifier, OCTET STRING }
    let mut outer = Der(&der.0);
    let mut info = Der(outer.take(0x30).ok_or_else(bad)?);
    if info.take(0x02).ok_or_else(bad)? != [0] {
        return Err(bad());
    }
    let algorithm = info.take(0x30).ok_or_else(bad)?;
    // id-ecPublicKey met de curve prime256v1.
    const EC: &[u8] = &[0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01];
    const P256: &[u8] = &[0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07];
    if !algorithm.starts_with(EC) || !algorithm[EC.len()..].starts_with(P256) {
        return Err(bad());
    }
    // ECPrivateKey ::= SEQUENCE { version 1, privateKey OCTET STRING (32), ... }
    let mut ec = Der(info.take(0x04).ok_or_else(bad)?);
    let mut ec = Der(ec.take(0x30).ok_or_else(bad)?);
    if ec.take(0x02).ok_or_else(bad)? != [1] {
        return Err(bad());
    }
    let scalar = ec.take(0x04).ok_or_else(bad)?;
    let mut key = Zeroizing::new([0u8; 32]);
    if scalar.len() != 32 {
        return Err(bad());
    }
    key.copy_from_slice(scalar);
    SigningKey::from_slice(key.as_slice()).map_err(|_| bad())?;
    Ok(key)
}

/// Wist de gedecodeerde sleutelbytes bij elk vertrek, ook bij een fout.
struct Wipe(Vec<u8>);
impl Drop for Wipe {
    fn drop(&mut self) {
        super::erase(&mut self.0);
    }
}

/// Een minimale DER-lezer voor korte en lange lengtes tot 64 KiB.
struct Der<'a>(&'a [u8]);
impl<'a> Der<'a> {
    fn take(&mut self, tag: u8) -> Option<&'a [u8]> {
        let (&first, rest) = self.0.split_first()?;
        if first != tag {
            return None;
        }
        let (&len, mut rest) = rest.split_first()?;
        let len = match len {
            n if n < 0x80 => usize::from(n),
            0x81 => {
                let (&n, r) = rest.split_first()?;
                rest = r;
                usize::from(n)
            }
            0x82 => {
                let (n, r) = rest.split_at_checked(2)?;
                rest = r;
                usize::from(u16::from_be_bytes([n[0], n[1]]))
            }
            _ => return None,
        };
        let (value, rest) = rest.split_at_checked(len)?;
        self.0 = rest;
        Some(value)
    }
}

/// Het provider-token voor APNs: ES256, met de Key ID en Team ID van Apple.
pub fn token(key: &[u8; 32], key_id: &str, team_id: &str, now: u64) -> Result<String> {
    if !valid_id(key_id) || !valid_id(team_id) {
        return Err(Error::Invalid("APNs: Key ID en Team ID zijn tien tekens"));
    }
    let signing = SigningKey::from_slice(key).map_err(|_| Error::Invalid("invalid APNs key"))?;
    let header = json::fields(&[
        ("alg", json::string("ES256")?),
        ("kid", json::string(key_id)?),
    ])?;
    let claims = json::fields(&[("iss", json::string(team_id)?), ("iat", Value::uint(now))])?;
    let input = join(&[
        &url(json::to_string(&header)
            .map_err(stulp_core::Error::from)?
            .as_bytes())?,
        ".",
        &url(json::to_string(&claims)
            .map_err(stulp_core::Error::from)?
            .as_bytes())?,
    ])?;
    let signature: Signature = signing.sign(input.as_bytes());
    join(&[&input, ".", &url(&signature.to_bytes())?])
}

fn valid_id(id: &str) -> bool {
    id.len() == 10
        && id
            .bytes()
            .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit())
}

fn url(bytes: &[u8]) -> Result<String> {
    leanbase64::URL
        .encode(bytes)
        .map_err(|_| Error::Core(stulp_core::Error::Memory))
}

/// Wat een melding meeneemt naast tekst.
#[derive(Default)]
pub struct Extras<'a> {
    /// Adres van een snapshot; de extensie in de app haalt hem op.
    pub image: Option<&'a str>,
    /// De camera waar het beeld vandaan komt; tikken opent hem.
    pub camera: Option<&'a str>,
}

/// De JSON-payload, met `mutable-content` als er beeld bij hoort zodat de
/// extensie het kan ophalen. Begrensd op [`MAX_PAYLOAD`].
pub fn payload(title: &str, body: &str, level: Level, extras: &Extras<'_>) -> Result<Vec<u8>> {
    let alert = json::fields(&[
        ("title", json::string(title)?),
        ("body", json::string(body)?),
    ])?;
    let sound = if level == Level::Critical {
        json::fields(&[
            ("critical", Value::uint(1)),
            ("name", json::string("default")?),
            ("volume", Value::Number(json::Number::Float(1.0))),
        ])?
    } else {
        json::string("default")?
    };
    let mut aps = json::fields(&[
        ("alert", alert),
        ("sound", sound),
        ("interruption-level", json::string(level.name())?),
    ])?;
    let mut stulp = json::object();
    if let Some(image) = extras.image.filter(|s| !s.is_empty()) {
        json::set(&mut aps, "mutable-content", Value::uint(1))?;
        json::set(&mut stulp, "image", json::string(image)?)?;
    }
    if let Some(camera) = extras.camera.filter(|s| !s.is_empty()) {
        json::set(&mut stulp, "camera", json::string(camera)?)?;
    }
    let mut root = json::fields(&[("aps", aps)])?;
    if stulp.as_object().is_some_and(|o| !o.is_empty()) {
        json::set(&mut root, "stulp", stulp)?;
    }
    let bytes = json::to_string(&root)
        .map_err(stulp_core::Error::from)?
        .into_bytes();
    if bytes.len() > MAX_PAYLOAD {
        return Err(Error::Invalid("APNs-payload groter dan 4 KiB"));
    }
    Ok(bytes)
}

/// Waar het push-token van de app thuishoort: een ontwikkelbuild uit Xcode
/// praat met de sandbox, TestFlight en de App Store met productie.
pub fn host(environment: &str) -> &'static str {
    if environment == "development" {
        "api.sandbox.push.apple.com"
    } else {
        "api.push.apple.com"
    }
}

/// Het HTTP/2-verzoek naar APNs voor één toestel.
pub fn request(
    device_token: &str,
    environment: &str,
    provider_token: &str,
    level: Level,
    payload: Vec<u8>,
) -> Result<HttpRequest> {
    if device_token.is_empty()
        || device_token.len() > 200
        || !device_token.bytes().all(|b| b.is_ascii_hexdigit())
    {
        return Err(Error::Invalid("APNs: ongeldig push-token van de app"));
    }
    let url = join(&["https://", host(environment), "/3/device/", device_token])?;
    let mut req = HttpRequest::get(&url)?;
    req.method = json::copy("POST")?;
    req.http2 = true;
    req.body = payload;
    req.limit = 4096;
    req.timeout_ms = 15_000;
    let authorization = join(&["bearer ", provider_token])?;
    for (k, v) in [
        ("authorization", authorization.as_str()),
        ("apns-topic", TOPIC),
        ("apns-push-type", "alert"),
        // 10: direct bezorgen. Ook voor gewone meldingen; een alarm wacht niet.
        ("apns-priority", "10"),
        ("apns-expiration", "0"),
        ("content-type", "application/json"),
    ] {
        json::push(&mut req.headers, (json::copy(k)?, json::copy(v)?), 8)?;
    }
    let _ = level;
    Ok(req)
}

/// APNs' reden bij een weigering, uit `{"reason": "..."}`; leeg als er geen is.
pub fn reason(body: &[u8]) -> Result<String> {
    let Ok(value) = json::parse(body) else {
        return Ok(String::new());
    };
    Ok(json::copy(json::text(&value, "reason"))?)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]
    use super::*;
    use p256::ecdsa::{VerifyingKey, signature::Verifier};

    // Een wegwerpsleutel, alleen voor deze test (openssl ecparam | pkcs8 -topk8).
    const P8: &str = "-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgzRteCB4XmtDpgVBO
7K8buoOvibGr2+nL9G64fzpBVnihRANCAASC0GkbvAr1imZsQi1l8MdMhkfWH+Rq
Vhiy3U8AKNG9+YkzASqCdH3vnXFZuzueTR7iBQkAMJDvuHZKUskavti3
-----END PRIVATE KEY-----
";

    #[test]
    fn the_p8_yields_its_raw_scalar_and_rejects_anything_else() -> Result {
        let key = key_from_p8(P8)?;
        assert_eq!(
            key.as_slice(),
            [
                0xcd, 0x1b, 0x5e, 0x08, 0x1e, 0x17, 0x9a, 0xd0, 0xe9, 0x81, 0x50, 0x4e, 0xec, 0xaf,
                0x1b, 0xba, 0x83, 0xaf, 0x89, 0xb1, 0xab, 0xdb, 0xe9, 0xcb, 0xf4, 0x6e, 0xb8, 0x7f,
                0x3a, 0x41, 0x56, 0x78,
            ]
        );
        assert!(key_from_p8("").is_err());
        assert!(
            key_from_p8("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----").is_err()
        );
        Ok(())
    }

    #[test]
    fn the_provider_token_is_a_verifiable_es256_jwt() -> Result {
        let key = key_from_p8(P8)?;
        let jwt = token(&key, "ABC123DEFG", "QVY3KR44QS", 1_790_000_000)?;
        let parts: Vec<&str> = jwt.split('.').collect();
        assert_eq!(parts.len(), 3);
        let header = json::parse(&leanbase64::URL.decode(parts[0].as_bytes()).unwrap()).unwrap();
        assert_eq!(json::text(&header, "alg"), "ES256");
        assert_eq!(json::text(&header, "kid"), "ABC123DEFG");
        let claims = json::parse(&leanbase64::URL.decode(parts[1].as_bytes()).unwrap()).unwrap();
        assert_eq!(json::text(&claims, "iss"), "QVY3KR44QS");
        assert_eq!(json::uint(&claims, "iat"), 1_790_000_000);
        let signature =
            Signature::from_slice(&leanbase64::URL.decode(parts[2].as_bytes()).unwrap()).unwrap();
        let verifying = VerifyingKey::from(&SigningKey::from_slice(key.as_slice()).unwrap());
        let input = join(&[parts[0], ".", parts[1]])?;
        assert!(verifying.verify(input.as_bytes(), &signature).is_ok());
        assert!(token(&key, "short", "QVY3KR44QS", 0).is_err());
        Ok(())
    }

    #[test]
    fn the_payload_carries_level_image_and_camera() -> Result {
        let plain = json::parse(&payload(
            "Stulp",
            "Hallo",
            Level::Active,
            &Extras::default(),
        )?)
        .unwrap();
        let aps = json::get(&plain, "aps").unwrap();
        assert_eq!(json::text(aps, "interruption-level"), "active");
        assert!(json::get(aps, "mutable-content").is_none());
        assert!(json::get(&plain, "stulp").is_none());

        let rich = json::parse(&payload(
            "Voordeur",
            "Iemand belt aan",
            Level::TimeSensitive,
            &Extras {
                image: Some("https://x/image/abc"),
                camera: Some("cam-1"),
            },
        )?)
        .unwrap();
        let aps = json::get(&rich, "aps").unwrap();
        assert_eq!(json::text(aps, "interruption-level"), "time-sensitive");
        assert_eq!(json::uint(aps, "mutable-content"), 1);
        let stulp = json::get(&rich, "stulp").unwrap();
        assert_eq!(json::text(stulp, "image"), "https://x/image/abc");
        assert_eq!(json::text(stulp, "camera"), "cam-1");

        let critical = json::parse(&payload(
            "Alarm",
            "Inbraak",
            Level::Critical,
            &Extras::default(),
        )?)
        .unwrap();
        let sound = json::get(json::get(&critical, "aps").unwrap(), "sound").unwrap();
        assert_eq!(json::uint(sound, "critical"), 1);

        let long = "x".repeat(5000);
        assert!(payload("t", &long, Level::Active, &Extras::default()).is_err());
        Ok(())
    }

    #[test]
    fn the_request_targets_the_right_host_over_http2() -> Result {
        let req = request("a1b2c3", "development", "jwt", Level::Active, Vec::new())?;
        assert!(req.http2);
        assert_eq!(
            req.url,
            "https://api.sandbox.push.apple.com/3/device/a1b2c3"
        );
        assert!(
            req.headers
                .iter()
                .any(|(k, v)| k == "apns-topic" && v == TOPIC)
        );
        assert!(
            req.headers
                .iter()
                .any(|(k, v)| k == "authorization" && v == "bearer jwt")
        );
        let prod = request("a1b2c3", "production", "jwt", Level::Active, Vec::new())?;
        assert_eq!(prod.url, "https://api.push.apple.com/3/device/a1b2c3");
        assert!(request("not hex!", "production", "jwt", Level::Active, Vec::new()).is_err());
        assert_eq!(reason(br#"{"reason":"BadDeviceToken"}"#)?, "BadDeviceToken");
        Ok(())
    }
}
