//! Matter-framekoppen en CCM-binding; de operationele node-ID komt uit de sessie.
use crate::{append, ccm::Ccm, copy};
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
/// IPv6-minimum-MTU minus IPv6- en UDP-kop.
pub const MAX_UDP: usize = 1232;
/// Onversleutelde kop, volledig opgenomen als authenticated additional data.
#[derive(Default, Debug, PartialEq, Eq)]
pub struct Header {
    /// Nul voor een onbeveiligde sessie.
    pub session: u16,
    /// Unicast is nul, groep is één.
    pub kind: u8,
    /// Message-counter control.
    pub control: bool,
    /// Monotone berichtteller binnen de sleutellevensduur.
    pub counter: u32,
    /// Optioneel op de wire; ontbreekt vaak bij CASE-unicast.
    pub source: Option<u64>,
    /// Unicast-bestemming.
    pub destination: Option<u64>,
    /// Groepsbestemming, nooit samen met destination.
    pub group: Option<u16>,
    /// Behouden headerextensies.
    pub extensions: Vec<u8>,
}
/// Exchange-kop, binnen de ciphertext bij beveiligde sessies.
#[derive(Default, Debug, PartialEq, Eq)]
pub struct Protocol {
    /// De afzender startte deze exchange.
    pub initiator: bool,
    /// Vraagt een MRP-bevestiging.
    pub reliable: bool,
    /// Bevestigt een ontvangen berichtteller.
    pub ack: Option<u32>,
    /// Protocolbewerking.
    pub opcode: u8,
    /// Exchange-ID.
    pub exchange: u16,
    /// Secure channel 0, interaction model 1.
    pub protocol: u16,
    /// Vendor-specifiek protocol.
    pub vendor: Option<u16>,
    /// Behouden secured extensions.
    pub extensions: Vec<u8>,
}
/// Eén volledig, eventueel geauthenticeerd bericht.
#[derive(Default, Debug, PartialEq, Eq)]
pub struct Message {
    /// Onversleutelde kop.
    pub header: Header,
    /// Exchange-kop.
    pub protocol: Protocol,
    /// Payload boven de exchange-laag.
    pub payload: Vec<u8>,
}
fn ext(out: &mut Vec<u8>, bytes: &[u8]) -> Result {
    if bytes.len() > 65535 {
        return Err(Error::Invalid("Matter extension too long"));
    }
    if !bytes.is_empty() {
        append(out, &(bytes.len() as u16).to_le_bytes())?;
        append(out, bytes)?;
    }
    Ok(())
}
impl Header {
    fn validate(&self) -> Result {
        if self.kind > 1
            || (self.destination.is_some() && self.group.is_some())
            || (self.kind == 1 && self.group.is_none())
        {
            return Err(Error::Invalid("invalid Matter addressing or session type"));
        }
        Ok(())
    }
    fn encode(&self) -> Result<Vec<u8>> {
        self.validate()?;
        let flags = if self.source.is_some() { 4 } else { 0 }
            | if self.destination.is_some() {
                1
            } else if self.group.is_some() {
                2
            } else {
                0
            };
        let security = self.kind
            | if self.control { 64 } else { 0 }
            | if self.extensions.is_empty() { 0 } else { 32 };
        let mut out = Vec::new();
        append(&mut out, &[flags])?;
        append(&mut out, &self.session.to_le_bytes())?;
        append(&mut out, &[security])?;
        append(&mut out, &self.counter.to_le_bytes())?;
        if let Some(n) = self.source {
            append(&mut out, &n.to_le_bytes())?;
        }
        if let Some(n) = self.destination {
            append(&mut out, &n.to_le_bytes())?;
        }
        if let Some(n) = self.group {
            append(&mut out, &n.to_le_bytes())?;
        }
        ext(&mut out, &self.extensions)?;
        Ok(out)
    }
    /// Leest uitsluitend de kop om de juiste sessie te selecteren, zonder authenticatieclaim.
    pub fn peek(bytes: &[u8]) -> Result<(Self, usize)> {
        let mut c = Cursor { bytes, at: 0 };
        let flags = c.le(1)? as u8;
        if flags & 0xf8 != 0 || flags & 3 == 3 {
            return Err(Error::Invalid("unsupported Matter message flags"));
        }
        let session = c.le(2)? as u16;
        let security = c.le(1)? as u8;
        if security & 0x9c != 0 || security & 3 > 1 {
            return Err(Error::Invalid("unsupported Matter security flags"));
        }
        let counter = c.le(4)? as u32;
        let source = if flags & 4 != 0 { Some(c.le(8)?) } else { None };
        let destination = if flags & 3 == 1 { Some(c.le(8)?) } else { None };
        let group = if flags & 3 == 2 {
            Some(c.le(2)? as u16)
        } else {
            None
        };
        let extensions = if security & 32 != 0 {
            c.extension()?
        } else {
            Vec::new()
        };
        let h = Self {
            session,
            kind: security & 3,
            control: security & 64 != 0,
            counter,
            source,
            destination,
            group,
            extensions,
        };
        h.validate()?;
        Ok((h, c.at))
    }
    fn nonce(&self, bytes: &[u8], source: u64) -> Result<[u8; 13]> {
        if self.source.is_some_and(|s| s != source) {
            return Err(Error::Invalid(
                "Matter source does not match secure session",
            ));
        }
        let mut n = [0; 13];
        n[0] = *bytes.get(3).ok_or(Error::Invalid("short Matter header"))?;
        n[1..5].copy_from_slice(&self.counter.to_le_bytes());
        n[5..].copy_from_slice(&source.to_le_bytes());
        Ok(n)
    }
}
impl Protocol {
    fn encode(&self) -> Result<Vec<u8>> {
        let flags = u8::from(self.initiator)
            | if self.ack.is_some() { 2 } else { 0 }
            | if self.reliable { 4 } else { 0 }
            | if self.extensions.is_empty() { 0 } else { 8 }
            | if self.vendor.is_some() { 16 } else { 0 };
        let mut out = Vec::new();
        append(&mut out, &[flags, self.opcode])?;
        append(&mut out, &self.exchange.to_le_bytes())?;
        if let Some(v) = self.vendor {
            append(&mut out, &v.to_le_bytes())?;
        }
        append(&mut out, &self.protocol.to_le_bytes())?;
        if let Some(a) = self.ack {
            append(&mut out, &a.to_le_bytes())?;
        }
        ext(&mut out, &self.extensions)?;
        Ok(out)
    }
    fn parse(bytes: &[u8]) -> Result<(Self, usize)> {
        let mut c = Cursor { bytes, at: 0 };
        let flags = c.le(1)? as u8;
        if flags & 0xe0 != 0 {
            return Err(Error::Invalid("reserved Matter exchange flag"));
        }
        let opcode = c.le(1)? as u8;
        let exchange = c.le(2)? as u16;
        let vendor = if flags & 16 != 0 {
            Some(c.le(2)? as u16)
        } else {
            None
        };
        let protocol = c.le(2)? as u16;
        let ack = if flags & 2 != 0 {
            Some(c.le(4)? as u32)
        } else {
            None
        };
        let extensions = if flags & 8 != 0 {
            c.extension()?
        } else {
            Vec::new()
        };
        Ok((
            Self {
                initiator: flags & 1 != 0,
                reliable: flags & 4 != 0,
                ack,
                opcode,
                exchange,
                vendor,
                protocol,
                extensions,
            },
            c.at,
        ))
    }
}
impl Message {
    fn unsecured(&self) -> Result {
        if self.header.session != 0 || self.header.kind != 0 {
            return Err(Error::Invalid(
                "unsecured Matter message requires unicast session zero",
            ));
        }
        Ok(())
    }
    /// Voor PASE- en CASE-opbouw, vóór er sleutels bestaan.
    pub fn encode(&self) -> Result<Vec<u8>> {
        self.unsecured()?;
        let mut out = self.header.encode()?;
        append(&mut out, &self.protocol.encode()?)?;
        append(&mut out, &self.payload)?;
        Ok(out)
    }
    /// Onbeveiligde berichten mogen geen beveiligde sessie claimen.
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        let (header, n) = Header::peek(bytes)?;
        let (protocol, p) = Protocol::parse(&bytes[n..])?;
        let m = Self {
            header,
            protocol,
            payload: copy(&bytes[n + p..])?,
        };
        m.unsecured()?;
        Ok(m)
    }
    /// Beveiligd bericht met de bron-ID uit PASE (0) of CASE (operationele node-ID).
    pub fn seal(&self, key: &[u8; 16], source: u64) -> Result<Vec<u8>> {
        if self.header.session == 0 {
            return Err(Error::Invalid(
                "secured Matter message requires nonzero session",
            ));
        }
        let mut header = self.header.encode()?;
        let mut plain = self.protocol.encode()?;
        append(&mut plain, &self.payload)?;
        let nonce = self.header.nonce(&header, source)?;
        let sealed = Ccm::new(key, 16)?.seal(&nonce, &plain, &header)?;
        append(&mut header, &sealed)?;
        Ok(header)
    }
    /// Authenticeert vóór de exchange-kop of payload geïnterpreteerd wordt.
    pub fn open(bytes: &[u8], key: &[u8; 16], source: u64) -> Result<Self> {
        let (header, n) = Header::peek(bytes)?;
        if header.session == 0 {
            return Err(Error::Invalid(
                "secured Matter message requires nonzero session",
            ));
        }
        let nonce = header.nonce(bytes, source)?;
        let plain = Ccm::new(key, 16)?.open(&nonce, &bytes[n..], &bytes[..n])?;
        let (protocol, p) = Protocol::parse(&plain)?;
        Ok(Self {
            header,
            protocol,
            payload: copy(&plain[p..])?,
        })
    }
}
struct Cursor<'a> {
    bytes: &'a [u8],
    at: usize,
}
impl Cursor<'_> {
    fn le(&mut self, n: usize) -> Result<u64> {
        let mut v = [0; 8];
        let b = self
            .bytes
            .get(self.at..self.at + n)
            .ok_or(Error::Invalid("short Matter header"))?;
        v.get_mut(..n)
            .ok_or(Error::Invalid("invalid integer width"))?
            .copy_from_slice(b);
        self.at += n;
        Ok(u64::from_le_bytes(v))
    }
    fn extension(&mut self) -> Result<Vec<u8>> {
        let n = self.le(2)? as usize;
        let b = self
            .bytes
            .get(self.at..self.at + n)
            .ok_or(Error::Invalid("short Matter extension"))?;
        self.at += n;
        copy(b)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn compact_case_nonce_uses_session_node_and_authenticates_header() -> Result {
        let m = Message {
            header: Header {
                session: 17,
                counter: 345,
                ..Header::default()
            },
            protocol: Protocol {
                initiator: true,
                reliable: true,
                ack: Some(12),
                exchange: 456,
                opcode: 5,
                protocol: 1,
                ..Protocol::default()
            },
            payload: copy(b"protected")?,
        };
        let mut wire = m.seal(&[42; 16], 0x1122334455667788)?;
        assert_eq!(Message::open(&wire, &[42; 16], 0x1122334455667788)?, m);
        assert!(Message::open(&wire, &[42; 16], 0).is_err());
        wire[4] ^= 1;
        assert!(Message::open(&wire, &[42; 16], 0x1122334455667788).is_err());
        assert!(Message::parse(&wire).is_err());
        Ok(())
    }
    #[test]
    fn unsecured_extensions_and_address_fields_survive() -> Result {
        let m = Message {
            header: Header {
                source: Some(123),
                destination: Some(456),
                extensions: copy(b"head")?,
                ..Header::default()
            },
            protocol: Protocol {
                vendor: Some(42),
                extensions: copy(b"exchange")?,
                ..Protocol::default()
            },
            payload: copy(b"body")?,
        };
        let bytes = m.encode()?;
        assert_eq!(Message::parse(&bytes)?, m);
        for n in 0..bytes.len() - 4 {
            assert!(Message::parse(&bytes[..n]).is_err());
        }
        let mut bad = bytes;
        bad[0] |= 8;
        assert!(Message::parse(&bad).is_err());
        Ok(())
    }
}
