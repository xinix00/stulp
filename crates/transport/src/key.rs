//! De private sleutel van de server: Go-PEM-sleutels geladen met RustCrypto.
//!
//! De TLS-server zelf is `leantls::server`; die laat de sleutel bij de
//! aanroeper via [`leantls::server::Identity`]. Hier wonen alleen het laden,
//! tekenen en (voor clients zonder X25519) P-256-ECDH.
use leantls::{Error, Result, server::Identity};
use p256::{
    ecdsa::{Signature, SigningKey, signature::Signer},
    pkcs8::DecodePrivateKey,
};

/// Een serveridentiteit voor de gebruikelijke Go PEM-sleutels: EC, Ed25519 en RSA.
/// Alle sleuteltypen wissen hun private componenten bij Drop.
pub struct KeyPair {
    chain: Vec<Vec<u8>>,
    key: Key,
}
enum Key {
    P256(SigningKey),
    P384(p384::ecdsa::SigningKey),
    Ed25519(ed25519_dalek::SigningKey),
    Rsa(rsa::pss::BlindedSigningKey<sha2::Sha256>),
}
fn invalid(why: &'static str) -> Error {
    Error::PeerRejected(why)
}
impl KeyPair {
    /// Laadt PKCS#8, SEC1 of PKCS#1 DER en controleert certificaat/sleutel vóór het luisteren.
    pub fn new(chain: Vec<Vec<u8>>, der: &[u8]) -> Result<Self> {
        use rsa::{pkcs1::DecodeRsaPrivateKey, traits::PublicKeyParts};
        let key = if let Ok(k) =
            p256::SecretKey::from_pkcs8_der(der).or_else(|_| p256::SecretKey::from_sec1_der(der))
        {
            Key::P256(SigningKey::from(k))
        } else if let Ok(k) =
            p384::SecretKey::from_pkcs8_der(der).or_else(|_| p384::SecretKey::from_sec1_der(der))
        {
            Key::P384(p384::ecdsa::SigningKey::from(k))
        } else if let Ok(k) = ed25519_dalek::SigningKey::from_pkcs8_der(der) {
            Key::Ed25519(k)
        } else {
            let k = rsa::RsaPrivateKey::from_pkcs8_der(der)
                .or_else(|_| rsa::RsaPrivateKey::from_pkcs1_der(der))
                .map_err(|_| invalid("unsupported or invalid TLS private key"))?;
            if !(2048..=4096).contains(&k.n().bits()) {
                return Err(invalid("RSA key must be 2048-4096 bits"));
            }
            k.validate()
                .map_err(|_| invalid("invalid RSA private key"))?;
            Key::Rsa(rsa::pss::BlindedSigningKey::new(k))
        };
        let identity = Self { chain, key };
        leantls::server::validate_identity(&identity)?;
        Ok(identity)
    }
}
fn copied(bytes: &[u8]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    out.try_reserve_exact(bytes.len())
        .map_err(|_| Error::Alloc)?;
    out.extend_from_slice(bytes);
    Ok(out)
}
impl Identity for KeyPair {
    fn certificates(&self) -> &[Vec<u8>] {
        &self.chain
    }
    fn signature_scheme(&self) -> u16 {
        match self.key {
            Key::P256(_) => 0x0403,
            Key::P384(_) => 0x0503,
            Key::Ed25519(_) => 0x0807,
            Key::Rsa(_) => 0x0804,
        }
    }
    fn sign(&self, message: &[u8], seed: [u8; 32]) -> Result<Vec<u8>> {
        match &self.key {
            Key::P256(key) => {
                let signature: Signature =
                    key.try_sign(message).map_err(|_| Error::BadSignature)?;
                copied(signature.to_der().as_bytes())
            }
            Key::P384(key) => {
                let signature: p384::ecdsa::Signature =
                    key.try_sign(message).map_err(|_| Error::BadSignature)?;
                copied(signature.to_der().as_bytes())
            }
            Key::Ed25519(key) => copied(
                &key.try_sign(message)
                    .map_err(|_| Error::BadSignature)?
                    .to_bytes(),
            ),
            Key::Rsa(key) => {
                use p256::ecdsa::signature::{RandomizedSigner, SignatureEncoding};
                use rand_chacha::{ChaCha20Rng, rand_core::SeedableRng};
                let mut rng = ChaCha20Rng::from_seed(seed);
                let signature = key
                    .try_sign_with_rng(&mut rng, message)
                    .map_err(|_| Error::BadSignature)?;
                Ok(signature.to_vec())
            }
        }
    }
    // Clients zonder X25519 (oudere Go- en Java-stacks) krijgen P-256.
    fn agree_p256(&self, seed: &[u8; 32], peer: &[u8; 65]) -> Option<Result<([u8; 65], [u8; 32])>> {
        Some((|| {
            use p256::elliptic_curve::sec1::ToEncodedPoint;
            let secret = p256::SecretKey::from_slice(seed).map_err(|_| Error::KeyShare)?;
            let peer = p256::PublicKey::from_sec1_bytes(peer).map_err(|_| Error::KeyShare)?;
            let shared = p256::ecdh::diffie_hellman(secret.to_nonzero_scalar(), peer.as_affine());
            let public = secret.public_key().to_encoded_point(false);
            let public: [u8; 65] = public.as_bytes().try_into().map_err(|_| Error::KeyShare)?;
            Ok((public, (*shared.raw_secret_bytes()).into()))
        })())
    }
    fn offers_p256(&self) -> bool {
        true
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic, clippy::expect_used)]
mod tests {
    use super::KeyPair;
    use core::future::{Future, poll_fn};
    use core::pin::{Pin, pin};
    use core::task::{Context, Poll, Waker};
    use leantls::{AsyncRead, AsyncWrite, Entropy};
    use std::io::{Read, Write};
    use std::net::{TcpListener, TcpStream};
    use std::path::PathBuf;
    use std::process::{Command, Stdio};
    use std::time::Duration;

