//! Hostproces voor de Matter-controllerplugin.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_matter::Matter::default)
}
