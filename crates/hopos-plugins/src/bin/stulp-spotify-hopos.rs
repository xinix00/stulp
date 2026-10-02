//! HopOS slot entry for the spotify plugin.
#![cfg_attr(target_os = "none", no_std, no_main)]
applib::main!(plugin);
#[cfg(not(target_os = "none"))]
fn main() {}
async fn plugin(app: &'static applib::App) {
    if let Err(error) = stulp_hopos::plugin::run(app, stulp_spotify::Spotify::default).await {
        app.log(format_args!("STULP_PLUGIN_FAIL {error}"));
        app.shutdown(1).await;
    }
}
