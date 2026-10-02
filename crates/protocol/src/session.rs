//! Begrensde RPC-correlatie met deadlines en expliciete eigendomsoverdracht.
use stulp_core::{Error, Result};

/// Verzoeken in vlucht per app; volle capaciteit geeft backpressure aan de eigenaar.
pub const MAX_PENDING: usize = 64;

#[derive(Clone, Copy)]
struct Pending {
    id: u64,
    deadline: u64,
    owner: u64,
}

/// De controller-taak bezit één sessie per appverbinding.
pub struct Session {
    pending: [Option<Pending>; MAX_PENDING],
    next: u64,
}

impl Default for Session {
    fn default() -> Self {
        Self::new()
    }
}

impl Session {
    /// Geen runtime-allocaties voor verzoekadministratie.
    pub const fn new() -> Self {
        Self {
            pending: [None; MAX_PENDING],
            next: 0,
        }
    }

    /// Registreert een verzoek vóór verzending; tijden zijn monotone milliseconden.
    pub fn begin(&mut self, owner: u64, now: u64, timeout: u64) -> Result<u64> {
        let next = self.next.checked_add(1).ok_or(Error::Full)?;
        let deadline = now.checked_add(timeout).ok_or(Error::Full)?;
        let slot = self
            .pending
            .iter_mut()
            .find(|p| p.is_none())
            .ok_or(Error::Full)?;
        *slot = Some(Pending {
            id: next,
            deadline,
            owner,
        });
        self.next = next;
        Ok(next)
    }

    /// Neemt één resultaat over; een laat of dubbel antwoord heeft geen eigenaar meer.
    pub fn complete(&mut self, id: u64) -> Option<u64> {
        self.pending
            .iter_mut()
            .find(|p| p.is_some_and(|p| p.id == id))
            .and_then(Option::take)
            .map(|p| p.owner)
    }

    /// Haalt één verlopen verzoek op; de aanroeper bericht die eigenaar een timeout.
    pub fn expire(&mut self, now: u64) -> Option<(u64, u64)> {
        self.pending
            .iter_mut()
            .find(|p| p.is_some_and(|p| p.deadline <= now))
            .and_then(Option::take)
            .map(|p| (p.id, p.owner))
    }

    /// Disconnect neemt alle resterende eigenaren terug zonder nog callbacks uit te voeren.
    pub fn disconnect(&mut self) -> impl Iterator<Item = u64> + '_ {
        self.pending
            .iter_mut()
            .filter_map(|p| p.take().map(|p| p.owner))
    }
}
