//! HopOS-adapters voor Stulp; de bestaande SDK parkeert synchrone opslag op async I/O.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
pub mod apps;
mod archive;
pub mod environment;
mod files;
mod media;
pub mod meter;
mod network;
mod poll;
pub mod replies;
pub mod server;
pub mod storage;
pub use files::run;
pub mod plugin;
mod upload;
