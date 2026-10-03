//! Native TCP/Unix/TLS adapters. Eén eigenaar per verbinding; geen thread per handshake.
#![forbid(unsafe_code)]
use std::{
    future::Future,
    io::{self, Read, Write},
    path::Path,
    pin::Pin,
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};
use stulp_platform::socket::Socket;
mod key;
pub use key::KeyPair;
use leantls::{AsyncRead, AsyncWrite, Conn, ConnError, Entropy};
use zeroize::Zeroizing;

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn tls_error(error: ConnError<io::Error>) -> io::Error {
    match error {
        ConnError::Transport(e) => e,
        ConnError::Tls(e) => io::Error::new(io::ErrorKind::InvalidData, e.to_string()),
    }
}
fn read_file(path: &Path) -> io::Result<Zeroizing<Vec<u8>>> {
    let mut out = Zeroizing::new(Vec::new());
    std::fs::File::open(path)?
        .take(1_048_577)
        .read_to_end(&mut out)?;
    if out.len() > 1_048_576 {
        return Err(invalid("TLS input exceeds 1 MiB"));
    }
    Ok(out)
}
/// Begrensde PEM-lezer. Sleutelmaterialen worden gewist, ook bij parsefouten.
fn pem(bytes: &[u8], private: bool) -> io::Result<Vec<Zeroizing<Vec<u8>>>> {
    let text = std::str::from_utf8(bytes).map_err(|_| invalid("PEM is not UTF-8"))?;
    let mut result = Vec::new();
    let mut rest = text;
    while let Some((_, after)) = rest.split_once("-----BEGIN ") {
        let (label, after) = after
            .split_once("-----")
            .ok_or_else(|| invalid("invalid PEM header"))?;
        let (body, tail) = after
            .split_once("-----END ")
            .ok_or_else(|| invalid("missing PEM footer"))?;
        let (end, after) = tail
            .split_once("-----")
            .ok_or_else(|| invalid("invalid PEM footer"))?;
        if label != end {
            return Err(invalid("PEM labels do not match"));
        }
        rest = after;
        let selected = if private {
            matches!(label, "PRIVATE KEY" | "EC PRIVATE KEY" | "RSA PRIVATE KEY")
        } else {
            label == "CERTIFICATE"
        };
        if !selected {
            continue;
        }
        if result.len() == leantls::x509::MAX_ROOTS {
            return Err(invalid("too many PEM blocks"));
        }
        let mut encoded = Zeroizing::new(String::new());
        encoded.try_reserve(body.len()).map_err(io::Error::other)?;
        for c in body.chars().filter(|c| !c.is_ascii_whitespace()) {
            encoded.push(c);
        }
        let decoded =
            Zeroizing::new(stulp_protocol::token::decode(&encoded).map_err(io::Error::other)?);
        result.try_reserve(1).map_err(io::Error::other)?;
        result.push(decoded);
    }
    if result.is_empty() {
        return Err(invalid("PEM holds no supported certificate or private key"));
    }
    Ok(result)
}
/// Laadt alleen expliciet opgegeven TLS-bestanden; faalt vóór een listener wordt gepubliceerd.
pub fn load_identity(certificate: &Path, key: &Path) -> io::Result<KeyPair> {
    let blocks = pem(&read_file(certificate)?, false)?;
    let mut chain = Vec::new();
    chain
        .try_reserve_exact(blocks.len())
        .map_err(io::Error::other)?;
    for b in blocks {
        let mut cert = Vec::new();
        cert.try_reserve_exact(b.len()).map_err(io::Error::other)?;
        cert.extend_from_slice(&b);
        chain.push(cert);
    }
    let keys = pem(&read_file(key)?, true)?;
    if keys.len() != 1 {
        return Err(invalid("PEM must hold exactly one private key"));
    }
    KeyPair::new(
        chain,
        keys.first().ok_or_else(|| invalid("private key missing"))?,
    )
    .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e.to_string()))
}
struct Wire(Socket);
fn polled(result: io::Result<usize>) -> Poll<io::Result<usize>> {
    match result {
        Err(e)
            if matches!(
                e.kind(),
                io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
            ) =>
        {
            Poll::Pending
        }
        r => Poll::Ready(r),
    }
}
impl AsyncRead for Wire {
    type Error = io::Error;
    fn poll_read(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<io::Result<usize>> {
        polled(self.get_mut().0.read(buf))
    }
}
impl AsyncWrite for Wire {
    type Error = io::Error;
    fn poll_write(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        polled(self.get_mut().0.write(buf))
    }
}
type Handshake<'a> = Pin<Box<dyn Future<Output = io::Result<Conn<Wire>>> + 'a>>;
// TLS owns about 49 KiB on the heap. Keeping the small record state inline avoids another box.
#[allow(clippy::large_enum_variant)]
enum State<'a> {
    Plain(Socket),
    Handshake(Handshake<'a>),
    Tls(Conn<Wire>),
    Closed,
}
/// Nonblocking appkanaal of begrensde blocking HTTP-verbinding, inclusief TLS-handshake.
pub struct Stream<'a> {
    state: State<'a>,
    handshake_deadline: Instant,
    blocking: bool,
    local: bool,
    timeout: Duration,
    prefix: [u8; 32],
    start: usize,
    end: usize,
}
impl<'a> Stream<'a> {
    fn new(state: State<'a>, local: bool) -> Self {
        Self {
            state,
            handshake_deadline: Instant::now() + Duration::from_secs(10),
            blocking: false,
            local,
            timeout: Duration::from_secs(15),
            prefix: [0; 32],
            start: 0,
            end: 0,
        }
    }
    /// Een expliciet onversleuteld of lokaal kanaal.
    pub fn plain(socket: Socket) -> io::Result<Self> {
        socket.nonblocking()?;
        let local = matches!(socket, Socket::Unix(_));
        Ok(Self::new(State::Plain(socket), local))
    }
    /// Een serverhandshake wordt gepompt door dezelfde eigenaar als het appprotocol.
    pub fn server(socket: Socket, identity: &'a KeyPair, http: bool) -> io::Result<Self> {
        socket.nonblocking()?;
        let entropy = Entropy::new(hostnet::entropy()?);
        let future = async move {
            leantls::server::accept(Wire(socket), identity, entropy, http)
                .await
                .map_err(tls_error)
        };
        Ok(Self::new(State::Handshake(Box::pin(future)), false))
    }
    /// TLS-client voor remote attach. De HMAC-begroeting blijft aanvullend verplicht.
    /// Een eigen CA vervangt de standaard wortels; insecure slaat alleen de ketencontrole over.
    pub fn client(
        socket: Socket,
        name: String,
        ca: Option<&Path>,
        insecure: bool,
    ) -> io::Result<Self> {
        socket.nonblocking()?;
        let mut roots = Vec::new();
        if let Some(path) = ca {
            for der in pem(&read_file(path)?, false)? {
                roots.try_reserve(der.len()).map_err(io::Error::other)?;
                roots.extend_from_slice(&der);
            }
        } else {
            roots
                .try_reserve(hostnet::ROOTS_DER.len())
                .map_err(io::Error::other)?;
            roots.extend_from_slice(hostnet::ROOTS_DER);
        }
        // Validate before starting; an invalid custom CA must never silently use public roots.
        leantls::Roots::from_concatenated_der(&roots).map_err(io::Error::other)?;
        let entropy = Entropy::new(hostnet::entropy()?);
        let future = async move {
            let roots = leantls::Roots::from_concatenated_der(&roots).map_err(io::Error::other)?;
            let verifier = Verification {
                chain: leantls::ChainVerifier::new(roots, hostnet::unix_secs()),
                insecure,
            };
            leantls::connect(
                Wire(socket),
                &leantls::Trust::Chain(&verifier),
                &name,
                entropy,
            )
            .await
            .map_err(tls_error)
        };
        Ok(Self::new(State::Handshake(Box::pin(future)), false))
    }
    /// Of de verbinding over een door de caller gecontroleerde Unix-socket loopt.
    pub fn is_local(&self) -> bool {
        self.local
    }
    /// HTTP gebruikt een vaste workerpool, met een begrensde wachttijd per I/O.
    pub fn blocking(&mut self, timeout: Duration) {
        self.blocking = true;
        self.timeout = timeout;
    }
    /// Sluit ook een nog niet voltooide handshake onmiddellijk.
    pub fn shutdown(&mut self) {
        self.state = State::Closed;
    }
    fn ready(&mut self, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        if let State::Handshake(future) = &mut self.state {
            if Instant::now() >= self.handshake_deadline {
                self.shutdown();
                return Poll::Ready(Err(io::ErrorKind::TimedOut.into()));
            }
            match future.as_mut().poll(cx) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(Ok(conn)) => self.state = State::Tls(conn),
                Poll::Ready(Err(e)) => {
                    self.shutdown();
                    return Poll::Ready(Err(e));
                }
            }
        }
        if matches!(self.state, State::Closed) {
            return Poll::Ready(Err(io::ErrorKind::NotConnected.into()));
        }
        Poll::Ready(Ok(()))
    }
    fn read_poll(&mut self, cx: &mut Context<'_>, bytes: &mut [u8]) -> Poll<io::Result<usize>> {
        std::task::ready!(self.ready(cx))?;
        match &mut self.state {
            State::Plain(socket) => polled(socket.read(bytes)),
            State::Tls(tls) => Pin::new(tls)
                .poll_read(cx, bytes)
                .map(|r| r.map_err(tls_error)),
            _ => Poll::Ready(Err(io::ErrorKind::NotConnected.into())),
        }
    }
    fn pump<T>(
        &mut self,
        mut poll: impl FnMut(&mut Self, &mut Context<'_>) -> Poll<io::Result<T>>,
    ) -> io::Result<T> {
        let deadline = Instant::now() + self.timeout;
        let mut cx = Context::from_waker(Waker::noop());
        loop {
            match poll(self, &mut cx) {
                Poll::Ready(result) => return result,
                Poll::Pending if !self.blocking => return Err(io::ErrorKind::WouldBlock.into()),
                // Een verlopen termijn is herstelbaar, zoals leanhttp's
                // AsyncRead-contract zegt: de SSE-stroom peilt elke beurt met een
                // termijn van 1 ms of de lezer er nog is. Alleen de
                // handshake-deadline in `ready` sluit de verbinding zelf.
                Poll::Pending if Instant::now() >= deadline => {
                    return Err(io::ErrorKind::TimedOut.into());
                }
                Poll::Pending => std::thread::sleep(Duration::from_millis(1)),
            }
        }
    }
    /// Behoudt de eerste HTTP-bytes, zodat ook een HTTPS-restore zijn streamingroute gebruikt.
    pub fn peek(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        if self.end < self.prefix.len() {
            let mut more = [0; 32];
            let remaining = self.prefix.len() - self.end;
            let n = self.pump(|s, cx| s.read_poll(cx, &mut more[..remaining]))?;
            self.prefix[self.end..self.end + n].copy_from_slice(&more[..n]);
            self.end += n;
        }
        let n = bytes.len().min(self.end - self.start);
        bytes[..n].copy_from_slice(&self.prefix[self.start..self.start + n]);
        Ok(n)
    }
    /// Stuurt close_notify met een budget van 250 ms en sluit daarna altijd.
    pub fn close(&mut self) {
        self.blocking = true;
        self.timeout = Duration::from_millis(250);
        let _ = self.pump(|s, cx| match &mut s.state {
            State::Tls(tls) => tls.poll_close_notify(cx).map(|r| r.map_err(tls_error)),
            _ => Poll::Ready(Ok(())),
        });
        self.shutdown();
    }
}
impl Read for Stream<'_> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if self.start < self.end {
            let n = buf.len().min(self.end - self.start);
            buf[..n].copy_from_slice(&self.prefix[self.start..self.start + n]);
            self.start += n;
            return Ok(n);
        }
        self.pump(|s, cx| s.read_poll(cx, buf))
    }
}
impl Write for Stream<'_> {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.pump(|s, cx| {
            std::task::ready!(s.ready(cx))?;
            match &mut s.state {
                State::Plain(socket) => polled(socket.write(buf)),
                State::Tls(tls) => Pin::new(tls)
                    .poll_write(cx, buf)
                    .map(|r| r.map_err(tls_error)),
                _ => Poll::Ready(Err(io::ErrorKind::NotConnected.into())),
            }
        })
    }
    fn flush(&mut self) -> io::Result<()> {
        self.pump(|s, cx| {
            std::task::ready!(s.ready(cx))?;
            match &mut s.state {
                State::Plain(socket) => Poll::Ready(socket.flush()),
                State::Tls(tls) => std::pin::pin!(tls.flush())
                    .poll(cx)
                    .map(|r| r.map_err(tls_error)),
                _ => Poll::Ready(Err(io::ErrorKind::NotConnected.into())),
            }
        })
    }
}
/// HTTP adapter met absolute fasetermijnen en een stiltetermijn van vijftien seconden.
pub struct Http<'a> {
    stream: Stream<'a>,
    read: Option<Instant>,
    write: Option<Instant>,
}
impl<'a> Http<'a> {
    /// De worker bezit de verbinding tot het laatste antwoord of een deadline.
    pub fn new(mut stream: Stream<'a>) -> Self {
        stream.blocking(Duration::from_secs(15));
        Self {
            stream,
            read: None,
            write: None,
        }
    }
}
fn remaining(deadline: Option<Instant>) -> Result<Duration, leanhttp::IoError> {
    let d = deadline.map_or(Duration::from_secs(15), |at| {
        at.saturating_duration_since(Instant::now())
    });
    if d.is_zero() {
        Err(leanhttp::IoError::TimedOut)
    } else {
        Ok(d)
    }
}
fn http_error(e: io::Error) -> leanhttp::IoError {
    match e.kind() {
        io::ErrorKind::TimedOut => leanhttp::IoError::TimedOut,
        io::ErrorKind::ConnectionReset => leanhttp::IoError::Reset,
        io::ErrorKind::NotConnected | io::ErrorKind::BrokenPipe => leanhttp::IoError::Closed,
        _ => leanhttp::IoError::Other,
    }
}
impl leanhttp::AsyncRead for Http<'_> {
    fn poll_read(
        &mut self,
        _: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<Result<usize, leanhttp::IoError>> {
        self.stream.timeout = remaining(self.read)?;
        Poll::Ready(self.stream.read(buf).map_err(http_error))
    }
    fn set_read_timeout(&mut self, timeout: Option<Duration>) -> Result<(), leanhttp::IoError> {
        self.read = timeout.map(|d| Instant::now() + d);
        Ok(())
    }
}
impl leanhttp::AsyncWrite for Http<'_> {
    fn poll_write(
        &mut self,
        _: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<Result<usize, leanhttp::IoError>> {
        self.stream.timeout = remaining(self.write)?;
        Poll::Ready(self.stream.write(buf).map_err(http_error))
    }
    fn poll_flush(&mut self, _: &mut Context<'_>) -> Poll<Result<(), leanhttp::IoError>> {
        self.stream.timeout = remaining(self.write)?;
        Poll::Ready(self.stream.flush().map_err(http_error))
    }
    fn set_write_timeout(&mut self, timeout: Option<Duration>) -> Result<(), leanhttp::IoError> {
        self.write = timeout.map(|d| Instant::now() + d);
        Ok(())
    }
}
impl leanhttp::Close for Http<'_> {
    fn poll_close(&mut self, _: &mut Context<'_>) -> Poll<Result<(), leanhttp::IoError>> {
        self.stream.close();
        Poll::Ready(Ok(()))
    }
}

