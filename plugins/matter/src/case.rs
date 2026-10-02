//! CASE-initiator: exacte NOC-pin uit commissioning, ECDH en wederzijdse signatures.
use crate::{
    ccm::Ccm,
    pase::{Session, Status},
    spake::Keys,
    tlv::{Node, Tag, Value, Writer},
};
use alloc::vec::Vec;
use hkdf::Hkdf;
use hmac::{Hmac, Mac};
use p256::{
    PublicKey, SecretKey,
    ecdh::diffie_hellman,
    ecdsa::{
        Signature, SigningKey, VerifyingKey,
        signature::{Signer, Verifier},
    },
    elliptic_curve::sec1::ToEncodedPoint,
};
use sha2::{Digest, Sha256};
use stulp_sdk::{Error, Result};
use subtle::ConstantTimeEq;
use zeroize::{Zeroize, Zeroizing};

fn root(bytes: &[u8]) -> Result<Node<'_>> {
    let n = Node::parse(bytes)?;
    if n.element.tag != Tag::Anonymous
        || n.element.value != Value::Structure
        || n.children
            .iter()
            .any(|n| !matches!(n.element.tag, Tag::Context(_)))
    {
        return Err(Error::Invalid(
            "CASE requires an anonymous context-tagged structure",
        ));
    }
    Ok(n)
}
fn bytes<'a>(n: &Node<'a>, tag: u8, min: usize, max: usize) -> Result<&'a [u8]> {
    let b = n.bytes(tag)?;
    if !(min..=max).contains(&b.len()) {
        return Err(Error::Invalid("CASE field length"));
    }
    Ok(b)
}
fn start() -> Result<Writer> {
    let mut w = Writer::default();
    w.start(Tag::Anonymous, Value::Structure)?;
    Ok(w)
}
fn derive<const N: usize>(input: &[u8], salt: &[u8], info: &[u8]) -> Result<Zeroizing<[u8; N]>> {
    let mut out = Zeroizing::new([0; N]);
    Hkdf::<Sha256>::new(Some(salt), input)
        .expand(info, &mut *out)
        .map_err(|_| Error::Invalid("CASE HKDF length"))?;
    Ok(out)
}
fn public(key: &SecretKey) -> Result<[u8; 65]> {
    key.public_key()
        .to_encoded_point(false)
        .as_bytes()
        .try_into()
        .map_err(|_| Error::Invalid("CASE public key"))
}

