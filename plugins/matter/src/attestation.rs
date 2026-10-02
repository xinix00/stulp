//! Bewijs van DAC-bezit, DAC→PAI-handtekening en CSR-binding; geen PAA/DCL-certificeringsclaim.
use crate::{
    der::{Reader, only, unsigned},
    tlv::{Node, Tag, Value},
};
use p256::{
    ecdsa::{
        Signature, VerifyingKey,
        signature::{Verifier, hazmat::PrehashVerifier},
    },
    elliptic_curve::sec1::ToEncodedPoint,
};
use sha2::{Digest, Sha256};
use stulp_sdk::{Error, Result};
use subtle::ConstantTimeEq;
const SIG: &[u8] = &[
    0x30, 0x0a, 0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x02,
];
const EC: &[u8] = &[
    0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06, 0x08, 0x2a, 0x86, 0x48,
    0xce, 0x3d, 0x03, 0x01, 0x07,
];
const MATTER: &[u8] = &[0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0xa2, 0x7c, 0x02];
fn invalid<T>() -> Result<T> {
    Err(Error::Invalid("invalid Matter attestation certificate"))
}
fn key(spki: &[u8]) -> Result<VerifyingKey> {
    let mut r = Reader(only(spki, 0x30)?.value);
    if r.take(0x30)?.wire != EC {
        return invalid();
    }
    let bits = r.take(3)?.value;
    r.end()?;
    if bits.len() != 66 || bits[0] != 0 {
        return invalid();
    }
    VerifyingKey::from_sec1_bytes(&bits[1..])
        .map_err(|_| Error::Invalid("attestation public key not on P-256"))
}
fn signature(bits: &[u8]) -> Result<Signature> {
    if bits.first() != Some(&0) {
        return invalid();
    }
    Signature::from_der(&bits[1..]).map_err(|_| Error::Invalid("attestation signature DER"))
}
struct Cert<'a> {
    tbs: &'a [u8],
    issuer: &'a [u8],
    subject: &'a [u8],
    key: VerifyingKey,
    signature: Signature,
    ca: bool,
    usage: Option<u16>,
}
impl<'a> Cert<'a> {
    fn parse(bytes: &'a [u8]) -> Result<Self> {
        let mut outer = Reader(only(bytes, 0x30)?.value);
        let tbs = outer.take(0x30)?.wire;
        if outer.take(0x30)?.wire != SIG {
            return invalid();
        }
        let signature = signature(outer.take(3)?.value)?;
        outer.end()?;
        let mut r = Reader(only(tbs, 0x30)?.value);
        if r.take(0xa0)?.value != [2, 1, 2] {
            return invalid();
        }
        let serial = unsigned(r.take(2)?)?;
        if serial.is_empty() || serial.len() > 20 {
            return invalid();
        }
        if r.take(0x30)?.wire != SIG {
            return invalid();
        }
        let issuer = r.take(0x30)?.wire;
        let mut validity = Reader(r.take(0x30)?.value);
        for _ in 0..2 {
            let t = validity.next()?;
            use ::der::Decode;
            match t.tag {
                23 => {
                    ::der::asn1::UtcTime::from_der(t.wire)
                        .map_err(|_| Error::Invalid("attestation UTC time"))?;
                }
                24 => {
                    ::der::asn1::GeneralizedTime::from_der(t.wire)
                        .map_err(|_| Error::Invalid("attestation general time"))?;
                }
                _ => return invalid(),
            }
        }
        validity.end()?;
        let subject = r.take(0x30)?.wire;
        let key = key(r.take(0x30)?.wire)?;
        let mut ca = false;
        let mut usage = None;
        if !r.0.is_empty() {
            let extensions = r.take(0xa3)?;
            let mut extensions = Reader(only(extensions.value, 0x30)?.value);
            let mut seen = alloc::vec::Vec::<&[u8]>::new();
            while !extensions.0.is_empty() {
                let mut e = Reader(extensions.take(0x30)?.value);
                let oid = e.take(6)?.value;
                if seen.contains(&oid) {
                    return invalid();
                }
                stulp_core::json::push(&mut seen, oid, 32)?;
                let critical = if e.0.first() == Some(&1) {
                    if e.take(1)?.value != [255] {
                        return invalid();
                    }
                    true
                } else {
                    false
                };
                let bytes = e.take(4)?.value;
                e.end()?;
                match oid {
                    [0x55, 0x1d, 19] => {
                        let mut b = Reader(only(bytes, 0x30)?.value);
                        if b.0.first() == Some(&1) {
                            if b.take(1)?.value != [255] {
                                return invalid();
                            }
                            ca = true;
                        }
                        if b.0.first() == Some(&2) {
                            let p = unsigned(b.take(2)?)?;
                            if !ca || p.len() > 4 {
                                return invalid();
                            }
                        }
                        b.end()?;
                    }
                    [0x55, 0x1d, 15] => {
                        let bits = only(bytes, 3)?.value;
                        if !(2..=3).contains(&bits.len())
                            || bits[0] > 7
                            || bits[bits.len() - 1] & ((1u8 << bits[0]) - 1) != 0
                        {
                            return invalid();
                        }
                        let mut u = u16::from(bits[1].reverse_bits());
                        if bits.len() == 3 {
                            u |= u16::from(bits[2].reverse_bits()) << 8;
                        }
                        usage = Some(u);
                    }
                    [0x55, 0x1d, 14] => {
                        if only(bytes, 4)?.value.len() != 20 {
                            return invalid();
                        }
                    }
                    [0x55, 0x1d, 35] => {
                        let mut a = Reader(only(bytes, 0x30)?.value);
                        while !a.0.is_empty() {
                            a.next()?;
                        }
                    }
                    _ if critical => {
                        return Err(Error::Invalid(
                            "unsupported critical attestation certificate extension",
                        ));
                    }
                    _ => (),
                }
            }
        }
        r.end()?;
        Ok(Self {
            tbs,
            issuer,
            subject,
            key,
            signature,
            ca,
            usage,
        })
    }
}
fn vid_pid(subject: &[u8]) -> Result<(u16, u16)> {
    let mut name = Reader(only(subject, 0x30)?.value);
    let mut vendor = None;
    let mut product = None;
    let mut count = 0;
    while !name.0.is_empty() {
        let mut rdn = Reader(name.take(0x31)?.value);
        while !rdn.0.is_empty() {
            count += 1;
            if count > 32 {
                return invalid();
            }
            let mut a = Reader(rdn.take(0x30)?.value);
            let oid = a.take(6)?.value;
            let text = a.next()?;
            a.end()?;
            if oid.len() != 10 || &oid[..9] != MATTER || !matches!(oid[9], 1 | 2) {
                continue;
            }
            if !matches!(text.tag, 12 | 19) || text.value.is_empty() || text.value.len() > 4 {
                return invalid();
            }
            let value = core::str::from_utf8(text.value)
                .ok()
                .and_then(|v| u16::from_str_radix(v, 16).ok())
                .ok_or(Error::Invalid("DAC vendor/product hex"))?;
            let target = if oid[9] == 1 {
                &mut vendor
            } else {
                &mut product
            };
            if target.is_some() {
                return invalid();
            }
            *target = Some(value);
        }
    }
    Ok((
        vendor.ok_or(Error::Invalid("DAC vendor ID missing"))?,
        product.ok_or(Error::Invalid("DAC product ID missing"))?,
    ))
}
/// Het geverifieerde DAC, gebonden aan onboarding vendor/product wanneer opgegeven.
pub struct Verified {
    key: VerifyingKey,
    /// Vendor-ID uit het ondertekende DAC.
    pub vendor: u16,
    /// Product-ID uit het ondertekende DAC.
    pub product: u16,
}
impl Verified {
    /// Toetst DAC→PAI en VID/PID; certificeringsbeleid en datumpolicy blijven apart.
    pub fn chain(pai: &[u8], dac: &[u8], vendor: u16, product: u16) -> Result<Self> {
        let pai = Cert::parse(pai)?;
        let dac = Cert::parse(dac)?;
        if !pai.ca || pai.usage.is_some_and(|u| u & 32 == 0) || dac.ca || dac.issuer != pai.subject
        {
            return invalid();
        }
        pai.key
            .verify(dac.tbs, &dac.signature)
            .map_err(|_| Error::Invalid("DAC is not signed by supplied PAI"))?;
        let (vid, pid) = vid_pid(dac.subject)?;
        if vendor != 0 && vendor != vid || product != 0 && product != pid {
            return Err(Error::Invalid("DAC vendor/product differ from onboarding"));
        }
        Ok(Self {
            key: dac.key,
            vendor: vid,
            product: pid,
        })
    }
    /// Nonce, elements en PASE-challenge zijn alle drie nodig voor dezelfde sessie.
    pub fn verify(
        &self,
        elements: &[u8],
        signature: &[u8],
        challenge: &[u8; 16],
        nonce: &[u8; 32],
    ) -> Result {
        if elements.len() > 4096 {
            return Err(Error::Invalid("attestation elements too large"));
        }
        let n = Node::parse(elements)?;
        if n.element.value != Value::Structure
            || n.element.tag != Tag::Anonymous
            || !bool::from(n.bytes(2)?.ct_eq(nonce))
        {
            return Err(Error::Invalid("attestation nonce or structure differs"));
        }
        let sig = Signature::from_slice(signature)
            .map_err(|_| Error::Invalid("attestation signature width"))?;
        let mut hash = Sha256::new();
        hash.update(elements);
        hash.update(challenge);
        self.key
            .verify_prehash(&hash.finalize(), &sig)
            .map_err(|_| Error::Invalid("attestation signature invalid"))
    }
    /// CSR bezit zijn eigen sleutel én is door hetzelfde DAC en dezelfde PASE-sessie getekend.
    pub fn csr(
        &self,
        elements: &[u8],
        signature: &[u8],
        challenge: &[u8; 16],
        nonce: &[u8; 32],
    ) -> Result<[u8; 65]> {
        self.verify(elements, signature, challenge, nonce)?;
        let n = Node::parse(elements)?;
        let csr = n.bytes(1)?;
        let mut outer = Reader(only(csr, 0x30)?.value);
        let tbs = outer.take(0x30)?;
        if outer.take(0x30)?.wire != SIG {
            return Err(Error::Invalid("CSR signature algorithm"));
        }
        let sig = signature_der(outer.take(3)?.value)?;
        outer.end()?;
        let mut r = Reader(tbs.value);
        if r.take(2)?.value != [0] {
            return Err(Error::Invalid("CSR version"));
        }
        r.take(0x30)?;
        let key = key(r.take(0x30)?.wire)?;
        if !r.0.is_empty() {
            let mut attrs = Reader(r.take(0xa0)?.value);
            let mut count = 0;
            while !attrs.0.is_empty() {
                count += 1;
                if count > 32 {
                    return Err(Error::Invalid("CSR attributes limit"));
                }
                let mut a = Reader(attrs.take(0x30)?.value);
                a.take(6)?;
                let mut values = Reader(a.take(0x31)?.value);
                while !values.0.is_empty() {
                    values.next()?;
                }
                a.end()?;
            }
        }
        r.end()?;
        key.verify(tbs.wire, &sig)
            .map_err(|_| Error::Invalid("CSR proof of possession failed"))?;
        key.as_affine()
            .to_encoded_point(false)
            .as_bytes()
            .try_into()
            .map_err(|_| Error::Invalid("CSR public key width"))
    }
}
// De methodeparameter heet signature; houd de DER-helper expliciet apart.
fn signature_der(bits: &[u8]) -> Result<Signature> {
    signature(bits)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn original_go_attestation_and_csr_bind_nonce_session_and_device() -> Result {
        let pai = include_bytes!("../tests/fixtures/pai.der");
        let dac = include_bytes!("../tests/fixtures/dac.der");
        let a = Verified::chain(pai, dac, 0xfff1, 0x8000)?;
        assert_eq!(a.vendor, 0xfff1);
        assert_eq!(a.product, 0x8000);
        let elements = include_bytes!("../tests/fixtures/attestation.tlv");
        let signature = include_bytes!("../tests/fixtures/attestation.sig");
        a.verify(elements, signature, &[43; 16], &[42; 32])?;
        assert!(a.verify(elements, signature, &[44; 16], &[42; 32]).is_err());
        assert!(a.verify(elements, signature, &[43; 16], &[44; 32]).is_err());
        let mut sig = *signature;
        sig[3] ^= 1;
        assert!(a.verify(elements, &sig, &[43; 16], &[42; 32]).is_err());
        let key = a.csr(
            include_bytes!("../tests/fixtures/csr.tlv"),
            include_bytes!("../tests/fixtures/csr.sig"),
            &[43; 16],
            &[42; 32],
        )?;
        assert_eq!(
            key.as_slice(),
            include_bytes!("../tests/fixtures/csr-public.bin")
        );
        assert!(a.csr(elements, signature, &[43; 16], &[42; 32]).is_err());
        assert!(Verified::chain(pai, dac, 1, 0x8000).is_err());
        assert!(Verified::chain(dac, dac, 0xfff1, 0x8000).is_err());
        let mut changed = crate::copy(dac)?;
        let n = changed.len();
        changed[n - 1] ^= 1;
        assert!(Verified::chain(pai, &changed, 0xfff1, 0x8000).is_err());
        for n in 0..dac.len() {
            assert!(Verified::chain(pai, &dac[..n], 0, 0).is_err());
        }
        Ok(())
    }
}
