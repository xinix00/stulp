//! De meetlat van een slot, door de app zelf op de console: idle en wekken
//! (de tellers die de slaper van applib publiceert), zendring-drops en
//! -wachten van de SDK, de TCP-tellers van Lean en de polls van de executor.
//! Om de [`EVERY`] één regel `STULP_LOAD`, met verschillen sinds de vorige.
//! Zonder deze regel is "traag" op de LicheeRV niet toe te schrijven aan
//! rekenen (idle laag), pakketverlies (retrans, drops) of dutten (wakes).
use alloc::string::String;
use applib::{App, EXEC};
use core::{fmt::Write, sync::atomic::Ordering::Relaxed, time::Duration};
use stulp_sdk::{Error, Result};

/// Het ritme van de regel.
pub const EVERY: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Default)]
struct Snapshot {
    at_ns: u64,
    idle: u64,
    wakes: u64,
    tx_drops: u64,
    tx_waits: u64,
    retrans: u64,
    fast: u64,
    zero_win: u64,
    rx_refused: u64,
    budget_refused: u64,
    segs_out: u64,
    segs_in: u64,
    polls: u64,
    rounds: u64,
}

/// Houdt de vorige stand vast; één per slot.
#[derive(Default)]
pub struct Meter {
    last: Option<Snapshot>,
}

impl Meter {
    /// Een lege meter.
    #[must_use]
    pub const fn new() -> Self {
        Self { last: None }
    }
    fn snapshot(app: &App) -> Snapshot {
        let stats = applib::appnet::net().and_then(|n| n.stats().ok());
        let exec = EXEC.get();
        let u = |v: usize| u64::try_from(v).unwrap_or(u64::MAX);
        Snapshot {
            at_ns: applib::clock::now_ns(),
            idle: app.idle_ticks(),
            wakes: app.wakes(),
            tx_drops: applib::net::TX_DROPS.load(Relaxed),
            tx_waits: applib::net::TX_WAITS.load(Relaxed),
            retrans: stats.as_ref().map_or(0, |s| u(s.tcp_retransmits)),
            fast: stats.as_ref().map_or(0, |s| u(s.tcp_fast_retransmits)),
            zero_win: stats.as_ref().map_or(0, |s| u(s.tcp_zero_windows)),
            rx_refused: stats.as_ref().map_or(0, |s| u(s.tcp_rx_grow_refused)),
            budget_refused: stats.as_ref().map_or(0, |s| u(s.refused_no_budget)),
            segs_out: stats.as_ref().map_or(0, |s| u(s.tcp_segs_out)),
            segs_in: stats.as_ref().map_or(0, |s| u(s.tcp_segs_in)),
            polls: exec.stats.polls.load(Relaxed),
            rounds: exec.stats.rounds.load(Relaxed),
        }
    }
    /// De regel sinds de vorige aanroep, of niets bij de eerste.
    pub fn line(&mut self, app: &App) -> Result<Option<String>> {
        let now = Self::snapshot(app);
        let Some(prev) = self.last.replace(now) else {
            return Ok(None);
        };
        let dt_ns = now.at_ns.saturating_sub(prev.at_ns);
        if dt_ns == 0 {
            return Ok(None);
        }
        let ticks = u128::from(applib::clock::hz()) * u128::from(dt_ns) / 1_000_000_000;
        let idle = if ticks == 0 {
            0
        } else {
            (u128::from(now.idle.wrapping_sub(prev.idle)) * 100 / ticks).min(100)
        };
        let per_s = |d: u64| u128::from(d) * 1_000_000_000 / u128::from(dt_ns);
        let d = |a: u64, b: u64| a.wrapping_sub(b);
        let mut s = String::new();
        s.try_reserve(240).map_err(|_| stulp_core::Error::Memory)?;
        write!(
            s,
            "STULP_LOAD idle={idle}% wakes={}/s polls={}/s rounds={}/s tx_drops={} tx_waits={} tcp_retrans={} fast={} zero_win={} rx_refused={} budget_refused={} segs_out={} segs_in={}",
            per_s(d(now.wakes, prev.wakes)),
            per_s(d(now.polls, prev.polls)),
            per_s(d(now.rounds, prev.rounds)),
            d(now.tx_drops, prev.tx_drops),
            d(now.tx_waits, prev.tx_waits),
            d(now.retrans, prev.retrans),
            d(now.fast, prev.fast),
            d(now.zero_win, prev.zero_win),
            d(now.rx_refused, prev.rx_refused),
            d(now.budget_refused, prev.budget_refused),
            d(now.segs_out, prev.segs_out),
            d(now.segs_in, prev.segs_in),
        )
        .map_err(|_| Error::Invalid("meter formatting"))?;
        Ok(Some(s))
    }
}

/// Een eigen taak die om de [`EVERY`] de regel logt: voor een slot zonder
/// eigen lus met klok (de plugin-bundel en de losse plugin).
pub fn spawn(app: &'static App) -> Result {
    EXEC.get()
        .spawn(async move {
            let mut meter = Meter::new();
            loop {
                EXEC.get().after(EVERY).await;
                match meter.line(app) {
                    Ok(Some(line)) => app.log(format_args!("{line}")),
                    Ok(None) => (),
                    Err(e) => app.log(format_args!("STULP_LOAD meter failed: {e}")),
                }
            }
        })
        .map_err(|_| Error::Transport("meter task unavailable"))
}
