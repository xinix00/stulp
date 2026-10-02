//! Handmatige codes en MT-QR-inhoud; checksum, veldbreedtes en optionele TLV worden gevalideerd.
use crate::{copy, tlv};
use alloc::{string::String, vec::Vec};
use stulp_sdk::{Error, Result};
const ALPHABET: &[u8] = b"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ-.";
/// Inhoud van een commissioning-code; een korte discriminator blijft herkenbaar kort.
#[derive(Default, Debug, PartialEq, Eq)]
pub struct Payload {
    /// Momenteel uitsluitend versie nul.
    pub version: u8,
    /// Vendor-ID.
    pub vendor: u16,
    /// Product-ID.
    pub product: u16,
    /// Standaard, gebruikersintentie of custom flow (0–2).
    pub flow: u8,
    /// QR-discoverymasker.
    pub discovery: u8,
    /// Volledige 12 bits, of de hoge vier bits uit een handmatige code.
    pub discriminator: u16,
    /// De lage acht bits ontbreken in handmatige codes.
    pub short: bool,
    /// Commissioning-passcode, nooit afleiden uit een sessieteller.
    pub passcode: u32,
    /// Gevalideerde extensies blijven bytegetrouw behouden.
    pub extensions: Vec<u8>,
}
fn passcode(n: u32) -> Result {
    if n >= 1 << 27
        || [
            0, 11111111, 22222222, 33333333, 44444444, 55555555, 66666666, 77777777, 88888888,
            99999999, 12345678, 87654321,
        ]
        .contains(&n)
    {
        return Err(Error::Invalid("invalid Matter passcode"));
    }
    Ok(())
}
fn decimal(out: &mut String, n: u32, width: usize) -> Result {
    let mut buf = [b'0'; 10];
    let mut n = n;
    for b in buf.iter_mut().rev() {
        *b += (n % 10) as u8;
        n /= 10;
    }
    let text = core::str::from_utf8(
        buf.get(10 - width..)
            .ok_or(Error::Invalid("decimal width"))?,
    )
    .map_err(|_| Error::Invalid("decimal encoding"))?;
    out.try_reserve(width)
        .map_err(|_| stulp_core::Error::Memory)?;
    out.push_str(text);
    Ok(())
}
fn checksum(bytes: &[u8], extra: usize) -> usize {
    let mut c = 0;
    for (i, b) in bytes.iter().rev().enumerate() {
        c = D[c][P[(i + extra) % 8][usize::from(*b - b'0')]];
    }
    c
}
impl Payload {
    /// Detecteert QR of handmatig; separators zijn alleen voor handmatige codes.
    pub fn parse(code: &str) -> Result<Self> {
        let s = code.trim();
        if s.len() > 8192 {
            return Err(Error::Invalid("onboarding code too long"));
        }
        if s.get(..3).is_some_and(|v| v.eq_ignore_ascii_case("MT:")) {
            Self::qr_parse(&s[3..])
        } else {
            Self::manual_parse(s)
        }
    }
    fn manual_parse(code: &str) -> Result<Self> {
        let mut digits = Vec::new();
        for b in code.bytes() {
            if b" -.".contains(&b) {
                continue;
            }
            if !b.is_ascii_digit() {
                return Err(Error::Invalid("pairing code contains non-digit"));
            }
            stulp_core::json::push(&mut digits, b, 21)?;
        }
        if ![11, 21].contains(&digits.len()) || checksum(&digits, 0) != 0 {
            return Err(Error::Invalid("pairing code length or check digit invalid"));
        }
        let first = digits[0] - b'0';
        let group = |range: core::ops::Range<usize>| {
            digits[range]
                .iter()
                .fold(0u32, |a, b| a * 10 + u32::from(*b - b'0'))
        };
        let g2 = group(1..6);
        let g3 = group(6..10);
        let vid = first & 4 != 0;
        if first > 7 || g2 > 65535 || vid != (digits.len() == 21) {
            return Err(Error::Invalid("invalid pairing code fields"));
        }
        let mut p = Self {
            discriminator: u16::from(first & 3) << 10 | ((g2 >> 14) as u16) << 8,
            short: true,
            passcode: g2 & 0x3fff | g3 << 14,
            ..Self::default()
        };
        if vid {
            p.vendor =
                u16::try_from(group(10..15)).map_err(|_| Error::Invalid("vendor ID overflow"))?;
            p.product =
                u16::try_from(group(15..20)).map_err(|_| Error::Invalid("product ID overflow"))?;
            p.flow = 2;
        }
        passcode(p.passcode)?;
        Ok(p)
    }
    /// Code met Verhoeff-checkdigit; custom flows dragen vendor en product mee.
    pub fn manual(&self) -> Result<String> {
        self.validate()?;
        let short = self.discriminator >> 8;
        let vid = self.flow != 0;
        let mut out = String::new();
        decimal(&mut out, u32::from(short >> 2) | if vid { 4 } else { 0 }, 1)?;
        decimal(
            &mut out,
            u32::from(short & 3) << 14 | self.passcode & 0x3fff,
            5,
        )?;
        decimal(&mut out, self.passcode >> 14, 4)?;
        if vid {
            decimal(&mut out, u32::from(self.vendor), 5)?;
            decimal(&mut out, u32::from(self.product), 5)?;
        }
        let digit = [0, 4, 3, 2, 1, 5, 6, 7, 8, 9][checksum(out.as_bytes(), 1)];
        decimal(&mut out, digit, 1)?;
        Ok(out)
    }
    fn validate(&self) -> Result {
        passcode(self.passcode)?;
        if self.version != 0 || self.flow > 2 || self.discriminator > 4095 {
            return Err(Error::Invalid("invalid QR payload fields"));
        }
        let mut r = tlv::Reader::new(&self.extensions)?;
        while r.next()?.is_some() {}
        Ok(())
    }
    /// Herbouwt de oorspronkelijke QR-inhoud, inclusief extensies.
    pub fn qr(&self) -> Result<String> {
        self.validate()?;
        if self.short {
            return Err(Error::Invalid("QR requires the full discriminator"));
        }
        let mut packed = [0u8; 11];
        let mut at = 0;
        for (value, n) in [
            (u32::from(self.version), 3),
            (u32::from(self.vendor), 16),
            (u32::from(self.product), 16),
            (u32::from(self.flow), 2),
            (u32::from(self.discovery), 8),
            (u32::from(self.discriminator), 12),
            (self.passcode, 27),
            (0, 4),
        ] {
            for bit in 0..n {
                packed[at / 8] |= (((value >> bit) & 1) as u8) << (at % 8);
                at += 1;
            }
        }
        let mut bytes = copy(&packed)?;
        crate::append(&mut bytes, &self.extensions)?;
        let mut text = stulp_core::json::copy("MT:")?;
        text.try_reserve(bytes.len().div_ceil(3) * 5)
            .map_err(|_| stulp_core::Error::Memory)?;
        for b in bytes.chunks(3) {
            let mut value = 0u32;
            for (i, b) in b.iter().enumerate() {
                value |= u32::from(*b) << (i * 8);
            }
            for _ in 0..match b.len() {
                1 => 2,
                2 => 4,
                _ => 5,
            } {
                text.push(char::from(ALPHABET[(value % 38) as usize]));
                value /= 38;
            }
        }
        Ok(text)
    }
    fn qr_parse(text: &str) -> Result<Self> {
        let mut bytes = Vec::new();
        for chars in text.as_bytes().chunks(5) {
            let n = match chars.len() {
                5 => 3,
                4 => 2,
                2 => 1,
                _ => return Err(Error::Invalid("invalid base38 trailing length")),
            };
            let mut value = 0u32;
            for ch in chars.iter().rev() {
                let index = ALPHABET
                    .iter()
                    .position(|b| b == ch)
                    .ok_or(Error::Invalid("invalid base38 character"))?;
                value = value * 38 + index as u32;
            }
            if value >= 1 << (n * 8) {
                return Err(Error::Invalid("base38 group overflow"));
            }
            crate::append(&mut bytes, &value.to_le_bytes()[..n])?;
        }
        if bytes.len() < 11 {
            return Err(Error::Invalid("QR header too short"));
        }
        let mut at = 0;
        let mut read = |n| {
            let mut v = 0;
            for i in 0..n {
                v |= u32::from((bytes[at / 8] >> (at % 8)) & 1) << i;
                at += 1;
            }
            v
        };
        let p = Self {
            version: read(3) as u8,
            vendor: read(16) as u16,
            product: read(16) as u16,
            flow: read(2) as u8,
            discovery: read(8) as u8,
            discriminator: read(12) as u16,
            passcode: read(27),
            short: false,
            extensions: Vec::new(),
        };
        if read(4) != 0 {
            return Err(Error::Invalid("QR padding is nonzero"));
        }
        let p = Self {
            extensions: copy(&bytes[11..])?,
            ..p
        };
        p.validate()?;
        Ok(p)
    }
}

