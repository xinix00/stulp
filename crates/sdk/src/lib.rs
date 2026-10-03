//! Eén plugin bezit zijn callbacks en lokale snapshots; het transport bezit alleen I/O.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
/// DNS-SD-codec voor de macOS-systeemadapter.
pub mod dnssd;
/// Coöperatieve lange opdrachten met een afzonderlijke, begrensde callbackbaan.
pub mod jobs;
/// Gedeelde, faalbare bewerkingen voor plugin-API's.
pub mod util;
/// Begrensde XML voor lokale apparaatprotocollen, zonder DTD of externe entiteiten.
pub mod xml;
use alloc::{collections::VecDeque, string::String, vec::Vec};
use core::{fmt, future::Future};
use stulp_core::json::{self, TryClone, Value};
use stulp_protocol::{Frame, Kind};

/// Fouten behouden hun categorie tot aan het appkanaal.
#[derive(Debug)]
pub enum Error {
    /// Het gedeelde JSON- of gegevenscontract is geschonden.
    Core(stulp_core::Error),
    /// Een pluginvoorwaarde ontbreekt of is ongeldig.
    Invalid(&'static str),
    /// De controller weigerde de opdracht, met zijn oorspronkelijke reden.
    Remote(String),
    /// Een transport of externe dienst viel weg.
    Transport(&'static str),
    /// De eigenaar vraagt een protocoltaak coöperatief te stoppen en op te ruimen.
    Cancelled,
    /// Een monotone deadline is verstreken.
    Timeout,
}
impl core::error::Error for Error {}
impl From<stulp_core::Error> for Error {
    fn from(e: stulp_core::Error) -> Self {
        Self::Core(e)
    }
}
impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Core(e) => write!(f, "{e}"),
            Self::Invalid(s) | Self::Transport(s) => f.write_str(s),
            Self::Remote(s) => f.write_str(s),
            Self::Cancelled => f.write_str("Opdracht geannuleerd."),
            Self::Timeout => f.write_str("operation timed out"),
        }
    }
}
/// Resultaten dragen geen platformfouten naar de pluginlogica.
pub type Result<T = ()> = core::result::Result<T, Error>;
/// Een kloktik wekt pollers en heartbeat zonder een extra thread per timer.
pub enum Event {
    /// Een compleet, gevalideerd appframe.
    Frame(Frame),
    /// Monotone tijd kan verder zijn gegaan.
    Tick,
}
/// Een uitgaand verzoek draagt zijn eigen bovengrens en totale termijn.
pub struct HttpRequest {
    /// Alleen lokale apparaten met een eigen certificaat; nooit voor cloudaccounts.
    pub device_certificate: bool,
    /// HTTP-methode.
    pub method: String,
    /// Absolute URL, inclusief geëncodeerde query.
    pub url: String,
    /// Extra headers; geheimen worden niet gelogd.
    pub headers: Vec<(String, String)>,
    /// Optionele payload.
    pub body: Vec<u8>,
    /// Maximale antwoordbody vóór allocatie.
    pub limit: usize,
    /// Totale tijd voor verbinden en antwoord.
    pub timeout_ms: u64,
}
impl HttpRequest {
    /// JSON-API-aanroep, begrensd op 1 MiB en twintig seconden.
    pub fn get(url: &str) -> Result<Self> {
        Ok(Self {
            device_certificate: false,
            method: json::copy("GET")?,
            url: json::copy(url)?,
            headers: Vec::new(),
            body: Vec::new(),
            limit: 1 << 20,
            timeout_ms: 20_000,
        })
    }
}
/// Ook een foutstatus is een echt HTTP-antwoord; de plugin leest zijn eigen protocolfout.
pub struct HttpResponse {
    /// Statuscode.
    pub status: u16,
    /// Antwoordheaders.
    pub headers: Vec<(String, String)>,
    /// Begrensde antwoordbody.
    pub body: Vec<u8>,
}
/// Ontdekking gebruikt een expliciete bestemming of mDNS op alle bruikbare LAN-interfaces.
pub enum DatagramTarget {
    /// Een numeriek IP-adres met poort, bijvoorbeeld SSDP of een lokale testpeer.
    Address(String),
    /// Legacy mDNS-query via IPv4 en scoped IPv6, met hop-limit 255.
    Mdns,
    /// SSDP over iedere actieve IPv4-LAN-interface.
    Ssdp,
}
/// Eén zoekvraag; de adapter sluit de sockets na deze ronde.
pub struct DatagramRequest {
    /// Geen hostname-resolutie tijdens ontdekking.
    pub target: DatagramTarget,
    /// Zoekvraag, hoogstens 8192 bytes.
    pub payload: Vec<u8>,
    /// Totale termijn, hoogstens dertig seconden.
    pub timeout_ms: u64,
}
/// Een antwoord is van de ontvanger, inclusief het adres van de afzender.
pub struct Datagram {
    /// IP-adres met poort.
    pub source: String,
    /// Interface-index voor link-local IPv6-records, ook wanneer het antwoord over IPv4 komt.
    pub interface: u32,
    /// Hoogstens 9000 bytes per antwoord, maximaal 256 antwoorden.
    pub payload: Vec<u8>,
}
/// Eén verzoek op een herbruikbare TCP-verbinding met een u16-lengteprefix.
pub struct TcpRequest {
    /// Host en poort.
    pub address: String,
    /// Een nieuwe generatie sluit een vorige, mogelijk onzuivere stroom.
    pub generation: u64,
    /// Compleet verzoek; maximaal 4096 bytes.
    pub frame: Vec<u8>,
    /// Kop vóór de body, maximaal 64 bytes.
    pub prefix: usize,
    /// Offset van de big-endian u16-bodylengte binnen die kop.
    pub length_at: usize,
    /// Kleinste toegestane body.
    pub minimum: usize,
    /// Grootste toegestane body, maximaal 4096 bytes.
    pub maximum: usize,
    /// Totale termijn voor verbinden, schrijven en lezen.
    pub timeout_ms: u64,
}
/// Een opdracht voor een langlevende stroom; IDs worden binnen één attach nooit hergebruikt.
pub enum StreamCommand {
    /// Open maximaal acht stromen. TLS met een eigen apparaatcertificaat is expliciet.
    Open {
        /// Uniek, niet-nul sessienummer.
        id: u64,
        /// Hostnaam zonder poort.
        host: String,
        /// TCP-poort.
        port: u16,
        /// TLS 1.3 of een onversleutelde lokale stroom.
        tls: bool,
        /// Alleen voor een lokaal apparaat met een eigen certificaat.
        device_certificate: bool,
    },
    /// Maximaal 64 KiB; de adapter herhaalt mislukte writes nooit.
    Write {
        /// Eerder geopend sessienummer.
        id: u64,
        /// De ontvanger neemt het eigendom over.
        bytes: Vec<u8>,
    },
    /// Sluit de socket en alle nog niet verstuurde bytes.
    Close {
        /// Eerder geopend sessienummer.
        id: u64,
    },
}
/// Bytes blijven in ontvangstreeks, ook wanneer meerdere sockets actief zijn.
pub enum StreamEvent {
    /// Verbinden en eventueel TLS zijn voltooid.
    Opened(u64),
    /// Maximaal 16 KiB; een volle ontvangstwachtrij oefent TCP-backpressure uit.
    Data(u64, Vec<u8>),
    /// De stream is gesloten; herstel vereist een nieuw sessienummer.
    Closed(u64, Error),
}
mod media;
mod udp;
pub use media::{MediaCommand, MediaEvent};
pub use udp::{UdpCommand, UdpEvent};
/// De adapter levert een begrensd frame of een kloktik, minstens elke 100 ms.
pub trait Transport {
    /// Diagnostiek verlaat de plugin via de platformadapter; geen huisconfig in logregels.
    fn log(&mut self, _level: &str, _message: &str) -> Result {
        Ok(())
    }

