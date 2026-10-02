//! Hostadapter voor Nibe.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_nibe::Nibe::default)
}
