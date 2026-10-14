//! TLS naar lokale apparaten met een eigen certificaat (UniFi, WiiM), vaak op
//! een kaal IP-adres.
//!
//! Alleen verzoeken en stromen die daar expliciet om vragen
//! (`device_certificate`) komen hier; cloudverkeer houdt de keten- en
//! naamcontrole van `leanhttps::WebDial`. De keten van het apparaat wordt niet
//! getoetst, de handtekening van de CertificateVerify en de records wel.
//!
//! `leanhttps::TlsDial` past hier niet: die weigert ketenverificatie op een
//! IP-adres (`ChainWithoutName`), en dat is bij een apparaat de gewone
//! toestand. Deze dialer staat één keer voor de host (boven
//! `leanhttp::host::TcpDial`) en HopOS (boven `applib::tcp::Dialer`).
#![no_std]
#![forbid(unsafe_code)]

use core::{
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};
use leanhttp::{AsyncRead, AsyncWrite, Close, IoError, Target};
use leanhttps::Error;
use leantls::{ChainVerifier, ConnError, Entropy, Roots, Trust, VerifyPeer};

/// Elke keten, maar wel de handtekening van de CertificateVerify.
struct DeviceCertificate<'a>(ChainVerifier<'a>);

impl VerifyPeer for DeviceCertificate<'_> {
    fn signature_algorithms(&self) -> &[u16] {
        self.0.signature_algorithms()
    }
    fn verify_chain(&self, _: leantls::CertChain<'_>, _: &str) -> leantls::Result {
        Ok(())
    }
    fn verify_signature(
        &self,
        leaf: &[u8],
        alg: u16,
        signed: &[u8],
        sig: &[u8],
    ) -> leantls::Result {
        self.0.verify_signature(leaf, alg, signed, sig)
    }
}

/// De kale verbinding in de vorm die leantls leest en schrijft.
struct Wire<C>(C);

impl<C: leanhttp::Conn + Unpin> leantls::AsyncRead for Wire<C> {
    type Error = IoError;
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<Result<usize, IoError>> {
        self.0.poll_read(cx, buf)
    }
}

impl<C: leanhttp::Conn + Unpin> leantls::AsyncWrite for Wire<C> {
    type Error = IoError;
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<Result<usize, IoError>> {
        self.0.poll_write(cx, buf)
    }
}

/// Een TLS-sessie met een apparaat, als leanhttp-verbinding. De termijnen van
/// leanhttp gaan naar de kale verbinding eronder.
pub struct Device<C>(leantls::Conn<Wire<C>>);

/// Een fout van de sessie in de vorm die leanhttp draagt.
fn io(e: ConnError<IoError>) -> IoError {
    match e {
        ConnError::Transport(e) => e,
        ConnError::Tls(_) => IoError::Other,
    }
}

impl<C: leanhttp::Conn + Unpin> AsyncRead for Device<C> {
    fn poll_read(&mut self, cx: &mut Context<'_>, buf: &mut [u8]) -> Poll<Result<usize, IoError>> {
        leantls::AsyncRead::poll_read(Pin::new(&mut self.0), cx, buf).map_err(io)
    }
    fn set_read_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        self.0.get_mut().0.set_read_timeout(d)
    }
}

impl<C: leanhttp::Conn + Unpin> AsyncWrite for Device<C> {
    fn poll_write(&mut self, cx: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize, IoError>> {
        leantls::AsyncWrite::poll_write(Pin::new(&mut self.0), cx, buf).map_err(io)
    }
    fn poll_flush(&mut self, _: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        // leantls meldt bytes pas als geschreven als hun record weg is.
        Poll::Ready(Ok(()))
    }
    fn set_write_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        self.0.get_mut().0.set_write_timeout(d)
    }
}

impl<C: leanhttp::Conn + Unpin> Close for Device<C> {
    fn poll_close(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        self.0.get_mut().0.poll_close(cx)
    }
    fn has_grown(&self) -> bool {
        self.0.get_ref().0.has_grown()
    }
}

/// Een [`leanhttp::Dial`] voor apparaten: `inner` opent de kale verbinding,
/// deze dialer doet de handshake. Alleen `https`; een kaal verzoek hoort
/// niet op dit pad.
pub struct DeviceDial<D, R> {
    inner: D,
    entropy: R,
    /// De termijn op het transport tijdens de handshake; daarna geen.
    pub handshake: Duration,
    last_error: Option<Error>,
}

impl<D, R> DeviceDial<D, R>
where
    D: leanhttp::Dial,
    D::Conn: Unpin,
    R: FnMut() -> Option<Entropy>,
{
    /// Een apparaatdialer over `inner`, met verse `entropy` per handshake en
    /// een handshaketermijn van 20 seconden.
    pub fn new(inner: D, entropy: R) -> Self {
        Self {
            inner,
            entropy,
            handshake: Duration::from_secs(20),
            last_error: None,
        }
    }

    /// Waarom de laatste dial in de TLS-laag mislukte, als dat zo was. Een
    /// fout van `inner` (DNS, TCP) staat in diens eigen `last_error`.
    pub fn last_error(&self) -> Option<Error> {
        self.last_error
    }

    /// Een weigering vóór er een verbinding opengaat.
    fn refuse(&mut self, why: Error) -> leanhttp::Error {
        self.last_error = Some(why);
        leanhttp::Error::Connect
    }
}

impl<D, R> leanhttp::Dial for DeviceDial<D, R>
where
    D: leanhttp::Dial,
    D::Conn: Unpin,
    R: FnMut() -> Option<Entropy>,
{
    type Conn = Device<D::Conn>;

    async fn dial(&mut self, target: Target<'_>) -> leanhttp::Result<Device<D::Conn>> {
        self.last_error = None;
        if !target.https {
            return Err(leanhttp::Error::NoHost);
        }
        let Ok(roots) = Roots::from_concatenated_der(leantls::MOZILLA_ROOTS) else {
            return Err(self.refuse(Error::Roots));
        };
        let Some(entropy) = (self.entropy)() else {
            return Err(self.refuse(Error::NoEntropy));
        };
        // Geen wandklok nodig: de keten (en dus haar geldigheid) wordt niet getoetst.
        let verifier = DeviceCertificate(ChainVerifier::new(roots, 0));
        let mut raw = self.inner.dial(target).await?;
        raw.set_read_timeout(Some(self.handshake))?;
        raw.set_write_timeout(Some(self.handshake))?;
        match leantls::connect(Wire(raw), &Trust::Chain(&verifier), target.host, entropy).await {
            Ok(mut tls) => {
                tls.get_mut().0.set_read_timeout(None)?;
                tls.get_mut().0.set_write_timeout(None)?;
                Ok(Device(tls))
            }
            Err(ConnError::Transport(e)) => {
                self.last_error = Some(Error::Transport(e));
                Err(leanhttp::Error::Io(e))
            }
            Err(ConnError::Tls(e)) => {
                self.last_error = Some(Error::Tls(e));
                Err(leanhttp::Error::Io(IoError::Other))
            }
        }
    }

    fn is_encrypted(&self) -> bool {
        true
    }
}