    /// URL van een private lokale mediabron; start de vaste uitvoerwerker zo nodig.
    fn media_url(&mut self, _token: &str) -> Result<String> {
        Err(Error::Transport("media output unavailable"))
    }
    /// Draagt een begrensd binair fragment over, nooit via JSON.
    fn media(&mut self, _command: MediaCommand) -> Result {
        Err(Error::Transport("media output unavailable"))
    }
    /// Sluiten bij ontbrekende kijkers of uitvoerfouten.
    fn poll_media(&mut self) -> Option<MediaEvent> {
        None
    }
    /// Niet-blokkerende overdracht naar de datagramsocket-eigenaar.
    fn udp(&mut self, _command: UdpCommand) -> Result {
        Err(Error::Transport("UDP socket adapter unavailable"))
    }
    /// Eén socketgebeurtenis, zonder op een datagram te wachten.
    fn poll_udp(&mut self) -> Option<UdpEvent> {
        None
    }

    /// Niet-blokkerende overdracht aan de eigenaar van langlevende sockets.
    fn stream(&mut self, _command: StreamCommand) -> Result {
        Err(Error::Transport("stream adapter unavailable"))
    }
    /// Eén ontvangen streamgebeurtenis, zonder op bytes te wachten.
    fn poll_stream(&mut self) -> Option<StreamEvent> {
        None
    }

