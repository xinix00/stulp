//! Eén vaste UDP-werker per plugin, alleen actief tijdens een zoekronde.
use std::{
    net::{SocketAddr, UdpSocket},
    sync::mpsc::{self, Receiver, SyncSender},
    thread::JoinHandle,
    time::{Duration, Instant},
};
use stulp_core::json;
use stulp_sdk::{Datagram, DatagramRequest, DatagramTarget, Error, Result};
pub(super) struct Worker {
    input: Option<SyncSender<DatagramRequest>>,
    output: Receiver<Result<Vec<Datagram>>>,
    thread: Option<JoinHandle<()>>,
    busy: bool,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        let (input, requests) = mpsc::sync_channel(1);
        let (answers, output) = mpsc::sync_channel(1);
        let thread = std::thread::Builder::new()
            .name("stulp-plugin-udp".into())
            .spawn(move || {
                while let Ok(request) = requests.recv() {
                    if answers.send(execute(request)).is_err() {
                        break;
                    }
                }
            })
            .map_err(|_| Error::Transport("cannot start UDP worker"))?;
        Ok(Self {
            input: Some(input),
            output,
            thread: Some(thread),
            busy: false,
        })
    }
    pub(super) fn start(&mut self, request: DatagramRequest) -> Result {
        if self.busy {
            if self.output.try_recv().is_ok() {
                self.busy = false;
            } else {
                return Err(Error::Transport("previous UDP round is still ending"));
            }
        }
        if request.payload.is_empty()
            || request.payload.len() > 8192
            || request.timeout_ms == 0
            || request.timeout_ms > 30_000
        {
            return Err(Error::Invalid("UDP request exceeds adapter bounds"));
        }
        self.input
            .as_ref()
            .ok_or(Error::Transport("UDP worker stopped"))?
            .try_send(request)
            .map_err(|_| Error::Transport("UDP worker unavailable"))?;
        self.busy = true;
        Ok(())
    }
    pub(super) fn poll(&mut self) -> Option<Result<Vec<Datagram>>> {
        match self.output.try_recv() {
            Ok(r) => {
                self.busy = false;
                Some(r)
            }
            Err(mpsc::TryRecvError::Empty) => None,
            Err(_) => Some(Err(Error::Transport("UDP worker stopped"))),
        }
    }
}
impl Drop for Worker {
    fn drop(&mut self) {
        self.input.take();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}
struct Probe {
    socket: UdpSocket,
    target: SocketAddr,
    interface: u32,
}
fn direct(target: &str) -> Result<Probe> {
    let target = target
        .parse::<SocketAddr>()
        .map_err(|_| Error::Invalid("UDP target must be an IP address and port"))?;
    if target.port() == 0 {
        return Err(Error::Invalid("UDP target port is zero"));
    }
    let socket = UdpSocket::bind(if target.is_ipv4() {
        "0.0.0.0:0"
    } else {
        "[::]:0"
    })
    .map_err(|_| Error::Transport("UDP bind failed"))?;
    if target.is_ipv4() {
        socket
            .set_multicast_ttl_v4(1)
            .map_err(|_| Error::Transport("UDP multicast TTL failed"))?;
    }
    socket
        .set_nonblocking(true)
        .map_err(|_| Error::Transport("UDP nonblocking setup failed"))?;
    Ok(Probe {
        socket,
        target,
        interface: 0,
    })
}
fn mdns_probe(address: std::net::IpAddr, index: u32) -> Result<Probe> {
    use std::net::{IpAddr, Ipv6Addr, SocketAddrV6};
    let mut p = direct(if address.is_ipv4() {
        "224.0.0.251:5353"
    } else {
        "[ff02::fb]:5353"
    })?;
    stulp_platform::multicast_interface(&p.socket, address, index)
        .map_err(|_| Error::Transport("cannot select multicast LAN interface"))?;
    match address {
        IpAddr::V4(_) => p
            .socket
            .set_multicast_ttl_v4(255)
            .map_err(|_| Error::Transport("mDNS TTL setup failed"))?,
        IpAddr::V6(_) => {
            stulp_platform::multicast_hops_v6(&p.socket, 255)
                .map_err(|_| Error::Transport("mDNS hop-limit setup failed"))?;
            p.target = SocketAddr::V6(SocketAddrV6::new(
                Ipv6Addr::new(0xff02, 0, 0, 0, 0, 0, 0, 0xfb),
                5353,
                0,
                index,
            ));
        }
    }
    p.interface = index;
    Ok(p)
}
fn probes(target: &DatagramTarget) -> Result<Vec<Probe>> {
    let mut result = Vec::new();
    match target {
        DatagramTarget::Address(target) => json::push(&mut result, direct(target)?, 32)?,
        DatagramTarget::Ssdp => {
            let interfaces = stulp_platform::interfaces()
                .map_err(|_| Error::Transport("cannot enumerate SSDP interfaces"))?;
            for interface in interfaces {
                if !interface.lan
                    || !interface.address.is_ipv4()
                    || result.iter().any(|p| p.interface == interface.index)
                {
                    continue;
                }
                let probe = direct("239.255.255.250:1900")?;
                if stulp_platform::multicast_interface(
                    &probe.socket,
                    interface.address,
                    interface.index,
                )
                .is_ok()
                {
                    json::push(
                        &mut result,
                        Probe {
                            interface: interface.index,
                            ..probe
                        },
                        32,
                    )?;
                }
            }
            if result.is_empty() {
                return Err(Error::Transport("no usable SSDP LAN interface"));
            }
        }
        DatagramTarget::Mdns => {
            let interfaces = stulp_platform::interfaces()
                .map_err(|_| Error::Transport("cannot enumerate multicast interfaces"))?;
            for interface in interfaces {
                if !interface.lan {
                    continue;
                }
                if let std::net::IpAddr::V6(v6) = interface.address
                    && !v6.is_unicast_link_local()
                {
                    continue;
                }
                if result.iter().any(|p| {
                    p.interface == interface.index
                        && p.target.is_ipv4() == interface.address.is_ipv4()
                }) {
                    continue;
                }
                match mdns_probe(interface.address, interface.index) {
                    Ok(probe) => json::push(&mut result, probe, 32)?,
                    Err(Error::Core(e)) => return Err(Error::Core(e)),
                    Err(_) => (),
                }
            }
            if result.is_empty() {
                return Err(Error::Transport("no usable multicast LAN interface"));
            }
        }
    }
    Ok(result)
}
fn execute(request: DatagramRequest) -> Result<Vec<Datagram>> {
    let started = Instant::now();
    let multicast = matches!(request.target, DatagramTarget::Mdns);
    let result = probes(&request.target).and_then(|probes| {
        collect(
            probes,
            &request.payload,
            request.timeout_ms,
            if multicast { 1000 } else { 150 },
        )
    });
    #[cfg(target_os = "macos")]
    if multicast && matches!(&result, Err(Error::Transport(_))) {
        let remaining = request
            .timeout_ms
            .saturating_sub(u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX));
        if remaining > 0 {
            return crate::dnssd::browse(&request.payload, remaining);
        }
    }
    #[cfg(not(target_os = "macos"))]
    let _ = started;
    result
}

