//! Slot clock and entropy stay with the controller owner.
use alloc::string::String;
use applib::{
    App,
    rand::{Origin, Rng},
};
use stulp_core::{Error, Result, json};
/// Environment for UUIDs, HMAC challenges and wall-clock timestamps.
pub struct Environment {
    app: &'static App,
    random: Rng,
}
impl Environment {
    /// Older kernels may inject a fresh 32-byte base64 seed in the job environment.
    /// Never silently use an unseeded generator for controller authentication.
    pub fn open(app: &'static App) -> Result<Self> {
        let mut random = Rng::open(app);
        if let Some(seed) = app.env("STULP_ENTROPY_SEED").filter(|s| !s.is_empty()) {
            let mut bytes = stulp_protocol::token::decode(seed)?;
            if bytes.len() != 32 {
                bytes.fill(0);
                return Err(Error::Invalid("entropy seed must contain 32 bytes"));
            }
            random.stir(&bytes);
            bytes.fill(0);
        } else if random.origin() == Origin::None {
            return Err(Error::Invalid(
                "kernel entropy unavailable; provide STULP_ENTROPY_SEED or update the kernel",
            ));
        }
        Ok(Self { app, random })
    }
    /// Draw fresh bytes; callers must not log or persist challenges as secrets.
    pub fn random(&mut self) -> [u8; 32] {
        self.random.array()
    }
    /// Unix milliseconds for transient statistics.
    pub fn unix_ms(&self) -> Result<u64> {
        self.app
            .wall_ns()
            .map(|ns| ns / 1_000_000)
            .ok_or(Error::Invalid("wall clock not synchronized"))
    }
}
impl stulp_web::Environment for Environment {
    fn id(&mut self) -> Result<String> {
        use core::fmt::Write;
        let bytes = self.random();
        let mut id = String::new();
        id.try_reserve_exact(36).map_err(|_| Error::Memory)?;
        for (i, byte) in bytes.iter().take(16).enumerate() {
            if matches!(i, 4 | 6 | 8 | 10) {
                id.push('-');
            }
            let byte = match i {
                6 => (byte & 15) | 64,
                8 => (byte & 63) | 128,
                _ => *byte,
            };
            write!(id, "{byte:02x}").map_err(|_| Error::Full)?;
        }
        Ok(id)
    }
    fn now(&self) -> Result<String> {
        json::timestamp(
            self.app
                .wall_ns()
                .ok_or(Error::Invalid("wall clock not synchronized"))?,
        )
    }
}
