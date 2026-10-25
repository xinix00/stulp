//! HTTP(S) for the controller and the plugins: Lean's `WebDial` over the slot's
//! TCP dialer, plus the device-certificate mode for local devices
//! (`stulp_device_tls`, the same dialer as on the host).
use applib::{App, EXEC, rand::Rng, tcp::Dialer, tcp::TcpConn};
use core::{
    task::{Context, Poll},
    time::Duration,
};
use leanhttp::IoError;
use leanhttps::{Link, WebDial};
use stulp_device_tls::{Device, DeviceDial};
/// DNS en TCP samen, zoals vóór WebDial.
const CONNECT: Duration = Duration::from_secs(15);
/// De TLS-handshake; daarna gelden de termijnen van de aanroeper (leanhttp's
/// `header_timeout`, de SDK-termijn van het verzoek, die van een stroom).
const HANDSHAKE: Duration = Duration::from_secs(15);
fn dialer() -> Dialer {
    Dialer::new(EXEC.get(), CONNECT)
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
    /// APNs: één HTTP/2-verzoek met een eigen TLS-sessie (`stulp_h2`) over
    /// de TCP van de slot, op de wandklok en uit de DRBG van deze werker.
    pub(crate) async fn h2(
        &mut self,
        request: &stulp_sdk::HttpRequest,
    ) -> stulp_sdk::Result<stulp_sdk::HttpResponse> {
        let mut seed = [0; stulp_h2::ENTROPY];
        self.random.fill(&mut seed);
        let now = self.app.wall_ns().map(|ns| ns / 1_000_000_000);
        stulp_h2::send(&mut dialer(), now, &mut seed, request).await
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
        let mut tcp = dialer();
        let random = &mut self.random;
        let (result, tls) = if self.device {
            let mut device = DeviceDial::new(&mut tcp, || Some(entropy(random)));
            device.handshake = HANDSHAKE;
            let result = device.dial(target).await.map(Connection::Device);
            (result, device.last_error())
        } else {
            let app = self.app;
            let mut web = WebDial::new(
                &mut tcp,
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
        // laat doen. Bij een fout staat erbij waarom: de TLS-laag, anders de
        // DNS of TCP eronder, anders leanhttp.
        let ms = applib::clock::now_ns().saturating_sub(tls0) / 1_000_000;
        let host = target.host;
        match (&result, tls, tcp.last_error()) {
            (Ok(_), _, _) => self
                .app
                .log(format_args!("STULP_TLS host={host} ms={ms} result=ok")),
            (Err(_), Some(why), _) => self.app.log(format_args!(
                "STULP_TLS host={host} ms={ms} result=failed reason={why}"
            )),
            (Err(_), None, Some(why)) => self.app.log(format_args!(
                "STULP_TLS host={host} ms={ms} result=failed reason={why}"
            )),
            (Err(e), None, None) => self.app.log(format_args!(
                "STULP_TLS host={host} ms={ms} result=failed reason={e}"
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
    Device(Device<TcpConn>),
}
impl leanhttp::AsyncRead for Connection {
    fn poll_read(&mut self, cx: &mut Context<'_>, buf: &mut [u8]) -> Poll<Result<usize, IoError>> {
        match self {
            Self::Web(c) => c.poll_read(cx, buf),
            Self::Device(c) => c.poll_read(cx, buf),
        }
    }
    fn set_read_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        match self {
            Self::Web(c) => c.set_read_timeout(d),
            Self::Device(c) => c.set_read_timeout(d),
        }
    }
}
impl leanhttp::AsyncWrite for Connection {
    fn poll_write(&mut self, cx: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize, IoError>> {
        match self {
            Self::Web(c) => c.poll_write(cx, buf),
            Self::Device(c) => c.poll_write(cx, buf),
        }
    }
    fn poll_flush(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        match self {
            Self::Web(c) => c.poll_flush(cx),
            Self::Device(c) => c.poll_flush(cx),
        }
    }
    fn set_write_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        match self {
            Self::Web(c) => c.set_write_timeout(d),
            Self::Device(c) => c.set_write_timeout(d),
        }
    }
}
impl leanhttp::Close for Connection {
    fn poll_close(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        match self {
            Self::Web(c) => c.poll_close(cx),
            Self::Device(c) => c.poll_close(cx),
        }
    }
}
