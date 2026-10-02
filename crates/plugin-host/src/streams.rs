//! Eén vaste worker bezit maximaal acht sockets; trage lezers begrenzen zichzelf
//! via TCP-backpressure. Er bestaat geen thread per camera of websocket.
use std::{
    collections::VecDeque,
    future::Future,
    io::{self, Read, Write},
    net::{TcpStream, ToSocketAddrs},
    pin::{Pin, pin},
    sync::mpsc::{self, Receiver, SyncSender, TrySendError},
    task::{Context, Poll, Waker},
    thread::JoinHandle,
    time::{Duration, Instant},
};
use stulp_core::json;
use stulp_sdk::{Error, Result, StreamCommand as Command, StreamEvent as Event};
const SLOTS: usize = 8;
const QUEUED: usize = 1 << 20;
pub(super) struct Worker {
    input: Option<SyncSender<Command>>,
    output: Option<Receiver<Event>>,
    thread: Option<JoinHandle<()>>,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        let (input, requests) = mpsc::sync_channel(32);
        let (answers, output) = mpsc::sync_channel(64);
        let thread = std::thread::Builder::new()
            .name("stulp-plugin-streams".into())
            .spawn(move || run(requests, answers))
            .map_err(|_| Error::Transport("cannot start stream worker"))?;
        Ok(Self {
            input: Some(input),
            output: Some(output),
            thread: Some(thread),
        })
    }
    pub(super) fn send(&mut self, command: Command) -> Result {
        match &command {
            Command::Open { id, host, port, .. }
                if *id == 0
                    || *port == 0
                    || host.is_empty()
                    || host.len() > 253
                    || host
                        .bytes()
                        .any(|b| b <= 32 || b >= 127 || b"/@\\?#".contains(&b)) =>
            {
                return Err(Error::Invalid("invalid stream target"));
            }
            Command::Write { bytes, .. } if bytes.is_empty() || bytes.len() > 65536 => {
                return Err(Error::Invalid("stream write exceeds 64 KiB"));
            }
            _ => (),
        }
        self.input
            .as_ref()
            .ok_or(Error::Transport("stream worker stopped"))?
            .try_send(command)
            .map_err(|_| Error::Transport("stream command queue full or stopped"))
    }
    pub(super) fn poll(&mut self) -> Option<Event> {
        self.output.as_ref()?.try_recv().ok()
    }
}
impl Drop for Worker {
    fn drop(&mut self) {
        self.input.take();
        // Een volle antwoordrij mag het stoppen niet tegenhouden.
        self.output.take();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}
struct Wire(TcpStream);
fn polled<T>(r: io::Result<T>) -> Poll<io::Result<T>> {
    match r {
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
impl leantls::AsyncRead for Wire {
    type Error = io::Error;
    fn poll_read(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
        b: &mut [u8],
    ) -> Poll<io::Result<usize>> {
        polled(self.get_mut().0.read(b))
    }
}
impl leantls::AsyncWrite for Wire {
    type Error = io::Error;
    fn poll_write(self: Pin<&mut Self>, _: &mut Context<'_>, b: &[u8]) -> Poll<io::Result<usize>> {
        polled(self.get_mut().0.write(b))
    }
}
// Acht vaste slots kosten samen hoogstens 8 KiB; een extra heapallocatie per TLS-socket helpt hier niet.
#[expect(clippy::large_enum_variant)]
enum Socket {
    Plain(Wire),
    Tls(leantls::Conn<Wire>),
}
impl Socket {
    fn read(&mut self, cx: &mut Context<'_>, b: &mut [u8]) -> Poll<Result<usize>> {
        match self {
            Self::Plain(w) => leantls::AsyncRead::poll_read(Pin::new(w), cx, b)
                .map(|r| r.map_err(|_| Error::Transport("stream read failed"))),
            Self::Tls(w) => leantls::AsyncRead::poll_read(Pin::new(w), cx, b)
                .map(|r| r.map_err(|_| Error::Transport("TLS stream read failed"))),
        }
    }
    fn write(&mut self, cx: &mut Context<'_>, b: &[u8]) -> Poll<Result<usize>> {
        match self {
            Self::Plain(w) => leantls::AsyncWrite::poll_write(Pin::new(w), cx, b)
                .map(|r| r.map_err(|_| Error::Transport("stream write failed"))),
            Self::Tls(w) => leantls::AsyncWrite::poll_write(Pin::new(w), cx, b)
                .map(|r| r.map_err(|_| Error::Transport("TLS stream write failed"))),
        }
    }
}
fn connect(host: &str, port: u16, tls: bool, device: bool) -> Result<Socket> {
    let deadline = Instant::now() + Duration::from_secs(15);
    let addresses = (host, port)
        .to_socket_addrs()
        .map_err(|_| Error::Transport("stream DNS failed"))?;
    let mut socket = None;
    for address in addresses.take(16) {
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Err(Error::Timeout);
        }
        if let Ok(s) = TcpStream::connect_timeout(&address, left) {
            socket = Some(s);
            break;
        }
    }
    let socket = socket.ok_or(Error::Transport("stream connect failed"))?;
    socket
        .set_nonblocking(true)
        .map_err(|_| Error::Transport("stream nonblocking failed"))?;
    socket
        .set_nodelay(true)
        .map_err(|_| Error::Transport("stream nodelay failed"))?;
    let wire = Wire(socket);
    if !tls {
        return Ok(Socket::Plain(wire));
    }
    let roots = leantls::Roots::from_concatenated_der(hostnet::ROOTS_DER)
        .map_err(|_| Error::Transport("TLS roots unavailable"))?;
    let chain = leantls::ChainVerifier::new(roots, hostnet::unix_secs());
    let local = super::device_tls::DeviceCertificate(chain);
    let verifier: &dyn leantls::VerifyPeer = if device { &local } else { &local.0 };
    let trust = leantls::Trust::Chain(verifier);
    let entropy = hostnet::entropy().map_err(|_| Error::Transport("TLS entropy unavailable"))?;
    let mut future = pin!(leantls::connect(
        wire,
        &trust,
        host,
        leantls::Entropy::new(entropy)
    ));
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if Instant::now() >= deadline {
            return Err(Error::Timeout);
        }
        if let Poll::Ready(r) = future.as_mut().poll(&mut cx) {
            return r
                .map(Socket::Tls)
                .map_err(|_| Error::Transport("TLS 1.3 stream handshake failed"));
        }
        std::thread::sleep(Duration::from_millis(2));
    }
}
struct Pending {
    bytes: Vec<u8>,
    offset: usize,
    deadline: Instant,
}
struct Connection {
    id: u64,
    socket: Socket,
    writes: VecDeque<Pending>,
    bytes: usize,
}
impl Connection {
    fn write(&mut self, bytes: Vec<u8>) -> Result {
        if self.writes.len() >= 64 || bytes.len() > QUEUED.saturating_sub(self.bytes) {
            return Err(Error::Transport("stream write queue exhausted"));
        }
        self.writes
            .try_reserve(1)
            .map_err(|_| Error::Transport("stream allocation failed"))?;
        self.bytes += bytes.len();
        self.writes.push_back(Pending {
            bytes,
            offset: 0,
            deadline: Instant::now() + Duration::from_secs(10),
        });
        Ok(())
    }
    fn step(&mut self, cx: &mut Context<'_>, buf: &mut [u8]) -> Result<Option<Event>> {
        if let Some(w) = self.writes.front_mut() {
            if Instant::now() >= w.deadline {
                return Err(Error::Timeout);
            }
            match self.socket.write(
                cx,
                w.bytes
                    .get(w.offset..)
                    .ok_or(Error::Invalid("stream write offset"))?,
            ) {
                Poll::Ready(Ok(0)) => return Err(Error::Transport("stream write closed")),
                Poll::Ready(Ok(n)) => {
                    w.offset += n;
                    if w.offset == w.bytes.len() {
                        self.bytes -= w.bytes.len();
                        self.writes.pop_front();
                    }
                }
                Poll::Ready(Err(e)) => return Err(e),
                Poll::Pending => (),
            }
        }
        match self.socket.read(cx, buf) {
            Poll::Ready(Ok(0)) => Err(Error::Transport("stream peer closed")),
            Poll::Ready(Ok(n)) => {
                let mut bytes = Vec::new();
                bytes
                    .try_reserve_exact(n)
                    .map_err(|_| Error::Transport("stream allocation failed"))?;
                bytes.extend_from_slice(buf.get(..n).ok_or(Error::Invalid("stream read length"))?);
                Ok(Some(Event::Data(self.id, bytes)))
            }
            Poll::Ready(Err(e)) => Err(e),
            Poll::Pending => Ok(None),
        }
    }
}
fn command(cmd: Command, conns: &mut Vec<Connection>, last: &mut u64) -> Option<Event> {
    match cmd {
        Command::Open {
            id,
            host,
            port,
            tls,
            device_certificate,
        } => {
            if id <= *last || conns.len() >= SLOTS {
                return Some(Event::Closed(
                    id,
                    Error::Invalid("stream id reused or slots full"),
                ));
            }
            *last = id;
            let result = connect(&host, port, tls, device_certificate).and_then(|socket| {
                json::push(
                    conns,
                    Connection {
                        id,
                        socket,
                        writes: VecDeque::new(),
                        bytes: 0,
                    },
                    SLOTS,
                )?;
                Ok(())
            });
            Some(match result {
                Ok(()) => Event::Opened(id),
                Err(e) => Event::Closed(id, e),
            })
        }
        Command::Close { id } => {
            conns.retain(|c| c.id != id);
            Some(Event::Closed(id, Error::Transport("stream locally closed")))
        }
        Command::Write { id, bytes } => {
            let result = conns
                .iter_mut()
                .find(|c| c.id == id)
                .ok_or(Error::Transport("stream missing"))
                .and_then(|c| c.write(bytes));
            if let Err(e) = result {
                conns.retain(|c| c.id != id);
                Some(Event::Closed(id, e))
            } else {
                None
            }
        }
    }
}
fn run(input: Receiver<Command>, output: SyncSender<Event>) {
    let mut conns = Vec::new();
    let mut pending = None;
    let mut last = 0;
    let mut cursor = 0;
    let mut buf = [0; 16384];
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Some(event) = pending.take() {
            match output.try_send(event) {
                Ok(()) => (),
                Err(TrySendError::Disconnected(_)) => return,
                Err(TrySendError::Full(event)) => {
                    pending = Some(event);
                    std::thread::sleep(Duration::from_millis(2));
                    continue;
                }
            }
        }
        match input.try_recv() {
            Ok(cmd) => {
                pending = command(cmd, &mut conns, &mut last);
                if pending.is_some() {
                    continue;
                }
            }
            Err(mpsc::TryRecvError::Disconnected) => return,
            Err(mpsc::TryRecvError::Empty) => (),
        }
        let n = conns.len();
        for _ in 0..n {
            if conns.is_empty() {
                break;
            }
            cursor %= conns.len();
            let c = &mut conns[cursor];
            match c.step(&mut cx, &mut buf) {
                Ok(Some(e)) => {
                    pending = Some(e);
                    cursor += 1;
                    break;
                }
                Ok(None) => cursor += 1,
                Err(e) => {
                    pending = Some(Event::Closed(c.id, e));
                    conns.remove(cursor);
                    break;
                }
            }
        }
        if pending.is_none() {
            std::thread::sleep(Duration::from_millis(2));
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    fn event(w: &mut Worker) -> Result<Event> {
        let until = Instant::now() + Duration::from_secs(3);
        loop {
            if let Some(e) = w.poll() {
                return Ok(e);
            }
            if Instant::now() >= until {
                return Err(Error::Timeout);
            }
            std::thread::sleep(Duration::from_millis(1));
        }
    }
    #[test]
    fn stream_reassembles_large_echo_and_old_ids_cannot_reopen() -> Result {
        let listener = std::net::TcpListener::bind("127.0.0.1:0")
            .map_err(|_| Error::Transport("test listen"))?;
        let port = listener
            .local_addr()
            .map_err(|_| Error::Transport("test address"))?
            .port();
        let peer = std::thread::spawn(move || -> io::Result<()> {
            let (mut socket, _) = listener.accept()?;
            socket.set_read_timeout(Some(Duration::from_secs(3)))?;
            socket.set_write_timeout(Some(Duration::from_secs(3)))?;
            let mut left = 40000;
            let mut buf = [0; 117];
            while left > 0 {
                let n = socket.read(&mut buf[..left.min(117)])?;
                if n == 0 {
                    return Err(io::ErrorKind::UnexpectedEof.into());
                }
                socket.write_all(&buf[..n])?;
                left -= n;
            }
            Ok(())
        });
        let mut w = Worker::new()?;
        w.send(Command::Open {
            id: 1,
            host: json::copy("127.0.0.1")?,
            port,
            tls: false,
            device_certificate: false,
        })?;
        assert!(matches!(event(&mut w)?, Event::Opened(1)));
        w.send(Command::Write {
            id: 1,
            bytes: vec![42; 40000],
        })?;
        let mut read = 0;
        while read < 40000 {
            match event(&mut w)? {
                Event::Data(1, b) => {
                    assert!(b.iter().all(|v| *v == 42));
                    read += b.len();
                }
                _ => return Err(Error::Invalid("unexpected test stream event")),
            }
        }
        assert!(matches!(event(&mut w)?, Event::Closed(1, _)));
        w.send(Command::Open {
            id: 1,
            host: json::copy("127.0.0.1")?,
            port,
            tls: false,
            device_certificate: false,
        })?;
        assert!(matches!(
            event(&mut w)?,
            Event::Closed(1, Error::Invalid(_))
        ));
        peer.join()
            .map_err(|_| Error::Invalid("test peer panicked"))?
            .map_err(|_| Error::Transport("test peer failed"))?;
        Ok(())
    }
}
