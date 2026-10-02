//! Hostproces voor de WiiM-plugin.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_wiim::Wiim::default)
}
