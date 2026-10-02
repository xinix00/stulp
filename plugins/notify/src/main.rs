//! Hostadapter voor browsermeldingen.
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_notify::Notify::default)
}
