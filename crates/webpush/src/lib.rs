//! Port van internal/webpush: RFC 8291-berichten en RFC 8292-VAPID.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use aes_gcm::{Aes128Gcm, KeyInit, Nonce, aead::AeadInPlace};
use alloc::{string::String, vec::Vec};
use hkdf::Hkdf;
use p256::{
    PublicKey, SecretKey,
    ecdsa::{Signature, SigningKey, signature::Signer},
    elliptic_curve::sec1::ToEncodedPoint,
};
use sha2::Sha256;
use stulp_core::json::{self, Value};
use stulp_protocol::token::base64;
use stulp_sdk::{
    Error, Result,
    util::{field, join},
};
use zeroize::Zeroizing;
/// De originele 4096-byte limiet minus header, afsluitbyte en authenticatietag.
pub const MAX_PAYLOAD: usize = 3993;
/// Tijdelijke sleutelbytes worden ook bij fouten gewist.
pub fn erase(bytes: &mut [u8]) {
    use zeroize::Zeroize;
    bytes.zeroize();
}
/// Zowel gewone base64 als base64url, met optionele padding zoals het Go-koppelpad.
pub fn decode(s: &str) -> Result<Vec<u8>> {
    let raw = s.trim();
    let s = raw.trim_end_matches('=');
    if s.len() > 16384 || s.len() % 4 == 1 || raw.len() - s.len() > 2 {
        return Err(Error::Invalid("invalid base64 key"));
    }
    let mut bytes = Vec::new();
    bytes
        .try_reserve(s.len() * 3 / 4)
        .map_err(|_| stulp_core::Error::Memory)?;
    let mut bits = 0u32;
    let mut count = 0;
    for b in s.bytes() {
        let v = match b {
            b'A'..=b'Z' => b - b'A',
            b'a'..=b'z' => b - b'a' + 26,
            b'0'..=b'9' => b - b'0' + 52,
            b'-' | b'+' => 62,
            b'_' | b'/' => 63,
            _ => return Err(Error::Invalid("invalid base64 key")),
        };
        bits = (bits << 6) | u32::from(v);
        count += 6;
        if count >= 8 {
            count -= 8;
            bytes.push((bits >> count) as u8);
            bits &= (1 << count) - 1;
        }
    }
    if bits != 0 {
        return Err(Error::Invalid("invalid base64 tail"));
    }
    Ok(bytes)
}
/// Het VAPID-publieke sleutelmateriaal wordt uit exact 32 geheime bytes afgeleid.
pub fn public(private: &[u8]) -> Result<[u8; 65]> {
    let key =
        SecretKey::from_slice(private).map_err(|_| Error::Invalid("invalid VAPID private key"))?;
    key.public_key()
        .to_encoded_point(false)
        .as_bytes()
        .try_into()
        .map_err(|_| Error::Invalid("invalid VAPID public key"))
}
/// Een pushabonnement blijft onderdeel van de identiteit van het telefoonapparaat.
pub struct Subscription {
    endpoint: String,
    public: PublicKey,
    auth: [u8; 16],
}
impl Subscription {
    /// Leest de bestaande vlakke data.endpoint/p256dh/auth-vorm.
    pub fn read(data: &Value) -> Result<Self> {
        let endpoint = json::text(data, "endpoint");
        origin(endpoint)?;
        let public = PublicKey::from_sec1_bytes(&decode(json::text(data, "p256dh"))?)
            .map_err(|_| Error::Invalid("invalid browser P-256 key"))?;
        let auth = decode(json::text(data, "auth"))?
            .as_slice()
            .try_into()
            .map_err(|_| Error::Invalid("invalid browser auth secret"))?;
        Ok(Self {
            endpoint: json::copy(endpoint)?,
            public,
            auth,
        })
    }
    /// Alleen dit geverifieerde endpoint krijgt de versleutelde body.
    pub fn endpoint(&self) -> &str {
        &self.endpoint
    }
}
/// Dezelfde HTTPS-grens als Go; pushdiensten worden niet op merknaam beperkt.
pub fn origin(endpoint: &str) -> Result<&str> {
    let tail = endpoint
        .strip_prefix("https://")
        .ok_or(Error::Invalid("push endpoint requires HTTPS"))?;
    if endpoint.len() > 4096
        || endpoint.bytes().any(|b| b <= 32 || b == 127)
        || endpoint.contains(['@', '\\', '#'])
    {
        return Err(Error::Invalid("invalid push endpoint"));
    }
    let end = tail.find(['/', '?']).unwrap_or(tail.len());
    if end == 0 {
        return Err(Error::Invalid("push host missing"));
    }
    Ok(&endpoint[..8 + end])
}
/// Ondertekent de origin, een geldigheidsduur van twaalf uur en het contactadres.
pub fn authorization(private: &[u8], endpoint: &str, subject: &str, now: u64) -> Result<String> {
    let key = SigningKey::from_slice(private).map_err(|_| Error::Invalid("invalid VAPID key"))?;
    let mut claims = json::fields(&[
        ("aud", json::string(origin(endpoint)?)?),
        (
            "exp",
            Value::uint(
                now.checked_add(43200)
                    .ok_or(Error::Invalid("VAPID expiry overflow"))?,
            ),
        ),
    ])?;
    if !subject.is_empty() {
        json::set(&mut claims, "sub", json::string(subject)?)?;
    }
    let claims = json::to_string(&claims).map_err(stulp_core::Error::from)?;
    let input = join(&[
        &base64(br#"{"typ":"JWT","alg":"ES256"}"#)?,
        ".",
        &base64(claims.as_bytes())?,
    ])?;
    let signature: Signature = key.sign(input.as_bytes());
    join(&[
        "vapid t=",
        &input,
        ".",
        &base64(&signature.to_bytes())?,
        ", k=",
        &base64(&public(private)?)?,
    ])
}
/// Versleutelt één record; salt en ephemeral private key zijn per verzending vers.
pub fn encrypt(
    subscription: &Subscription,
    ephemeral: &[u8; 32],
    salt: &[u8; 16],
    message: &[u8],
) -> Result<Vec<u8>> {
    if message.len() > MAX_PAYLOAD {
        return Err(Error::Invalid("push message exceeds 3993 bytes"));
    }
    let sender =
        SecretKey::from_slice(ephemeral).map_err(|_| Error::Invalid("invalid ephemeral key"))?;
    let shared =
        p256::ecdh::diffie_hellman(sender.to_nonzero_scalar(), subscription.public.as_affine());
    let recipient = subscription.public.to_encoded_point(false);
    let sender_public = sender.public_key().to_encoded_point(false);
    let mut context = [0u8; 144];
    context[..14].copy_from_slice(b"WebPush: info\0");
    context[14..79].copy_from_slice(recipient.as_bytes());
    context[79..].copy_from_slice(sender_public.as_bytes());
    let mut ikm = Zeroizing::new([0u8; 32]);
    Hkdf::<Sha256>::new(Some(&subscription.auth), shared.raw_secret_bytes())
        .expand(&context, ikm.as_mut())
        .map_err(|_| Error::Invalid("push HKDF failed"))?;
    let derive = Hkdf::<Sha256>::new(Some(salt), ikm.as_ref());
    let mut key = Zeroizing::new([0u8; 16]);
    let mut nonce = [0u8; 12];
    derive
        .expand(b"Content-Encoding: aes128gcm\0", key.as_mut())
        .map_err(|_| Error::Invalid("push content key failed"))?;
    derive
        .expand(b"Content-Encoding: nonce\0", &mut nonce)
        .map_err(|_| Error::Invalid("push nonce failed"))?;
    let mut bytes = Vec::new();
    bytes
        .try_reserve(103 + message.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    bytes.extend_from_slice(salt);
    bytes.extend_from_slice(&4096u32.to_be_bytes());
    bytes.push(65);
    bytes.extend_from_slice(sender_public.as_bytes());
    bytes.extend_from_slice(message);
    bytes.push(2);
    let cipher = Aes128Gcm::new_from_slice(key.as_ref())
        .map_err(|_| Error::Invalid("push cipher failed"))?;
    let tag = cipher
        .encrypt_in_place_detached(Nonce::from_slice(&nonce), &[], &mut bytes[86..])
        .map_err(|_| Error::Invalid("push encryption failed"))?;
    bytes.extend_from_slice(&tag);
    Ok(bytes)
}
/// Een bericht houdt dezelfde service-worker-velden als de Go-versie.
pub fn message(title: &str, body: &str, image: Option<&str>) -> Result<Vec<u8>> {
    let mut m = json::fields(&[
        ("title", json::string(title)?),
        ("body", json::string(body)?),
        ("url", json::string("/")?),
    ])?;
    if let Some(image) = image {
        json::set(&mut m, "image", json::string(image)?)?;
    }
    let s = json::to_string(&m).map_err(stulp_core::Error::from)?;
    if s.len() > MAX_PAYLOAD {
        return Err(Error::Invalid("push message exceeds 3993 bytes"));
    }
    Ok(s.into_bytes())
}
/// Accepteert dezelfde tekstvormen als de Flow-kaart, zonder debug-JSON voor strings.
pub fn text(value: &Value) -> Result<String> {
    match value {
        Value::Null => Ok(String::new()),
        Value::String(s) => Ok(json::copy(s)?),
        _ => json::to_string(value).map_err(|e| Error::Core(e.into())),
    }
}
/// Autocomplete-keuzes bewaren een id; een apparaatargument is een apart type.
pub fn choice(value: &Value) -> &str {
    value
        .as_str()
        .unwrap_or_else(|| field(value, "id").as_str().unwrap_or(""))
}
