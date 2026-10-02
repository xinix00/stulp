//! HTTP(S) connection adapter with the existing TLS verifier and SDK deadlines.
use applib::{App, EXEC, appnet, rand::Rng, tcp::TcpConn};
use core::{
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};
use leanhttp::{AsyncRead as _, AsyncWrite as _, IoError};
use stulp_tls::{AsyncRead as TlsRead, AsyncWrite as TlsWrite};
struct Wire(TcpConn);
impl TlsRead for Wire {
    type Error = IoError;
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<Result<usize, IoError>> {
        self.0.poll_read(cx, buf)
    }
}
impl TlsWrite for Wire {
    type Error = IoError;
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<Result<usize, IoError>> {
        self.0.poll_write(cx, buf)
    }
}
// A fixed number of connections avoids one extra allocation per TLS handshake.
#[allow(clippy::large_enum_variant)]
enum Transport {
    Plain(TcpConn),
    Tls(stulp_tls::Conn<Wire>),
}
pub(crate) struct Connection(Option<Transport>);
fn error(e: stulp_tls::ConnError<IoError>) -> IoError {
    match e {
        stulp_tls::ConnError::Transport(e) => e,
        _ => IoError::Other,
    }
}
impl leanhttp::AsyncRead for Connection {
    fn poll_read(&mut self, cx: &mut Context<'_>, buf: &mut [u8]) -> Poll<Result<usize, IoError>> {
        match &mut self.0 {
            Some(Transport::Plain(c)) => c.poll_read(cx, buf),
            Some(Transport::Tls(c)) => Pin::new(c).poll_read(cx, buf).map_err(error),
            None => Poll::Ready(Err(IoError::Closed)),
        }
    }
    fn set_read_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        match &mut self.0 {
            Some(Transport::Plain(c)) => c.set_read_timeout(d),
            Some(Transport::Tls(c)) => c.get_mut().0.set_read_timeout(d),
            None => Err(IoError::Closed),
        }
    }
}
impl leanhttp::AsyncWrite for Connection {
    fn poll_write(&mut self, cx: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize, IoError>> {
        match &mut self.0 {
            Some(Transport::Plain(c)) => c.poll_write(cx, buf),
            Some(Transport::Tls(c)) => Pin::new(c).poll_write(cx, buf).map_err(error),
            None => Poll::Ready(Err(IoError::Closed)),
        }
    }
    fn poll_flush(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        match &mut self.0 {
            Some(Transport::Plain(c)) => c.poll_flush(cx),
            Some(Transport::Tls(c)) => Pin::new(c).poll_flush(cx).map_err(error),
            None => Poll::Ready(Err(IoError::Closed)),
        }
    }
    fn set_write_timeout(&mut self, d: Option<Duration>) -> Result<(), IoError> {
        match &mut self.0 {
            Some(Transport::Plain(c)) => c.set_write_timeout(d),
            Some(Transport::Tls(c)) => c.get_mut().0.set_write_timeout(d),
            None => Err(IoError::Closed),
        }
    }
}
impl leanhttp::Close for Connection {
    fn poll_close(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), IoError>> {
        let result = match &mut self.0 {
            Some(Transport::Plain(c)) => c.poll_close(cx),
            Some(Transport::Tls(c)) => c.get_mut().0.poll_close(cx),
            None => Poll::Ready(Ok(())),
        };
        if result.is_ready() {
            self.0 = None;
        }
        result
    }
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
        let net = appnet::net().ok_or(leanhttp::Error::Connect)?;
        let ip = net
            .resolve(target.host)
            .await
            .map_err(|_| leanhttp::Error::Connect)?;
        let stream = appnet::TcpStream::connect_timeout(ip, target.port, Duration::from_secs(15))
            .await
            .map_err(|_| leanhttp::Error::Connect)?;
        let mut conn = TcpConn::new(stream, EXEC.get());
        conn.set_read_timeout(Some(Duration::from_secs(15)))
            .map_err(leanhttp::Error::Io)?;
        conn.set_write_timeout(Some(Duration::from_secs(15)))
            .map_err(leanhttp::Error::Io)?;
        if !target.https {
            return Ok(Connection(Some(Transport::Plain(conn))));
        }
        let roots = stulp_tls::Roots::from_concatenated_der(include_bytes!(
            "../../tls/testdata/github/mozilla-roots.der"
        ))
        .map_err(|_| leanhttp::Error::Connect)?;
        let now = self.app.wall_ns().ok_or(leanhttp::Error::Connect)? / 1_000_000_000;
        let verifier = Verification {
            chain: stulp_tls::ChainVerifier::new(roots, now),
            device: self.device,
        };
        let mut seed = [0; 96];
        self.random.fill(&mut seed);
        let tls0 = applib::clock::now_ns();
        let result = stulp_tls::connect(
            Wire(conn),
            &stulp_tls::Trust::Chain(&verifier),
            target.host,
            stulp_tls::Entropy::new(seed),
        )
        .await;
        seed.fill(0);
        // De meetlat van de handshake: op de LicheeRV is dit het rekenwerk
        // dat de bundel het langst in één adem laat doen.
        self.app.log(format_args!(
            "STULP_TLS host={} ms={} result={}",
            target.host,
            applib::clock::now_ns().saturating_sub(tls0) / 1_000_000,
            if result.is_ok() { "ok" } else { "failed" }
        ));
        let conn = result.map_err(|_| leanhttp::Error::Connect)?;
        Ok(Connection(Some(Transport::Tls(conn))))
    }
}

struct Verification<'a> {
    chain: stulp_tls::ChainVerifier<'a>,
    device: bool,
}
impl stulp_tls::VerifyPeer for Verification<'_> {
    fn signature_algorithms(&self) -> &[u16] {
        self.chain.signature_algorithms()
    }
    fn verify_chain(&self, chain: stulp_tls::CertChain<'_>, name: &str) -> stulp_tls::Result {
        if self.device {
            Ok(())
        } else {
            self.chain.verify_chain(chain, name)
        }
    }
    fn verify_signature(
        &self,
        leaf: &[u8],
        alg: u16,
        signed: &[u8],
        sig: &[u8],
    ) -> stulp_tls::Result {
        self.chain.verify_signature(leaf, alg, signed, sig)
    }
}
