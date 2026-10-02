//! Lokale apparaten dragen een eigen certificaat. Alleen expliciete verzoeken
//! gebruiken dit pad; cloud-HTTP behoudt de gewone keten- en naamcontrole.
//! De TLS-handtekening en recordbeveiliging worden ook hier geverifieerd.
use leanhttp::{AsyncRead, AsyncWrite, Close, IoError};
use leantls::VerifyPeer;
use std::{
    io::{self, Read, Write},
    net::{TcpStream, ToSocketAddrs},
    pin::Pin,
    task::{Context, Poll},
    time::{Duration, Instant},
};
use stulp_core::json;
use stulp_sdk::{Error, HttpRequest, HttpResponse, Result};
pub(super) struct DeviceCertificate(pub(super) leantls::ChainVerifier<'static>);
impl VerifyPeer for DeviceCertificate {
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
/// De draad onder TLS: een blokkerende socket die voor elke lees of schrijf de
/// resterende tijd tot de absolute termijn als kerneltermijn zet. hostnet's
/// `StdConn` spreekt leanhttp 3.1.1 en past daarom niet onder leantls 3.1.5.
struct Wire {
    socket: TcpStream,
    deadline: Instant,
}
/// Een socket-termijn korter dan dit rondt de kernel af naar "geen termijn".
const MIN_TIMEOUT: Duration = Duration::from_millis(1);
impl Wire {
    /// De tijd tot de termijn, of `TimedOut` als die voorbij is.
    fn left(&self) -> core::result::Result<Duration, IoError> {
        let left = self.deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            Err(IoError::TimedOut)
        } else {
            Ok(left.max(MIN_TIMEOUT))
        }
    }
}
/// Een std-fout als verbindingsfout, zoals hostnet die ook indeelt.
fn io_error(e: &io::Error) -> IoError {
    match e.kind() {
        io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut => IoError::TimedOut,
        io::ErrorKind::ConnectionReset | io::ErrorKind::ConnectionAborted => IoError::Reset,
        io::ErrorKind::BrokenPipe | io::ErrorKind::NotConnected => IoError::Closed,
        _ => IoError::Other,
    }
}
impl leantls::AsyncRead for Wire {
    type Error = IoError;
    fn poll_read(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<core::result::Result<usize, IoError>> {
        let wire = self.get_mut();
        let left = match wire.left() {
            Ok(left) => left,
            Err(e) => return Poll::Ready(Err(e)),
        };
        if wire.socket.set_read_timeout(Some(left)).is_err() {
            return Poll::Ready(Err(IoError::Other));
        }
        loop {
            match wire.socket.read(buf) {
                Ok(n) => return Poll::Ready(Ok(n)),
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Poll::Ready(Err(io_error(&e))),
            }
        }
    }
}
impl leantls::AsyncWrite for Wire {
    type Error = IoError;
    fn poll_write(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<core::result::Result<usize, IoError>> {
        let wire = self.get_mut();
        let left = match wire.left() {
            Ok(left) => left,
            Err(e) => return Poll::Ready(Err(e)),
        };
        if wire.socket.set_write_timeout(Some(left)).is_err() {
            return Poll::Ready(Err(IoError::Other));
        }
        loop {
            match wire.socket.write(buf) {
                Ok(n) => return Poll::Ready(Ok(n)),
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Poll::Ready(Err(io_error(&e))),
            }
        }
    }
}
struct Connection {
    tls: Option<leantls::Conn<Wire>>,
}
impl AsyncRead for Connection {
    fn poll_read(
        &mut self,
        cx: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<core::result::Result<usize, IoError>> {
        let Some(tls) = self.tls.as_mut() else {
            return Poll::Ready(Err(IoError::Closed));
        };
        leantls::AsyncRead::poll_read(Pin::new(tls), cx, buf).map(|r| r.map_err(|_| IoError::Other))
    }
    // De onderliggende socket draagt de absolute termijn voor de gehele aanroep.
    fn set_read_timeout(&mut self, _: Option<Duration>) -> core::result::Result<(), IoError> {
        Ok(())
    }
}
impl AsyncWrite for Connection {
    fn poll_write(
        &mut self,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<core::result::Result<usize, IoError>> {
        let Some(tls) = self.tls.as_mut() else {
            return Poll::Ready(Err(IoError::Closed));
        };
        leantls::AsyncWrite::poll_write(Pin::new(tls), cx, buf)
            .map(|r| r.map_err(|_| IoError::Other))
    }
    fn poll_flush(&mut self, _: &mut Context<'_>) -> Poll<core::result::Result<(), IoError>> {
        Poll::Ready(Ok(()))
    }
    fn set_write_timeout(&mut self, _: Option<Duration>) -> core::result::Result<(), IoError> {
        Ok(())
    }
}
impl Close for Connection {
    fn poll_close(&mut self, _: &mut Context<'_>) -> Poll<core::result::Result<(), IoError>> {
        self.tls.take();
        Poll::Ready(Ok(()))
    }
}
struct Dial {
    deadline: Instant,
}
impl leanhttp::Dial for Dial {
    type Conn = Connection;
    fn is_encrypted(&self) -> bool {
        true
    }
    async fn dial(&mut self, t: leanhttp::Target<'_>) -> leanhttp::Result<Connection> {
        if !t.https {
            return Err(leanhttp::Error::NoHost);
        }
        let addresses = (t.host, t.port)
            .to_socket_addrs()
            .map_err(|_| leanhttp::Error::Io(IoError::Other))?;
        let mut socket = None;
        for address in addresses.take(16) {
            let left = self.deadline.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Err(leanhttp::Error::Io(IoError::TimedOut));
            }
            if let Ok(s) = TcpStream::connect_timeout(&address, left) {
                socket = Some(s);
                break;
            }
        }
        let socket = socket.ok_or(leanhttp::Error::Io(IoError::Other))?;
        let roots = leantls::Roots::from_concatenated_der(hostnet::ROOTS_DER)
            .map_err(|_| leanhttp::Error::Io(IoError::Other))?;
        let trust = DeviceCertificate(leantls::ChainVerifier::new(roots, hostnet::unix_secs()));
        let seed = hostnet::entropy().map_err(|_| leanhttp::Error::Io(IoError::Other))?;
        let wire = Wire {
            socket,
            deadline: self.deadline,
        };
        let tls = leantls::connect(
            wire,
            &leantls::Trust::Chain(&trust),
            t.host,
            leantls::Entropy::new(seed),
        )
        .await
        .map_err(|_| leanhttp::Error::Io(IoError::Other))?;
        Ok(Connection { tls: Some(tls) })
    }
}
pub(super) fn execute(req: HttpRequest) -> Result<HttpResponse> {
    hostnet::block_on(async {
        let mut header = leanhttp::Header::new();
        for (k, v) in &req.headers {
            header
                .set(k, v)
                .map_err(|_| Error::Invalid("invalid device HTTP header"))?;
        }
        let call = leanhttp::Call {
            method: &req.method,
            url: &req.url,
            header,
            body: Some(&req.body),
            no_follow: true,
            ..Default::default()
        };
        let mut dial = Dial {
            deadline: Instant::now() + Duration::from_millis(req.timeout_ms),
        };
        let mut reply = leanhttp::fetch(&mut dial, call)
            .await
            .map_err(|_| Error::Transport("device TLS 1.3 connection or HTTP failed"))?;
        let mut headers = Vec::new();
        for (k, v) in reply.header.iter() {
            json::push(&mut headers, (json::copy(k)?, json::copy(v)?), 256)?;
        }
        for cookie in &reply.set_cookie {
            json::push(
                &mut headers,
                (json::copy("Set-Cookie")?, json::copy(cookie)?),
                256,
            )?;
        }
        let body = reply
            .read_to_end(req.limit)
            .await
            .map_err(|_| Error::Transport("device HTTP body failed"))?;
        Ok(HttpResponse {
            status: reply.status,
            headers,
            body,
        })
    })
}
