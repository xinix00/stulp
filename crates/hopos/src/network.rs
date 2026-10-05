//! HTTP(S) for the controller and the plugins: Lean's `WebDial` over the slot's
//! TCP dialer, plus the device-certificate mode for local devices.
use applib::{App, EXEC, rand::Rng, tcp::Dialer, tcp::TcpConn};
use core::{
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};
use leanhttp::{AsyncRead as _, AsyncWrite as _, Dial as _, IoError};
use leanhttps::{Link, WebDial};
/// DNS en TCP samen, zoals vóór WebDial.
const CONNECT: Duration = Duration::from_secs(15);
/// De TLS-handshake; daarna gelden de termijnen van de aanroeper (leanhttp's
/// `header_timeout`, de SDK-termijn van het verzoek, die van een stroom).
const HANDSHAKE: Duration = Duration::from_secs(15);
fn dialer() -> Dialer {
    Dialer {
        exec: EXEC.get(),
        connect: CONNECT,
    }
}
/// Vers uit de DRBG van deze werker, die uit de slot-entropie (en
/// `STULP_ENTROPY_SEED`) gezaaid is.
fn entropy(random: &mut Rng) -> leantls::Entropy {
    let mut seed = [0; leantls::Entropy::LEN];
    random.fill(&mut seed);
    let entropy = leantls::Entropy::new(seed);
    seed.fill(0);
    entropy
}
pub(crate) struct Dial {
    app: &'static App,
    random: Rng,
    device: bool,
}
impl Dial {
    pub(crate) fn device(&mut self, device: bool) {
        self.device = device;
    }
    pub(crate) fn new(app: &'static App, seed: &[u8]) -> Self {
        Self {
            app,
            random: Rng::from_seed(seed, applib::clock::now_ns),
            device: false,
        }
    }
}
impl leanhttp::Dial for Dial {
    type Conn = Connection;
    fn is_encrypted(&self) -> bool {
        true
    }
    async fn dial(&mut self, target: leanhttp::Target<'_>) -> leanhttp::Result<Connection> {
        if !target.https {
            return Ok(Connection::Web(Link::Plain(dialer().dial(target).await?)));
        }
        let tls0 = applib::clock::now_ns();
        let (result, reason) = if self.device {
            let result = device(target, &mut self.random).await;
            (result.map(Connection::Device), None)
        } else {
            let app = self.app;
            let random = &mut self.random;
            let mut web = WebDial::new(
                dialer(),
                leantls::MOZILLA_ROOTS,
                || app.wall_ns().map(|ns| ns / 1_000_000_000),
                || Some(entropy(random)),
            );
            web.handshake = HANDSHAKE;
            let result = web.dial(target).await.map(Connection::Web);
            (result, web.last_error())
        };
        // De meetlat van de verbinding (DNS, TCP en handshake): op de LicheeRV
        // is de handshake het rekenwerk dat de bundel het langst in één adem
        // laat doen. Bij een fout staat erbij waarom.
        let ms = applib::clock::now_ns().saturating_sub(tls0) / 1_000_000;
        match (&result, reason) {
            (Ok(_), _) => self.app.log(format_args!(
                "STULP_TLS host={} ms={ms} result=ok",
                target.host
            )),
            (Err(_), Some(why)) => self.app.log(format_args!(
                "STULP_TLS host={} ms={ms} result=failed reason={why}",
                target.host
            )),
            (Err(e), None) => self.app.log(format_args!(
                "STULP_TLS host={} ms={ms} result=failed reason={e}",
                target.host
            )),
        }
        result
    }
}
/// Een verbinding naar buiten: `WebDial` (kaal of met een geverifieerde
/// keten), of een lokaal apparaat met een eigen certificaat.
// Een vast aantal verbindingen; een box zou per handshake een allocatie extra kosten.
#[allow(clippy::large_enum_variant)]
pub(crate) enum Connection {
    Web(Link<TcpConn>),
    Device(leantls::Conn<Wire>),
}
impl leanhttp::AsyncRead for Connection {
    fn poll_read(&mut self, cx: &mut Context<'_>, buf: &mut [u8]) -> Poll<Result<usize, IoError>> {
        match self {
            Self::Web(c) => c.poll_read(cx, buf),
            Self::Device(c) => leantls::AsyncRead::poll_read(Pin::new(c), cx, buf).map_err(error),
        }
    }
    fn set_read_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        match self {
            Self::Web(c) => c.set_read_timeout(d),
            Self::Device(c) => c.get_mut().0.set_read_timeout(d),
        }
    }
}
impl leanhttp::AsyncWrite for Connection {
    fn poll_write(&mut self, cx: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize, IoError>> {
        match self {
            Self::Web(c) => c.poll_write(cx, buf),
            Self::Device(c) => leantls::AsyncWrite::poll_write(Pin::new(c), cx, buf).map_err(error),
        }
    }
    fn poll_flush(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        match self {
            Self::Web(c) => c.poll_flush(cx),
            // leantls meldt bytes pas als geschreven als hun record weg is.
            Self::Device(_) => Poll::Ready(Ok(())),
        }
    }
    fn set_write_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        match self {
            Self::Web(c) => c.set_write_timeout(d),
            Self::Device(c) => c.get_mut().0.set_write_timeout(d),
        }
    }
}
impl leanhttp::Close for Connection {
    fn poll_close(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        match self {
            Self::Web(c) => c.poll_close(cx),
            Self::Device(c) => c.get_mut().0.poll_close(cx),
        }
    }
}
/// Lokale apparaten (UniFi, WiiM) dragen een eigen certificaat, vaak op een
/// IP-adres. Alleen verzoeken die daar expliciet om vragen komen hier; de
/// handtekening van de handshake en de records worden wel geverifieerd.
async fn device(
    target: leanhttp::Target<'_>,
    random: &mut Rng,
) -> leanhttp::Result<leantls::Conn<Wire>> {
    let mut conn = dialer().dial(target).await?;
    conn.set_read_timeout(Some(HANDSHAKE))?;
    conn.set_write_timeout(Some(HANDSHAKE))?;
    let roots = leantls::Roots::from_concatenated_der(leantls::MOZILLA_ROOTS)
        .map_err(|_| leanhttp::Error::Connect)?;
    // Geen wandklok nodig: de keten (en dus haar geldigheid) wordt niet getoetst.
    let verifier = DeviceCertificate(leantls::ChainVerifier::new(roots, 0));
    let mut tls = leantls::connect(
        Wire(conn),
        &leantls::Trust::Chain(&verifier),
        target.host,
        entropy(random),
    )
    .await
    .map_err(|e| leanhttp::Error::Io(error(e)))?;
    tls.get_mut().0.set_read_timeout(None)?;
    tls.get_mut().0.set_write_timeout(None)?;
    Ok(tls)
}
/// Elke keten, maar wel de handtekening van de CertificateVerify.
struct DeviceCertificate<'a>(leantls::ChainVerifier<'a>);
impl leantls::VerifyPeer for DeviceCertificate<'_> {
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
/// De slotverbinding onder de TLS van een apparaat.
pub(crate) struct Wire(TcpConn);
impl leantls::AsyncRead for Wire {
    type Error = IoError;
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<Result<usize, IoError>> {
        self.0.poll_read(cx, buf)
    }
}
impl leantls::AsyncWrite for Wire {
    type Error = IoError;
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<Result<usize, IoError>> {
        self.0.poll_write(cx, buf)
    }
}
fn error(e: leantls::ConnError<IoError>) -> IoError {
    match e {
        leantls::ConnError::Transport(e) => e,
        _ => IoError::Other,
    }
}
