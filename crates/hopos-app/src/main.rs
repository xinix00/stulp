//! Stulp controller image; Hop places external plugins in separate slots.
#![cfg_attr(target_os = "none", no_std, no_main)]
use applib::{App, appnet, stacktask::Task};
use stulp_core::{Error, Result};
applib::main!(controller);
#[cfg(not(target_os = "none"))]
fn main() {}
async fn controller(app: &'static App) {
    if let Err(error) = run(app).await {
        app.log(format_args!("STULP_CONTROLLER_FAIL {error}"));
        app.shutdown(1).await;
    }
}
async fn run(app: &'static App) -> Result {
    appnet::up(app).map_err(|_| Error::Storage)?;
    // SAFETY: One controller owner on this executor core. JSON depth is bounded
    // by stulp-core; an 8 MiB stack matches the storage probe. Every suspend is
    // fallible and unwinds this closure normally on cancellation; no reference escapes.
    let task = unsafe {
        Task::new(8 << 20, |s| {
            stulp_hopos::run(app, &stulp_hopos::storage::Park(s))
        })
    }
    .map_err(|_| Error::Memory)?;
    task.await
}
