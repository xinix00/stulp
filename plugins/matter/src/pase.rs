//! PASE-berichten en commissioner-typestate; keys komen pas vrij na het eindrapport.
use crate::{
    spake,
    tlv::{Node, Tag, Value, Writer},
};
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
use subtle::ConstantTimeEq;
use zeroize::Zeroize;
fn root(bytes: &[u8]) -> Result<Node<'_>> {
    let n = Node::parse(bytes)?;
    if n.element.tag != Tag::Anonymous || n.element.value != Value::Structure {
        return Err(Error::Invalid("PASE body must be an anonymous structure"));
    }
    if n.children
        .iter()
        .any(|n| !matches!(n.element.tag, Tag::Context(_)))
    {
        return Err(Error::Invalid("PASE members require context tags"));
    }
    Ok(n)
}
fn start() -> Result<Writer> {
    let mut w = Writer::default();
    w.start(Tag::Anonymous, Value::Structure)?;
    Ok(w)
}
fn session(n: u64) -> Result<u16> {
    u16::try_from(n)
        .ok()
        .filter(|n| *n != 0)
        .ok_or(Error::Invalid("PASE session ID must be nonzero u16"))
}
fn octets<'a>(n: &Node<'a>, tag: u8, min: usize, max: usize) -> Result<&'a [u8]> {
    let b = n.bytes(tag)?;
    if !(min..=max).contains(&b.len()) {
        return Err(Error::Invalid("PASE octet field has wrong length"));
    }
    Ok(b)
}
/// Dezelfde default MRP/sessionparameters als de Go-stack.
pub fn parameters(w: &mut Writer, tag: Tag) -> Result {
    w.start(tag, Value::Structure)?;
    for (tag, n, width) in [
        (1, 500, 4),
        (2, 300, 4),
        (3, 4000, 2),
        (4, 21, 2),
        (5, 12, 2),
        (6, 0x01060100, 4),
        (7, 1, 2),
    ] {
        w.uint_width(Tag::Context(tag), n, width)?;
    }
    w.end()
}
/// PBKDF-verzoek vóór het wachtwoord gebruikt wordt.
pub struct Request {
    /// Door de initiator gegenereerde nonce.
    pub random: [u8; 32],
    /// ID waaronder de initiator antwoorden ontvangt.
    pub session: u16,
    /// Commissioning gebruikt passcode-ID nul.
    pub passcode_id: u16,
    /// De initiator kent salt en iteraties al.
    pub has_parameters: bool,
}
impl Request {
    /// Encodeert de vaste breedtes uit Matter, niet alleen canonieke TLV-breedtes.
    pub fn encode(&self) -> Result<Vec<u8>> {
        session(u64::from(self.session))?;
        let mut w = start()?;
        w.bytes(Tag::Context(1), &self.random)?;
        w.uint_width(Tag::Context(2), u64::from(self.session), 2)?;
        w.uint_width(Tag::Context(3), u64::from(self.passcode_id), 2)?;
        w.boolean(Tag::Context(4), self.has_parameters)?;
        parameters(&mut w, Tag::Context(5))?;
        w.end()?;
        w.finish()
    }
    /// Alle vier verplichte velden blijven verplicht, ook als hun waarde nul is.
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        let n = root(bytes)?;
        let random = octets(&n, 1, 32, 32)?
            .try_into()
            .map_err(|_| Error::Invalid("PASE random"))?;
        let session = session(n.uint(2)?)?;
        let passcode_id =
            u16::try_from(n.uint(3)?).map_err(|_| Error::Invalid("PASE passcode ID width"))?;
        let has_parameters = match n.get(4).map(|n| n.element.value) {
            Some(Value::Bool(v)) => v,
            _ => return Err(Error::Invalid("missing PASE has-parameters flag")),
        };
        Ok(Self {
            random,
            session,
            passcode_id,
            has_parameters,
        })
    }
}
/// PBKDF-antwoord; de exacte bytes hiervan binden de SPAKE-context.
pub struct Response {
    /// Echo van de initiatornonce.
    pub initiator: [u8; 32],
    /// Eigen respondernonce.
    pub responder: [u8; 32],
    /// ID waaronder de responder versleutelde berichten ontvangt.
    pub session: u16,
    /// Optionele iteraties en salt.
    pub parameters: Option<(u32, Vec<u8>)>,
}
impl Response {
    /// Behoudt de wirebreedtes en standaard sessionparameters.
    pub fn encode(&self) -> Result<Vec<u8>> {
        session(u64::from(self.session))?;
        let mut w = start()?;
        w.bytes(Tag::Context(1), &self.initiator)?;
        w.bytes(Tag::Context(2), &self.responder)?;
        w.uint_width(Tag::Context(3), u64::from(self.session), 2)?;
        if let Some((iterations, salt)) = &self.parameters {
            bounds(*iterations, salt)?;
            w.start(Tag::Context(4), Value::Structure)?;
            w.uint_width(Tag::Context(1), u64::from(*iterations), 4)?;
            w.bytes(Tag::Context(2), salt)?;
            w.end()?;
        }
        parameters(&mut w, Tag::Context(5))?;
        w.end()?;
        w.finish()
    }
    /// Negeert onbekende velden pas nadat hun volledige TLV gecontroleerd is.
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        let n = root(bytes)?;
        let initiator = octets(&n, 1, 32, 32)?
            .try_into()
            .map_err(|_| Error::Invalid("PASE random"))?;
        let responder = octets(&n, 2, 32, 32)?
            .try_into()
            .map_err(|_| Error::Invalid("PASE random"))?;
        let session = session(n.uint(3)?)?;
        let parameters = if let Some(p) = n.get(4) {
            if p.element.value != Value::Structure {
                return Err(Error::Invalid("PASE parameters must be a structure"));
            }
            let iterations =
                u32::try_from(p.uint(1)?).map_err(|_| Error::Invalid("PASE iteration overflow"))?;
            let salt = octets(p, 2, 16, 32)?;
            bounds(iterations, salt)?;
            Some((iterations, crate::copy(salt)?))
        } else {
            None
        };
        Ok(Self {
            initiator,
            responder,
            session,
            parameters,
        })
    }
}
fn bounds(iterations: u32, salt: &[u8]) -> Result {
    if !(1000..=100000).contains(&iterations) || !(16..=32).contains(&salt.len()) {
        return Err(Error::Invalid(
            "PASE PBKDF parameters outside Matter bounds",
        ));
    }
    Ok(())
}
/// Pake1/Pake3 en Pake2 gebruiken alleen octetvelden.
pub fn pake(first: &[u8], confirmation: Option<&[u8]>) -> Result<Vec<u8>> {
    if ![32, 65].contains(&first.len())
        || confirmation.is_some_and(|b| b.len() != 32)
        || confirmation.is_some() && first.len() != 65
    {
        return Err(Error::Invalid("invalid PASE Pake lengths"));
    }
    let mut w = start()?;
    w.bytes(Tag::Context(1), first)?;
    if let Some(b) = confirmation {
        w.bytes(Tag::Context(2), b)?;
    }
    w.end()?;
    w.finish()
}
/// StatusReport is little-endian, niet TLV.
pub struct Status<'a> {
    /// Algemene status.
    pub general: u16,
    /// Protocol-ID inclusief vendorbreedte.
    pub protocol: u32,
    /// Secure-channel statuscode.
    pub code: u16,
    /// Aanvullende gegevens, bijvoorbeeld een busy-delay.
    pub data: &'a [u8],
}
impl<'a> Status<'a> {
    /// Leest de vaste acht-byte kop en behoudt de rest.
    pub fn parse(bytes: &'a [u8]) -> Result<Self> {
        let b = bytes
            .get(..8)
            .ok_or(Error::Invalid("short PASE status report"))?;
        Ok(Self {
            general: u16::from_le_bytes([b[0], b[1]]),
            protocol: u32::from_le_bytes([b[2], b[3], b[4], b[5]]),
            code: u16::from_le_bytes([b[6], b[7]]),
            data: &bytes[8..],
        })
    }
    /// Alleen SecureChannel-success voltooit een sessie.
    pub fn is_success(&self) -> bool {
        self.general == 0 && self.protocol == 0 && self.code == 0
    }
    /// Encodeert het eindrapport.
    pub fn encode(&self) -> Result<Vec<u8>> {
        let mut out = Vec::new();
        crate::append(&mut out, &self.general.to_le_bytes())?;
        crate::append(&mut out, &self.protocol.to_le_bytes())?;
        crate::append(&mut out, &self.code.to_le_bytes())?;
        crate::append(&mut out, self.data)?;
        Ok(out)
    }
}
/// Commissioner vóór het PBKDF-antwoord.
pub struct Start {
    request: Request,
    wire: Vec<u8>,
}
/// Commissioner na Pake1, wachtend op de responderbevestiging.
pub struct Proving {
    local: u16,
    peer: u16,
    prover: spake::Prover,
}
/// Commissioner na Pake3; keys zijn nog niet beschikbaar voor het apparaatmodel.
pub struct Confirming {
    local: u16,
    peer: u16,
    keys: spake::Keys,
}
/// Voltooide sessie voor transport en commissioning.
pub struct Session {
    /// Onze ontvangende sessie-ID.
    pub local: u16,
    /// De ontvangende sessie-ID van de peer.
    pub peer: u16,
    /// Bevestigde sleutels; Drop wist ze.
    pub keys: spake::Keys,
}
impl Start {
    /// Nonce en sessie-ID worden door de ene transport-eigenaar uitgegeven.
    pub fn new(random: [u8; 32], session: u16) -> Result<Self> {
        let request = Request {
            random,
            session,
            passcode_id: 0,
            has_parameters: false,
        };
        let wire = request.encode()?;
        Ok(Self { request, wire })
    }
    /// PBKDFParamRequest, opcode 0x20.
    pub fn request(&self) -> &[u8] {
        &self.wire
    }
    /// Controleert de nonce-echo vóór de kostbare wachtwoordafleiding.
    pub fn response(
        self,
        bytes: &[u8],
        mut passcode: u32,
        entropy: [u8; 32],
    ) -> Result<(Proving, Vec<u8>)> {
        let prepared = self.prepare(bytes)?;
        let scalars = spake::Scalars::derive(passcode, &prepared.salt, prepared.iterations);
        passcode.zeroize();
        prepared.prove(scalars?, entropy)
    }
    /// Valideert de nonce vóór de eigenaar een coöperatieve wachtwoordafleiding begint.
    pub fn prepare(self, bytes: &[u8]) -> Result<Prepared> {
        let response = Response::parse(bytes)?;
        if !bool::from(response.initiator.ct_eq(&self.request.random)) {
            return Err(Error::Invalid(
                "PASE response did not echo initiator random",
            ));
        }
        let (iterations, salt) = response.parameters.ok_or(Error::Invalid(
            "PASE response omitted requested PBKDF parameters",
        ))?;
        Ok(Prepared {
            local: self.request.session,
            peer: response.session,
            context: spake::context(&self.wire, bytes),
            iterations,
            salt,
        })
    }
}
/// Gecontroleerde PBKDF-parameters en transcriptbinding voor coöperatieve afleiding.
pub struct Prepared {
    local: u16,
    peer: u16,
    context: [u8; 32],
    /// Begrensd op 1000–100000.
    pub iterations: u32,
    /// Gecontroleerde 16–32 byte salt.
    pub salt: Vec<u8>,
}
impl Prepared {
    /// Publiceert Pake1 nadat de volledige afleiding klaar is.
    pub fn prove(self, scalars: spake::Scalars, entropy: [u8; 32]) -> Result<(Proving, Vec<u8>)> {
        let prover = spake::Prover::new(self.context, scalars, entropy)?;
        let wire = pake(prover.share(), None)?;
        Ok((
            Proving {
                local: self.local,
                peer: self.peer,
                prover,
            },
            wire,
        ))
    }
}

