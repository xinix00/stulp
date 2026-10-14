//! Wat een host-proces van het OS nodig heeft naast sockets: een executor van
//! één thread, de wandklok en entropie.
//!
//! Tot 05-10 kwam dit uit Hop's `hostnet` (v3.0.0-alpha.10), dat Lean v3.1.1
//! meetrok naast de Lean van Stulp. De HTTP-kant staat nu in Lean zelf
//! (`leanhttp::host`, `leanhttps::WebDial`); deze drie functies zijn het
//! platformdeel dat overbleef.
use std::{
    future::Future,
    io::{self, Read},
    pin::pin,
    task::{Context, Poll, Waker},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

/// Hoe lang een onverwachte `Pending` de thread laat rusten voor de volgende poll.
const PENDING_NAP: Duration = Duration::from_millis(1);

/// Draait `future` op deze thread tot hij klaar is.
///
/// Blokkerende sockets zijn in hun poll meteen `Ready`; een future die toch
/// `Pending` geeft, krijgt een korte slaap en een nieuwe poll. Een spin met
/// rem, geen reactor: de eigenaar van een verbinding is één thread.
pub fn block_on<F: Future>(future: F) -> F::Output {
    let mut future = pin!(future);
    // Niemand om te wekken: deze thread pollt zelf.
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Poll::Ready(v) = future.as_mut().poll(&mut cx) {
            return v;
        }
        std::thread::sleep(PENDING_NAP);
    }
}

/// Nu in Unix-seconden volgens de systeemklok; `None` vóór 1970.
pub fn unix_seconds() -> Option<u64> {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .map(|d| d.as_secs())
}

/// `N` bytes uit de entropiebron van het OS (`/dev/urandom`).
///
/// Linux en macOS hebben hem allebei; zonder is er geen sleutel en geen
/// handshake, en dat is een fout, geen zwakke sleutel.
pub fn entropy<const N: usize>() -> io::Result<[u8; N]> {
    let mut out = [0u8; N];
    std::fs::File::open("/dev/urandom")?.read_exact(&mut out)?;
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn block_on_polls_again_after_pending() {
        let mut polls = 0;
        let value = block_on(core::future::poll_fn(|_| {
            polls += 1;
            if polls < 3 {
                Poll::Pending
            } else {
                Poll::Ready(polls)
            }
        }));
        assert_eq!(value, 3);
    }

    #[test]
    #[cfg_attr(miri, ignore = "Miri isoleert het bestandssysteem")]
    fn entropy_fills_and_differs() -> io::Result<()> {
        let a = entropy::<32>()?;
        let b = entropy::<32>()?;
        assert_ne!(a, b);
        Ok(())
    }
}