    /// Draagt één TCP-opdracht over aan de verbindingseigenaar.
    fn start_tcp(&mut self, _request: TcpRequest) -> Result {
        Err(Error::Transport("TCP adapter unavailable"))
    }
    /// Compleet antwoord, inclusief kop.
    fn poll_tcp(&mut self) -> Option<Result<Vec<u8>>> {
        None
    }
    /// Eén begrensde UDP-zoekronde, zonder het controllerkanaal te bezetten.
    fn start_datagrams(&mut self, _request: DatagramRequest) -> Result {
        Err(Error::Transport("UDP adapter unavailable"))
    }
    /// Antwoorden uit de huidige zoekronde, in ontvangstreeks.
    fn poll_datagrams(&mut self) -> Option<Result<Vec<Datagram>>> {
        None
    }
    /// Naamresolutie loopt naast de controllerpomp; invoer is host:poort.
    fn start_resolve(&mut self, _address: String) -> Result {
        Err(Error::Transport("name resolver unavailable"))
    }
    /// Alle numerieke kandidaten van dezelfde aanvraag, inclusief IPv6-interface.
    fn poll_resolve(&mut self) -> Option<Result<Vec<String>>> {
        None
    }
    /// Stuurt één frame binnen een begrensde schrijftijd.
    fn send(&mut self, value: &Value) -> impl Future<Output = Result>;
    /// Wacht op I/O of de volgende kloktik.
    fn next(&mut self) -> impl Future<Output = Result<Event>>;
    /// Milliseconden sinds het ontstaan van dit transport.
    fn now(&self) -> u64;
    /// Wandklok voor vervaldatums van externe sessies.
    fn wall_time(&self) -> Result<u64> {
        Err(Error::Transport("wall clock unavailable"))
    }
    /// Identiteiten komen uit de platformentropy, nooit uit een teller.
    fn random(&mut self) -> Result<[u8; 32]>;
    /// De I/O-eigenaar accepteert hoogstens één HTTP-aanroep tegelijk.
    fn start_http(&mut self, _request: HttpRequest) -> Result {
        Err(Error::Transport("HTTP adapter unavailable"))
    }
    /// Neemt het antwoord over zonder het appkanaal te blokkeren.
    fn poll_http(&mut self) -> Option<Result<HttpResponse>> {
        None
    }
}
/// Plugins hebben één eigenaar en kunnen over controllercalls heen wachten.
pub trait Plugin {
    /// Own the initialized callback loop; specialized plugins may use bounded device workers.
    fn serve<T: Transport>(&mut self, client: &mut Client<T>) -> impl Future<Output = Result>
    where
        Self: Sized,
    {
        client.serve_serial(self)
    }
    /// Het originele manifest wordt samen met de binary gebouwd.
    fn manifest(&self) -> &'static [u8];
    /// Ingebedde paden; de controller haalt de bytes pas op wanneer de browser ze nodig heeft.
    fn assets(&self) -> &'static [&'static str] {
        &[]
    }
    /// Controllercallbacks komen op één worker, in wirevolgorde.
    fn handle<T: Transport>(
        &mut self,
        client: &mut Client<T>,
        method: &str,
        params: &Value,
    ) -> impl Future<Output = Result<Value>>;
    /// Controleert een browserpatch vóór opslag; netwerkfouten blijven zichtbaar voor de gebruiker.
    /// Pollers lezen daarna de bevestigde state.device-snapshot.
    fn settings<T: Transport>(
        &mut self,
        _client: &mut Client<T>,
        _id: &str,
        _patch: &Value,
    ) -> impl Future<Output = Result<Value>> {
        core::future::ready(Ok(Value::Null))
    }
    /// De eerstvolgende poller mag de controller niet met lege rondes belasten.
    fn tick<T: Transport>(&mut self, _client: &mut Client<T>) -> impl Future<Output = Result> {
        core::future::ready(Ok(()))
    }
}

/// De authoritative snapshot komt uitsluitend van Stulp.
pub struct State {
    root: Value,
    revision: u64,
}
impl State {
    fn empty() -> Self {
        Self {
            root: json::object(),
            revision: 0,
        }
    }
    fn load(&mut self, id: &str, value: Value) -> Result {
        if json::uint(&value, "protocol") != 1 || json::text(&value, "appId") != id {
            return Err(Error::Invalid("invalid controller welcome"));
        }
        for key in ["devices", "settings", "manifest"] {
            if json::get(&value, key).and_then(Value::as_object).is_none() {
                return Err(Error::Invalid("invalid welcome snapshot"));
            }
        }
        self.root = value;
        self.revision = self.revision.wrapping_add(1);
        Ok(())
    }
    /// Metadata, instellingen en apparaten zoals de controller ze heeft bevestigd.
    pub fn root(&self) -> &Value {
        &self.root
    }
    /// Leest één apparaat zonder een RPC of een kopie.
    pub fn device(&self, id: &str) -> Result<&Value> {
        json::get(&self.root, "devices")
            .and_then(|v| json::get(v, id))
            .ok_or(Error::Invalid("device is absent from controller snapshot"))
    }
    /// Appinstellingen blijven binnen de geauthenticeerde appgrens.
    pub fn setting(&self, key: &str) -> Option<&Value> {
        json::get(&self.root, "settings").and_then(|v| json::get(v, key))
    }
    fn apply(&mut self, frame: &Frame) -> Result {
        self.revision = self.revision.wrapping_add(1);
        let params = json::get(&frame.value, "p").unwrap_or(&Value::Null);
        match frame.method() {
            "state.snapshot" => {
                let id = json::copy(json::text(&self.root, "appId"))?;
                self.load(&id, clone(params)?)?;
            }
            "state.app" => {
                json::set(&mut self.root, "appState", clone(params)?)?;
            }
            "state.settings" => {
                if params.as_object().is_none() {
                    return Err(Error::Invalid("invalid settings snapshot"));
                }
                json::set(&mut self.root, "settings", clone(params)?)?;
            }
            "state.device" => {
                let id = json::text(params, "deviceId");
                if id.is_empty() {
                    return Err(Error::Invalid("device id missing in snapshot"));
                }
                let value =
                    json::get(params, "device").ok_or(Error::Invalid("device snapshot missing"))?;
                if !value.is_null() && value.as_object().is_none() {
                    return Err(Error::Invalid("invalid device snapshot"));
                }
                // Ter plekke: alleen dit ene apparaat wordt gekopieerd, niet de
                // hele lijst (en via json::set niet de rest van de staat).
                let devices = self
                    .root
                    .as_object_mut()
                    .and_then(|r| r.get_mut("devices"))
                    .ok_or(Error::Invalid("welcome missing"))?;
                if value.is_null() {
                    json::remove(devices, id)?;
                } else {
                    json::set(devices, id, clone(value)?)?;
                }
            }
            _ => (),
        }
        Ok(())
    }
}

