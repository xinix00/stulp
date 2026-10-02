//! Hosttransport voor geïsoleerde Rust-plugins; geen huisstaat en geen device-I/O.
#![forbid(unsafe_code)]
use std::{
    io::{Read, Write},
    net::TcpStream,
    time::{Duration, Instant},
};
use stulp_core::json::{self, Value};
use stulp_protocol::{
    Decoder, Frame, MAX_FRAME, MAX_GREETING,
    token::{self, Direction},
};
mod datagrams;
mod device_tls;
#[cfg(target_os = "macos")]
mod dnssd;
mod http;
mod media;
mod resolve;
mod streams;
mod tcp;
mod udp;
use stulp_sdk::{Error, Event, Plugin, Result, Transport};

/// Eén verbinding bezit zijn decoder en bewaart coalesced bytes tussen frames.
pub struct Connection {
    socket: stulp_transport::Stream<'static>,
    decoder: Decoder,
    buffer: [u8; 8192],
    offset: usize,
    length: usize,
    started: Instant,
    http: http::Worker,
    datagrams: datagrams::Worker,
    tcp: tcp::Worker,
    streams: Option<streams::Worker>,
    udp: Option<udp::Worker>,
    media: Option<media::Worker>,
    resolver: Option<resolve::Worker>,
}
impl Connection {
    /// Remote attach gebruikt TLS, tenzij plaintext expliciet is gekozen.
    pub fn attach(address: &str, secret: &str, manifest: &[u8]) -> Result<Self> {
        if secret.is_empty() {
            return Err(Error::Invalid("STULP_ATTACH_TOKEN is required"));
        }
        let socket = TcpStream::connect(address)
            .map_err(|_| Error::Transport("cannot connect to controller"))?;
        let socket = stulp_platform::socket::Socket::Tcp(socket);
        let socket = if std::env::var("STULP_ATTACH_PLAINTEXT").ok().as_deref() == Some("1") {
            stulp_transport::Stream::plain(socket)
        } else {
            let name = address
                .rsplit_once(':')
                .map(|(host, _)| host.trim_start_matches('[').trim_end_matches(']'))
                .ok_or(Error::Invalid("invalid attach address"))?;
            let ca = std::env::var("STULP_ATTACH_CA").unwrap_or_default();
            let insecure = std::env::var("STULP_ATTACH_INSECURE").ok().as_deref() == Some("1");
            stulp_transport::Stream::client(
                socket,
                name.into(),
                if ca.is_empty() || insecure {
                    None
                } else {
                    Some(std::path::Path::new(&ca))
                },
                insecure,
            )
        }
        .map_err(|e| Error::Remote(format!("attach TLS: {e}")))?;
        Self::connect(socket, secret, manifest)
    }
    /// Een lokaal proces gebruikt een privésocket zonder openbare listener.
    pub fn local(path: &str, secret: &str, manifest: &[u8]) -> Result<Self> {
        let socket = std::os::unix::net::UnixStream::connect(path)
            .map_err(|_| Error::Transport("cannot connect to local controller"))?;
        if !stulp_platform::peer::same_user(&socket)
            .map_err(|_| Error::Transport("cannot identify local controller"))?
        {
            return Err(Error::Invalid("local controller belongs to another user"));
        }
        Self::connect(
            stulp_transport::Stream::plain(stulp_platform::socket::Socket::Unix(socket))
                .map_err(|_| Error::Transport("cannot configure local socket"))?,
            secret,
            manifest,
        )
    }
    fn connect(
        socket: stulp_transport::Stream<'static>,
        secret: &str,
        manifest: &[u8],
    ) -> Result<Self> {
        let local = socket.is_local();
        if secret.is_empty() && !local {
            return Err(Error::Invalid("STULP_ATTACH_TOKEN is required"));
        }
        let mut connection = Self::from_socket(socket, MAX_GREETING)?;
        let hello = connection.greeting()?;
        if json::uint(&hello, "protocol") != 1 || (!local && json::text(&hello, "nonce").is_empty())
        {
            return Err(Error::Invalid("invalid attach greeting"));
        }
        let manifest = json::parse(manifest).map_err(stulp_core::Error::from)?;
        let id = json::text(&manifest, "id");
        let nonce = token::base64(&connection.random()?)?;
        let proof = if local && json::text(&hello, "nonce").is_empty() {
            String::new()
        } else {
            token::proof(secret, Direction::App, json::text(&hello, "nonce"), id)?
        };
        connection.write(&json::fields(&[
            ("protocol", Value::uint(1)),
            ("appId", json::string(id)?),
            ("nonce", json::string(&nonce)?),
            ("proof", json::string(&proof)?),
            ("manifest", stulp_sdk::clone(&manifest)?),
        ])?)?;
        let answer = connection.greeting()?;
        if !json::boolean(&answer, "ok") {
            return Err(Error::Remote(json::copy(json::text(&answer, "error"))?));
        }
        if !json::boolean(&answer, "ok")
            || (!local
                && !token::equal(
                    json::text(&answer, "proof"),
                    &token::proof(secret, Direction::Stulp, &nonce, id)?,
                ))
        {
            return Err(Error::Invalid("controller attach proof failed"));
        }
        connection.decoder = Decoder::new(MAX_FRAME);
        Ok(connection)
    }
    /// Geërfd fd 3 spreekt meteen appproto; er is geen publieke attach-begroeting.
    pub fn inherited() -> Result<Self> {
        let socket = stulp_platform::socket::inherited_control()
            .map_err(|_| Error::Transport("no control connection on fd 3"))?;
        Self::from_socket(
            stulp_transport::Stream::plain(stulp_platform::socket::Socket::Unix(socket))
                .map_err(|_| Error::Transport("cannot configure inherited socket"))?,
            MAX_FRAME,
        )
    }
    fn from_socket(socket: stulp_transport::Stream<'static>, limit: usize) -> Result<Self> {
        Ok(Self {
            socket,
            decoder: Decoder::new(limit),
            buffer: [0; 8192],
            offset: 0,
            length: 0,
            started: Instant::now(),
            http: http::Worker::new()?,
            datagrams: datagrams::Worker::new()?,
            tcp: tcp::Worker::new()?,
            streams: None,
            udp: None,
            media: None,
            resolver: None,
        })
    }
    fn greeting(&mut self) -> Result<Value> {
        let deadline = self.now().saturating_add(10_000);
        loop {
            if let Some(bytes) = self.read()? {
                return json::parse(&bytes).map_err(|e| Error::Core(e.into()));
            }
            if self.now() >= deadline {
                return Err(Error::Timeout);
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    fn read(&mut self) -> Result<Option<Vec<u8>>> {
        for _ in 0..16 {
            if self.offset == self.length {
                match self.socket.read(&mut self.buffer) {
                    Ok(0) => return Err(Error::Transport("controller disconnected")),
                    Ok(length) => {
                        self.length = length;
                        self.offset = 0;
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => return Ok(None),
                    Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                    Err(e) => return Err(Error::Remote(format!("controller read: {e}"))),
                }
            }
            self.offset += self.decoder.feed(
                self.buffer
                    .get(self.offset..self.length)
                    .ok_or(Error::Invalid("read buffer range"))?,
            )?;
            if let Some(bytes) = self.decoder.take() {
                return Ok(Some(bytes));
            }
        }
        Ok(None)
    }
    fn write(&mut self, value: &Value) -> Result {
        let bytes = stulp_protocol::encode(value)?;
        let deadline = self.now().saturating_add(5000);
        let mut offset = 0;
        while offset < bytes.len() {
            if self.now() >= deadline {
                return Err(Error::Timeout);
            }
            match self.socket.write(
                bytes
                    .get(offset..)
                    .ok_or(Error::Invalid("write buffer range"))?,
            ) {
                Ok(0) => return Err(Error::Transport("controller write closed")),
                Ok(n) => offset += n,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(1))
                }
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => (),
                Err(_) => return Err(Error::Transport("controller write failed")),
            }
        }
        Ok(())
    }
}
impl Transport for Connection {
    fn start_resolve(&mut self, address: String) -> Result {
        if self.resolver.is_none() {
            self.resolver = Some(resolve::Worker::new()?);
        }
        self.resolver
            .as_mut()
            .ok_or(Error::Transport("resolver unavailable"))?
            .start(address)
    }
    fn poll_resolve(&mut self) -> Option<Result<Vec<String>>> {
        self.resolver.as_mut()?.poll()
    }
    fn log(&mut self, level: &str, message: &str) -> Result {
        eprintln!("{level}\t{}", message.escape_debug());
        Ok(())
    }

    fn media_url(&mut self, token: &str) -> Result<String> {
        if self.media.is_none() {
            self.media = Some(media::Worker::new()?);
        }
        self.media
            .as_ref()
            .ok_or(Error::Transport("media unavailable"))?
            .url(token)
    }
    fn media(&mut self, command: stulp_sdk::MediaCommand) -> Result {
        self.media
            .as_mut()
            .ok_or(Error::Transport("media listener not started"))?
            .send(command)
    }
    fn poll_media(&mut self) -> Option<stulp_sdk::MediaEvent> {
        self.media.as_mut()?.poll()
    }
    fn udp(&mut self, command: stulp_sdk::UdpCommand) -> Result {
        if self.udp.is_none() {
            self.udp = Some(udp::Worker::new()?);
        }
        self.udp
            .as_mut()
            .ok_or(Error::Transport("UDP socket worker missing"))?
            .send(command)
    }
    fn poll_udp(&mut self) -> Option<stulp_sdk::UdpEvent> {
        self.udp.as_mut().and_then(udp::Worker::poll)
    }

    fn stream(&mut self, command: stulp_sdk::StreamCommand) -> Result {
        if self.streams.is_none() {
            self.streams = Some(streams::Worker::new()?);
        }
        self.streams
            .as_mut()
            .ok_or(Error::Transport("stream worker missing"))?
            .send(command)
    }
    fn poll_stream(&mut self) -> Option<stulp_sdk::StreamEvent> {
        self.streams.as_mut().and_then(streams::Worker::poll)
    }

    fn start_tcp(&mut self, request: stulp_sdk::TcpRequest) -> Result {
        self.tcp.start(request)
    }
    fn poll_tcp(&mut self) -> Option<Result<Vec<u8>>> {
        self.tcp.poll()
    }
    fn start_datagrams(&mut self, request: stulp_sdk::DatagramRequest) -> Result {
        self.datagrams.start(request)
    }
    fn poll_datagrams(&mut self) -> Option<Result<Vec<stulp_sdk::Datagram>>> {
        self.datagrams.poll()
    }
    fn wall_time(&self) -> Result<u64> {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .map_err(|_| Error::Transport("system clock precedes Unix epoch"))
    }
    async fn send(&mut self, value: &Value) -> Result {
        self.write(value)
    }
    async fn next(&mut self) -> Result<Event> {
        if let Some(bytes) = self.read()? {
            return Ok(Event::Frame(Frame::decode(&bytes)?));
        }
        std::thread::sleep(Duration::from_millis(10));
        Ok(Event::Tick)
    }
    fn start_http(&mut self, request: stulp_sdk::HttpRequest) -> Result {
        self.http.start(request)
    }
    fn poll_http(&mut self) -> Option<Result<stulp_sdk::HttpResponse>> {
        self.http.poll()
    }
    fn now(&self) -> u64 {
        u64::try_from(self.started.elapsed().as_millis()).unwrap_or(u64::MAX)
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        let entropy = hostnet::entropy().map_err(|_| Error::Transport("OS entropy unavailable"))?;
        let mut bytes = [0; 32];
        bytes.copy_from_slice(entropy.get(..32).ok_or(Error::Invalid("short entropy"))?);
        Ok(bytes)
    }
}

/// Start één pluginproces; heraanmelden maakt alle pluginstaat opnieuw, zonder achterblijvende pollers.
pub fn run<P: Plugin>(factory: impl Fn() -> P) -> std::process::ExitCode {
    let address = std::env::var("STULP_ATTACH").unwrap_or_default();
    let mut local = std::env::var("STULP_SOCKET").unwrap_or_default();
    if local.is_empty() && ["/", "./", "../"].iter().any(|p| address.starts_with(p)) {
        local = address.clone();
    }
    let inherited = address.is_empty() && local.is_empty();
    let managed = inherited || std::env::var("STULP_MANAGED").ok().as_deref() == Some("1");
    let token = std::env::var("STULP_ATTACH_TOKEN").unwrap_or_default();
    if !inherited && local.is_empty() && (token.is_empty() || address.is_empty()) {
        eprintln!(
            "[stulp:plugin-config] remote attach requires STULP_ATTACH and STULP_ATTACH_TOKEN"
        );
        return std::process::ExitCode::FAILURE;
    }
    let mut delay = 1;
    loop {
        let plugin = factory();
        let result = stulp_sdk::manifest(&plugin).and_then(|manifest| {
            if inherited {
                Connection::inherited()
            } else if local.is_empty() {
                Connection::attach(&address, &token, manifest.as_bytes())
            } else {
                Connection::local(&local, &token, manifest.as_bytes())
            }
        });
        let result = match result {
            Ok(connection) => {
                delay = 1;
                hostnet::block_on(stulp_sdk::Client::new(connection).serve(plugin))
            }
            Err(error) => Err(error),
        };
        if let Err(error) = result {
            eprintln!("[stulp:plugin-disconnected] retry_seconds={delay} error={error}");
        }
        if managed {
            return std::process::ExitCode::FAILURE;
        }
        std::thread::sleep(Duration::from_secs(delay));
        delay = (delay * 2).min(30);
    }
}
