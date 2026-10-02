//! Operationele certificaten: identieke DER/TLV-representatie en PKCS#8-import.
//! Accepteert het door Stulp uitgegeven Matter-profiel; onbekende extensies falen expliciet.
use crate::{
    der::{self, Reader, object, only},
    key_id::key_id,
    tlv::{Node, Tag, Value, Writer},
};
use alloc::vec::Vec;
use core::time::Duration;
use p256::{
    PublicKey, SecretKey,
    ecdsa::{
        Signature, SigningKey, VerifyingKey,
        signature::{Signer, Verifier},
    },
    elliptic_curve::sec1::ToEncodedPoint,
};
use stulp_sdk::{Error, Result};
use zeroize::Zeroizing;
const EPOCH: u64 = 946684800;
const SIG: &[u8] = &[
    0x30, 0x0a, 0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x02,
];
const EC: &[u8] = &[
    0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06, 0x08, 0x2a, 0x86, 0x48,
    0xce, 0x3d, 0x03, 0x01, 0x07,
];
const DN: &[u8] = &[0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0xa2, 0x7c, 0x01];
const EKU: &[u8] = &[0x2b, 0x06, 0x01, 0x05, 0x05, 0x07, 0x03];
const HEX: &[u8] = b"0123456789ABCDEF";
fn invalid<T>() -> Result<T> {
    Err(Error::Invalid("invalid Matter operational certificate"))
}
/// Het operationele DN-profiel van Stulp; een root of een node binnen een fabric.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Name {
    /// Root CA-ID.
    Root(u64),
    /// Node-ID en fabric-ID.
    Node(u64, u64),
}
impl Name {
    fn fields(self) -> [(u8, Option<u64>); 3] {
        match self {
            Self::Root(id) => [(17, None), (20, Some(id)), (21, None)],
            Self::Node(node, fabric) => [(17, Some(node)), (20, None), (21, Some(fabric))],
        }
    }
    fn der(self) -> Result<Vec<u8>> {
        let mut out = Vec::new();
        for (tag, n) in self.fields() {
            if let Some(n) = n {
                if n == 0 {
                    return invalid();
                }
                let mut hex = [0; 16];
                for (i, b) in hex.iter_mut().enumerate() {
                    *b = HEX[((n >> ((15 - i) * 4)) & 15) as usize];
                }
                let oid = object(6, &[DN, &[tag - 16]])?;
                let value = object(12, &[&hex])?;
                let entry = object(0x31, &[&object(0x30, &[&oid, &value])?])?;
                crate::append(&mut out, &entry)?;
            }
        }
        object(0x30, &[&out])
    }
    fn tlv(self, w: &mut Writer, tag: u8) -> Result {
        w.start(Tag::Context(tag), Value::List)?;
        for (tag, n) in self.fields() {
            if let Some(n) = n {
                w.uint_width(Tag::Context(tag), n, 8)?;
            }
        }
        w.end()
    }
    fn values(values: [Option<u64>; 3]) -> Result<Self> {
        match values {
            [None, Some(n), None] if n != 0 => Ok(Self::Root(n)),
            [Some(n), None, Some(f)] if n != 0 && f != 0 => Ok(Self::Node(n, f)),
            _ => invalid(),
        }
    }
    fn from_der(input: &[u8]) -> Result<Self> {
        let mut r = Reader(only(input, 0x30)?.value);
        let mut values = [None; 3];
        while !r.0.is_empty() {
            let set = r.take(0x31)?;
            let mut a = Reader(only(set.value, 0x30)?.value);
            let oid = a.take(6)?.value;
            if oid.len() != 10 || &oid[..9] != DN {
                return invalid();
            }
            let i = match oid[9] {
                1 => 0,
                4 => 1,
                5 => 2,
                _ => return invalid(),
            };
            if values[i].is_some() {
                return invalid();
            }
            let hex = a.take(12)?.value;
            if hex.len() != 16 {
                return invalid();
            }
            let mut n = 0u64;
            for b in hex {
                n = (n << 4)
                    | u64::from(match b {
                        b'0'..=b'9' => b - b'0',
                        b'A'..=b'F' => b - b'A' + 10,
                        _ => return invalid(),
                    });
            }
            values[i] = Some(n);
            a.end()?;
        }
        Self::values(values)
    }
    fn from_tlv(n: &Node<'_>) -> Result<Self> {
        if n.element.value != Value::List {
            return invalid();
        }
        let mut values = [None; 3];
        for c in &n.children {
            let i = match c.element.tag {
                Tag::Context(17) => 0,
                Tag::Context(20) => 1,
                Tag::Context(21) => 2,
                _ => return invalid(),
            };
            if values[i].is_some() {
                return invalid();
            }
            let Value::Uint(value) = c.element.value else {
                return invalid();
            };
            values[i] = Some(value);
        }
        Self::values(values)
    }
}
fn date(seconds: u32) -> Result<Vec<u8>> {
    let time = if seconds == 0 {
        ::der::DateTime::new(9999, 12, 31, 23, 59, 59)
    } else {
        ::der::DateTime::from_unix_duration(Duration::from_secs(EPOCH + u64::from(seconds)))
    }
    .map_err(|_| Error::Invalid("certificate date range"))?;
    let long = time.year() >= 2050;
    let mut text = [0; 15];
    let mut i = 0;
    if long {
        text[0] = b'0' + (time.year() / 1000) as u8;
        text[1] = b'0' + ((time.year() / 100) % 10) as u8;
        i = 2;
    }
    for value in [
        (time.year() % 100) as u8,
        time.month(),
        time.day(),
        time.hour(),
        time.minutes(),
        time.seconds(),
    ] {
        text[i] = b'0' + value / 10;
        text[i + 1] = b'0' + value % 10;
        i += 2;
    }
    text[i] = b'Z';
    object(if long { 24 } else { 23 }, &[&text[..=i]])
}
fn parse_date(e: der::Element<'_>) -> Result<u32> {
    let (start, century) = match e.tag {
        23 if e.value.len() == 13 => (0, 0),
        24 if e.value.len() == 15 => (2, 1),
        _ => return invalid(),
    };
    let b = e.value;
    if b[b.len() - 1] != b'Z' || b[..b.len() - 1].iter().any(|b| !b.is_ascii_digit()) {
        return invalid();
    }
    let pair = |i: usize| u16::from(b[i] - b'0') * 10 + u16::from(b[i + 1] - b'0');
    let year = if century == 1 {
        pair(0) * 100 + pair(2)
    } else {
        let n = pair(0);
        if n >= 50 { 1900 + n } else { 2000 + n }
    };
    if year == 9999 && &b[4..] == b"1231235959Z" {
        return Ok(0);
    }
    let d = ::der::DateTime::new(
        year,
        pair(start + 2) as u8,
        pair(start + 4) as u8,
        pair(start + 6) as u8,
        pair(start + 8) as u8,
        pair(start + 10) as u8,
    )
    .map_err(|_| Error::Invalid("invalid certificate date"))?;
    u32::try_from(
        d.unix_duration()
            .as_secs()
            .checked_sub(EPOCH)
            .ok_or(Error::Invalid("certificate predates Matter epoch"))?,
    )
    .map_err(|_| Error::Invalid("certificate exceeds Matter epoch"))
}
fn extension(oid: u8, critical: bool, payload: &[u8]) -> Result<Vec<u8>> {
    let oid = object(6, &[&[0x55, 0x1d, oid]])?;
    let flag: &[u8] = if critical { &[1, 1, 255] } else { &[] };
    object(0x30, &[&oid, flag, &object(4, &[payload])?])
}
/// Een certificaat uit het beperkte operationele profiel; velden wijzigen kan alleen via signing.
pub struct Certificate {
    serial: Vec<u8>,
    issuer: Name,
    subject: Name,
    before: u32,
    after: u32,
    public: [u8; 65],
    ca: bool,
    usage: u16,
    eku: Vec<u8>,
    skid: [u8; 20],
    akid: [u8; 20],
    signature: [u8; 64],
}
impl Certificate {
    /// Volledige publieke operationele sleutel.
    pub fn public(&self) -> &[u8; 65] {
        &self.public
    }
    /// Gecontroleerde identiteit.
    pub fn subject(&self) -> Name {
        self.subject
    }
    /// Rootstatus, uit BasicConstraints.
    pub fn is_ca(&self) -> bool {
        self.ca
    }
    fn extensions(&self) -> Result<Vec<u8>> {
        let mut out = Vec::new();
        let bc: &[u8] = if self.ca {
            &[0x30, 3, 1, 1, 255]
        } else {
            &[0x30, 0]
        };
        crate::append(&mut out, &extension(19, true, bc)?)?;
        let lo = (self.usage as u8).reverse_bits();
        let hi = ((self.usage >> 8) as u8).reverse_bits();
        let bits = if hi != 0 {
            object(3, &[&[hi.trailing_zeros() as u8, lo, hi]])?
        } else if lo != 0 {
            object(3, &[&[lo.trailing_zeros() as u8, lo]])?
        } else {
            object(3, &[&[0]])?
        };
        crate::append(&mut out, &extension(15, true, &bits)?)?;
        if !self.eku.is_empty() {
            let mut eku = Vec::new();
            for usage in &self.eku {
                crate::append(&mut eku, &object(6, &[EKU, &[*usage]])?)?;
            }
            crate::append(&mut out, &extension(37, true, &object(0x30, &[&eku])?)?)?;
        }
        crate::append(&mut out, &extension(14, false, &object(4, &[&self.skid])?)?)?;
        crate::append(
            &mut out,
            &extension(35, false, &object(0x30, &[&object(0x80, &[&self.akid])?])?)?,
        )?;
        object(0xa3, &[&object(0x30, &[&out])?])
    }
    fn tbs(&self) -> Result<Vec<u8>> {
        object(
            0x30,
            &[
                &[0xa0, 3, 2, 1, 2],
                &der::integer(&self.serial)?,
                SIG,
                &self.issuer.der()?,
                &object(0x30, &[&date(self.before)?, &date(self.after)?])?,
                &self.subject.der()?,
                &object(0x30, &[EC, &object(3, &[&[0], &self.public])?])?,
                &self.extensions()?,
            ],
        )
    }
    /// Standaard X.509 DER, inclusief dezelfde TBS-signature als de compacte vorm.
    pub fn der(&self) -> Result<Vec<u8>> {
        let sig = Signature::from_slice(&self.signature)
            .map_err(|_| Error::Invalid("certificate signature scalar"))?
            .to_der();
        object(
            0x30,
            &[&self.tbs()?, SIG, &object(3, &[&[0], sig.as_bytes()])?],
        )
    }
    /// Matter Operational Credentials-certificate, maximaal 400 bytes.
    pub fn tlv(&self) -> Result<Vec<u8>> {
        let mut w = Writer::default();
        w.start(Tag::Anonymous, Value::Structure)?;
        w.bytes(Tag::Context(1), &self.serial)?;
        w.uint_width(Tag::Context(2), 1, 1)?;
        self.issuer.tlv(&mut w, 3)?;
        w.uint_width(Tag::Context(4), u64::from(self.before), 4)?;
        w.uint_width(Tag::Context(5), u64::from(self.after), 4)?;
        self.subject.tlv(&mut w, 6)?;
        w.uint_width(Tag::Context(7), 1, 1)?;
        w.uint_width(Tag::Context(8), 1, 1)?;
        w.bytes(Tag::Context(9), &self.public)?;
        w.start(Tag::Context(10), Value::List)?;
        w.start(Tag::Context(1), Value::Structure)?;
        w.boolean(Tag::Context(1), self.ca)?;
        w.end()?;
        w.uint_width(Tag::Context(2), u64::from(self.usage), 2)?;
        if !self.eku.is_empty() {
            w.start(Tag::Context(3), Value::Array)?;
            for u in &self.eku {
                w.uint_width(Tag::Anonymous, u64::from(*u), 1)?;
            }
            w.end()?;
        }
        w.bytes(Tag::Context(4), &self.skid)?;
        w.bytes(Tag::Context(5), &self.akid)?;
        w.end()?;
        w.bytes(Tag::Context(11), &self.signature)?;
        w.end()?;
        let bytes = w.finish()?;
        if bytes.len() > 400 {
            return invalid();
        }
        Ok(bytes)
    }
    fn validate(&self) -> Result {
        if self.serial.is_empty()
            || self.serial.len() > 20
            || self.serial[0] == 0
            || self.serial[0] & 128 != 0
            || self.before == 0
            || self.after != 0 && self.before >= self.after
        {
            return invalid();
        }
        if self.eku.len() > 2
            || self.eku.iter().any(|u| !matches!(u, 1 | 2))
            || self.eku.len() == 2 && self.eku[0] == self.eku[1]
        {
            return invalid();
        }
        PublicKey::from_sec1_bytes(&self.public)
            .map_err(|_| Error::Invalid("certificate P-256 key"))?;
        if self.skid != key_id(&self.public) {
            return Err(Error::Invalid("certificate key identifier differs"));
        }
        // Stulp geeft alleen een root en rechtstreeks daaronder operationele nodes uit.
        match (self.ca, self.subject, self.issuer) {
            (true, Name::Root(_), Name::Root(_))
                if self.subject == self.issuer
                    && self.usage == 96
                    && self.eku.is_empty()
                    && self.skid == self.akid => {}
            (false, Name::Node(_, _), Name::Root(_))
                if self.usage == 1 && self.eku.contains(&1) && self.eku.contains(&2) => {}
            _ => return invalid(),
        }
        Ok(())
    }
    /// Verifieert de root-handtekening en de issuer/SKID-binding; datumbeleid ligt bij de eigenaar.
    pub fn verify(&self, issuer: &Certificate) -> Result {
        self.validate()?;
        issuer.validate()?;
        if !issuer.ca || self.issuer != issuer.subject || self.akid != issuer.skid {
            return invalid();
        }
        let key = VerifyingKey::from_sec1_bytes(&issuer.public)
            .map_err(|_| Error::Invalid("certificate issuer key"))?;
        let sig = Signature::from_slice(&self.signature)
            .map_err(|_| Error::Invalid("certificate signature"))?;
        key.verify(&self.tbs()?, &sig)
            .map_err(|_| Error::Invalid("certificate signature invalid"))
    }
    /// Maakt een root met aangeleverde CSPRNG-sleutel/serial en expliciete Matter-epochdatums.
    pub fn root(
        key: &SecretKey,
        id: u64,
        serial: [u8; 16],
        before: u32,
        after: u32,
    ) -> Result<Self> {
        let public: [u8; 65] = key
            .public_key()
            .to_encoded_point(false)
            .as_bytes()
            .try_into()
            .map_err(|_| Error::Invalid("root key"))?;
        let mut cert = Self {
            serial: positive_serial(serial)?,
            issuer: Name::Root(id),
            subject: Name::Root(id),
            before,
            after,
            public,
            ca: true,
            usage: 96,
            eku: Vec::new(),
            skid: key_id(&public),
            akid: key_id(&public),
            signature: [0; 64],
        };
        cert.sign(key)?;
        cert.verify(&cert)?;
        Ok(cert)
    }
    /// Geeft een nodecertificaat uit; een verkeerde rootsleutel kan niets ondertekenen.
    pub fn issue(
        &self,
        key: &SecretKey,
        public: &[u8],
        identity: (u64, u64),
        serial: [u8; 16],
        validity: (u32, u32),
    ) -> Result<Self> {
        self.verify(self)?;
        if key.public_key().to_encoded_point(false).as_bytes() != self.public {
            return Err(Error::Invalid("certificate signing key differs from root"));
        }
        let public: [u8; 65] = public
            .try_into()
            .map_err(|_| Error::Invalid("node key width"))?;
        let mut cert = Self {
            serial: positive_serial(serial)?,
            issuer: self.subject,
            subject: Name::Node(identity.0, identity.1),
            before: validity.0,
            after: validity.1,
            public,
            ca: false,
            usage: 1,
            eku: crate::copy(&[2, 1])?,
            skid: key_id(&public),
            akid: self.skid,
            signature: [0; 64],
        };
        cert.sign(key)?;
        cert.verify(self)?;
        Ok(cert)
    }
    fn sign(&mut self, key: &SecretKey) -> Result {
        self.validate()?;
        let sig: Signature = SigningKey::from(key)
            .try_sign(&self.tbs()?)
            .map_err(|_| Error::Invalid("certificate signing failed"))?;
        self.signature.copy_from_slice(&sig.to_bytes());
        Ok(())
    }
    /// Importeert bestaande Go-X.509-opslag en weigert DER/TLV-verschillen in de getekende TBS.
    pub fn from_der(input: &[u8]) -> Result<Self> {
        let mut outer = Reader(only(input, 0x30)?.value);
        let tbs = outer.take(0x30)?;
        if outer.take(0x30)?.wire != SIG {
            return invalid();
        }
        let sig = outer.take(3)?.value;
        outer.end()?;
        if sig.first() != Some(&0) {
            return invalid();
        }
        let signature = Signature::from_der(&sig[1..])
            .map_err(|_| Error::Invalid("certificate signature DER"))?
            .to_bytes()
            .into();
        let mut r = Reader(tbs.value);
        if r.take(0xa0)?.value != [2, 1, 2] {
            return invalid();
        }
        let serial = crate::copy(der::unsigned(r.take(2)?)?)?;
        if r.take(0x30)?.wire != SIG {
            return invalid();
        }
        let issuer = Name::from_der(r.take(0x30)?.wire)?;
        let mut times = Reader(r.take(0x30)?.value);
        let before = parse_date(times.next()?)?;
        let after = parse_date(times.next()?)?;
        times.end()?;
        let subject = Name::from_der(r.take(0x30)?.wire)?;
        let mut pk = Reader(r.take(0x30)?.value);
        if pk.take(0x30)?.wire != EC {
            return invalid();
        }
        let public = pk.take(3)?.value;
        pk.end()?;
        if public.len() != 66 || public[0] != 0 {
            return invalid();
        }
        let public = public[1..]
            .try_into()
            .map_err(|_| Error::Invalid("certificate key length"))?;
        let mut cert = Self {
            serial,
            issuer,
            subject,
            before,
            after,
            public,
            ca: false,
            usage: 0,
            eku: Vec::new(),
            skid: [0; 20],
            akid: [0; 20],
            signature,
        };
        cert.read_extensions(r.take(0xa3)?.value)?;
        r.end()?;
        cert.validate()?;
        if cert.tbs()?.as_slice() != tbs.wire {
            return Err(Error::Invalid(
                "certificate DER cannot be represented losslessly as Matter TLV",
            ));
        }
        Ok(cert)
    }
    fn read_extensions(&mut self, input: &[u8]) -> Result {
        let mut r = Reader(only(input, 0x30)?.value);
        let mut seen = 0u8;
        while !r.0.is_empty() {
            let mut ext = Reader(r.take(0x30)?.value);
            let oid = ext.take(6)?.value;
            if oid.len() != 3 || oid[..2] != [0x55, 0x1d] {
                return invalid();
            }
            if ext.0.first() == Some(&1) && ext.take(1)?.value != [255] {
                return invalid();
            }
            let payload = ext.take(4)?.value;
            ext.end()?;
            let (mask, kind) = match oid[2] {
                19 => (1, 1),
                15 => (2, 2),
                37 => (4, 3),
                14 => (8, 4),
                35 => (16, 5),
                _ => return invalid(),
            };
            if seen & mask != 0 {
                return invalid();
            }
            seen |= mask;
            match kind {
                1 => match payload {
                    [0x30, 0] => self.ca = false,
                    [0x30, 3, 1, 1, 255] => self.ca = true,
                    _ => return invalid(),
                },
                2 => {
                    let bits = only(payload, 3)?.value;
                    if !(2..=3).contains(&bits.len()) || bits[0] > 7 {
                        return invalid();
                    }
                    self.usage = u16::from(bits[1].reverse_bits());
                    if bits.len() == 3 {
                        self.usage |= u16::from(bits[2].reverse_bits()) << 8;
                    }
                }
                3 => {
                    let mut r = Reader(only(payload, 0x30)?.value);
                    while !r.0.is_empty() {
                        let oid = r.take(6)?.value;
                        if oid.len() != 8 || &oid[..7] != EKU {
                            return invalid();
                        }
                        stulp_core::json::push(&mut self.eku, oid[7], 2)?;
                    }
                }
                4 => {
                    self.skid = only(payload, 4)?
                        .value
                        .try_into()
                        .map_err(|_| Error::Invalid("certificate SKID width"))?
                }
                5 => {
                    self.akid = only(only(payload, 0x30)?.value, 0x80)?
                        .value
                        .try_into()
                        .map_err(|_| Error::Invalid("certificate AKID width"))?
                }
                _ => return invalid(),
            }
        }
        if seen & 27 != 27 {
            return invalid();
        }
        Ok(())
    }
    /// Parseert compacte certificaten; valideert daarna de handtekening met `verify`.
    pub fn from_tlv(input: &[u8]) -> Result<Self> {
        if input.len() > 400 {
            return invalid();
        }
        let n = Node::parse(input)?;
        if n.element.tag != Tag::Anonymous
            || n.element.value != Value::Structure
            || n.children.len() != 11
            || (1..=11).any(|t| n.get(t).is_none())
            || n.uint(2)? != 1
            || n.uint(7)? != 1
            || n.uint(8)? != 1
        {
            return invalid();
        }
        let field = |tag| {
            n.get(tag)
                .ok_or(Error::Invalid("certificate field missing"))
        };
        let ext = field(10)?;
        if ext.element.value != Value::List {
            return invalid();
        }
        let mut seen = 0u8;
        for e in &ext.children {
            let Tag::Context(tag @ 1..=5) = e.element.tag else {
                return invalid();
            };
            let bit = 1 << (tag - 1);
            if seen & bit != 0 {
                return invalid();
            }
            seen |= bit;
        }
        if seen & 27 != 27 {
            return invalid();
        }
        let bc = ext
            .get(1)
            .ok_or(Error::Invalid("certificate constraints"))?;
        if bc.element.value != Value::Structure || bc.children.len() != 1 {
            return invalid();
        }
        let ca = match bc.get(1).map(|v| v.element.value) {
            Some(Value::Bool(b)) => b,
            _ => return invalid(),
        };
        let mut eku = Vec::new();
        if let Some(usages) = ext.get(3) {
            if usages.element.value != Value::Array {
                return invalid();
            }
            for u in &usages.children {
                let Value::Uint(v @ 1..=2) = u.element.value else {
                    return invalid();
                };
                if u.element.tag != Tag::Anonymous {
                    return invalid();
                }
                stulp_core::json::push(&mut eku, v as u8, 2)?;
            }
        }
        let c = Self {
            serial: crate::copy(n.bytes(1)?)?,
            issuer: Name::from_tlv(field(3)?)?,
            subject: Name::from_tlv(field(6)?)?,
            before: u32::try_from(n.uint(4)?)
                .map_err(|_| Error::Invalid("certificate time width"))?,
            after: u32::try_from(n.uint(5)?)
                .map_err(|_| Error::Invalid("certificate time width"))?,
            public: n
                .bytes(9)?
                .try_into()
                .map_err(|_| Error::Invalid("certificate key width"))?,
            ca,
            usage: u16::try_from(ext.uint(2)?)
                .map_err(|_| Error::Invalid("certificate usage width"))?,
            eku,
            skid: ext
                .bytes(4)?
                .try_into()
                .map_err(|_| Error::Invalid("certificate SKID width"))?,
            akid: ext
                .bytes(5)?
                .try_into()
                .map_err(|_| Error::Invalid("certificate AKID width"))?,
            signature: n
                .bytes(11)?
                .try_into()
                .map_err(|_| Error::Invalid("certificate signature width"))?,
        };
        c.validate()?;
        Ok(c)
    }
}
fn positive_serial(mut serial: [u8; 16]) -> Result<Vec<u8>> {
    serial[0] &= 127;
    serial[0] |= 1;
    crate::copy(&serial)
}
/// Importeert PKCS#8 van de bestaande Go-opslag inclusief publieke/private key-consistentie.
pub fn private_key(input: &[u8]) -> Result<SecretKey> {
    if input.len() > 512 {
        return Err(Error::Invalid("PKCS#8 length limit"));
    }
    let mut outer = Reader(only(input, 0x30)?.value);
    if outer.take(2)?.value != [0] || outer.take(0x30)?.wire != EC {
        return invalid();
    }
    let payload = outer.take(4)?.value;
    outer.end()?;
    let mut inner = Reader(only(payload, 0x30)?.value);
    if inner.take(2)?.value != [1] {
        return invalid();
    }
    let secret = inner.take(4)?.value;
    if secret.len() != 32 {
        return invalid();
    }
    let key = SecretKey::from_slice(secret).map_err(|_| Error::Invalid("invalid P-256 scalar"))?;
    if inner.0.first() == Some(&0xa0)
        && only(inner.take(0xa0)?.value, 6)?.value
            != [0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07]
    {
        return invalid();
    }
    if inner.0.first() == Some(&0xa1) {
        let public = only(inner.take(0xa1)?.value, 3)?.value;
        if public.len() != 66
            || public[0] != 0
            || public[1..] != *key.public_key().to_encoded_point(false).as_bytes()
        {
            return invalid();
        }
    }
    inner.end()?;
    Ok(key)
}
/// Fallible PKCS#8-export; iedere tijdelijke buffer met sleutelbytes wordt gewist.
pub fn private_der(key: &SecretKey) -> Result<Zeroizing<Vec<u8>>> {
    let scalar = Zeroizing::new(key.to_bytes());
    let public = key.public_key().to_encoded_point(false);
    let octets = Zeroizing::new(object(4, &[&scalar])?);
    let inner = Zeroizing::new(object(
        0x30,
        &[
            &[2, 1, 1],
            &octets,
            &object(0xa1, &[&object(3, &[&[0], public.as_bytes()])?])?,
        ],
    )?);
    let wrapped = Zeroizing::new(object(4, &[&inner])?);
    Ok(Zeroizing::new(object(0x30, &[&[2, 1, 0], EC, &wrapped])?))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn original_go_certificates_roundtrip_and_keep_signatures() -> Result {
        let root = Certificate::from_der(include_bytes!("../tests/fixtures/root.der"))?;
        let node = Certificate::from_der(include_bytes!("../tests/fixtures/node.der"))?;
        root.verify(&root)?;
        node.verify(&root)?;
        assert!(root.is_ca());
        assert!(!node.is_ca());
        assert!(node.subject() == Name::Node(0x3333, 0x1111));
        assert_eq!(root.tlv()?, include_bytes!("../tests/fixtures/root.tlv"));
        assert_eq!(node.tlv()?, include_bytes!("../tests/fixtures/node.tlv"));
        for cert in [&root, &node] {
            let reconstructed = Certificate::from_tlv(&cert.tlv()?)?;
            reconstructed.verify(&root)?;
            assert_eq!(reconstructed.der()?, cert.der()?);
        }
        for bytes in [
            include_bytes!("../tests/fixtures/root-test.pkcs8").as_slice(),
            include_bytes!("../tests/fixtures/node-test.pkcs8").as_slice(),
        ] {
            let key = private_key(bytes)?;
            assert_eq!(private_der(&key)?.as_slice(), bytes);
            let mut wrong = crate::copy(bytes)?;
            let n = wrong.len();
            wrong[n - 1] ^= 1;
            assert!(private_key(&wrong).is_err());
        }
        let mut changed = Certificate::from_tlv(&node.tlv()?)?;
        changed.subject = Name::Node(0x4444, 0x1111);
        assert!(changed.verify(&root).is_err());
        let mut changed = Certificate::from_tlv(&node.tlv()?)?;
        changed.signature[12] ^= 1;
        assert!(changed.verify(&root).is_err());
        Ok(())
    }
    #[test]
    fn new_root_signs_nodes_but_not_wrong_keys_or_malformed_der() -> Result {
        let key = private_key(include_bytes!("../tests/fixtures/root-test.pkcs8"))?;
        let peer = private_key(include_bytes!("../tests/fixtures/node-test.pkcs8"))?;
        let root = Certificate::root(&key, 0x2222, [255; 16], 800000000, 1800000000)?;
        let node = root.issue(
            &key,
            peer.public_key().to_encoded_point(false).as_bytes(),
            (0x3333, 0x1111),
            [0; 16],
            (800000000, 1800000000),
        )?;
        Certificate::from_der(&node.der()?)?.verify(&root)?;
        assert!(
            root.issue(
                &peer,
                node.public(),
                (1, 1),
                [0; 16],
                (800000000, 1800000000)
            )
            .is_err()
        );
        assert!(
            root.issue(
                &key,
                node.public(),
                (0, 1),
                [0; 16],
                (800000000, 1800000000)
            )
            .is_err()
        );
        assert!(
            root.issue(
                &key,
                node.public(),
                (1, 1),
                [0; 16],
                (1800000000, 800000000)
            )
            .is_err()
        );
        for b in [
            &[0x30, 0x80, 0, 0][..],
            &[0x30, 0x81, 0],
            &[0x30, 0x82, 0, 128],
            &[0x30, 0x82, 255, 255],
        ] {
            assert!(Certificate::from_der(b).is_err());
        }
        let der = node.der()?;
        for n in 0..der.len() {
            assert!(Certificate::from_der(&der[..n]).is_err());
        }
        for seconds in [1, 800000000, 1500000000, 1800000000, u32::MAX, 0] {
            assert_eq!(parse_date(Reader(&date(seconds)?).next()?)?, seconds);
        }
        Ok(())
    }
}