const MAX_INBOX: usize = 64;
/// Zoveel staat-events houdt het journaal van een pool vast tussen twee rondes.
const MAX_JOURNAL: usize = 256;
/// Eén callback mag schrijven terwijl state-events zijn lokale kopie bijwerken.
pub struct Client<T> {
    transport: T,
    state: State,
    inbox: Vec<Frame>,
    next_id: u64,
    heartbeat: Option<(u64, u64)>,
    next_heartbeat: u64,
    /// Wanneer de controller het laatst iets stuurde.
    last_heard: u64,
    /// Of de huidige stilte al gemeld is.
    slow: bool,
    /// Protocolbeurten sinds de laatste verplichte yield.
    turns: u32,
    udp_inbox: VecDeque<UdpEvent>,
    discard_datagrams: bool,
    discard_resolve: bool,
    interrupted_job: bool,
    /// De toegepaste staat-events, voor een pool die ze aan zijn werkers
    /// doorgeeft in plaats van telkens de hele staat te kopiëren.
    journal: Option<Vec<Frame>>,
    /// Het journaal liep over: de werkers hebben een volledige kopie nodig.
    journal_lost: bool,
}
/// Hoe lang een ping op zijn antwoord mag wachten voordat de stilte gemeld
/// wordt (zie `Client::pump`). Een gemiste hartslag sluit niets af.
pub(crate) const HEARTBEAT_DEADLINE_MS: u64 = 35_000;
/// Pas na zoveel volledige stilte van de controller geeft de plugin de
/// verbinding op. Een afsluiting kost ~70 inits en tientallen
/// Matter-handshakes, en die kosten veroorzaakten op de LicheeRV de volgende
/// afsluiting; daarom is een trage controller geen dode controller.
pub(crate) const SILENCE_LIMIT_MS: u64 = 300_000;
/// Na zoveel protocolbeurten geeft een plugin de executor verplicht terug,
/// ook als elke await meteen klaar was (het coöperatieve budget).
const COOP_TURNS: u32 = 32;
/// Vanaf deze duur is een plugin-callback een waarschuwing op de console waard.
const SLOW_CALLBACK_MS: u64 = 500;
impl<T: Transport> Client<T> {
    /// De attach-begroeting is al door de transportadapter geverifieerd.
    pub fn new(transport: T) -> Self {
        let last_heard = transport.now();
        Self {
            transport,
            state: State::empty(),
            inbox: Vec::new(),
            next_id: 0,
            heartbeat: None,
            next_heartbeat: 5000,
            last_heard,
            slow: false,
            turns: 0,
            udp_inbox: VecDeque::new(),
            discard_datagrams: false,
            discard_resolve: false,
            interrupted_job: false,
            journal: None,
            journal_lost: false,
        }
    }
    /// Een begrensde diagnostiekregel, ook vanuit een coöperatieve protocoltaak.
    pub fn log(&mut self, level: &str, message: &str) -> Result {
        if !matches!(level, "debug" | "info" | "warn" | "error") || message.len() > 4096 {
            return Err(Error::Invalid("invalid plugin log record"));
        }
        self.transport.log(level, message)
    }
    /// De plugin krijgt alleen een lening van de huidige snapshot.
    pub fn state(&self) -> &State {
        &self.state
    }
    /// Begint een lokale subtaak met een reeds geauthenticeerde snapshot.
    pub fn from_snapshot(transport: T, snapshot: Value) -> Result<Self> {
        let id = json::copy(json::text(&snapshot, "appId"))?;
        let mut c = Self::new(transport);
        c.state.load(&id, snapshot)?;
        Ok(c)
    }
    /// Pollintervallen gebruiken dezelfde monotone klok als de RPC-deadlines.
    pub fn now(&self) -> u64 {
        self.transport.now()
    }
    /// Unix-seconden voor externe sessievervaldatums.
    pub fn wall_time(&self) -> Result<u64> {
        self.transport.wall_time()
    }
    /// Bewaart privéstaat en vervangt de lokale kopie pas na duurzame bevestiging.
    pub async fn app_state(&mut self, state: Value) -> Result {
        self.call("state.set", &json::fields(&[("state", clone(&state)?)])?)
            .await?;
        let event = Frame {
            kind: Kind::Event,
            id: 0,
            value: Frame::request(0, "state.app", &state)?,
        };
        json::set(&mut self.state.root, "appState", state)?;
        self.state.revision = self.state.revision.wrapping_add(1);
        self.record(event)
    }
    /// Onthoudt een toegepast staat-event als een pool erom vroeg (begrensd;
    /// bij overloop krijgen de werkers weer een volledige kopie).
    fn record(&mut self, frame: Frame) -> Result {
        if let Some(journal) = &mut self.journal {
            if journal.len() >= MAX_JOURNAL || journal.try_reserve(1).is_err() {
                journal.clear();
                self.journal_lost = true;
            } else {
                journal.push(frame);
            }
        }
        Ok(())
    }
    /// Start een zoekronde die de plugin naast bestaande netwerkprotocollen kan pollen.
    pub fn start_datagrams(&mut self, request: DatagramRequest) -> Result {
        if self.discard_datagrams {
            if self.transport.poll_datagrams().is_some() {
                self.discard_datagrams = false;
            } else {
                return Err(Error::Transport(
                    "cancelled discovery round is still closing",
                ));
            }
        }
        self.transport.start_datagrams(request)
    }
    /// Begin één begrensde naamlookup; een geannuleerde voorganger wordt eerst afgevoerd.
    pub fn start_resolve(&mut self, address: String) -> Result {
        if address.is_empty() || address.len() > 320 {
            return Err(Error::Invalid("invalid resolver address"));
        }
        if self.discard_resolve {
            if self.transport.poll_resolve().is_none() {
                return Err(Error::Transport("cancelled lookup is still ending"));
            }
            self.discard_resolve = false;
        }
        self.transport.start_resolve(address)
    }
    /// Neemt de complete lookup over zonder op DNS te blokkeren.
    pub fn poll_resolve(&mut self) -> Option<Result<Vec<String>>> {
        let result = self.transport.poll_resolve();
        if self.discard_resolve {
            if result.is_some() {
                self.discard_resolve = false;
            }
            None
        } else {
            result
        }
    }
    /// Resolveert een naam met tien seconden budget, terwijl heartbeats en snapshots doorgaan.
    pub async fn resolve(&mut self, address: &str) -> Result<Vec<String>> {
        if address.parse::<core::net::SocketAddr>().is_ok() {
            let mut result = Vec::new();
            json::push(&mut result, json::copy(address)?, 32)?;
            return Ok(result);
        }
        self.start_resolve(json::copy(address)?)?;
        let deadline = self.now().saturating_add(10_000);
        loop {
            if let Some(result) = self.poll_resolve() {
                return result;
            }
            if self.now() >= deadline {
                self.discard_resolve = true;
                return Err(Error::Timeout);
            }
            self.idle().await?;
        }
    }
    /// Neemt uitsluitend een complete zoekronde over, zonder op de deadline te wachten.
    pub fn poll_datagrams(&mut self) -> Option<Result<Vec<Datagram>>> {
        let result = self.transport.poll_datagrams();
        if self.discard_datagrams {
            if result.is_some() {
                self.discard_datagrams = false;
            }
            None
        } else {
            result
        }
    }
    /// Opent, verzendt of sluit zonder controllerheartbeats op te houden.
    pub fn udp(&mut self, command: UdpCommand) -> Result {
        self.transport.udp(command)
    }
    /// Eén compleet datagram of een socketstatus.
    pub fn poll_udp(&mut self) -> Option<UdpEvent> {
        self.udp_inbox
            .pop_front()
            .or_else(|| self.transport.poll_udp())
    }
    /// Private URL voor de controllerproxy.
    pub fn media_url(&mut self, token: &str) -> Result<String> {
        self.transport.media_url(token)
    }
    /// De media-eigenaar neemt binaire bytes over.
    pub fn media(&mut self, command: MediaCommand) -> Result {
        self.transport.media(command)
    }
    /// Terugmeldingen om een ongebruikte bron ook bij de camera te sluiten.
    pub fn poll_media(&mut self) -> Option<MediaEvent> {
        self.transport.poll_media()
    }
    /// Een socketopdracht wacht niet op netwerk-I/O.
    pub fn stream(&mut self, command: StreamCommand) -> Result {
        self.transport.stream(command)
    }
    /// De plugin bezit het protocol bovenop deze geordende bytegebeurtenissen.
    pub fn poll_stream(&mut self) -> Option<StreamEvent> {
        self.transport.poll_stream()
    }
    /// Verse bytes voor pairing en protocolnonces.
    pub fn random(&mut self) -> Result<[u8; 32]> {
        self.transport.random()
    }
    fn id(&mut self) -> Result<u64> {
        self.next_id = self
            .next_id
            .checked_add(1)
            .ok_or(Error::Invalid("RPC ids exhausted"))?;
        Ok(self.next_id)
    }
    /// Iedere mutatie wacht op het echte antwoord; een fout publiceert geen verzonnen succes.
    pub async fn call(&mut self, method: &str, params: &Value) -> Result<Value> {
        let id = self.id()?;
        self.transport
            .send(&Frame::request(id, method, params)?)
            .await?;
        let deadline = self.now().saturating_add(30_000);
        loop {
            if self.now() >= deadline {
                return Err(Error::Timeout);
            }
            if let Some(frame) = self.pump().await? {
                if frame.id == id && matches!(frame.kind, Kind::Response | Kind::Error) {
                    if frame.kind == Kind::Error {
                        return Err(Error::Remote(json::copy(
                            json::get(&frame.value, "e")
                                .map(|e| json::text(e, "message"))
                                .unwrap_or("controller rejected request"),
                        )?));
                    }
                    return clone(json::get(&frame.value, "r").unwrap_or(&Value::Null));
                }
                if frame.kind == Kind::Request {
                    json::push(&mut self.inbox, frame, MAX_INBOX)?;
                }
            }
        }
    }
    async fn pump(&mut self) -> Result<Option<Frame>> {
        if self.interrupted_job {
            return Err(Error::Transport(
                "protocol owner was interrupted; reconnect required",
            ));
        }
        // Het coöperatieve budget: elke lus van elke plugin loopt hierlangs,
        // dus hier krijgen de andere taken op de executor gegarandeerd een
        // beurt, ook als deze plugin alleen maar meteen-klare awaits ziet.
        self.turns = self.turns.wrapping_add(1);
        if self.turns.is_multiple_of(COOP_TURNS) {
            hop_sync::yield_now().await;
        }
        let now = self.now();
        if self.heartbeat.is_none() && now >= self.next_heartbeat {
            let id = self.id()?;
            self.transport
                .send(&Frame::request(id, "$appproto.ping", &Value::Null)?)
                .await?;
            // De termijn is ruim: op een gedeeld core beslist de buur hoe snel
            // het antwoord komt, en een te krappe termijn maakte van elke drukke
            // minuut een storm van tien herverbindingen (LicheeRV, 02-10). De
            // controller zelf geeft een app 15 s; deze kant mag niet eerder
            // opgeven dan hij.
            self.heartbeat = Some((id, self.now().saturating_add(HEARTBEAT_DEADLINE_MS)));
        }
        let Event::Frame(frame) = self.transport.next().await? else {
            // Drain already received replies before declaring silence. On a
            // shared core another task may have delayed our turn past the
            // deadline even though the controller answered in time.
            let now = self.now();
            if now.saturating_sub(self.last_heard) >= SILENCE_LIMIT_MS {
                self.log(
                    "warn",
                    "STULP_HEARTBEAT_TIMEOUT controller silent for 5 minutes",
                )?;
                return Err(Error::Timeout);
            }
            if self.heartbeat.is_some_and(|(_, deadline)| now >= deadline) {
                // Traag, niet dood: melden, en de volgende ping gaat gewoon uit.
                if !self.slow {
                    self.slow = true;
                    self.log("warn", "STULP_HEARTBEAT_SLOW controller reply missing")?;
                }
                self.heartbeat = None;
                self.next_heartbeat = now.saturating_add(5000);
            }
            return Ok(None);
        };
        self.last_heard = self.now();
        if self.slow {
            self.slow = false;
            self.log("info", "STULP_HEARTBEAT_RECOVERED")?;
        }
        if matches!(frame.kind, Kind::Response | Kind::Error)
            && self.heartbeat.is_some_and(|(id, _)| id == frame.id)
        {
            if frame.kind == Kind::Error {
                return Err(Error::Invalid("heartbeat rejected"));
            }
            self.heartbeat = None;
            self.next_heartbeat = self.now().saturating_add(5000);
            return Ok(None);
        }
        if frame.kind == Kind::Event {
            self.state.apply(&frame)?;
            self.record(frame)?;
            return Ok(None);
        }
        if frame.kind == Kind::Request && frame.method() == "$appproto.ping" {
            self.transport
                .send(&Frame::response(frame.id, Ok(Value::Null))?)
                .await?;
            return Ok(None);
        }
        Ok(Some(frame))
    }
    /// Eén coöperatieve wachtstap voor protocollen met een eigen netwerk-state-machine.
    /// Heartbeats/snapshots gaan door; callbacks blijven geordend in de bestaande inbox.
    pub async fn idle(&mut self) -> Result {
        if let Some(frame) = self.pump().await?
            && frame.kind == Kind::Request
        {
            json::push(&mut self.inbox, frame, MAX_INBOX)?;
        }
        Ok(())
    }
    /// Externe HTTP wacht terwijl heartbeats en lokale snapshots blijven doorlopen.
    pub async fn http(&mut self, request: HttpRequest) -> Result<HttpResponse> {
        let deadline = self.now().saturating_add(request.timeout_ms);
        self.transport.start_http(request)?;
        loop {
            if let Some(result) = self.transport.poll_http() {
                return result;
            }
            if self.now() >= deadline {
                return Err(Error::Timeout);
            }
            if let Some(frame) = self.pump().await?
                && frame.kind == Kind::Request
            {
                json::push(&mut self.inbox, frame, MAX_INBOX)?;
            }
        }
    }
    /// UDP-ontdekking houdt dezelfde hartslag en callbackwachtrij als HTTP.
    pub async fn datagrams(&mut self, request: DatagramRequest) -> Result<Vec<Datagram>> {
        let deadline = self
            .now()
            .saturating_add(request.timeout_ms)
            .saturating_add(1000);
        self.transport.start_datagrams(request)?;
        loop {
            if let Some(result) = self.transport.poll_datagrams() {
                return result;
            }
            if self.now() >= deadline {
                return Err(Error::Timeout);
            }
            if let Some(frame) = self.pump().await?
                && frame.kind == Kind::Request
            {
                json::push(&mut self.inbox, frame, MAX_INBOX)?;
            }
        }
    }
    /// TCP wacht zonder heartbeats of controllerupdates tegen te houden.
    pub async fn tcp(&mut self, request: TcpRequest) -> Result<Vec<u8>> {
        let deadline = self.now().saturating_add(request.timeout_ms);
        self.transport.start_tcp(request)?;
        loop {
            if let Some(result) = self.transport.poll_tcp() {
                return result;
            }
            if self.now() >= deadline {
                return Err(Error::Timeout);
            }
            if let Some(frame) = self.pump().await?
                && frame.kind == Kind::Request
            {
                json::push(&mut self.inbox, frame, MAX_INBOX)?;
            }
        }
    }
    /// Een onbereikbaar apparaat houdt zijn laatst bekende metingen.
    pub async fn unavailable(&mut self, id: &str, message: &str) -> Result {
        self.call(
            "device.set",
            &json::fields(&[
                ("deviceId", json::string(id)?),
                ("field", json::string("unavailableMessage")?),
                ("value", json::string(message)?),
            ])?,
        )
        .await?;
        self.available(id, false).await
    }
    /// Device-maps worden door Stulp gemerged, daarna teruggeduwd en pas dan bevestigd.
    pub async fn merge(&mut self, id: &str, field: &str, patch: Value) -> Result {
        self.call(
            "device.merge",
            &json::fields(&[
                ("deviceId", json::string(id)?),
                ("field", json::string(field)?),
                ("patch", patch),
            ])?,
        )
        .await?;
        Ok(())
    }
    /// Een waarneming vervangt geen duurzame storewaarde.
    pub async fn values(&mut self, id: &str, patch: Value) -> Result {
        self.merge(id, "state", patch).await
    }
    /// Bewaart de pluginwaarheid vóórdat daaruit live-waarden worden gepubliceerd.
    pub async fn store(&mut self, id: &str, patch: Value) -> Result {
        self.merge(id, "store", patch).await
    }
    /// Bereikbaarheid loopt over hetzelfde geordende kanaal.
    pub async fn available(&mut self, id: &str, available: bool) -> Result {
        self.call(
            "device.set",
            &json::fields(&[
                ("deviceId", json::string(id)?),
                ("field", json::string("available")?),
                ("value", Value::Bool(available)),
            ])?,
        )
        .await?;
        Ok(())
    }
    /// Instellingen schrijven en het authoritative snapshot afwachten.
    pub async fn setting(&mut self, key: &str, value: Value) -> Result {
        self.call(
            "setting.set",
            &json::fields(&[("key", json::string(key)?), ("value", value)])?,
        )
        .await?;
        Ok(())
    }
    /// Ontvangt de eerste authoritative snapshot voordat callbacks beginnen.
    pub async fn hello(&mut self, id: &str) -> Result {
        let welcome = self
            .call(
                "hello",
                &json::fields(&[("protocol", Value::uint(1)), ("appId", json::string(id)?)])?,
            )
            .await?;
        self.state.load(id, welcome)
    }
    /// Geeft de adapter terug na het beëindigen van deze client.
    pub fn into_transport(self) -> T {
        self.transport
    }

