//! Uitgaand HTTP(S) van de host: Lean's `TcpDial` (DNS, hoogstens zestien
//! adressen, sockettermijnen) onder `WebDial` (keten tegen de Mozilla-wortels)
//! of de apparaatdialer, met de klok en entropie van het OS.
//!
//! Wat hier blijft is alleen de naad: één totale grens per aanroep op de kale
//! verbinding, en de twee platformfuncties voor de handshake.
use std::time::{Duration, Instant};

use leanhttp::host::{DialError, Reactor, TcpConn, TcpDial};
use leanhttp::{IoError, Target};
use leanhttps::WebDial;
use leantls::Entropy;
use stulp_device_tls::DeviceDial;

pub use leanhttp::host::Blocking;

/// De kale TCP-dial van de host, met één totale grens over verbinden, TLS,
/// kop, body en elke redirect.
///
/// Een fasetermijn begint bij elke fase opnieuw; een server die elke paar
/// seconden één byte druppelt, houdt een aanroep dan eindeloos open. De grens
/// gaat als `TcpConn::set_limit` mee met elke verbinding, en geen adres krijgt
/// een verbindtermijn voorbij de grens.
pub struct Tcp<R: Reactor + Clone = Blocking> {
    dial: TcpDial<R>,
    connect: Duration,
    until: Option<Instant>,
}

impl<R: Reactor + Clone> Tcp<R> {
    /// `connect` per adres, `idle` per lees of schrijf zolang leanhttp geen
    /// termijn zet, en `until` als totale grens (`None`: geen).
    pub fn new(
        reactor: R,
        connect: Duration,
        idle: Option<Duration>,
        until: Option<Instant>,
    ) -> Self {
        let mut dial = TcpDial::new(reactor);
        dial.connect = connect;
        dial.idle = idle;
        Self {
            dial,
            connect,
            until,
        }
    }

    /// Waarom de laatste dial geen verbinding kreeg (DNS of TCP), als dat zo was.
    pub fn last_error(&self) -> Option<&DialError> {
        self.dial.last_error()
    }
}

impl<R: Reactor + Clone> leanhttp::Dial for Tcp<R> {
    type Conn = TcpConn<R>;

    async fn dial(&mut self, target: Target<'_>) -> leanhttp::Result<TcpConn<R>> {
        if let Some(until) = self.until {
            let left = until.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Err(leanhttp::Error::Io(IoError::TimedOut));
            }
            self.dial.connect = self.connect.min(left);
        }
        let mut conn = self.dial.dial(target).await?;
        conn.set_limit(self.until);
        Ok(conn)
    }
}

/// De systeemklok voor de ketentoets.
fn clock() -> Option<u64> {
    stulp_platform::host::unix_seconds()
}

/// Verse entropie per handshake; zonder bron geen handshake.
fn entropy() -> Option<Entropy> {
    stulp_platform::host::entropy().ok().map(Entropy::new)
}

/// De webdialer van de host: zie [`web`].
pub type Web<D> = WebDial<D, fn() -> Option<u64>, fn() -> Option<Entropy>>;

/// `http://` kaal over `inner`, `https://` met ketenverificatie tegen de
/// Mozilla-wortels op de systeemklok.
pub fn web<D: leanhttp::Dial>(inner: D) -> Web<D>
where
    D::Conn: Unpin,
{
    let clock: fn() -> Option<u64> = clock;
    let entropy: fn() -> Option<Entropy> = entropy;
    WebDial::new(inner, leantls::MOZILLA_ROOTS, clock, entropy)
}

/// De apparaatdialer van de host: zie [`device`].
pub type Device<D> = DeviceDial<D, fn() -> Option<Entropy>>;

/// TLS naar een lokaal apparaat met een eigen certificaat over `inner`
/// (`stulp_device_tls`); alleen expliciete verzoeken komen hier.
pub fn device<D: leanhttp::Dial>(inner: D) -> Device<D>
where
    D::Conn: Unpin,
{
    let entropy: fn() -> Option<Entropy> = entropy;
    DeviceDial::new(inner, entropy)
}
