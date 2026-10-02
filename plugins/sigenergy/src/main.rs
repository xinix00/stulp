//! Hostproces voor de Sigenergy-plugin.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_sigenergy::Sigenergy::default)
}
