//! Geïsoleerde koude-herstartproef van het Stulp-document, geen productiecontroller.
#![cfg_attr(target_os = "none", no_std, no_main)]
extern crate alloc;
use applib::{App, EXEC, appnet, stacktask::Task};
use core::time::Duration;
use stulp_core::{Error, Result, json, slots, store::Store};
use stulp_hopos::storage::{Files, Park};
applib::main!(probe);
#[cfg(not(target_os = "none"))]
fn main() {}
async fn probe(app: &'static App) {
    match run(app).await {
        Ok(false) => applib::log!("STULP_PERSIST_WRITE"),
        Ok(true) => applib::log!("STULP_PERSIST_READ"),
        Err(e) => {
            applib::log!("STULP_PERSIST_FAIL {e}");
            return app.shutdown(1).await;
        }
    }
    // De harness stopt QEMU hard vóór een lifecycle-stop nog iets kan syncen.
    loop {
        EXEC.get().after(Duration::from_secs(60)).await;
    }
}
async fn run(app: &'static App) -> Result<bool> {
    let net = appnet::up(app).map_err(|_| Error::Storage)?;
    // SAFETY: Eén documenteigenaar op deze core. De test gebruikt JSON-diepte
    // twee, vaste veldnamen en een heapstring van 192 KiB; 8 MiB stack bevat
    // ruim de native getoetste callstack. Cancelled verlaat alle callbacks met
    // Error::Storage en de closure keert terug. Geen verwijzing ontsnapt.
    let task = unsafe {
        Task::new(8 << 20, |s| -> Result<bool> {
            let (files, bytes) = slots::Files::open(Files::new(
                net.system_client(),
                Park(s),
                "/data/stulp-check.json",
            )?)?;
            let mut store = Store::open(&bytes, files)?;
            let previous =
                json::get(store.document().root(), "system").unwrap_or(&json::Value::Null);
            if !json::text(previous, "fixture").is_empty() {
                if json::text(previous, "fixture") != "Stulp Rust persisted"
                    || json::text(previous, "padding").len() != 192 * 1024
                {
                    return Err(Error::Changed);
                }
                return Ok(true);
            }
            let mut padding = alloc::string::String::new();
            padding
                .try_reserve_exact(192 * 1024)
                .map_err(|_| Error::Memory)?;
            for _ in 0..192 * 1024 {
                padding.push('x');
            }
            store.system(json::fields(&[
                ("fixture", json::string("Stulp Rust persisted")?),
                ("padding", json::Value::String(padding)),
            ])?)?;
            Ok(false)
        })
    }
    .map_err(|_| Error::Memory)?;
    task.await
}
