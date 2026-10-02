//! De platformvrije eigenaar van Stulps configuratie en automations.
#![no_std]
#![forbid(unsafe_code)]
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used, clippy::panic))]

extern crate alloc;

pub mod capability;
pub mod display;
pub mod document;
pub mod flow;
pub mod json;
mod json_bounded;
pub mod manifest;
pub mod slots;
pub mod store;
pub mod units;

use core::fmt;

/// Fouten blijven getypeerd tot aan de transportgrens.
#[derive(Debug, PartialEq)]
pub enum Error {
    /// Een begrensde verzameling of buffer zit vol.
    Full,
    /// De heap kon de gevraagde ruimte niet leveren.
    Memory,
    /// Een contractvoorwaarde is geschonden.
    Invalid(&'static str),
    /// Het gevraagde record bestaat niet.
    Missing(&'static str),
    /// Het record bestaat al.
    Conflict(&'static str),
    /// Een optimistische schrijfopdracht is verouderd.
    Changed,
    /// De opslag kon de kandidaat niet vastleggen.
    Storage,
    /// JSON kon niet worden gelezen of geschreven.
    Json(hop_types::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Full => f.write_str("capacity exceeded"),
            Self::Memory => f.write_str("allocation failed"),
            Self::Invalid(s) | Self::Missing(s) | Self::Conflict(s) => f.write_str(s),
            Self::Changed => f.write_str("record changed since it was read"),
            Self::Storage => f.write_str("document persistence failed"),
            Self::Json(e) => write!(f, "JSON: {e}"),
        }
    }
}

impl core::error::Error for Error {}

impl From<hop_types::Error> for Error {
    fn from(value: hop_types::Error) -> Self {
        Self::Json(value)
    }
}

/// Resultaat van een platformvrije operatie.
pub type Result<T = ()> = core::result::Result<T, Error>;

pub mod stats;