impl Proving {
    /// Pake2 (0x23) wordt één keer geconsumeerd; het antwoord is Pake3 (0x24).
    pub fn finish(self, bytes: &[u8]) -> Result<(Confirming, Vec<u8>)> {
        let n = root(bytes)?;
        let share = octets(&n, 1, 65, 65)?;
        let confirmation = octets(&n, 2, 32, 32)?;
        let (a, keys) = self.prover.finish(share, confirmation)?;
        Ok((
            Confirming {
                local: self.local,
                peer: self.peer,
                keys,
            },
            pake(&a, None)?,
        ))
    }
}
impl Confirming {
    /// De transportlaag bevestigt het eindbericht ook wanneer de status afwijzend is.
    pub fn finish(self, bytes: &[u8]) -> Result<Session> {
        if !Status::parse(bytes)?.is_success() {
            return Err(Error::Invalid("peer rejected PASE session"));
        }
        Ok(Session {
            local: self.local,
            peer: self.peer,
            keys: self.keys,
        })
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn full_pase_releases_keys_only_after_secure_channel_success() -> Result {
        let start = Start::new([42; 32], 12)?;
        let request = Request::parse(start.request())?;
        assert_eq!(request.session, 12);
        assert!(!request.has_parameters);
        let response = Response {
            initiator: request.random,
            responder: [43; 32],
            session: 34,
            parameters: Some((1000, crate::copy(b"SPAKE2P Key Salt!")?)),
        }
        .encode()?;
        let ctx = spake::context(start.request(), &response);
        let registration =
            spake::Scalars::derive(20202021, b"SPAKE2P Key Salt!", 1000)?.register()?;
        let (proving, pake1) = start.response(&response, 20202021, [1; 32])?;
        let n = root(&pake1)?;
        let verifier = spake::Verifier::new(ctx, registration).accept(n.bytes(1)?, [2; 32])?;
        let (confirming, pake3) =
            proving.finish(&pake(verifier.share(), Some(verifier.confirmation()))?)?;
        let n = root(&pake3)?;
        let keys = verifier.confirm(n.bytes(1)?)?;
        let session = confirming.finish(&[0; 8])?;
        assert_eq!(session.local, 12);
        assert_eq!(session.peer, 34);
        assert_eq!(session.keys.i2r, keys.i2r);
        let start = Start::new([42; 32], 12)?;
        let wrong = Response {
            initiator: [0; 32],
            responder: [43; 32],
            session: 34,
            parameters: Some((1000, crate::copy(b"SPAKE2P Key Salt!")?)),
        }
        .encode()?;
        assert!(start.response(&wrong, 20202021, [1; 32]).is_err());
        assert!(!Status::parse(&[0, 0, 1, 0, 0, 0, 0, 0])?.is_success());
        Ok(())
    }
}
