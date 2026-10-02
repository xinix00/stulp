//! Shared controller jobs: the host and HopOS use the same lifecycle and automation owners.
#![cfg_attr(not(feature = "std"), no_std)]
#![forbid(unsafe_code)]
extern crate alloc;
use stulp_core::{Result, json::Value};
use stulp_web::Response;

/// Nonblocking, authenticated app operations supplied by the transport owner.
pub trait Apps {
    /// Monotonic milliseconds, independent of wall-clock corrections.
    fn now(&self) -> u64;
    /// Whether app, driver and device initialization has completed.
    fn running(&self, id: &str) -> bool;
    /// Queue a callback whose completion carries `owner`.
    fn call(&mut self, app: &str, owner: u64, method: &str, params: &Value) -> Result;
    /// Publish a newly adopted device before calling its initialization hooks.
    fn adopt(&mut self, device: &Value) -> Result;
    /// Platform logging without changing the application outcome.
    fn log(&self, message: core::fmt::Arguments<'_>);
}
/// An owned response receiver. Dropping it cancels its route.
pub trait Inbox {
    /// None means pending; cancellation is an error.
    fn receive(&self) -> Result<Option<Response>>;
}
/// Response routing supplied by the platform; copies carry only a route handle.
pub trait Reply: Clone {
    /// Receiver owned by exactly one waiting request/job.
    type Inbox: Inbox;
    /// Allocate a bounded local completion route.
    fn channel() -> Result<(Self, Self::Inbox)>;
    /// Deliver a response or report a canceled/full route.
    fn send(&self, response: Response) -> Result;
}

/// Embedded and installed plugin UI, locale fallback and browser context.
pub mod app_ui;
mod calendar;
/// Browser API to plugin callback mapping.
pub mod callbacks;
/// Asynchronous plugin Flow-card registration.
pub mod catalog;
/// Flow execution and automatic triggers.
pub mod flows;
/// MCP operations routed through the same lifecycle owners.
pub mod mcp;
/// Device pairing, settings and deletion lifecycle.
pub mod pairing;
/// Scene activation, restore and manual-override tracking.
pub mod scenes;
/// Platform-independent TZif decoding and civil-time conversion.
pub mod timezone;

#[cfg(feature = "std")]
mod native {
    use super::*;
    use std::sync::mpsc;
    impl Reply for mpsc::SyncSender<Response> {
        type Inbox = mpsc::Receiver<Response>;
        fn channel() -> Result<(Self, Self::Inbox)> {
            Ok(mpsc::sync_channel(1))
        }
        fn send(&self, response: Response) -> Result {
            self.try_send(response)
                .map_err(|_| stulp_core::Error::Invalid("response route closed or full"))
        }
    }
    impl Inbox for mpsc::Receiver<Response> {
        fn receive(&self) -> Result<Option<Response>> {
            match self.try_recv() {
                Ok(response) => Ok(Some(response)),
                Err(mpsc::TryRecvError::Empty) => Ok(None),
                Err(mpsc::TryRecvError::Disconnected) => {
                    Err(stulp_core::Error::Invalid("response route closed"))
                }
            }
        }
    }
}
/// Portable archive validation for external-slot deployments.
pub mod archive;
/// Media callback validation.
pub mod media;