    /// Handshake, geordende callbacks en timers blijven bij één eigenaar.
    pub async fn serve(mut self, mut plugin: impl Plugin) -> Result {
        let manifest = json::parse(plugin.manifest()).map_err(stulp_core::Error::from)?;
        let id = json::text(&manifest, "id");
        self.hello(id).await?;
        self.serve_initialized(&mut plugin).await
    }
    /// Hervat een reeds geauthenticeerde snapshot zonder een tweede hello te sturen.
    /// De eigenaar kan zo een bootstrap uitvoeren en daarna dezelfde callbacklus gebruiken.
    pub async fn serve_initialized(&mut self, plugin: &mut impl Plugin) -> Result {
        plugin.serve(self).await
    }
    /// Default ordered callback loop, also used inside a specialized worker pool.
    pub async fn serve_serial(&mut self, plugin: &mut impl Plugin) -> Result {
        let manifest = json::parse(plugin.manifest()).map_err(stulp_core::Error::from)?;
        if json::text(&manifest, "id").is_empty()
            || json::text(&manifest, "id") != json::text(self.state.root(), "appId")
        {
            return Err(Error::Invalid(
                "plugin does not match initialized controller snapshot",
            ));
        }
        loop {
            let frame = if self.inbox.is_empty() {
                self.pump().await?
            } else {
                Some(self.inbox.remove(0))
            };
            let Some(frame) = frame else {
                plugin.tick(self).await?;
                continue;
            };
            if frame.kind != Kind::Request {
                continue;
            }
            let params = json::get(&frame.value, "p").unwrap_or(&Value::Null);
            let callback0 = self.now();
            let handled =
                if frame.method() == "ui.asset" && !valid_asset(json::text(params, "path")) {
                    Err(Error::Invalid("invalid app asset path"))
                } else if frame.method() == "device.settings" {
                    let id = json::text(params, "deviceId");
                    let patch = json::get(params, "settings").unwrap_or(&Value::Null);
                    if patch.as_object().is_none() || self.state.device(id).is_err() {
                        Err(Error::Invalid("invalid device settings patch"))
                    } else {
                        plugin.settings(self, id, patch).await
                    }
                } else {
                    plugin.handle(self, frame.method(), params).await
                };
            let callback_ms = self.now().saturating_sub(callback0);
            if callback_ms >= SLOW_CALLBACK_MS {
                // De meetlat van een plugin op de node: een callback die zo lang
                // duurt, houdt op een gedeeld core de buren even stil.
                let mut line = String::new();
                if line.try_reserve(128).is_ok() {
                    use core::fmt::Write as _;
                    let _ = write!(
                        line,
                        "STULP_SLOW_CALLBACK method={} ms={callback_ms}",
                        frame.method()
                    );
                    self.log("warn", &line)?;
                }
            }
            let response = match handled {
                Ok(value) => Frame::response(frame.id, Ok(value))?,
                Err(error) => Frame::response(frame.id, Err(&message(&error)?))?,
            };
            self.transport.send(&response).await?;
        }
    }
}

