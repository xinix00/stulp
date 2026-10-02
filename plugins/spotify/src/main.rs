//! Hostadapter voor Spotify Connect.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_spotify::Spotify::default)
}
