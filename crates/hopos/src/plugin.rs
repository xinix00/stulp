//! External plugin attach and fixed I/O owners for the HopOS slot runtime.
use alloc::{string::String, vec::Vec};
use applib::{App, EXEC, appnet::TcpStream};
use core::time::Duration;
use hop_sync::select;
use stulp_core::json::{self, Value};
use stulp_protocol::{
    Decoder, Frame, MAX_FRAME, MAX_GREETING,
    token::{self, Direction},
};
use stulp_sdk::{Error, Event, Plugin, Result, Transport};
mod media;
mod requests;
mod streams;
mod udp;
/// De tien plugins delen een netstack, maar nooit hun I/O-antwoorden.
pub const BUNDLE_CAP: usize = 10;
/// Rekentijd per plugin in de bundel, in nanoseconden: de som van zijn
/// executor-beurten. De meter zet het verschil per 30 s in `STULP_LOAD`.
static BUSY_NS: [core::sync::atomic::AtomicU64; BUNDLE_CAP] =
    [const { core::sync::atomic::AtomicU64::new(0) }; BUNDLE_CAP];
/// De rekentijd van plugin `index` tot nu toe.
#[must_use]
pub fn busy_ns(index: usize) -> u64 {
    BUSY_NS
        .get(index)
        .map_or(0, |b| b.load(core::sync::atomic::Ordering::Relaxed))
}
/// Vanaf zoveel is één beurt van een plugin een `STULP_LONG_POLL`-regel.
const LONG_POLL_MS: u64 = 200;
/// De tik van het transport: de korrel van de termijnen die de plugin zelf
/// bewaakt (hartslag 5 s, MRP-hertransmissies, Flow-timers). Werk komt
/// eerder, via de wek van de controller-socket, een UDP-datagram, een
/// I/O-werker of een stream; dit is alleen de vloer (HopOS docs/apps.md).
const TICK: Duration = Duration::from_millis(50);
/// Met een actieve media-eigenaar korter: die bedient zijn kijkers en
/// bronnen nog per tik.
const MEDIA_TICK: Duration = Duration::from_millis(5);
/// One authenticated attach owns protocol buffers and all device I/O leases.
pub struct Connection {
    app: &'static App,
    socket: TcpStream,
    decoder: Decoder,
    buffer: [u8; 8192],
    offset: usize,
    length: usize,
    env: crate::environment::Environment,
    requests: requests::Requests,
    udp: udp::Sockets,
    streams: streams::Streams,
    media: Option<media::Worker>,
}
impl Connection {
    /// Attach only within the private Hop slot network, with mutual HMAC challenges.
    async fn attach_in(
        app: &'static App,
        manifest: &[u8],
        index: usize,
        queues: &'static requests::Queues,
    ) -> Result<Self> {
        let address = app
            .env("STULP_ATTACH")
            .ok_or(Error::Invalid("STULP_ATTACH is required"))?;
        let announced = json::parse(manifest).map_err(stulp_core::Error::from)?;
        let secret = attach_token(
            app.env("STULP_TOKENS"),
            app.env("STULP_ATTACH_TOKEN"),
            app.env("STULP_ATTACH_SECRET"),
            json::text(&announced, "id"),
        )?;
        let (ip, port) = requests::resolve(address).await?;
        let socket = TcpStream::connect_timeout(ip, port, Duration::from_secs(10))
            .await
            .map_err(|_| Error::Transport("controller connect failed"))?;
        let mut c = Self {
            app,
            socket,
            decoder: Decoder::new(MAX_GREETING),
            buffer: [0; 8192],
            offset: 0,
            length: 0,
            env: crate::environment::Environment::open(app)?,
            requests: requests::Requests::new(queues),
            udp: udp::Sockets::new(),
            streams: streams::Streams::new(index)?,
            media: None,
        };
        let hello = c.greeting().await?;
        if json::uint(&hello, "protocol") != 1 || json::text(&hello, "nonce").is_empty() {
            return Err(Error::Invalid("invalid attach greeting"));
        }
        let manifest = json::parse(manifest).map_err(stulp_core::Error::from)?;
        let id = json::text(&manifest, "id");
        let nonce = token::base64(&c.random()?)?;
        let proof = token::proof(&secret, Direction::App, json::text(&hello, "nonce"), id)?;
        c.send(&json::fields(&[
            ("protocol", Value::uint(1)),
            ("appId", json::string(id)?),
            ("nonce", json::string(&nonce)?),
            ("proof", json::string(&proof)?),
            ("manifest", stulp_sdk::clone(&manifest)?),
        ])?)
        .await?;
        let answer = c.greeting().await?;
        if !json::boolean(&answer, "ok") {
            return Err(Error::Remote(json::copy(json::text(&answer, "error"))?));
        }
        if !token::equal(
            json::text(&answer, "proof"),
            &token::proof(&secret, Direction::Stulp, &nonce, id)?,
        ) {
            return Err(Error::Invalid("controller attach proof failed"));
        }
        c.decoder = Decoder::new(MAX_FRAME);
        Ok(c)
    }
    async fn greeting(&mut self) -> Result<Value> {
        let deadline = self.now() + 10_000;
        loop {
            if let Some(bytes) = self.read()? {
                return json::parse(&bytes).map_err(|e| Error::Core(e.into()));
            }
            let remaining = deadline.saturating_sub(self.now());
            if remaining == 0 {
                return Err(Error::Timeout);
            }
            let _ = select(
                self.socket.readable(),
                EXEC.get().after(Duration::from_millis(remaining)),
            )
            .await;
        }
    }
    fn read(&mut self) -> Result<Option<Vec<u8>>> {
        for _ in 0..16 {
            if self.offset == self.length {
                match crate::poll::once(self.socket.read(&mut self.buffer)) {
                    Some(Ok(0)) => return Err(Error::Transport("controller disconnected")),
                    Some(Ok(n)) => {
                        self.length = n;
                        self.offset = 0;
                    }
                    None => return Ok(None),
                    Some(Err(_)) => return Err(Error::Transport("controller read failed")),
                }
            }
            self.offset += self.decoder.feed(&self.buffer[self.offset..self.length])?;
            if let Some(bytes) = self.decoder.take() {
                return Ok(Some(bytes));
            }
        }
        Ok(None)
    }
}
impl Transport for Connection {
    fn log(&mut self, level: &str, message: &str) -> Result {
        self.app
            .log(format_args!("[stulp:plugin:{level}] {message}"));
        Ok(())
    }
    fn now(&self) -> u64 {
        applib::clock::now_ns() / 1_000_000
    }
    fn wall_time(&self) -> Result<u64> {
        self.app
            .wall_ns()
            .map(|v| v / 1_000_000_000)
            .ok_or(Error::Transport("wall clock unavailable"))
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        Ok(self.env.random())
    }
    async fn send(&mut self, value: &Value) -> Result {
        let bytes = stulp_protocol::encode(value)?;
        self.socket.set_timeout(Some(Duration::from_secs(5)));
        let result = self
            .socket
            .write_all(&bytes)
            .await
            .map_err(|_| Error::Transport("controller write failed"));
        self.socket.set_timeout(None);
        result
    }
    async fn next(&mut self) -> Result<Event> {
        self.udp.tick()?;
        if let Some(media) = &mut self.media {
            media.tick()?;
        }
        if let Some(bytes) = self.read()? {
            return Ok(Event::Frame(Frame::decode(&bytes)?));
        }
        // Slapen op gebeurtenissen: de controller-socket, een datagram, een
        // antwoord van een I/O-werker, een streamgebeurtenis, of de tik.
        let tick = if self.media.is_some() {
            MEDIA_TICK
        } else {
            TICK
        };
        let Self {
            socket,
            udp,
            requests,
            streams,
            ..
        } = self;
        let _ = select(
            select(socket.readable(), udp.wait()),
            select(
                select(requests.wait(), streams.wait()),
                EXEC.get().after(tick),
            ),
        )
        .await;
        Ok(Event::Tick)
    }
    fn start_http(&mut self, r: stulp_sdk::HttpRequest) -> Result {
        self.requests.http(r)
    }
    fn poll_http(&mut self) -> Option<Result<stulp_sdk::HttpResponse>> {
        self.requests.poll_http()
    }
    fn start_tcp(&mut self, r: stulp_sdk::TcpRequest) -> Result {
        self.requests.tcp(r)
    }
    fn poll_tcp(&mut self) -> Option<Result<Vec<u8>>> {
        self.requests.poll_tcp()
    }
    fn start_resolve(&mut self, r: String) -> Result {
        self.requests.resolve(r)
    }
    fn poll_resolve(&mut self) -> Option<Result<Vec<String>>> {
        self.requests.poll_resolve()
    }
    fn start_datagrams(&mut self, r: stulp_sdk::DatagramRequest) -> Result {
        self.requests.datagrams(r)
    }
    fn poll_datagrams(&mut self) -> Option<Result<Vec<stulp_sdk::Datagram>>> {
        self.requests.poll_datagrams()
    }
    fn udp(&mut self, c: stulp_sdk::UdpCommand) -> Result {
        self.udp.command(c)
    }
    fn poll_udp(&mut self) -> Option<stulp_sdk::UdpEvent> {
        self.udp.poll()
    }
    fn stream(&mut self, c: stulp_sdk::StreamCommand) -> Result {
        self.streams.start(self.app, &mut self.env)?;
        self.streams.command(c)
    }
    fn poll_stream(&mut self) -> Option<stulp_sdk::StreamEvent> {
        self.streams.poll()
    }
    fn media_url(&mut self, token: &str) -> Result<String> {
        if self.media.is_none() {
            self.media = Some(media::Worker::new()?);
        }
        self.media
            .as_ref()
            .ok_or(Error::Transport("media owner missing"))?
            .url(token)
    }
    fn media(&mut self, c: stulp_sdk::MediaCommand) -> Result {
        self.media
            .as_mut()
            .ok_or(Error::Transport("media listener not started"))?
            .send(c)
    }
    fn poll_media(&mut self) -> Option<stulp_sdk::MediaEvent> {
        self.media.as_mut()?.poll()
    }
}
/// Run one of the original plugin implementations and recreate its state after reconnect.
pub async fn run<P: Plugin>(app: &'static App, factory: impl Fn() -> P) -> Result {
    applib::appnet::up(app).map_err(|_| Error::Transport("plugin network unavailable"))?;
    crate::meter::spawn(app)?;
    let mut env = crate::environment::Environment::open(app)?;
    let queues = requests::start(0, app, &mut env)?;
    run_in(app, 0, queues, factory).await
}
/// Eén taak per plugin in de bundel; netwerkinitialisatie doet de aanroeper één keer.
pub fn spawn<P: Plugin + 'static>(
    app: &'static App,
    index: usize,
    factory: impl Fn() -> P + 'static,
) -> Result {
    let mut env = crate::environment::Environment::open(app)?;
    let queues = requests::start(index, app, &mut env)?;
    EXEC.get()
        .spawn(async move {
            // Elke beurt van deze plugin gemeten: wie de executor lang
            // vasthoudt, staat met naam op de console (STULP_PLUGIN zegt bij de
            // start welke index welke app is).
            let mut inner = core::pin::pin!(run_in(app, index, queues, factory));
            let result = core::future::poll_fn(|cx| {
                let t0 = applib::clock::now_ns();
                let poll = inner.as_mut().poll(cx);
                let dt = applib::clock::now_ns().saturating_sub(t0);
                if let Some(b) = BUSY_NS.get(index) {
                    b.fetch_add(dt, core::sync::atomic::Ordering::Relaxed);
                }
                if dt / 1_000_000 >= LONG_POLL_MS {
                    app.log(format_args!(
                        "STULP_LONG_POLL index={index} ms={}",
                        dt / 1_000_000
                    ));
                }
                poll
            })
            .await;
            if let Err(error) = result {
                app.log(format_args!(
                    "STULP_BUNDLE_PLUGIN_FAIL index={index} error={error}"
                ));
            }
        })
        .map_err(|_| Error::Transport("plugin task unavailable"))
}
async fn run_in<P: Plugin>(
    app: &'static App,
    index: usize,
    queues: &'static requests::Queues,
    factory: impl Fn() -> P,
) -> Result {
    let mut delay = 1;
    let mut announced = false;
    loop {
        let plugin = factory();
        let manifest = stulp_sdk::manifest(&plugin)?;
        if !announced {
            announced = true;
            if let Ok(m) = json::parse(manifest.as_bytes()) {
                app.log(format_args!(
                    "STULP_PLUGIN index={index} id={}",
                    json::text(&m, "id")
                ));
            }
        }
        let result = match Connection::attach_in(app, manifest.as_bytes(), index, queues).await {
            Ok(connection) => {
                delay = 1;
                stulp_sdk::Client::new(connection).serve(plugin).await
            }
            Err(e) => Err(e),
        };
        if let Err(error) = result {
            app.log(format_args!(
                "[stulp:plugin-disconnected] index={index} retry_seconds={delay} error={error}"
            ));
        }
        EXEC.get().after(Duration::from_secs(delay)).await;
        delay = (delay * 2).min(30);
    }
}

