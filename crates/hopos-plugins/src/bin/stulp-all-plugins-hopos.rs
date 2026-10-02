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
    core::future::pending::<()>().await;
}
fn start(app: &'static applib::App) -> stulp_core::Result {
    applib::appnet::up(app)
        .map_err(|_| stulp_core::Error::Invalid("bundle network unavailable"))?;
    stulp_hopos::meter::spawn(app).map_err(|_| stulp_core::Error::Invalid("meter unavailable"))?;
    // `STULP_BUNDLE_SKIP=com.stulp.matter,...`: die apps draaien in een eigen
    // slot (eigen executor, eigen hartslag), zodat hun rekenwerk en dat van de
    // rest elkaar niet in de wachtrij zetten; de bundel laat ze hier weg.
    let skip = app.env("STULP_BUNDLE_SKIP").unwrap_or("");
    let wanted = |id: &str| !skip.split(',').any(|s| s.trim() == id);
    let mut started = 0_usize;
    macro_rules! plugin {
        ($index:expr, $id:literal, $factory:expr, $name:literal) => {
            if wanted($id) {
                stulp_hopos::plugin::spawn(app, $index, $factory).map_err(|_| {
                    stulp_core::Error::Invalid(concat!($name, " bundle startup failed"))
                })?;
                started += 1;
            }
        };
    }
    plugin!(
        0,
        "com.stulp.virtualdevices",
        stulp_virtualdevices::VirtualDevices::default,
        "virtualdevices"
    );
    plugin!(
        1,
        "com.stulp.weather",
        stulp_weather::WeatherPlugin::default,
        "weather"
    );
    plugin!(2, "com.stulp.somfy", stulp_somfy::Somfy::default, "somfy");
    plugin!(3, "com.stulp.nibe", stulp_nibe::Nibe::default, "nibe");
    plugin!(
        4,
        "com.stulp.spotify",
        stulp_spotify::Spotify::default,
        "spotify"
    );
    plugin!(
        5,
        "com.stulp.notify",
        stulp_notify::Notify::default,
        "notify"
    );
    plugin!(6, "com.stulp.wiim", stulp_wiim::Wiim::default, "wiim");
    plugin!(
        7,
        "com.stulp.sigenergy",
        stulp_sigenergy::Sigenergy::default,
        "sigenergy"
    );
    plugin!(8, "com.stulp.unifi", stulp_unifi::Unifi::default, "unifi");
    plugin!(
        9,
        "com.stulp.matter",
        stulp_matter::Matter::default,
        "matter"
    );
    app.log(format_args!(
        "STULP_BUNDLE_READY plugins={started} skipped={skip:?}"
    ));
    Ok(())
}
