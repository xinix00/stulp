//! Eén vaste werker bezit acht nonblocking UDP-sockets; geen DNS of thread per peer.
use std::{
    collections::VecDeque,
    net::{Ipv4Addr, Ipv6Addr, SocketAddr, UdpSocket},
    sync::mpsc::{self, Receiver, SyncSender, TrySendError},
    thread::JoinHandle,
    time::Duration,
};
use stulp_core::json;
use stulp_sdk::{Error, Result, UdpCommand as Command, UdpEvent as Event};
const MAX: usize = 8192;
pub(super) struct Worker {
    input: Option<SyncSender<Command>>,
    output: Option<Receiver<Event>>,
    thread: Option<JoinHandle<()>>,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        let (input, requests) = mpsc::sync_channel(32);
        let (answers, output) = mpsc::sync_channel(64);
        let thread = std::thread::Builder::new()
            .name("stulp-plugin-sockets".into())
            .spawn(move || run(requests, answers))
            .map_err(|_| Error::Transport("cannot start UDP socket worker"))?;
        Ok(Self {
            input: Some(input),
            output: Some(output),
            thread: Some(thread),
        })
    }
    pub(super) fn send(&mut self, command: Command) -> Result {
        match &command {
            Command::Bind { address, .. } | Command::Send { address, .. }
                if address.len() > 128 =>
            {
                return Err(Error::Invalid("UDP address too long"));
            }
            Command::Send { bytes, .. } if bytes.len() > MAX => {
                return Err(Error::Invalid("UDP datagram too large"));
            }
            _ => (),
        }
        self.input
            .as_ref()
            .ok_or(Error::Transport("UDP socket worker stopped"))?
            .try_send(command)
            .map_err(|_| Error::Transport("UDP socket command queue unavailable"))
    }
    pub(super) fn poll(&mut self) -> Option<Event> {
        self.output.as_ref().and_then(|r| r.try_recv().ok())
    }
}
impl Drop for Worker {
    fn drop(&mut self) {
        self.input.take();
        self.output.take();
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}
struct Slot {
    id: u64,
    socket: UdpSocket,
}
fn run(input: Receiver<Command>, output: SyncSender<Event>) {
    let mut slots = Vec::<Slot>::new();
    let mut pending = VecDeque::<Event>::new();
    let mut highest = 0u64;
    let mut cursor = 0usize;
    if slots.try_reserve(MAX_SOCKETS).is_err() || pending.try_reserve(2).is_err() {
        return;
    }
    let mut buffer = [0; MAX + 1];
    loop {
        if let Some(event) = pending.pop_front() {
            match output.try_send(event) {
                Ok(()) => (),
                Err(TrySendError::Full(event)) => {
                    pending.push_front(event);
                    std::thread::sleep(Duration::from_millis(2));
                    continue;
                }
                Err(TrySendError::Disconnected(_)) => return,
            }
        }
        match input.try_recv() {
            Ok(command) => {
                if let Some(event) = execute(command, &mut slots, &mut highest) {
                    pending.push_back(event);
                }
            }
            Err(mpsc::TryRecvError::Disconnected) => return,
            Err(mpsc::TryRecvError::Empty) => (),
        }
        // Bij een volle mailbox blijft het geheugen begrensd. UDP-verlies wordt door MRP hersteld.
        if pending.is_empty() && !slots.is_empty() {
            cursor %= slots.len();
            let slot = &slots[cursor];
            cursor += 1;
            let event = match slot.socket.recv_from(&mut buffer) {
                Ok((n, _)) if n > MAX => Some(Event::Error(
                    slot.id,
                    Error::Invalid("received UDP datagram exceeds limit"),
                )),
                Ok((n, from)) => {
                    let result = (|| {
                        let mut bytes = Vec::new();
                        bytes
                            .try_reserve_exact(n)
                            .map_err(|_| stulp_core::Error::Memory)?;
                        bytes.extend_from_slice(&buffer[..n]);
                        Ok(Event::Data {
                            id: slot.id,
                            address: address(from)?,
                            bytes,
                        })
                    })();
                    Some(result.unwrap_or_else(|e| Event::Error(slot.id, e)))
                }
                Err(e)
                    if matches!(
                        e.kind(),
                        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::Interrupted
                    ) =>
                {
                    None
                }
                Err(_) => Some(Event::Error(
                    slot.id,
                    Error::Transport("UDP socket receive failed"),
                )),
            };
            if let Some(event) = event {
                pending.push_back(event);
            }
        }
        if pending.is_empty() {
            std::thread::sleep(Duration::from_millis(1));
        }
    }
}
fn address(addr: SocketAddr) -> Result<String> {
    use std::fmt::Write;
    let mut out = String::new();
    out.try_reserve(64).map_err(|_| stulp_core::Error::Memory)?;
    write!(&mut out, "{addr}").map_err(|_| Error::Invalid("UDP address formatting"))?;
    Ok(out)
}
/// Sockets per plugin: twee per werker (IPv4 en IPv6) voor de hoofdwerker en
/// acht commandowerkers van Matter, plus ruimte voor discovery. Dezelfde grens
/// als de HopOS-adapter (crates/hopos/src/plugin/udp.rs).
const MAX_SOCKETS: usize = 24;
fn execute(command: Command, slots: &mut Vec<Slot>, highest: &mut u64) -> Option<Event> {
    let id = match &command {
        Command::Bind { id, .. }
        | Command::Send { id, .. }
        | Command::Close { id }
        | Command::JoinV4 { id, .. }
        | Command::JoinV6 { id, .. } => *id,
    };
    let result = (|| match command {
        Command::Bind { id, address: bind } => {
            // Werkers van één plugin binden in willekeurige volgorde; alleen
            // een ID dat nu in gebruik is, is fout.
            if id == 0 || slots.iter().any(|s| s.id == id) {
                return Err(Error::Invalid("UDP socket ID in use"));
            }
            *highest = (*highest).max(id);
            if slots.len() >= MAX_SOCKETS {
                return Err(Error::Invalid("UDP socket limit reached"));
            }
            let bind = bind
                .parse::<SocketAddr>()
                .map_err(|_| Error::Invalid("UDP bind requires literal IP address"))?;
            let socket = UdpSocket::bind(bind).map_err(|_| Error::Transport("UDP bind failed"))?;
            socket
                .set_nonblocking(true)
                .map_err(|_| Error::Transport("UDP nonblocking failed"))?;
            if bind.is_ipv4() {
                socket
                    .set_multicast_ttl_v4(255)
                    .map_err(|_| Error::Transport("UDP multicast TTL failed"))?;
            }
            let local = address(
                socket
                    .local_addr()
                    .map_err(|_| Error::Transport("UDP local address failed"))?,
            )?;
            // Tot 04-10 stond hier nog 8: met een Matter-werker per node-eigenaar
            // (twee sockets elk) kregen werkers 4 tot 8 geen socket, stil.
            json::push(slots, Slot { id, socket }, MAX_SOCKETS)?;
            Ok(Some(Event::Bound(id, local)))
        }
        Command::Close { id } => {
            if let Some(i) = slots.iter().position(|s| s.id == id) {
                slots.remove(i);
            }
            Ok(Some(Event::Closed(id)))
        }
        command => {
            let slot = slots
                .iter()
                .find(|s| s.id == id)
                .ok_or(Error::Invalid("UDP socket is not open"))?;
            match command {
                Command::Send { address, bytes, .. } => {
                    if bytes.len() > MAX {
                        return Err(Error::Invalid("UDP datagram too large"));
                    }
                    let target = address.parse::<SocketAddr>().map_err(|_| {
                        Error::Invalid("UDP destination requires literal IP address")
                    })?;
                    let n = slot.socket.send_to(&bytes, target).map_err(|e| {
                        if target.is_ipv6() && e.kind() == std::io::ErrorKind::NetworkUnreachable {
                            Error::Transport("no IPv6 route")
                        } else {
                            Error::Transport("UDP datagram send failed")
                        }
                    })?;
                    if n != bytes.len() {
                        return Err(Error::Transport("UDP datagram write incomplete"));
                    }
                }
                Command::JoinV4 {
                    group, interface, ..
                } => slot
                    .socket
                    .join_multicast_v4(&Ipv4Addr::from(group), &Ipv4Addr::from(interface))
                    .map_err(|_| Error::Transport("UDP IPv4 multicast join failed"))?,
                Command::JoinV6 {
                    group, interface, ..
                } => slot
                    .socket
                    .join_multicast_v6(&Ipv6Addr::from(group), interface)
                    .map_err(|_| Error::Transport("UDP IPv6 multicast join failed"))?,
                _ => return Err(Error::Invalid("invalid UDP socket operation")),
            }
            Ok(None)
        }
    })();
    match result {
        Ok(event) => event,
        Err(e) => Some(Event::Error(id, e)),
    }
}
#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic)]
mod tests {
    use super::*;
    fn receive(worker: &mut Worker) -> Event {
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        loop {
            if let Some(event) = worker.poll() {
                return event;
            }
            assert!(std::time::Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(1));
        }
    }
    #[test]
    fn persistent_udp_preserves_datagrams_ipv6_scopes_and_socket_generation() {
        for bind in ["127.0.0.1:0", "[::1]:0"] {
            let peer = UdpSocket::bind(bind).unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
            let mut worker = Worker::new().unwrap();
            worker
                .send(Command::Bind {
                    id: 1,
                    address: bind.into(),
                })
                .unwrap();
            let local = match receive(&mut worker) {
                Event::Bound(1, local) => local,
                _ => panic!("bind failed"),
            };
            for payload in [b"first".as_slice(), b"", &[42; 8192]] {
                worker
                    .send(Command::Send {
                        id: 1,
                        address: peer.local_addr().unwrap().to_string(),
                        bytes: payload.to_vec(),
                    })
                    .unwrap();
                let mut buffer = [0; 8193];
                let (n, from) = peer.recv_from(&mut buffer).unwrap();
                assert_eq!(&buffer[..n], payload);
                assert_eq!(from.to_string(), local);
                peer.send_to(&buffer[..n], from).unwrap();
                match receive(&mut worker) {
                    Event::Data {
                        id: 1,
                        address,
                        bytes,
                    } => {
                        assert_eq!(address, peer.local_addr().unwrap().to_string());
                        assert_eq!(bytes, payload);
                    }
                    _ => panic!("datagram missing"),
                }
            }
            peer.send_to(&[0; 8193], &local).unwrap();
            assert!(matches!(receive(&mut worker), Event::Error(1, _)));
            worker.send(Command::Close { id: 1 }).unwrap();
            assert!(matches!(receive(&mut worker), Event::Closed(1)));
            // Een gesloten ID mag weer: werkers binden in willekeurige volgorde.
            worker
                .send(Command::Bind {
                    id: 1,
                    address: bind.into(),
                })
                .unwrap();
            assert!(matches!(receive(&mut worker), Event::Bound(1, _)));
            // Een ID dat in gebruik is, niet.
            worker
                .send(Command::Bind {
                    id: 1,
                    address: bind.into(),
                })
                .unwrap();
            assert!(matches!(receive(&mut worker), Event::Error(1, _)));
            worker
                .send(Command::Bind {
                    id: 2,
                    address: bind.into(),
                })
                .unwrap();
            assert!(matches!(receive(&mut worker), Event::Bound(2, _)));
        }
        assert_eq!(
            address("[fe80::1%42]:5540".parse().unwrap()).unwrap(),
            "[fe80::1%42]:5540"
        );
    }
    #[test]
    fn every_matter_worker_gets_its_two_sockets() {
        // De hoofdwerker en acht werkers: achttien sockets, alle gebonden.
        let mut worker = Worker::new().unwrap();
        for id in 1..=18 {
            worker
                .send(Command::Bind {
                    id,
                    address: "127.0.0.1:0".into(),
                })
                .unwrap();
            assert!(
                matches!(receive(&mut worker), Event::Bound(bound, _) if bound == id),
                "socket {id} was refused"
            );
        }
    }
}