struct Verification<'a> {
    chain: leantls::ChainVerifier<'a>,
    insecure: bool,
}
impl leantls::VerifyPeer for Verification<'_> {
    fn signature_algorithms(&self) -> &[u16] {
        self.chain.signature_algorithms()
    }
    fn verify_chain(&self, chain: leantls::CertChain<'_>, name: &str) -> leantls::Result {
        if self.insecure {
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
    ) -> leantls::Result {
        self.chain.verify_signature(leaf, alg, signed, sig)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn stalled_handshake_expires_and_drops_the_socket() -> io::Result<()> {
        use std::net::{TcpListener, TcpStream};
        let listener = TcpListener::bind("127.0.0.1:0")?;
        let client = TcpStream::connect(listener.local_addr()?)?;
        let (mut peer, _) = listener.accept()?;
        peer.set_read_timeout(Some(Duration::from_secs(1)))?;
        let mut stream = Stream::client(Socket::Tcp(client), "stulp.test".into(), None, false)?;
        stream.handshake_deadline = Instant::now();
        assert_eq!(
            stream.read(&mut [0]).err().map(|e| e.kind()),
            Some(io::ErrorKind::TimedOut)
        );
        assert_eq!(peer.read(&mut [0])?, 0);
        Ok(())
    }
    #[test]
    fn peek_preserves_the_complete_request_and_partial_prefixes() -> io::Result<()> {
        let (socket, mut peer) = std::os::unix::net::UnixStream::pair()?;
        let mut stream = Stream::plain(Socket::Unix(socket))?;
        peer.write_all(b"POST /api/")?;
        let mut prefix = [0; 32];
        assert_eq!(stream.peek(&mut prefix)?, 10);
        peer.write_all(b"stulp/restore HTTP/1.1\r\n")?;
        assert_eq!(stream.peek(&mut prefix)?, 32);
        peer.shutdown(std::net::Shutdown::Write)?;
        let mut data = Vec::new();
        stream.read_to_end(&mut data)?;
        assert_eq!(data, b"POST /api/stulp/restore HTTP/1.1\r\n");
        Ok(())
    }
}