/// Identiteit wordt alleen uit de al gepinde operationele NOC gelezen.
pub struct Identity {
    /// Operationeel node-ID.
    pub node: u64,
    /// Fabric-ID.
    pub fabric: u64,
    /// Publieke operationele sleutel.
    pub key: VerifyingKey,
}
impl Identity {
    /// Controleert veldtypen en dubbele DN-tags; vervangt geen certificaatvalidatie.
    pub fn parse(noc: &[u8]) -> Result<Self> {
        if noc.is_empty() || noc.len() > 400 {
            return Err(Error::Invalid("CASE NOC length"));
        }
        let n = root(noc)?;
        let key = VerifyingKey::from_sec1_bytes(bytes(&n, 9, 65, 65)?)
            .map_err(|_| Error::Invalid("CASE NOC has invalid P-256 key"))?;
        let subject = n.get(6).ok_or(Error::Invalid("CASE NOC subject missing"))?;
        if subject.element.value != Value::List {
            return Err(Error::Invalid("CASE NOC subject type"));
        }
        for (i, a) in subject.children.iter().enumerate() {
            if !matches!(a.element.tag, Tag::Context(_))
                || subject.children[..i]
                    .iter()
                    .any(|b| b.element.tag == a.element.tag)
            {
                return Err(Error::Invalid("CASE NOC ambiguous subject"));
            }
        }
        let node = subject.uint(17)?;
        let fabric = subject.uint(21)?;
        if node == 0 || fabric == 0 {
            return Err(Error::Invalid("CASE NOC zero identity"));
        }
        Ok(Self { node, fabric, key })
    }
}
/// Operationele CASE-gegevens; de eigenaar heeft NOC en root eerder gevalideerd.
/// Privésleutels worden niet gekloond en verdwijnen bij Drop.
pub struct Fabric {
    id: u64,
    node: u64,
    root: [u8; 65],
    ipk: Zeroizing<[u8; 16]>,
    key: SigningKey,
    noc: Vec<u8>,
}
impl Fabric {
    /// Importeert een gevalideerde fabric en controleert NOC/key/IDs onderling.
    pub fn new(
        id: u64,
        node: u64,
        root_public: &[u8],
        mut ipk: [u8; 16],
        mut private: [u8; 32],
        noc: &[u8],
    ) -> Result<Self> {
        let secret = SecretKey::from_slice(&private);
        private.zeroize();
        let protected_ipk = Zeroizing::new(ipk);
        ipk.zeroize();
        let key =
            SigningKey::from(secret.map_err(|_| Error::Invalid("invalid CASE operational key"))?);
        let identity = Identity::parse(noc)?;
        if id != identity.fabric || node != identity.node || identity.key != *key.verifying_key() {
            return Err(Error::Invalid("CASE local key, NOC and identity differ"));
        }
        let root: [u8; 65] = root_public
            .try_into()
            .map_err(|_| Error::Invalid("CASE root key length"))?;
        PublicKey::from_sec1_bytes(&root)
            .map_err(|_| Error::Invalid("CASE root key not on P-256"))?;
        Ok(Self {
            id,
            node,
            root,
            ipk: protected_ipk,
            key,
            noc: crate::copy(noc)?,
        })
    }
    /// Oorspronkelijke epoch-key voor AddNOC, vóór de GroupKey v1.0-afleiding.
    pub fn epoch_ipk(&self) -> Zeroizing<[u8; 16]> {
        Zeroizing::new(*self.ipk)
    }
    /// Operationeel discovery-ID, met fabric-ID in big-endian HKDF-salt.
    pub fn compressed_id(&self) -> Result<[u8; 8]> {
        Ok(*derive::<8>(
            &self.root[1..],
            &self.id.to_be_bytes(),
            b"CompressedFabric",
        )?)
    }
    /// GroupKey v1.0 voor CASE en group key management.
    pub fn operational_ipk(&self) -> Result<Zeroizing<[u8; 16]>> {
        derive::<16>(&*self.ipk, &self.compressed_id()?, b"GroupKey v1.0")
    }
    /// Selecteert fabric en peer zonder een operationeel ID onversleuteld te sturen.
    pub fn destination(&self, random: &[u8; 32], peer: u64) -> Result<[u8; 32]> {
        if peer == 0 {
            return Err(Error::Invalid("CASE peer ID is zero"));
        }
        let ipk = self.operational_ipk()?;
        let mut mac =
            Hmac::<Sha256>::new_from_slice(&*ipk).map_err(|_| Error::Invalid("CASE HMAC key"))?;
        for part in [
            random.as_slice(),
            self.root.as_slice(),
            &self.id.to_le_bytes(),
            &peer.to_le_bytes(),
        ] {
            mac.update(part);
        }
        Ok(mac.finalize().into_bytes().into())
    }
    /// Eigen operationele node-ID.
    pub fn node(&self) -> u64 {
        self.node
    }
}
/// Een nieuwe verbinding houdt een borrow op de fabric; verwijderen kan niet tussendoor.
pub struct Start<'a> {
    fabric: &'a Fabric,
    peer: u64,
    expected_noc: Vec<u8>,
    local: u16,
    ephemeral: SecretKey,
    public: [u8; 65],
    sigma1: Vec<u8>,
}
/// Sigma3 is verstuurd, maar de sessiesleutels zijn nog niet vrijgegeven.
pub struct Confirming {
    session: Session,
}
impl<'a> Start<'a> {
    /// Verwacht de exacte NOC die succesvol en duurzaam tijdens commissioning is opgeslagen.
    pub fn new(
        fabric: &'a Fabric,
        peer: u64,
        expected_noc: &[u8],
        local: u16,
        random: [u8; 32],
        mut entropy: [u8; 32],
    ) -> Result<Self> {
        let ephemeral = SecretKey::from_slice(&entropy);
        entropy.zeroize();
        let ephemeral = ephemeral.map_err(|_| Error::Invalid("invalid CASE ephemeral scalar"))?;
        let identity = Identity::parse(expected_noc)?;
        if local == 0 || identity.node != peer || identity.fabric != fabric.id {
            return Err(Error::Invalid("CASE peer identity or session invalid"));
        }
        let public = public(&ephemeral)?;
        let mut w = start()?;
        w.bytes(Tag::Context(1), &random)?;
        w.uint_width(Tag::Context(2), u64::from(local), 2)?;
        w.bytes(Tag::Context(3), &fabric.destination(&random, peer)?)?;
        w.bytes(Tag::Context(4), &public)?;
        w.start(Tag::Context(5), Value::Structure)?;
        for (tag, number, width) in [(1, 5000, 4), (2, 300, 4), (3, 4000, 2)] {
            w.uint_width(Tag::Context(tag), number, width)?;
        }
        w.end()?;
        w.end()?;
        Ok(Self {
            fabric,
            peer,
            expected_noc: crate::copy(expected_noc)?,
            local,
            ephemeral,
            public,
            sigma1: w.finish()?,
        })
    }
    /// SecureChannel CASESigma1, opcode 0x30.
    pub fn request(&self) -> &[u8] {
        &self.sigma1
    }
    /// Consumeert Sigma2 één keer en geeft Sigma3 terug, opcode 0x32.
    pub fn response(self, sigma2: &[u8]) -> Result<(Confirming, Vec<u8>)> {
        let n = root(sigma2)?;
        let random = bytes(&n, 1, 32, 32)?;
        let peer = u16::try_from(n.uint(2)?)
            .ok()
            .filter(|n| *n != 0)
            .ok_or(Error::Invalid("CASE peer session ID"))?;
        let peer_public = bytes(&n, 3, 65, 65)?;
        let public = PublicKey::from_sec1_bytes(peer_public)
            .map_err(|_| Error::Invalid("CASE peer ephemeral key"))?;
        let shared = diffie_hellman(self.ephemeral.to_nonzero_scalar(), public.as_affine());
        let ipk = self.fabric.operational_ipk()?;
        let mut salt2 = Zeroizing::new([0; 145]);
        salt2[..16].copy_from_slice(&*ipk);
        salt2[16..48].copy_from_slice(random);
        salt2[48..113].copy_from_slice(peer_public);
        salt2[113..].copy_from_slice(&Sha256::digest(&self.sigma1));
        let key = derive::<16>(shared.raw_secret_bytes(), &*salt2, b"Sigma2")?;
        let plaintext = Zeroizing::new(Ccm::new(&key, 16)?.open(
            b"NCASE_Sigma2N",
            bytes(&n, 4, 17, 1024)?,
            &[],
        )?);
        let tbe = root(&plaintext)?;
        let noc = bytes(&tbe, 1, 1, 400)?;
        // Een valide signature van een onbekende NOC is geen voldoende bewijs.
        if !bool::from(noc.ct_eq(&self.expected_noc)) {
            return Err(Error::Invalid(
                "CASE peer NOC differs from commissioned certificate",
            ));
        }
        let identity = Identity::parse(noc)?;
        if identity.node != self.peer || identity.fabric != self.fabric.id {
            return Err(Error::Invalid("CASE peer NOC identity differs"));
        }
        let icac = if tbe.get(2).is_some() {
            bytes(&tbe, 2, 0, 400)?
        } else {
            &[]
        };
        bytes(&tbe, 4, 16, 16)?;
        let signature = Signature::from_slice(bytes(&tbe, 3, 64, 64)?)
            .map_err(|_| Error::Invalid("CASE peer signature encoding"))?;
        identity
            .key
            .verify(&tbs(noc, icac, peer_public, &self.public)?, &signature)
            .map_err(|_| Error::Invalid("CASE peer signature invalid"))?;
        let signed = tbs(&self.fabric.noc, &[], &self.public, peer_public)?;
        let signature: Signature = self
            .fabric
            .key
            .try_sign(&signed)
            .map_err(|_| Error::Invalid("CASE signing failed"))?;
        let mut w = start()?;
        w.bytes(Tag::Context(1), &self.fabric.noc)?;
        w.bytes(Tag::Context(3), &signature.to_bytes())?;
        w.end()?;
        let tbe3 = Zeroizing::new(w.finish()?);
        let mut hash = Sha256::new();
        hash.update(&self.sigma1);
        hash.update(sigma2);
        let mut salt = Zeroizing::new([0; 48]);
        salt[..16].copy_from_slice(&*ipk);
        salt[16..].copy_from_slice(&hash.clone().finalize());
        let key = derive::<16>(shared.raw_secret_bytes(), &*salt, b"Sigma3")?;
        let encrypted = Ccm::new(&key, 16)?.seal(b"NCASE_Sigma3N", &tbe3, &[])?;
        let mut w = start()?;
        w.bytes(Tag::Context(1), &encrypted)?;
        w.end()?;
        let sigma3 = w.finish()?;
        hash.update(&sigma3);
        salt[16..].copy_from_slice(&hash.finalize());
        let keys = derive::<48>(shared.raw_secret_bytes(), &*salt, b"SessionKeys")?;
        let mut k = Keys {
            i2r: [0; 16],
            r2i: [0; 16],
            challenge: [0; 16],
        };
        k.i2r.copy_from_slice(&keys[..16]);
        k.r2i.copy_from_slice(&keys[16..32]);
        k.challenge.copy_from_slice(&keys[32..]);
        Ok((
            Confirming {
                session: Session {
                    local: self.local,
                    peer,
                    keys: k,
                },
            },
            sigma3,
        ))
    }
}
fn tbs(noc: &[u8], icac: &[u8], sender: &[u8], receiver: &[u8]) -> Result<Vec<u8>> {
    let mut w = start()?;
    w.bytes(Tag::Context(1), noc)?;
    if !icac.is_empty() {
        w.bytes(Tag::Context(2), icac)?;
    }
    w.bytes(Tag::Context(3), sender)?;
    w.bytes(Tag::Context(4), receiver)?;
    w.end()?;
    w.finish()
}
impl Confirming {
    /// De transportlaag ACKt ook een afwijzing voordat zij de exchange afsluit.
    pub fn finish(self, status: &[u8]) -> Result<Session> {
        if !Status::parse(status)?.is_success() {
            return Err(Error::Invalid("peer rejected CASE session"));
        }
        Ok(self.session)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn noc(key: &SecretKey, node: u64, fabric: u64, serial: u64) -> Result<Vec<u8>> {
        let mut w = start()?;
        w.uint(Tag::Context(1), serial)?;
        w.start(Tag::Context(6), Value::List)?;
        w.uint(Tag::Context(17), node)?;
        w.uint(Tag::Context(21), fabric)?;
        w.end()?;
        w.bytes(Tag::Context(9), &public(key)?)?;
        w.end()?;
        w.finish()
    }
    fn fixture() -> Result<(Fabric, SecretKey, Vec<u8>)> {
        let own = SecretKey::from_slice(&[1; 32]).map_err(|_| Error::Invalid("fixture"))?;
        let peer = SecretKey::from_slice(&[2; 32]).map_err(|_| Error::Invalid("fixture"))?;
        let root = SecretKey::from_slice(&[3; 32]).map_err(|_| Error::Invalid("fixture"))?;
        let f = Fabric::new(
            1,
            2,
            &public(&root)?,
            [5; 16],
            [1; 32],
            &noc(&own, 2, 1, 1)?,
        )?;
        let peer_noc = noc(&peer, 3, 1, 2)?;
        Ok((f, peer, peer_noc))
    }
    fn response(
        start: &Start<'_>,
        signing: &SecretKey,
        noc: &[u8],
        corrupt_signature: bool,
    ) -> Result<(Vec<u8>, Zeroizing<[u8; 32]>)> {
        let ephemeral = SecretKey::from_slice(&[6; 32]).map_err(|_| Error::Invalid("fixture"))?;
        let pubkey = public(&ephemeral)?;
        let initiator =
            PublicKey::from_sec1_bytes(&start.public).map_err(|_| Error::Invalid("fixture"))?;
        let shared = diffie_hellman(ephemeral.to_nonzero_scalar(), initiator.as_affine());
        let signed = tbs(noc, &[], &pubkey, &start.public)?;
        let sig: Signature = SigningKey::from(signing)
            .try_sign(&signed)
            .map_err(|_| Error::Invalid("fixture"))?;
        let mut sig = sig.to_bytes();
        if corrupt_signature {
            sig[0] ^= 1;
        }
        let mut w = super::start()?;
        w.bytes(Tag::Context(1), noc)?;
        w.bytes(Tag::Context(3), &sig)?;
        w.bytes(Tag::Context(4), &[0; 16])?;
        w.end()?;
        let mut salt = Zeroizing::new([0; 145]);
        salt[..16].copy_from_slice(&*start.fabric.operational_ipk()?);
        salt[16..48].fill(7);
        salt[48..113].copy_from_slice(&pubkey);
        salt[113..].copy_from_slice(&Sha256::digest(start.request()));
        let key = derive::<16>(shared.raw_secret_bytes(), &*salt, b"Sigma2")?;
        let encrypted = Ccm::new(&key, 16)?.seal(b"NCASE_Sigma2N", &w.finish()?, &[])?;
        let mut w = super::start()?;
        w.bytes(Tag::Context(1), &[7; 32])?;
        w.uint_width(Tag::Context(2), 19, 2)?;
        w.bytes(Tag::Context(3), &pubkey)?;
        w.bytes(Tag::Context(4), &encrypted)?;
        w.end()?;
        Ok((
            w.finish()?,
            Zeroizing::new((*shared.raw_secret_bytes()).into()),
        ))
    }
    #[test]
    fn peer_pin_signature_and_sigma3_transcript() -> Result {
        let (fabric, peer, noc) = fixture()?;
        let start = Start::new(&fabric, 3, &noc, 17, [8; 32], [9; 32])?;
        let s1 = crate::copy(start.request())?;
        let public = start.public;
        let (s2, shared) = response(&start, &peer, &noc, false)?;
        let (confirm, s3) = start.response(&s2)?;
        let mut hash = Sha256::new();
        hash.update(&s1);
        hash.update(&s2);
        let mut salt = [0; 48];
        salt[..16].copy_from_slice(&*fabric.operational_ipk()?);
        salt[16..].copy_from_slice(&hash.clone().finalize());
        let key = derive::<16>(&*shared, &salt, b"Sigma3")?;
        let n = root(&s3)?;
        let plain = Ccm::new(&key, 16)?.open(b"NCASE_Sigma3N", n.bytes(1)?, &[])?;
        let tbe = root(&plain)?;
        assert_eq!(tbe.bytes(1)?, fabric.noc);
        let peer_public = root(&s2)?.bytes(3)?;
        let signature =
            Signature::from_slice(tbe.bytes(3)?).map_err(|_| Error::Invalid("fixture"))?;
        fabric
            .key
            .verifying_key()
            .verify(&tbs(&fabric.noc, &[], &public, peer_public)?, &signature)
            .map_err(|_| Error::Invalid("bad Sigma3"))?;
        hash.update(&s3);
        salt[16..].copy_from_slice(&hash.finalize());
        let keys = derive::<48>(&*shared, &salt, b"SessionKeys")?;
        let session = confirm.finish(&[0; 8])?;
        assert_eq!(session.local, 17);
        assert_eq!(session.peer, 19);
        assert_eq!(session.keys.i2r, keys[..16]);
        assert_eq!(session.keys.r2i, keys[16..32]);
        assert_eq!(session.keys.challenge, keys[32..]);
        for bad_signature in [false, true] {
            let start = Start::new(&fabric, 3, &noc, 17, [8; 32], [9; 32])?;
            // Ook een geldige signature met dezelfde public key maar een andere NOC faalt.
            let presented = if bad_signature {
                crate::copy(&noc)?
            } else {
                tests::noc(&peer, 3, 1, 99)?
            };
            let (s2, _) = response(&start, &peer, &presented, bad_signature)?;
            assert!(start.response(&s2).is_err());
        }
        let start = Start::new(&fabric, 3, &noc, 17, [8; 32], [9; 32])?;
        let (mut s2, _) = response(&start, &peer, &noc, false)?;
        let length = s2.len();
        s2[length - 3] ^= 1;
        assert!(start.response(&s2).is_err());
        let start = Start::new(&fabric, 3, &noc, 17, [8; 32], [9; 32])?;
        let (s2, _) = response(&start, &peer, &noc, false)?;
        let (confirm, _) = start.response(&s2)?;
        assert!(confirm.finish(&[0, 0, 1, 0, 0, 0, 0, 0]).is_err());
        assert!(Start::new(&fabric, 4, &noc, 17, [8; 32], [9; 32]).is_err());
        Ok(())
    }
}
