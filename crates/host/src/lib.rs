//! Hostadapter voor het platformvrije Stulp.
#![forbid(unsafe_code)]
pub mod apps;
pub mod archive;
pub mod cli;
pub mod files;
mod flows;
pub mod logging;
pub mod server;

/// De host levert alleen tijd en entropy; de mutatielogica zit in de kern.
pub struct Environment;

impl stulp_web::Environment for Environment {
    fn id(&mut self) -> stulp_core::Result<String> {
        use std::fmt::Write;
        let bytes =
            hostnet::entropy().map_err(|_| stulp_core::Error::Invalid("OS entropy unavailable"))?;
        let mut id = String::new();
        id.try_reserve_exact(36)
            .map_err(|_| stulp_core::Error::Memory)?;
        for (i, b) in bytes.iter().take(16).enumerate() {
            if matches!(i, 4 | 6 | 8 | 10) {
                id.push('-');
            }
            let b = match i {
                6 => (b & 15) | 64,
                8 => (b & 63) | 128,
                _ => *b,
            };
            write!(id, "{b:02x}").map_err(|_| stulp_core::Error::Full)?;
        }
        Ok(id)
    }

    fn now(&self) -> stulp_core::Result<String> {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(|_| stulp_core::Error::Invalid("clock predates epoch"))?
            .as_nanos();
        stulp_core::json::timestamp(u64::try_from(nanos).map_err(|_| stulp_core::Error::Full)?)
    }
}

mod app_ui;

mod pairing;

mod media;

mod processes;

mod scenes;

mod catalog;

mod calendar;
mod timezone;

mod mcp;
