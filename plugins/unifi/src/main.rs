//! Afzonderlijk hostproces voor UniFi Protect.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_unifi::Unifi::default)
}