fn collect(
    mut probes: Vec<Probe>,
    payload: &[u8],
    timeout_ms: u64,
    repeat_ms: u64,
) -> Result<Vec<Datagram>> {
    let deadline = Instant::now() + Duration::from_millis(timeout_ms);
    probes.retain(|p| p.socket.send_to(payload, p.target).is_ok());
    if probes.is_empty() {
        return Err(Error::Transport(
            "UDP query could not be sent on any selected interface",
        ));
    }
    let mut resend = Some(Instant::now() + Duration::from_millis(repeat_ms));
    let mut answers = Vec::new();
    let mut buffer = [0; 9001];
    while Instant::now() < deadline {
        if resend.is_some_and(|at| Instant::now() >= at) {
            for p in &probes {
                let _ = p.socket.send_to(payload, p.target);
            }
            resend = None;
        }
        let mut received = false;
        for probe in &probes {
            match probe.socket.recv_from(&mut buffer) {
                Ok((n, _)) if n > 9000 => {
                    received = true;
                }
                Ok((n, from)) => {
                    received = true;
                    let mut payload = Vec::new();
                    payload
                        .try_reserve_exact(n)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    payload.extend_from_slice(&buffer[..n]);
                    use std::fmt::Write;
                    let mut source = String::new();
                    source
                        .try_reserve(64)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    write!(&mut source, "{from}")
                        .map_err(|_| Error::Invalid("UDP address formatting failed"))?;
                    json::push(
                        &mut answers,
                        Datagram {
                            source,
                            interface: probe.interface,
                            payload,
                        },
                        256,
                    )?;
                }
                Err(e)
                    if matches!(
                        e.kind(),
                        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::Interrupted
                    ) => {}
                Err(_) => return Err(Error::Transport("UDP discovery receive failed")),
            }
        }
        if !received {
            std::thread::sleep(
                Duration::from_millis(2).min(deadline.saturating_duration_since(Instant::now())),
            );
        }
    }
    Ok(answers)
}
#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]
    use super::*;
    #[test]
    fn discovery_collects_real_loopback_datagrams_and_finishes() {
        let peer = UdpSocket::bind("127.0.0.1:0").unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        let target = peer.local_addr().unwrap().to_string();
        let server = std::thread::spawn(move || {
            let mut buffer = [0; 64];
            for _ in 0..2 {
                let (n, from) = peer.recv_from(&mut buffer).unwrap();
                assert_eq!(&buffer[..n], b"M-SEARCH test");
                peer.send_to(b"answer", from).unwrap();
            }
        });
        let mut w = Worker::new().unwrap();
        w.start(DatagramRequest {
            target: DatagramTarget::Address(target),
            payload: b"M-SEARCH test".to_vec(),
            timeout_ms: 250,
        })
        .unwrap();
        let deadline = Instant::now() + Duration::from_secs(3);
        let answers = loop {
            if let Some(answer) = w.poll() {
                break answer.unwrap();
            }
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(5));
        };
        assert_eq!(answers.len(), 2);
        assert!(
            answers
                .iter()
                .all(|d| d.source.starts_with("127.0.0.1:") && d.payload == b"answer")
        );
        server.join().unwrap();
    }
    #[test]
    fn multiple_probe_interfaces_keep_ipv6_record_scopes_and_drop_oversized_datagrams() -> Result {
        use std::net::{Ipv4Addr, Ipv6Addr};
        let v4 = UdpSocket::bind((Ipv4Addr::LOCALHOST, 0))
            .map_err(|_| Error::Transport("test v4 bind"))?;
        let v6 = UdpSocket::bind((Ipv6Addr::LOCALHOST, 0))
            .map_err(|_| Error::Transport("test v6 bind"))?;
        let p4 = Probe {
            socket: UdpSocket::bind("127.0.0.1:0").unwrap(),
            target: v4.local_addr().unwrap(),
            interface: 42,
        };
        let p6 = Probe {
            socket: UdpSocket::bind("[::1]:0").unwrap(),
            target: v6.local_addr().unwrap(),
            interface: 43,
        };
        p4.socket.set_nonblocking(true).unwrap();
        p6.socket.set_nonblocking(true).unwrap();
        let peers = [v4, v6].map(|peer| {
            std::thread::spawn(move || {
                peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
                let mut buf = [0; 64];
                let (_, from) = peer.recv_from(&mut buf).unwrap();
                peer.send_to(&vec![0; 9001], from).unwrap();
                peer.send_to(b"DNS answer", from).unwrap();
            })
        });
        let answers = collect(vec![p4, p6], b"test DNS query", 100, 1000)?;
        for peer in peers {
            peer.join().unwrap();
        }
        assert_eq!(answers.len(), 2);
        assert!(answers.iter().all(|p| p.payload == b"DNS answer"));
        assert!(
            answers
                .iter()
                .any(|p| p.interface == 42 && p.source.starts_with("127.0.0.1:"))
        );
        assert!(
            answers
                .iter()
                .any(|p| p.interface == 43 && p.source.starts_with("[::1]:"))
        );
        Ok(())
    }
}
