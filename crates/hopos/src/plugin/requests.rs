//! Four fixed workers; generation tags prevent replies crossing attach lifetimes.
use alloc::{string::String, vec::Vec};
use applib::{
    App, EXEC,
    appnet::{self, TcpStream},
};
use core::{cell::Cell, time::Duration};
use hop_sync::{Either, Local, Signal, mpsc::Mailbox, select};
use stulp_core::json;
use stulp_sdk::{
    Datagram, DatagramRequest, DatagramTarget, Error, HttpRequest, HttpResponse, Result, TcpRequest,
};
pub(super) struct Queues {
    generation: Local<Cell<u64>>,
    started: Local<Cell<bool>>,
    /// Gaat af als een antwoord klaarstaat: de wek van het plugin-transport.
    done: Local<Signal>,
    /// Gaat af als de attach wisselt, één bel per werker (een `Signal` kent
    /// één wachter): de werker laat zijn lopende verzoek dan vallen.
    closed: [Local<Signal>; WORKERS],
    http: Local<Mailbox<(u64, HttpRequest), 1>>,
    http_out: Local<Mailbox<(u64, Result<HttpResponse>), 1>>,
    tcp: Local<Mailbox<(u64, TcpRequest), 1>>,
    tcp_out: Local<Mailbox<(u64, Result<Vec<u8>>), 1>>,
    dns: Local<Mailbox<(u64, String), 1>>,
    dns_out: Local<Mailbox<(u64, Result<Vec<String>>), 1>>,
    browse: Local<Mailbox<(u64, DatagramRequest), 1>>,
    browse_out: Local<Mailbox<(u64, Result<Vec<Datagram>>), 1>>,
}
impl Queues {
    const fn new() -> Self {
        Self {
            generation: Local::new(Cell::new(0)),
            started: Local::new(Cell::new(false)),
            done: Local::new(Signal::new()),
            closed: [const { Local::new(Signal::new()) }; WORKERS],
            http: Local::new(Mailbox::new()),
            http_out: Local::new(Mailbox::new()),
            tcp: Local::new(Mailbox::new()),
            tcp_out: Local::new(Mailbox::new()),
            dns: Local::new(Mailbox::new()),
            dns_out: Local::new(Mailbox::new()),
            browse: Local::new(Mailbox::new()),
            browse_out: Local::new(Mailbox::new()),
        }
    }
}
static QUEUES: [Queues; super::BUNDLE_CAP] = [const { Queues::new() }; super::BUNDLE_CAP];
pub(super) struct Requests {
    queues: &'static Queues,
    generation: u64,
    http: bool,
    tcp: bool,
    dns: bool,
    browse: bool,
}
fn send<T>(queue: &Mailbox<(u64, T), 1>, generation: u64, value: T, busy: &mut bool) -> Result {
    if *busy {
        return Err(Error::Transport("previous I/O request is still ending"));
    }
    queue
        .try_send((generation, value))
        .map_err(|_| Error::Transport("I/O request queue full"))?;
    *busy = true;
    Ok(())
}
fn take<T>(
    queue: &Mailbox<(u64, Result<T>), 1>,
    generation: u64,
    busy: &mut bool,
) -> Option<Result<T>> {
    let (id, value) = queue.try_recv()?;
    if id != generation {
        return None;
    }
    *busy = false;
    Some(value)
}
fn answer<T>(
    owner: &Queues,
    queue: &Mailbox<(u64, Result<T>), 1>,
    generation: u64,
    value: Result<T>,
) {
    if generation != owner.generation.get().get() {
        return;
    }
    if let Err(hop_sync::Full(value)) = queue.try_send((generation, value)) {
        queue.try_recv();
        let _ = queue.try_send(value);
    }
    owner.done.get().set();
}
impl Drop for Requests {
    fn drop(&mut self) {
        if self.queues.generation.get().get() == self.generation {
            self.queues
                .generation
                .get()
                .set(self.generation.wrapping_add(1));
            self.queues.ring_closed();
        }
    }
}
impl Queues {
    /// Elke werker hoort dat de attach wisselde.
    fn ring_closed(&self) {
        for bell in &self.closed {
            bell.get().set();
        }
    }
}
impl Requests {
    /// Klaar zodra een werker een antwoord klaarzette.
    pub(super) async fn wait(&self) {
        self.queues.done.get().wait().await;
    }
    pub(super) fn new(queues: &'static Queues) -> Self {
        let generation = queues.generation.get().get().wrapping_add(1);
        queues.generation.get().set(generation);
        queues.ring_closed();
        Self {
            queues,
            generation,
            http: false,
            tcp: false,
            dns: false,
            browse: false,
        }
    }
    pub(super) fn http(&mut self, r: HttpRequest) -> Result {
        if self.http {
            let _ = take(&self.queues.http_out, self.generation, &mut self.http);
        }
        if r.limit > 8 << 20 || r.body.len() > 8 << 20 || r.timeout_ms == 0 || r.timeout_ms > 60000
        {
            return Err(Error::Invalid("HTTP request exceeds bounds"));
        }
        send(&self.queues.http, self.generation, r, &mut self.http)
    }
    pub(super) fn poll_http(&mut self) -> Option<Result<HttpResponse>> {
        take(&self.queues.http_out, self.generation, &mut self.http)
    }
    pub(super) fn tcp(&mut self, r: TcpRequest) -> Result {
        if self.tcp {
            let _ = take(&self.queues.tcp_out, self.generation, &mut self.tcp);
        }
        if r.prefix > 64
            || r.length_at + 2 > r.prefix
            || r.maximum > 4096
            || r.frame.len() > 4096
            || r.minimum > r.maximum
            || r.timeout_ms == 0
            || r.timeout_ms > 60000
        {
            return Err(Error::Invalid("TCP request exceeds bounds"));
        }
        send(&self.queues.tcp, self.generation, r, &mut self.tcp)
    }
    pub(super) fn poll_tcp(&mut self) -> Option<Result<Vec<u8>>> {
        take(&self.queues.tcp_out, self.generation, &mut self.tcp)
    }
    pub(super) fn resolve(&mut self, r: String) -> Result {
        if self.dns {
            let _ = take(&self.queues.dns_out, self.generation, &mut self.dns);
        }
        if r.len() > 320 {
            return Err(Error::Invalid("DNS address too long"));
        }
        send(&self.queues.dns, self.generation, r, &mut self.dns)
    }
    pub(super) fn poll_resolve(&mut self) -> Option<Result<Vec<String>>> {
        take(&self.queues.dns_out, self.generation, &mut self.dns)
    }
    pub(super) fn datagrams(&mut self, r: DatagramRequest) -> Result {
        if self.browse {
            let _ = take(&self.queues.browse_out, self.generation, &mut self.browse);
        }
        if r.payload.len() > 8192 || r.timeout_ms == 0 || r.timeout_ms > 30000 {
            return Err(Error::Invalid("discovery request exceeds bounds"));
        }
        send(&self.queues.browse, self.generation, r, &mut self.browse)
    }
    pub(super) fn poll_datagrams(&mut self) -> Option<Result<Vec<Datagram>>> {
        take(&self.queues.browse_out, self.generation, &mut self.browse)
    }
}
pub(super) async fn resolve(address: &str) -> Result<([u8; 4], u16)> {
    let (host, port) = address
        .rsplit_once(':')
        .ok_or(Error::Invalid("expected host:port"))?;
    let port = port
        .parse()
        .ok()
        .filter(|p| *p != 0)
        .ok_or(Error::Invalid("invalid peer port"))?;
    let ip = appnet::resolve(host)
        .await
        .map_err(|_| Error::Transport("name resolution failed"))?;
    Ok((ip, port))
}
async fn resolve_addresses(address: &str) -> Result<Vec<String>> {
    use super::udp::{Peer, address as display, endpoint};
    let peer = if address.parse::<core::net::SocketAddr>().is_ok() {
        endpoint(address)?
    } else {
        let (host, port) = address
            .rsplit_once(':')
            .ok_or(Error::Invalid("expected host:port"))?;
        let port = port
            .parse::<u16>()
            .ok()
            .filter(|p| *p != 0)
            .ok_or(Error::Invalid("invalid peer port"))?;
        let net = appnet::net().ok_or(Error::Transport("network unavailable"))?;
        let mut six = core::pin::pin!(net.resolve6(host));
        let mut four = core::pin::pin!(net.resolve(host));
        // Beide families lopen tegelijk; een stille AAAA-resolver vertraagt IPv4 niet.
        match select(six.as_mut(), four.as_mut()).await {
            Either::Left(Ok(ip)) => Peer::V6(appnet::Endpoint6 { ip, port }),
            Either::Right(Ok(ip)) => Peer::V4(appnet::Endpoint { ip, port }),
            Either::Left(Err(_)) => Peer::V4(appnet::Endpoint {
                ip: four
                    .await
                    .map_err(|_| Error::Transport("name resolution failed"))?,
                port,
            }),
            Either::Right(Err(_)) => Peer::V6(appnet::Endpoint6 {
                ip: six
                    .await
                    .map_err(|_| Error::Transport("name resolution failed"))?,
                port,
            }),
        }
    };
    let mut out = Vec::new();
    json::push(&mut out, display(peer), 16)?;
    Ok(out)
}
/// De vier werkers; elk heeft zijn eigen bel in [`Queues::closed`].
const WORKERS: usize = 4;
const HTTP: usize = 0;
const TCP: usize = 1;
const DNS: usize = 2;
const BROWSE: usize = 3;
/// `f` tot `ms` na nu, of tot de attach wisselt. De annulering wacht op de
/// bel van `worker` en op de termijn, niet op een dutje van 5 ms.
async fn until<T>(
    owner: &Queues,
    worker: usize,
    generation: u64,
    ms: u64,
    f: impl core::future::Future<Output = Result<T>>,
) -> Result<T> {
    if generation != owner.generation.get().get() {
        return Err(Error::Transport("attach closed"));
    }
    let cancel = async {
        let exec = EXEC.get();
        let deadline = exec.now().saturating_add(ms.saturating_mul(1_000_000));
        let Some(bell) = owner.closed.get(worker) else {
            return Error::Invalid("request worker index out of range");
        };
        loop {
            if generation != owner.generation.get().get() {
                return Error::Transport("attach closed");
            }
            if exec.now() >= deadline {
                return Error::Timeout;
            }
            // Een bel die al stond (de vorige attach) is één ronde extra.
            let _ = select(bell.get().wait(), exec.until(deadline)).await;
        }
    };
    match select(f, cancel).await {
        Either::Left(r) => r,
        Either::Right(error) => Err(error),
    }
}
pub(super) fn start(
    index: usize,
    app: &'static App,
    env: &mut crate::environment::Environment,
) -> Result<&'static Queues> {
    let owner = QUEUES
        .get(index)
        .ok_or(Error::Invalid("plugin bundle index out of range"))?;
    if owner.started.get().replace(true) {
        return Err(Error::Invalid("plugin I/O already started"));
    }
    let mut dial = crate::network::Dial::new(app, &env.random());
    EXEC.get()
        .spawn(async move {
            loop {
                let (id, r) = owner.http.recv().await;
                let ms = r.timeout_ms;
                let result = until(owner, HTTP, id, ms, http(&mut dial, r)).await;
                answer(owner, &owner.http_out, id, result);
            }
        })
        .map_err(|_| Error::Transport("HTTP worker unavailable"))?;
    EXEC.get()
        .spawn(async move {
            let mut socket = None;
            let mut identity = (0, String::new(), 0);
            loop {
                let (id, r) = owner.tcp.recv().await;
                if identity.0 != id || identity.1 != r.address || identity.2 != r.generation {
                    socket = None;
                    identity = (
                        id,
                        match json::copy(&r.address) {
                            Ok(v) => v,
                            Err(e) => {
                                answer(owner, &owner.tcp_out, id, Err(e.into()));
                                continue;
                            }
                        },
                        r.generation,
                    );
                }
                let result = until(owner, TCP, id, r.timeout_ms, tcp(&mut socket, r)).await;
                if result.is_err() {
                    socket = None;
                }
                answer(owner, &owner.tcp_out, id, result);
            }
        })
        .map_err(|_| Error::Transport("TCP worker unavailable"))?;
    EXEC.get()
        .spawn(async move {
            loop {
                let (id, r) = owner.dns.recv().await;
                let result = until(owner, DNS, id, 5000, resolve_addresses(&r)).await;
                answer(owner, &owner.dns_out, id, result);
            }
        })
        .map_err(|_| Error::Transport("DNS worker unavailable"))?;
    EXEC.get()
        .spawn(async move {
            loop {
                let (id, r) = owner.browse.recv().await;
                let result = browse(owner, id, r).await;
                answer(owner, &owner.browse_out, id, result);
            }
        })
        .map_err(|_| Error::Transport("discovery worker unavailable"))?;
    Ok(owner)
}
async fn http(dial: &mut crate::network::Dial, r: HttpRequest) -> Result<HttpResponse> {
    dial.device(r.device_certificate);
    let mut header = leanhttp::Header::new();
    for (k, v) in &r.headers {
        header
            .set(k, v)
            .map_err(|_| Error::Invalid("invalid HTTP header"))?;
    }
    let call = leanhttp::Call {
        method: &r.method,
        url: &r.url,
        header,
        body: Some(&r.body),
        header_timeout: Some(Duration::from_secs(15)),
        ..Default::default()
    };
    let mut response = leanhttp::fetch(dial, call)
        .await
        .map_err(|_| Error::Transport("HTTP or TLS request failed"))?;
    let mut headers = Vec::new();
    for (k, v) in response.header.iter() {
        json::push(&mut headers, (json::copy(k)?, json::copy(v)?), 256)?;
    }
    for cookie in &response.set_cookie {
        json::push(
            &mut headers,
            (json::copy("Set-Cookie")?, json::copy(cookie)?),
            256,
        )?;
    }
    let body = response
        .read_to_end(r.limit)
        .await
        .map_err(|_| Error::Transport("HTTP body failed or exceeds limit"))?;
    Ok(HttpResponse {
        status: response.status,
        headers,
        body,
    })
}
async fn read(socket: &mut TcpStream, mut bytes: &mut [u8]) -> Result {
    while !bytes.is_empty() {
        let n = socket
            .read(bytes)
            .await
            .map_err(|_| Error::Transport("TCP read failed"))?;
        if n == 0 {
            return Err(Error::Transport("TCP peer closed"));
        }
        bytes = &mut bytes[n..];
    }
    Ok(())
}
async fn tcp(socket: &mut Option<TcpStream>, r: TcpRequest) -> Result<Vec<u8>> {
    if socket.is_none() {
        let (ip, port) = resolve(&r.address).await?;
        *socket = Some(
            TcpStream::connect(ip, port)
                .await
                .map_err(|_| Error::Transport("TCP connect failed"))?,
        );
    }
    let socket = socket
        .as_mut()
        .ok_or(Error::Transport("TCP socket missing"))?;
    socket
        .write_all(&r.frame)
        .await
        .map_err(|_| Error::Transport("TCP write failed"))?;
    let mut prefix = [0; 64];
    read(socket, &mut prefix[..r.prefix]).await?;
    let n = usize::from(u16::from_be_bytes([
        prefix[r.length_at],
        prefix[r.length_at + 1],
    ]));
    if n < r.minimum || n > r.maximum {
        return Err(Error::Invalid("TCP response outside bounds"));
    }
    let mut answer = Vec::new();
    answer
        .try_reserve_exact(n + r.prefix)
        .map_err(|_| stulp_core::Error::Memory)?;
    answer.extend_from_slice(&prefix[..r.prefix]);
    answer.resize(r.prefix + n, 0);
    read(socket, &mut answer[r.prefix..]).await?;
    Ok(answer)
}
async fn browse(owner: &Queues, generation: u64, r: DatagramRequest) -> Result<Vec<Datagram>> {
    use super::udp::{INTERFACE, Peer, Socket, address, endpoint};
    let mut sockets = Vec::new();
    let mut targets = Vec::new();
    match r.target {
        DatagramTarget::Address(s) => json::push(&mut targets, endpoint(&s)?, 2)?,
        DatagramTarget::Mdns => {
            json::push(&mut targets, endpoint("224.0.0.251:5353")?, 2)?;
            json::push(&mut targets, endpoint("[ff02::fb%1]:5353")?, 2)?;
        }
        DatagramTarget::Ssdp => json::push(&mut targets, endpoint("239.255.255.250:1900")?, 2)?,
    }
    let deadline = applib::clock::now_ns().saturating_add(r.timeout_ms * 1_000_000);
    for target in targets {
        let bind = match target {
            Peer::V4(_) => endpoint("0.0.0.0:0")?,
            Peer::V6(_) => endpoint("[::]:0")?,
        };
        let socket = Socket::bind(bind)?;
        until(owner, BROWSE, generation, r.timeout_ms.min(5000), async {
            socket.send(target, &r.payload).await
        })
        .await?;
        json::push(&mut sockets, socket, 2)?;
    }
    let mut result = Vec::new();
    let mut buffer = [0; 9001];
    loop {
        if generation != owner.generation.get().get() {
            return Err(Error::Transport("attach closed"));
        }
        let remaining = deadline.saturating_sub(applib::clock::now_ns());
        if remaining == 0 {
            return Ok(result);
        }
        // A discovery deadline returns collected answers, not a request timeout.
        // Check cancellation even when no datagrams arrive.
        for socket in &sockets {
            if let Some(received) = crate::poll::once(socket.recv(&mut buffer)) {
                let (n, from) = received?;
                if n > 9000 {
                    return Err(Error::Invalid("discovery datagram too large"));
                }
                let mut payload = Vec::new();
                payload
                    .try_reserve_exact(n)
                    .map_err(|_| stulp_core::Error::Memory)?;
                payload.extend_from_slice(&buffer[..n]);
                json::push(
                    &mut result,
                    Datagram {
                        source: address(from),
                        interface: INTERFACE,
                        payload,
                    },
                    256,
                )?;
            }
        }
        // Slapen tot een antwoord, de termijn of een wisselende attach; niet
        // elke 5 ms kijken.
        let Some(bell) = owner.closed.get(BROWSE) else {
            return Err(Error::Invalid("request worker index out of range"));
        };
        let _ = select(
            super::udp::any_readable(&sockets),
            select(
                bell.get().wait(),
                EXEC.get().after(Duration::from_nanos(remaining)),
            ),
        )
        .await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use core::{
        future::Future,
        pin::pin,
        task::{Context, Poll, Waker},
    };

    #[test]
    fn bundle_replies_and_disconnects_stay_with_their_plugin() -> Result {
        static A: Queues = Queues::new();
        static B: Queues = Queues::new();
        let mut a = Requests::new(&A);
        let mut b = Requests::new(&B);
        a.resolve(json::copy("one:80")?)?;
        b.resolve(json::copy("two:80")?)?;
        answer(
            &A,
            &A.dns_out,
            a.generation,
            Ok(alloc::vec![json::copy("1.2.3.4")?]),
        );
        answer(
            &B,
            &B.dns_out,
            b.generation,
            Ok(alloc::vec![json::copy("5.6.7.8")?]),
        );
        assert!(matches!(a.poll_resolve(), Some(Ok(v)) if v[0] == "1.2.3.4"));
        let old = a.generation;
        drop(a);
        let mut next = Requests::new(&A);
        answer(&A, &A.dns_out, old, Ok(Vec::new()));
        assert!(next.poll_resolve().is_none());
        assert!(matches!(b.poll_resolve(), Some(Ok(v)) if v[0] == "5.6.7.8"));
        assert_eq!(B.generation.get().get(), b.generation);
        Ok(())
    }

    #[test]
    fn dropped_attach_never_starts_queued_network_work() {
        static Q: Queues = Queues::new();
        let owner = Requests::new(&Q);
        let stale = owner.generation;
        drop(owner);
        let current = Requests::new(&Q);
        let touched = Cell::new(false);
        let mut work = pin!(until(&Q, HTTP, stale, 1000, async {
            touched.set(true);
            Ok(())
        }));
        let mut context = Context::from_waker(Waker::noop());
        assert!(matches!(
            work.as_mut().poll(&mut context),
            Poll::Ready(Err(_))
        ));
        assert!(!touched.get());
        assert_eq!(Q.generation.get().get(), current.generation);
    }
}