#[cfg(test)]
mod heartbeat_tests;
/// Faalbaar kopiëren houdt allocatiefouten in hetzelfde Result-type.
pub fn clone(value: &Value) -> Result<Value> {
    value.try_clone().map_err(|e| Error::Core(e.into()))
}
/// Begrensde fouttekst voor de wire, met behoud van de oorspronkelijke reden.
pub fn message(error: &Error) -> Result<String> {
    use fmt::Write;
    let mut text = String::new();
    let bound = match error {
        Error::Remote(s) => s.len(),
        _ => 512,
    };
    text.try_reserve(bound)
        .map_err(|_| Error::Core(stulp_core::Error::Memory))?;
    write!(text, "{error}").map_err(|_| Error::Invalid("error formatting failed"))?;
    Ok(text)
}

/// Go's []byte JSON-vorm gebruikt standaard base64 met padding, niet base64url.
pub fn asset(bytes: &[u8]) -> Result<Value> {
    let url = stulp_protocol::token::base64(bytes)?;
    let mut encoded = String::new();
    encoded
        .try_reserve(url.len() + 3)
        .map_err(|_| stulp_core::Error::Memory)?;
    for c in url.chars() {
        encoded.push(match c {
            '-' => '+',
            '_' => '/',
            _ => c,
        });
    }
    while !encoded.len().is_multiple_of(4) {
        encoded.push('=');
    }
    Ok(json::fields(&[
        ("found", Value::Bool(true)),
        ("data", json::string(&encoded)?),
    ])?)
}