    fn block_on<F: Future>(f: F) -> F::Output {
        let mut f = pin!(f);
        let mut cx = Context::from_waker(Waker::noop());
        loop {
            if let Poll::Ready(v) = f.as_mut().poll(&mut cx) {
                return v;
            }
            std::thread::yield_now();
        }
    }
    struct Tcp(TcpStream);
    impl AsyncRead for Tcp {
        type Error = std::io::Error;
        fn poll_read(
            self: Pin<&mut Self>,
            _: &mut Context<'_>,
            buf: &mut [u8],
        ) -> Poll<Result<usize, Self::Error>> {
            Poll::Ready(self.get_mut().0.read(buf))
        }
    }
    impl AsyncWrite for Tcp {
        type Error = std::io::Error;
        fn poll_write(
            self: Pin<&mut Self>,
            _: &mut Context<'_>,
            buf: &[u8],
        ) -> Poll<Result<usize, Self::Error>> {
            Poll::Ready(self.get_mut().0.write(buf))
        }
    }
    fn entropy() -> Entropy {
        let mut b = [0u8; Entropy::LEN];
        std::fs::File::open("/dev/urandom")
            .unwrap()
            .read_exact(&mut b)
            .unwrap();
        Entropy::new(b)
    }
    async fn write_all<W: AsyncWrite + Unpin>(w: &mut W, mut buf: &[u8]) -> Result<(), W::Error> {
        while !buf.is_empty() {
            let n = poll_fn(|cx| Pin::new(&mut *w).poll_write(cx, buf)).await?;
            buf = &buf[n..];
        }
        Ok(())
    }
    async fn read_exact<R: AsyncRead + Unpin>(
        r: &mut R,
        mut buf: &mut [u8],
    ) -> Result<(), R::Error> {
        while !buf.is_empty() {
            let n = poll_fn(|cx| Pin::new(&mut *r).poll_read(cx, buf)).await?;
            assert!(n > 0, "stream ended early");
            buf = &mut buf[n..];
        }
        Ok(())
    }

    // Server parity is checked with a separate Go client, never with our own verifier alone.
    #[test]
    fn server_accepts_go_clients_with_every_supported_identity() {
        let dir = std::env::temp_dir().join(format!("stulp-tls-server-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let peer = dir.join("peer");
        let source = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("testdata/goclient/main.go");
        assert!(
            Command::new("go")
                .args(["build", "-o"])
                .arg(&peer)
                .arg(source)
                .status()
                .unwrap()
                .success()
        );
        for algorithm in ["p256", "p384", "ed25519", "rsa"] {
            assert!(
                Command::new(&peer)
                    .arg("cert")
                    .arg(&dir)
                    .arg(algorithm)
                    .status()
                    .unwrap()
                    .success()
            );
            let cert = std::fs::read(dir.join("cert.der")).unwrap();
            let der = std::fs::read(dir.join("key.der")).unwrap();
            if let Ok(legacy) = std::fs::read(dir.join("legacy.der")) {
                KeyPair::new(vec![cert.clone()], &legacy).unwrap();
            }
            let wrong = p256::SecretKey::from_slice(&[1; 32])
                .unwrap()
                .to_sec1_der()
                .unwrap();
            assert!(
                KeyPair::new(vec![cert.clone()], &wrong).is_err(),
                "mismatched certificate accepted"
            );
            let identity = KeyPair::new(vec![cert], &der).unwrap();
            for mode in ["echo", "echo-p256", "wrong-name", "wrong-alpn", "tls12"] {
                let listener = TcpListener::bind("127.0.0.1:0").unwrap();
                let mut client = Command::new(&peer)
                    .arg(mode)
                    .arg(&dir)
                    .arg(listener.local_addr().unwrap().to_string())
                    .stdout(Stdio::piped())
                    .spawn()
                    .unwrap();
                let (stream, _) = listener.accept().unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                stream
                    .set_write_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let result = block_on(leantls::server::accept(
                    Tcp(stream),
                    &identity,
                    entropy(),
                    true,
                ));
                if mode.starts_with("echo") {
                    let mut conn = result.unwrap();
                    let mut data = vec![0; b"independent TLS record test".len() * 1700];
                    // Read across different Go and Rust TLS record boundaries.
                    block_on(read_exact(&mut conn, &mut data)).unwrap();
                    block_on(write_all(&mut conn, &data)).unwrap();
                    block_on(conn.close_notify()).unwrap();
                } else {
                    assert!(result.is_err(), "{algorithm} {mode} accepted");
                }
                let mut output = String::new();
                client
                    .stdout
                    .take()
                    .unwrap()
                    .read_to_string(&mut output)
                    .unwrap();
                assert!(
                    client.wait().unwrap().success(),
                    "{algorithm} {mode}: {output}"
                );
            }
        }
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
