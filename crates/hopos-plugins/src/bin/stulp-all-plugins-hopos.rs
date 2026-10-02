//! Alle tien plugins in één HopOS-image, met één netwerk en één executor.
#![cfg_attr(target_os = "none", no_std, no_main)]
applib::main!(bundle);
#[cfg(not(target_os = "none"))]
fn main() {}
async fn bundle(app: &'static applib::App) {
    if let Err(error) = start(app) {
        app.log(format_args!("STULP_BUNDLE_FAIL {error}"));
        app.shutdown(1).await;
    }
    app.log(format_args!("STULP_BUNDLE_READY plugins=10"));
    core::future::pending::<()>().await;
}
fn start(app: &'static applib::App) -> stulp_core::Result {
    applib::appnet::up(app)
        .map_err(|_| stulp_core::Error::Invalid("bundle network unavailable"))?;
    stulp_hopos::meter::spawn(app).map_err(|_| stulp_core::Error::Invalid("meter unavailable"))?;
    stulp_hopos::plugin::spawn(app, 0, stulp_virtualdevices::VirtualDevices::default)
        .map_err(|_| stulp_core::Error::Invalid("virtualdevices bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 1, stulp_weather::WeatherPlugin::default)
        .map_err(|_| stulp_core::Error::Invalid("weather bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 2, stulp_somfy::Somfy::default)
        .map_err(|_| stulp_core::Error::Invalid("somfy bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 3, stulp_nibe::Nibe::default)
        .map_err(|_| stulp_core::Error::Invalid("nibe bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 4, stulp_spotify::Spotify::default)
        .map_err(|_| stulp_core::Error::Invalid("spotify bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 5, stulp_notify::Notify::default)
        .map_err(|_| stulp_core::Error::Invalid("notify bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 6, stulp_wiim::Wiim::default)
        .map_err(|_| stulp_core::Error::Invalid("wiim bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 7, stulp_sigenergy::Sigenergy::default)
        .map_err(|_| stulp_core::Error::Invalid("sigenergy bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 8, stulp_unifi::Unifi::default)
        .map_err(|_| stulp_core::Error::Invalid("unifi bundle startup failed"))?;
    stulp_hopos::plugin::spawn(app, 9, stulp_matter::Matter::default)
        .map_err(|_| stulp_core::Error::Invalid("matter bundle startup failed"))?;
    Ok(())
}