/// Een canonieke meting voor de bestaande app-UI.
pub fn measure(value: f64, units: &str) -> Result<Value> {
    Ok(json::fields(&[
        ("$measure", Value::Number(json::Number::Float(value))),
        ("units", json::string(units)?),
    ])?)
}
/// Een querywaarde wordt UTF-8-bytegewijs geëncodeerd, inclusief ampersands en procenttekens.
pub fn query(value: &str) -> Result<String> {
    use core::fmt::Write;
    let mut out = String::new();
    out.try_reserve(value.len().checked_mul(3).ok_or(stulp_core::Error::Full)?)
        .map_err(|_| stulp_core::Error::Memory)?;
    for b in value.bytes() {
        if b.is_ascii_alphanumeric() || b"-._~".contains(&b) {
            out.push(char::from(b));
        } else {
            write!(&mut out, "%{b:02X}").map_err(|_| Error::Invalid("query encoding failed"))?;
        }
    }
    Ok(out)
}
/// Declaraties voor plugins die elke eigen manifestkaart daadwerkelijk afhandelen.
pub fn registrations(manifest: &Value) -> Result<Value> {
    let mut drivers = Vec::new();
    for driver in json::array(manifest, "drivers") {
        json::push(&mut drivers, json::string(json::text(driver, "id"))?, 128)?;
    }
    let mut cards = Vec::new();
    if let Some(flow) = json::get(manifest, "flow") {
        for (list, kind) in [
            ("triggers", "trigger"),
            ("conditions", "condition"),
            ("actions", "action"),
        ] {
            for card in json::array(flow, list) {
                json::push(
                    &mut cards,
                    json::fields(&[
                        ("id", json::string(json::text(card, "id"))?),
                        ("type", json::string(kind)?),
                        ("runListener", Value::Bool(true)),
                        ("autocomplete", Value::Array(Vec::new())),
                    ])?,
                    512,
                )?;
            }
        }
    }
    Ok(json::fields(&[
        ("drivers", Value::Array(drivers)),
        ("flows", Value::Array(cards)),
    ])?)
}

/// Het attach-manifest bevat alleen de paden van de ingebedde interface.
pub fn manifest(plugin: &impl Plugin) -> Result<String> {
    let mut manifest = json::parse(plugin.manifest()).map_err(stulp_core::Error::from)?;
    let mut assets = Vec::new();
    for path in plugin.assets() {
        if !valid_asset(path) {
            return Err(Error::Invalid("invalid embedded asset path"));
        }
        json::push(&mut assets, json::string(path)?, 4096)?;
    }
    let mut ui = clone(
        json::get(&manifest, "ui")
            .filter(|v| v.as_object().is_some())
            .unwrap_or(&json::object()),
    )?;
    json::set(&mut ui, "assets", Value::Array(assets))?;
    json::set(&mut manifest, "ui", ui)?;
    json::to_string(&manifest).map_err(|e| Error::Core(e.into()))
}
/// Paden blijven binnen de ingebedde app-UI, zonder normalisatie van parent-segmenten.
pub fn valid_asset(path: &str) -> bool {
    !path.is_empty()
        && !path.contains(['\\', '\0'])
        && path.split('/').all(|p| !matches!(p, "" | "." | ".."))
}
