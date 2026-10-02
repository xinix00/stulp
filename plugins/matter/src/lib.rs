//! Matter-protocollen van Stulp: begrensde codecs zonder platform-I/O.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
#[cfg(test)]
extern crate std;
mod app;
mod engine;
mod maintenance;
mod mesh;
mod route;
mod ui;
pub use app::Matter;
pub mod attestation;
pub mod attributes;
pub mod capabilities;
pub mod case;
pub mod ccm;
pub mod certificate;
pub mod commands;
pub mod commissioning;
mod der;
pub mod devices;
pub mod diagnostics;
pub mod discovery;
pub mod fabric;
pub mod im;
pub mod interaction;
pub mod inventory;
mod key_id;
pub mod message;
pub mod model;
pub mod mrp;
pub mod network;
pub mod onboarding;
pub mod pase;
pub mod reports;
pub mod settings;
pub mod spake;
pub mod tlv;
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
const MAX: usize = 65535;
fn append(out: &mut Vec<u8>, bytes: &[u8]) -> Result {
    if bytes.len() > MAX.saturating_sub(out.len()) {
        return Err(Error::Invalid("Matter buffer exceeds 65535 bytes"));
    }
    out.try_reserve(bytes.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    out.extend_from_slice(bytes);
    Ok(())
}
fn copy(bytes: &[u8]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    append(&mut out, bytes)?;
    Ok(out)
}
