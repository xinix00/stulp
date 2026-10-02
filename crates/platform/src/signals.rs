//! Alleen de beëindigingsvlag is gedeeld; de controller ruimt zijn eigen kinderen op.
use std::{
    io,
    sync::atomic::{AtomicBool, Ordering},
};
static STOP: AtomicBool = AtomicBool::new(false);
extern "C" fn stopped(_: libc::c_int) {
    STOP.store(true, Ordering::Relaxed);
}
/// Een ISR-veilige vlag, zonder opslag of allocatie in de signaalhandler.
pub fn requested() -> bool {
    STOP.load(Ordering::Relaxed)
}
/// De hoofdtaak bezit en herstelt de vorige handlers bij afsluiten.
pub struct Signals {
    previous: [libc::sigaction; 2],
}
impl Signals {
    /// Installeer eenmaal bij boot, vóór de HTTP-werkers starten.
    pub fn install() -> io::Result<Self> {
        STOP.store(false, Ordering::Relaxed);
        // SAFETY: sigaction is een C-record; nul initialiseert alle velden, waarna
        // handler en sigset expliciet worden ingevuld vóór registratie.
        let mut action: libc::sigaction = unsafe { std::mem::zeroed() };
        // SAFETY: Dezelfde C-layout is schrijfbaar voor de twee oude OS-records.
        let mut previous: [libc::sigaction; 2] = unsafe { std::mem::zeroed() };
        action.sa_sigaction = stopped as *const () as usize;
        // SAFETY: action.sa_mask is een geldig, exclusief sigset_t-record.
        if unsafe { libc::sigemptyset(&raw mut action.sa_mask) } != 0 {
            return Err(io::Error::last_os_error());
        }
        for (index, signal) in [libc::SIGINT, libc::SIGTERM].into_iter().enumerate() {
            // SAFETY: De handler doet alleen een AtomicBool-store; de records leven
            // gedurende de call en sigaction kopieert ze naar de kernel.
            if unsafe { libc::sigaction(signal, &raw const action, &raw mut previous[index]) } != 0
            {
                if index == 1 {
                    // SAFETY: Het eerste oude record kwam uit een geslaagde sigaction.
                    unsafe {
                        libc::sigaction(libc::SIGINT, &raw const previous[0], std::ptr::null_mut())
                    };
                }
                return Err(io::Error::last_os_error());
            }
        }
        Ok(Self { previous })
    }
}
impl Drop for Signals {
    fn drop(&mut self) {
        for (signal, previous) in [libc::SIGINT, libc::SIGTERM]
            .into_iter()
            .zip(&self.previous)
        {
            // SAFETY: previous is het door sigaction teruggegeven OS-record; Drop loopt eenmaal.
            unsafe { libc::sigaction(signal, previous, std::ptr::null_mut()) };
        }
    }
}
