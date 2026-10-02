//! macOS gebruikt dns-sd alleen wanneer eigen multicast niet werkt, net als de Go-port.
use std::{
    io::Read,
    process::{Child, ChildStdout, Command, Stdio},
    time::{Duration, Instant},
};
use stulp_core::json;
use stulp_sdk::{Datagram, Error, Result};
const MAX_OUTPUT: usize = 1024 * 1024;
struct Probe {
    child: Child,
    output: ChildStdout,
    bytes: Vec<u8>,
    service: String,
}
impl Drop for Probe {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
impl Probe {
    fn start(command: &mut Command, service: String) -> Result<Self> {
        let mut child = command
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|_| Error::Transport("cannot start mDNSResponder query"))?;
        let output = match child.stdout.take() {
            Some(output) => output,
            None => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(Error::Transport("DNS-SD output missing"));
            }
        };
        let probe = Self {
            child,
            output,
            bytes: Vec::new(),
            service,
        };
        stulp_platform::socket::nonblocking(&probe.output)
            .map_err(|_| Error::Transport("cannot poll DNS-SD output"))?;
        Ok(probe)
    }
    fn read(&mut self) -> Result {
        let mut buffer = [0; 8192];
        for _ in 0..16 {
            match self.output.read(&mut buffer) {
                Ok(0) => break,
                Ok(n) => {
                    if n > MAX_OUTPUT.saturating_sub(self.bytes.len()) {
                        return Err(Error::Invalid("DNS-SD output exceeds limit"));
                    }
                    self.bytes
                        .try_reserve(n)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    self.bytes.extend_from_slice(&buffer[..n]);
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => return Err(Error::Transport("cannot read DNS-SD output")),
            }
        }
        Ok(())
    }
}
fn collect(probes: &mut [Probe], deadline: Instant) -> Result {
    while Instant::now() < deadline {
        let mut done = true;
        for probe in probes.iter_mut() {
            probe.read()?;
            if probe
                .child
                .try_wait()
                .map_err(|_| Error::Transport("cannot poll DNS-SD child"))?
                .is_none()
            {
                done = false;
            }
        }
        if done {
            break;
        }
        std::thread::sleep(
            Duration::from_millis(5).min(deadline.saturating_duration_since(Instant::now())),
        );
    }
    for probe in probes {
        let _ = probe.child.kill();
        probe
            .child
            .wait()
            .map_err(|_| Error::Transport("cannot reap DNS-SD child"))?;
        probe.read()?;
    }
    Ok(())
}
pub(super) fn browse(query: &[u8], window: u64) -> Result<Vec<Datagram>> {
    let services = stulp_sdk::dnssd::services(query)?;
    let deadline =
        Instant::now() + Duration::from_millis((window / 2).clamp(500, 1500).min(window));
    let mut probes = Vec::new();
    for service in services {
        let mut command = Command::new("/usr/bin/dns-sd");
        command.args(["-Z", service.trim_end_matches(".local."), "local."]);
        match Probe::start(&mut command, service) {
            Ok(probe) => json::push(&mut probes, probe, 12)?,
            Err(Error::Core(e)) => return Err(Error::Core(e)),
            Err(_) => (),
        }
    }
    if probes.is_empty() {
        return Err(Error::Transport("system DNS-SD unavailable"));
    }
    collect(&mut probes, deadline)?;
    let mut packets = Vec::new();
    for probe in &probes {
        for packet in stulp_sdk::dnssd::zone(&probe.service, &probe.bytes)? {
            json::push(&mut packets, packet, 256)?;
        }
    }
    Ok(packets)
}
#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    #[test]
    fn bounded_pipe_worker_drains_and_reaps_without_network_discovery() {
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "printf 'fixture bytes'; exec sleep 10"]);
        let probe = Probe::start(&mut command, "fixture".into()).unwrap();
        let mut probes = [probe];
        collect(&mut probes, Instant::now() + Duration::from_millis(80)).unwrap();
        assert_eq!(probes[0].bytes, b"fixture bytes");
        assert!(probes[0].child.try_wait().unwrap().is_some());
    }
}
