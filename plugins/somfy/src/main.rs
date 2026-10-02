//! Hostadapter voor Somfy.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_somfy::Somfy::default)
}
