//! SPAKE2+ over P-256; iedere rol verbruikt zijn sessie bij de bevestiging.
use hkdf::Hkdf;
use hmac::{Hmac, Mac};
use p256::{
    ProjectivePoint, PublicKey, Scalar, SecretKey,
    elliptic_curve::{Field, Group, sec1::ToEncodedPoint},
};
use sha2::{Digest, Sha256};
use stulp_sdk::{Error, Result};
use subtle::ConstantTimeEq;
use zeroize::{Zeroize, Zeroizing};
const M: [u8; 33] = [
    0x02, 0x88, 0x6e, 0x2f, 0x97, 0xac, 0xe4, 0x6e, 0x55, 0xba, 0x9d, 0xd7, 0x24, 0x25, 0x79, 0xf2,
    0x99, 0x3b, 0x64, 0xe1, 0x6e, 0xf3, 0xdc, 0xab, 0x95, 0xaf, 0xd4, 0x97, 0x33, 0x3d, 0x8f, 0xa1,
    0x2f,
];
const N: [u8; 33] = [
    0x03, 0xd8, 0xbb, 0xd6, 0xc6, 0x39, 0xc6, 0x29, 0x37, 0xb0, 0x4d, 0x99, 0x7f, 0x38, 0xc3, 0x77,
    0x07, 0x19, 0xc6, 0x29, 0xd7, 0x01, 0x4d, 0x49, 0xa2, 0x4b, 0x4f, 0x98, 0xba, 0xa1, 0x29, 0x2b,
    0x49,
];
fn point(bytes: &[u8]) -> Result<ProjectivePoint> {
    PublicKey::from_sec1_bytes(bytes)
        .map(|k| ProjectivePoint::from(*k.as_affine()))
        .map_err(|_| Error::Invalid("SPAKE point is not finite and on P-256"))
}
fn encoded(point: ProjectivePoint) -> Result<[u8; 65]> {
    point
        .to_affine()
        .to_encoded_point(false)
        .as_bytes()
        .try_into()
        .map_err(|_| Error::Invalid("SPAKE identity point"))
}
fn scalar(mut bytes: [u8; 32]) -> Result<Scalar> {
    let key = SecretKey::from_slice(&bytes);
    bytes.zeroize();
    key.map(|s| *s.to_nonzero_scalar().as_ref())
        .map_err(|_| Error::Invalid("invalid random SPAKE scalar"))
}
fn mac(key: &[u8], parts: &[&[u8]]) -> Result<[u8; 32]> {
    let mut h = Hmac::<Sha256>::new_from_slice(key).map_err(|_| Error::Invalid("HMAC key"))?;
    for part in parts {
        h.update(part);
    }
    Ok(h.finalize().into_bytes().into())
}
/// De twee wachtwoordscalars; Drop wist ze ook op foutpaden.
pub struct Scalars {
    w0: Scalar,
    w1: Scalar,
}
impl Drop for Scalars {
    fn drop(&mut self) {
        self.w0.zeroize();
        self.w1.zeroize();
    }
}
impl Scalars {
    /// PBKDF2-SHA256 met 16–32 byte salt en 1000–100000 iteraties.
    pub fn derive(passcode: u32, salt: &[u8], iterations: u32) -> Result<Self> {
        let mut work = Pbkdf::new(passcode, salt, iterations)?;
        while !work.step(4096)? {}
        work.finish()
    }
    fn reduced(derived: &[u8; 96]) -> Result<Self> {
        // Horner over scalars reduceert alle 40 bytes, zonder variabele big integers.
        let reduce = |bytes: &[u8]| {
            let mut n = Scalar::ZERO;
            for b in bytes {
                n = n * Scalar::from(256u64) + Scalar::from(u64::from(*b));
            }
            n
        };
        let w0 = reduce(&derived[..40]);
        let w1 = reduce(&derived[40..80]);
        if bool::from(w0.is_zero()) || bool::from(w1.is_zero()) {
            return Err(Error::Invalid("SPAKE zero derived scalar"));
        }
        Ok(Self { w0, w1 })
    }
    /// De responder bewaart w0 en het openbare L, geen passcode of w1.
    pub fn register(&self) -> Result<Registration> {
        Ok(Registration {
            w0: self.w0,
            l: encoded(ProjectivePoint::GENERATOR * self.w1)?,
        })
    }
}
/// PBKDF2 in begrensde stappen; de eigenaar kan tussen stappen heartbeats afhandelen.
pub struct Pbkdf {
    password: Zeroizing<[u8; 4]>,
    salt: [u8; 32],
    salt_len: usize,
    derived: Zeroizing<[u8; 96]>,
    u: Zeroizing<[u8; 32]>,
    iterations: u32,
    iteration: u32,
    block: usize,
}
impl Pbkdf {
    /// Valideert iteraties en salt vóór kostbaar werk; alle geheime tussenwaarden worden gewist.
    pub fn new(mut passcode: u32, salt: &[u8], iterations: u32) -> Result<Self> {
        let password = Zeroizing::new(passcode.to_le_bytes());
        passcode.zeroize();
        if !(16..=32).contains(&salt.len()) || !(1000..=100000).contains(&iterations) {
            return Err(Error::Invalid(
                "SPAKE PBKDF parameters outside Matter bounds",
            ));
        }
        let mut saved = [0; 32];
        saved[..salt.len()].copy_from_slice(salt);
        Ok(Self {
            password,
            salt: saved,
            salt_len: salt.len(),
            derived: Zeroizing::new([0; 96]),
            u: Zeroizing::new([0; 32]),
            iterations,
            iteration: 0,
            block: 0,
        })
    }
    /// Maximaal 4096 HMAC-rondes per stap. True betekent dat alle drie blokken gereed zijn.
    pub fn step(&mut self, rounds: u32) -> Result<bool> {
        if rounds == 0 || rounds > 4096 {
            return Err(Error::Invalid("invalid PBKDF work budget"));
        }
        for _ in 0..rounds {
            if self.block == 3 {
                return Ok(true);
            }
            let at = self.block * 32;
            if self.iteration == 0 {
                *self.u = mac(
                    &*self.password,
                    &[
                        &self.salt[..self.salt_len],
                        &((self.block + 1) as u32).to_be_bytes(),
                    ],
                )?;
                self.derived[at..at + 32].copy_from_slice(&*self.u);
            } else {
                *self.u = mac(&*self.password, &[&*self.u])?;
                for (b, v) in self.derived[at..at + 32].iter_mut().zip(self.u.iter()) {
                    *b ^= v;
                }
            }
            self.iteration += 1;
            if self.iteration == self.iterations {
                self.iteration = 0;
                self.block += 1;
            }
        }
        Ok(self.block == 3)
    }
    /// Halve afleidingen kunnen nooit als sleutel gebruikt worden.
    pub fn finish(self) -> Result<Scalars> {
        if self.block != 3 {
            return Err(Error::Invalid("SPAKE PBKDF is incomplete"));
        }
        Scalars::reduced(&self.derived)
    }
}
/// Verificatiemateriaal van een geopende commissioning window.
pub struct Registration {
    w0: Scalar,
    l: [u8; 65],
}
impl Drop for Registration {
    fn drop(&mut self) {
        self.w0.zeroize();
    }
}
impl Registration {
    /// De serialisatie voor OpenCommissioningWindow: w0 || L, precies 97 bytes.
    pub fn bytes(&self) -> [u8; 97] {
        let mut out = [0; 97];
        out[..32].copy_from_slice(&self.w0.to_bytes());
        out[32..].copy_from_slice(&self.l);
        out
    }
}
/// Sleutels komen uitsluitend na wederzijdse bevestiging beschikbaar.
pub struct Keys {
    /// Initiator naar responder.
    pub i2r: [u8; 16],
    /// Responder naar initiator.
    pub r2i: [u8; 16],
    /// Uitdaging voor attestation tijdens commissioning.
    pub challenge: [u8; 16],
}
impl Drop for Keys {
    fn drop(&mut self) {
        self.i2r.zeroize();
        self.r2i.zeroize();
        self.challenge.zeroize();
    }
}
/// De exacte PASE-context: geen extra lengteprefixen rond deze twee TLV-berichten.
pub fn context(request: &[u8], response: &[u8]) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(b"CHIP PAKE V1 Commissioning");
    h.update(request);
    h.update(response);
    h.finalize().into()
}
/// Commissioner met een verse geheime scalar en zijn openbare Pake1-share.
pub struct Prover {
    context: [u8; 32],
    scalars: Scalars,
    x: Scalar,
    share: [u8; 65],
}
impl Drop for Prover {
    fn drop(&mut self) {
        self.x.zeroize();
    }
}
impl Prover {
    /// De aanroeper levert verse OS-entropy; een ongeldig scalar vraagt nieuwe bytes.
    pub fn new(context: [u8; 32], scalars: Scalars, entropy: [u8; 32]) -> Result<Self> {
        let x = scalar(entropy)?;
        let share = encoded(ProjectivePoint::GENERATOR * x + point(&M)? * scalars.w0)?;
        Ok(Self {
            context,
            scalars,
            x,
            share,
        })
    }
    /// X gaat als Pake1 over de onbeveiligde sessie.
    pub fn share(&self) -> &[u8; 65] {
        &self.share
    }
    /// Consumeert Pake2 en de geheime sessie, controleert cB en levert cA plus keys.
    pub fn finish(self, peer: &[u8], confirmation: &[u8]) -> Result<([u8; 32], Keys)> {
        let unmasked = point(peer)? - point(&N)? * self.scalars.w0;
        if bool::from(unmasked.is_identity()) {
            return Err(Error::Invalid("SPAKE peer unmasks to identity"));
        }
        let z = Zeroizing::new(encoded(unmasked * self.x)?);
        let v = Zeroizing::new(encoded(unmasked * self.scalars.w1)?);
        let (keys, a, b) = schedule(&self.context, &self.share, peer, &*z, &*v, self.scalars.w0)?;
        if !bool::from(b.ct_eq(confirmation)) {
            return Err(Error::Invalid(
                "SPAKE passcode or peer confirmation mismatch",
            ));
        }
        Ok((a, keys))
    }
}
/// Responder vóór ontvangst van de eerste peer-share.
pub struct Verifier {
    context: [u8; 32],
    registration: Registration,
}
/// Responder na Pake2; keys blijven privé tot Pake3 klopt.
pub struct AwaitConfirm {
    share: [u8; 65],
    a: [u8; 32],
    b: [u8; 32],
    keys: Keys,
}
impl Verifier {
    /// Bezit de registratie en context voor precies één handshake.
    pub fn new(context: [u8; 32], registration: Registration) -> Self {
        Self {
            context,
            registration,
        }
    }
    /// Valideert X en produceert Y en cB met een verse responder-scalar.
    pub fn accept(self, peer: &[u8], entropy: [u8; 32]) -> Result<AwaitConfirm> {
        let y = Zeroizing::new(scalar(entropy)?);
        let unmasked = point(peer)? - point(&M)? * self.registration.w0;
        if bool::from(unmasked.is_identity()) {
            return Err(Error::Invalid("SPAKE peer unmasks to identity"));
        }
        let share = encoded(ProjectivePoint::GENERATOR * *y + point(&N)? * self.registration.w0)?;
        let z = Zeroizing::new(encoded(unmasked * *y)?);
        let v = Zeroizing::new(encoded(point(&self.registration.l)? * *y)?);
        let (keys, a, b) = schedule(&self.context, peer, &share, &*z, &*v, self.registration.w0)?;
        Ok(AwaitConfirm { share, a, b, keys })
    }
}
impl AwaitConfirm {
    /// Openbare responder-share Y.
    pub fn share(&self) -> &[u8; 65] {
        &self.share
    }
    /// cB bevestigt bezit van het wachtwoordmateriaal.
    pub fn confirmation(&self) -> &[u8; 32] {
        &self.b
    }
    /// Keys verlaten de responder pas nadat cA constant-time gecontroleerd is.
    pub fn confirm(self, confirmation: &[u8]) -> Result<Keys> {
        if !bool::from(self.a.ct_eq(confirmation)) {
            return Err(Error::Invalid("SPAKE commissioner confirmation mismatch"));
        }
        Ok(self.keys)
    }
}
fn schedule(
    context: &[u8],
    x: &[u8],
    y: &[u8],
    z: &[u8],
    v: &[u8],
    w0: Scalar,
) -> Result<(Keys, [u8; 32], [u8; 32])> {
    let m = encoded(point(&M)?)?;
    let n = encoded(point(&N)?)?;
    let w0 = Zeroizing::new(w0.to_bytes());
    let mut transcript = Sha256::new();
    for item in [context, &[], &[], &m, &n, x, y, z, v, w0.as_slice()] {
        transcript.update((item.len() as u64).to_le_bytes());
        transcript.update(item);
    }
    let main = Zeroizing::new(transcript.finalize());
    let mut confirm = Zeroizing::new([0; 32]);
    let mut session = Zeroizing::new([0; 48]);
    Hkdf::<Sha256>::new(None, &main[..16])
        .expand(b"ConfirmationKeys", &mut *confirm)
        .map_err(|_| Error::Invalid("SPAKE confirmation key schedule"))?;
    Hkdf::<Sha256>::new(None, &main[16..])
        .expand(b"SessionKeys", &mut *session)
        .map_err(|_| Error::Invalid("SPAKE session key schedule"))?;
    let mut keys = Keys {
        i2r: [0; 16],
        r2i: [0; 16],
        challenge: [0; 16],
    };
    keys.i2r.copy_from_slice(&session[..16]);
    keys.r2i.copy_from_slice(&session[16..32]);
    keys.challenge.copy_from_slice(&session[32..]);
    Ok((keys, mac(&confirm[..16], &[y])?, mac(&confirm[16..], &[x])?))
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn roles_agree_and_wrong_passcode_is_rejected() -> Result {
        let salt = b"SPAKE2P Key Salt!";
        for wrong in [false, true] {
            let scalars = Scalars::derive(20202021, salt, 1000)?;
            let registration = scalars.register()?;
            let scalars = if wrong {
                Scalars::derive(20202022, salt, 1000)?
            } else {
                scalars
            };
            let ctx = context(b"request", b"response");
            let p = Prover::new(ctx, scalars, [1; 32])?;
            let v = Verifier::new(ctx, registration).accept(p.share(), [2; 32])?;
            let result = p.finish(v.share(), v.confirmation());
            if wrong {
                assert!(result.is_err());
                assert!(v.confirm(&[0; 32]).is_err());
            } else {
                let (a, k) = result?;
                let peer = v.confirm(&a)?;
                assert_eq!(k.i2r, peer.i2r);
                assert_eq!(k.r2i, peer.r2i);
                assert_eq!(k.challenge, peer.challenge);
                assert_ne!(k.i2r, k.r2i);
            }
        }
        assert!(point(&[0; 65]).is_err());
        assert!(Scalars::derive(20202021, b"short", 1000).is_err());
        Ok(())
    }
}
