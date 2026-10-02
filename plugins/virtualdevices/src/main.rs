//! Hoststart voor de zelfstandige virtuele-apparatenplugin.
#![forbid(unsafe_code)]
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_virtualdevices::VirtualDevices::default)
}
