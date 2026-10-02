//! Expliciete proceslevensloop, generaties en begrensde herstartvertraging.
use stulp_core::{Error, Result};

/// Een extern gestarte app wordt nooit door Stulp opnieuw gestart.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// Het proces is eigendom van Stulp.
    Spawned,
    /// Een container of Hop bezit het proces.
    Attached,
}
/// Waarneembare status voor Manage.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    /// Bewust uitgeschakeld.
    Stopped,
    /// Externe app mag zich opnieuw aanmelden.
    Waiting,
    /// Handshake of initcallbacks zijn bezig.
    Starting,
    /// Alle initcallbacks zijn bevestigd.
    Running,
    /// Eigen proces wacht op zijn backofftermijn.
    Crashed,
}

/// Oude exitmeldingen mogen een nieuwe verbinding niet omleggen.
pub struct Supervisor {
    mode: Mode,
    state: State,
    generation: u64,
    retries: u32,
    retry_at: u64,
}
impl Supervisor {
    /// Boot zonder lopend proces.
    pub const fn new(mode: Mode) -> Self {
        Self {
            mode,
            state: State::Stopped,
            generation: 0,
            retries: 0,
            retry_at: 0,
        }
    }
    /// Huidige status.
    pub fn state(&self) -> State {
        self.state
    }
    /// Aantal mislukte starts achter elkaar.
    pub fn retries(&self) -> u32 {
        self.retries
    }
    /// Begin een nieuwe levensduur.
    pub fn start(&mut self) -> Result<u64> {
        if matches!(self.state, State::Starting | State::Running) {
            return Err(Error::Conflict("app is already running"));
        }
        self.generation = self.generation.checked_add(1).ok_or(Error::Full)?;
        self.state = State::Starting;
        Ok(self.generation)
    }
    /// Alleen de actuele start mag running publiceren.
    pub fn ready(&mut self, generation: u64) -> Result {
        if generation != self.generation || self.state != State::Starting {
            return Err(Error::Changed);
        }
        self.state = State::Running;
        self.retries = 0;
        Ok(())
    }
    /// Registreert één exit; dubbele of oude meldingen hebben geen effect.
    pub fn exited(&mut self, generation: u64, now: u64) -> bool {
        if generation != self.generation || !matches!(self.state, State::Starting | State::Running)
        {
            return false;
        }
        match self.mode {
            Mode::Attached => self.state = State::Waiting,
            Mode::Spawned => {
                let delay = 1000_u64
                    .checked_shl(self.retries.min(5))
                    .unwrap_or(30_000)
                    .min(30_000);
                self.retries = self.retries.saturating_add(1);
                self.retry_at = now.saturating_add(delay);
                self.state = State::Crashed;
            }
        }
        true
    }
    /// Monotone geplande herstart; nul betekent geen actieve backoff.
    pub fn retry_at(&self) -> u64 {
        self.retry_at
    }

    /// Een timer is een verzoek om werk, geen automatische spawn vanuit een callback.
    pub fn retry_due(&self, now: u64) -> bool {
        self.mode == Mode::Spawned && self.state == State::Crashed && now >= self.retry_at
    }
    /// Uitschakelen annuleert iedere geplande herstart.
    pub fn stop(&mut self) {
        self.state = State::Stopped;
        self.retry_at = 0;
    }
}
