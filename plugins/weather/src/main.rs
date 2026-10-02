//! Zelfstandige Rust-start van de weerplugin.
#![forbid(unsafe_code)]
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_weather::WeatherPlugin::default)
}