fn attach_token(
    tokens: Option<&str>,
    single: Option<&str>,
    secret: Option<&str>,
    id: &str,
) -> Result<String> {
    if id.is_empty() {
        return Err(Error::Invalid("plugin manifest has no id"));
    }
    if let Some(raw) = tokens.filter(|s| !s.is_empty()) {
        let tokens = json::parse(raw.as_bytes()).map_err(stulp_core::Error::from)?;
        if tokens.as_object().is_none() {
            return Err(Error::Invalid("STULP_TOKENS must be an object"));
        }
        let token = json::text(&tokens, id);
        if !token.is_empty() {
            return Ok(json::copy(token)?);
        }
    }
    if let Some(secret) = secret.filter(|s| !s.is_empty()) {
        return Ok(token::token(secret, id)?);
    }
    if let Some(single) = single.filter(|s| !s.is_empty()) {
        return Ok(json::copy(single)?);
    }
    Err(Error::Invalid(
        "STULP_ATTACH_TOKEN or STULP_ATTACH_SECRET is required",
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn bundle_tokens_are_scoped_and_explicit_tokens_win() -> Result {
        let a = attach_token(None, None, Some("seed"), "com.stulp.somfy")?;
        let b = attach_token(None, None, Some("seed"), "com.stulp.matter")?;
        assert_ne!(a, b);
        assert_eq!(a, token::token("seed", "com.stulp.somfy")?);
        assert_eq!(
            attach_token(
                Some(r#"{"com.stulp.somfy":"override"}"#),
                None,
                Some("seed"),
                "com.stulp.somfy"
            )?,
            "override"
        );
        assert!(attach_token(None, None, None, "com.stulp.somfy").is_err());
        Ok(())
    }
}