const D: [[usize; 10]; 10] = [
    [0, 1, 2, 3, 4, 5, 6, 7, 8, 9],
    [1, 2, 3, 4, 0, 6, 7, 8, 9, 5],
    [2, 3, 4, 0, 1, 7, 8, 9, 5, 6],
    [3, 4, 0, 1, 2, 8, 9, 5, 6, 7],
    [4, 0, 1, 2, 3, 9, 5, 6, 7, 8],
    [5, 9, 8, 7, 6, 0, 4, 3, 2, 1],
    [6, 5, 9, 8, 7, 1, 0, 4, 3, 2],
    [7, 6, 5, 9, 8, 2, 1, 0, 4, 3],
    [8, 7, 6, 5, 9, 3, 2, 1, 0, 4],
    [9, 8, 7, 6, 5, 4, 3, 2, 1, 0],
];

const P: [[usize; 10]; 8] = [
    [0, 1, 2, 3, 4, 5, 6, 7, 8, 9],
    [1, 5, 7, 6, 2, 8, 3, 0, 9, 4],
    [5, 8, 0, 3, 7, 9, 6, 1, 4, 2],
    [8, 9, 1, 6, 0, 4, 3, 5, 2, 7],
    [9, 4, 5, 3, 1, 2, 6, 8, 7, 0],
    [4, 2, 8, 6, 5, 7, 3, 9, 0, 1],
    [2, 7, 9, 3, 8, 0, 6, 4, 1, 5],
    [7, 0, 4, 6, 9, 1, 3, 2, 5, 8],
];
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn canonical_matter_codes_and_full_width_fields() -> Result {
        let mut p = Payload::parse("MT:-24J0AFN00KA0648G00")?;
        assert_eq!(p.vendor, 65521);
        assert_eq!(p.discriminator, 3840);
        assert_eq!(p.passcode, 20202021);
        assert_eq!(p.qr()?, "MT:-24J0AFN00KA0648G00");
        assert_eq!(p.manual()?, "34970112332");
        let short = Payload::parse("3497-011-2332")?;
        assert!(short.short);
        assert_eq!(short.discriminator, 3840);
        assert!(short.qr().is_err());
        p.flow = 2;
        p.product = 32768;
        let parsed = Payload::parse(&p.manual()?)?;
        assert_eq!(parsed.vendor, 65521);
        assert_eq!(parsed.product, 32768);
        assert!(Payload::parse("34970112333").is_err());
        assert!(Payload::parse("MT:ZZZZZ").is_err());
        let mut w = tlv::Writer::default();
        w.start(tlv::Tag::Anonymous, tlv::Value::Structure)?;
        w.string(tlv::Tag::Context(0), "serial123")?;
        w.end()?;
        p.extensions = w.finish()?;
        assert_eq!(Payload::parse(&p.qr()?)?, p);
        Ok(())
    }
}
