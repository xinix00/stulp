//! Eén routeherstelvenster voor de controller, zonder apparaten bij boot grijs te maken.
use stulp_sdk::Error;
pub(crate) fn missing(error: &Error) -> bool {
    match error {
        Error::Transport(s) | Error::Invalid(s) => s.contains("no IPv6 route"),
        Error::Remote(s) => s.contains("no IPv6 route"),
        _ => false,
    }
}
#[derive(Default)]
pub(crate) struct Recovery {
    since: Option<u64>,
    pub(crate) next: u64,
    backoff: u64,
}
impl Recovery {
    pub(crate) fn failed(&mut self, now: u64, entropy: u8) -> bool {
        let first = self.since.is_none();
        self.since.get_or_insert(now);
        let delay = self.backoff.max(1000);
        let jitter = delay * u64::from(entropy) * 2 / (255 * 5);
        self.next = now.saturating_add(delay - delay / 5 + jitter);
        self.backoff = (delay * 2).min(5000);
        first
    }
    pub(crate) fn loud(&self, now: u64) -> bool {
        self.since
            .is_some_and(|since| now.saturating_sub(since) > 120_000)
    }
    pub(crate) fn recovered(&mut self) -> bool {
        let recovered = self.since.is_some();
        *self = Self::default();
        recovered
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn shared_backoff_keeps_two_minutes_of_grace_and_resets_on_recovery() {
        let mut r = Recovery::default();
        assert!(r.failed(0, 0));
        assert_eq!(r.next, 800);
        assert!(!r.failed(800, 255));
        assert_eq!(r.next, 3200);
        assert!(!r.failed(3200, 255));
        assert_eq!(r.next, 8000);
        assert!(!r.failed(8000, 255));
        assert_eq!(r.next, 14000);
        assert!(!r.loud(120_000));
        assert!(r.loud(120_001));
        assert!(r.recovered());
        assert_eq!(r.next, 0);
        assert!(!r.loud(130_000));
        assert!(!r.recovered());
        assert!(missing(&Error::Transport("no IPv6 route")));
        assert!(!missing(&Error::Timeout));
    }
}
