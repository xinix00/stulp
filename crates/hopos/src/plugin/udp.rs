//! Bounded datagram sockets; unsupported address families return explicit events.
use alloc::{collections::VecDeque, format, string::String, vec::Vec};
use applib::appnet::{self, Endpoint, Endpoint6, Udp6Socket, UdpSocket};
use stulp_core::json;
use stulp_sdk::{Error, Result, UdpCommand as Command, UdpEvent as Event};
/// Eén slot heeft één interface; 0 is default, 1 is de expliciete scope.
pub(super) const INTERFACE: u32 = 1;
#[derive(Clone, Copy)]
pub(super) enum Peer {
    V4(Endpoint),
    V6(Endpoint6),
}
pub(super) fn endpoint(address: &str) -> Result<Peer> {
    match address
        .parse::<core::net::SocketAddr>()
        .map_err(|_| Error::Invalid("expected numeric UDP endpoint"))?
    {
        core::net::SocketAddr::V4(v) => Ok(Peer::V4(Endpoint {
            ip: v.ip().octets(),
            port: v.port(),
        })),
        core::net::SocketAddr::V6(v) => {
            if v.scope_id() != 0 && v.scope_id() != INTERFACE {
                return Err(Error::Invalid("IPv6 scope is not assigned to this slot"));
            }
            Ok(Peer::V6(Endpoint6 {
                ip: v.ip().octets(),
                port: v.port(),
            }))
        }
    }
}
pub(super) fn address(e: Peer) -> String {
    match e {
        Peer::V4(e) => format!("{}.{}.{}.{}:{}", e.ip[0], e.ip[1], e.ip[2], e.ip[3], e.port),
        Peer::V6(e) => {
            let ip = core::net::Ipv6Addr::from(e.ip);
            format!(
                "{}",
                core::net::SocketAddrV6::new(
                    ip,
                    e.port,
                    0,
                    if ip.is_unicast_link_local() {
                        INTERFACE
                    } else {
                        0
                    }
                )
            )
        }
    }
}
pub(super) enum Socket {
    V4(UdpSocket),
    V6(Udp6Socket),
}
impl Socket {
    pub(super) fn bind(peer: Peer) -> Result<Self> {
        match peer {
            Peer::V4(e) => {
                if e.ip != [0; 4] && appnet::net().is_none_or(|n| n.ip() != e.ip) {
                    return Err(Error::Invalid(
                        "UDP bind address is not assigned to this slot",
                    ));
                }
                UdpSocket::bind(e.port)
                    .map(Self::V4)
                    .map_err(|_| Error::Transport("UDP bind failed"))
            }
            Peer::V6(e) => {
                if e.ip != [0; 16] {
                    return Err(Error::Invalid("IPv6 UDP supports wildcard bindings only"));
                }
                Udp6Socket::bind(e.port)
                    .map(Self::V6)
                    .map_err(|_| Error::Transport("IPv6 UDP bind failed"))
            }
        }
    }
    pub(super) fn local(&self) -> Result<Peer> {
        match self {
            Self::V4(s) => s.local().map(Peer::V4),
            Self::V6(s) => s.local().map(Peer::V6),
        }
        .map_err(|_| Error::Transport("UDP local address unavailable"))
    }
    pub(super) async fn send(&self, to: Peer, bytes: &[u8]) -> Result<usize> {
        match (self, to) {
            (Self::V4(s), Peer::V4(p)) => s.send_to(p, bytes).await,
            (Self::V6(s), Peer::V6(p)) => s.send_to(p, bytes).await,
            _ => return Err(Error::Invalid("UDP address family mismatch")),
        }
        .map_err(|e| match e {
            appnet::NetError::Stack(
                appnet::StackError::NoRoute6 | appnet::StackError::NoRoute { .. },
            ) => Error::Transport("no route to UDP peer"),
            _ => Error::Transport("UDP send failed"),
        })
    }
    pub(super) async fn recv(&self, buf: &mut [u8]) -> Result<(usize, Peer)> {
        match self {
            Self::V4(s) => s.recv_from(buf).await.map(|(n, p)| (n, Peer::V4(p))),
            Self::V6(s) => s.recv_from(buf).await.map(|(n, p)| (n, Peer::V6(p))),
        }
        .map_err(|_| Error::Transport("UDP receive failed"))
    }
    /// Wacht tot er een datagram klaarligt, zonder het te lezen (docs/apps.md
    /// van HopOS: slapen op de socket, niet op de klok).
    pub(super) async fn readable(&self) -> Result {
        match self {
            Self::V4(s) => s.readable().await,
            Self::V6(s) => s.readable().await,
        }
        .map_err(|_| Error::Transport("UDP readiness failed"))
    }
}
/// Klaar zodra één van `sockets` leesbaar is; zonder sockets nooit (de
/// aanroeper zet er zijn eigen termijn naast).
pub(super) async fn any_readable(sockets: &[Socket]) {
    core::future::poll_fn(|cx| {
        for s in sockets {
            if core::pin::pin!(s.readable()).poll(cx).is_ready() {
                return core::task::Poll::Ready(());
            }
        }
        core::task::Poll::Pending
    })
    .await;
}
impl Socket {}
struct Slot {
    id: u64,
    socket: Socket,
}
pub(super) struct Sockets {
    slots: Vec<Slot>,
    input: VecDeque<Command>,
    output: VecDeque<Event>,
    highest: u64,
    cursor: usize,
}
impl Sockets {
    pub(super) fn new() -> Self {
        Self {
            slots: Vec::new(),
            input: VecDeque::new(),
            output: VecDeque::new(),
            highest: 0,
            cursor: 0,
        }
    }
    pub(super) fn command(&mut self, c: Command) -> Result {
        if self.input.len() >= 32 {
            return Err(Error::Transport("UDP command queue full"));
        }
        match &c {
            Command::Send { address, bytes, .. } if address.len() > 128 || bytes.len() > 8192 => {
                return Err(Error::Invalid("UDP command exceeds bounds"));
            }
            _ => (),
        };
        self.input
            .try_reserve(1)
            .map_err(|_| stulp_core::Error::Memory)?;
        self.input.push_back(c);
        Ok(())
    }
    pub(super) fn poll(&mut self) -> Option<Event> {
        self.output.pop_front()
    }
    /// Klaar zodra er werk is: een opdracht of gebeurtenis in de rij, of een
    /// datagram op één van de sockets. Zonder sockets en zonder werk wacht
    /// hij; de aanroeper zet er zijn tik naast.
    pub(super) async fn wait(&self) {
        if !self.input.is_empty() || !self.output.is_empty() {
            return;
        }
        core::future::poll_fn(|cx| {
            for s in &self.slots {
                if core::pin::pin!(s.socket.readable()).poll(cx).is_ready() {
                    return core::task::Poll::Ready(());
                }
            }
            core::task::Poll::Pending
        })
        .await;
    }
    pub(super) fn tick(&mut self) -> Result {
        if self.output.len() >= 64 {
            return Ok(());
        }
        self.output
            .try_reserve(2)
            .map_err(|_| stulp_core::Error::Memory)?;
        if let Some(c) = self.input.pop_front() {
            let id = match &c {
                Command::Bind { id, .. }
                | Command::Send { id, .. }
                | Command::Close { id }
                | Command::JoinV4 { id, .. }
                | Command::JoinV6 { id, .. } => *id,
            };
            let result = (|| -> Result<Option<Event>> {
                match c {
                    Command::Bind { id, address: a } => {
                        if id == 0 || id <= self.highest || self.slots.len() >= 8 {
                            return Err(Error::Invalid("UDP slot limit or reused id"));
                        }
                        self.highest = id;
                        let e = endpoint(&a)?;
                        let socket = Socket::bind(e)?;
                        let local = socket.local()?;
                        json::push(&mut self.slots, Slot { id, socket }, 8)?;
                        Ok(Some(Event::Bound(id, address(local))))
                    }
                    Command::Close { id } => {
                        self.slots.retain(|s| s.id != id);
                        Ok(Some(Event::Closed(id)))
                    }
                    Command::JoinV4 {
                        id,
                        group,
                        interface,
                    } => {
                        if !self
                            .slots
                            .iter()
                            .any(|s| s.id == id && matches!(s.socket, Socket::V4(_)))
                        {
                            return Err(Error::Invalid("UDP socket missing"));
                        }
                        if interface != [0; 4] && appnet::net().is_none_or(|n| n.ip() != interface)
                        {
                            return Err(Error::Invalid("UDP interface not assigned to slot"));
                        }
                        appnet::join_group(group)
                            .map_err(|_| Error::Transport("UDP multicast join failed"))?;
                        Ok(None)
                    }
                    Command::JoinV6 {
                        id,
                        group,
                        interface,
                    } => {
                        if !self
                            .slots
                            .iter()
                            .any(|s| s.id == id && matches!(s.socket, Socket::V6(_)))
                        {
                            return Err(Error::Invalid("IPv6 UDP socket missing"));
                        }
                        if interface != 0 && interface != INTERFACE {
                            return Err(Error::Invalid("IPv6 interface not assigned to slot"));
                        }
                        appnet::net()
                            .ok_or(Error::Transport("network unavailable"))?
                            .join_group6(group)
                            .map_err(|_| Error::Transport("IPv6 multicast join failed"))?;
                        Ok(None)
                    }
                    Command::Send { id, address, bytes } => {
                        let s = self
                            .slots
                            .iter()
                            .find(|s| s.id == id)
                            .ok_or(Error::Invalid("UDP socket missing"))?;
                        match crate::poll::once(s.socket.send(endpoint(&address)?, &bytes)) {
                            None => {
                                self.input.push_front(Command::Send { id, address, bytes });
                                Ok(None)
                            }
                            Some(Ok(n)) if n == bytes.len() => Ok(None),
                            Some(Err(error)) => Err(error),
                            Some(Ok(_)) => Err(Error::Transport("UDP send incomplete")),
                        }
                    }
                }
            })();
            match result {
                Ok(Some(e)) => self.output.push_back(e),
                Ok(None) => (),
                Err(e) => self.output.push_back(Event::Error(id, e)),
            }
        }
        if !self.slots.is_empty() {
            self.cursor %= self.slots.len();
            let s = &self.slots[self.cursor];
            self.cursor += 1;
            let mut buf = [0; 8193];
            match crate::poll::once(s.socket.recv(&mut buf)) {
                Some(Ok((n, from))) if n <= 8192 => {
                    let mut bytes = Vec::new();
                    bytes
                        .try_reserve_exact(n)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    bytes.extend_from_slice(&buf[..n]);
                    self.output.push_back(Event::Data {
                        id: s.id,
                        address: address(from),
                        bytes,
                    });
                }
                Some(_) => self.output.push_back(Event::Error(
                    s.id,
                    Error::Transport("UDP receive failed or oversized"),
                )),
                None => (),
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn slot_scope_is_preserved_and_foreign_interfaces_are_rejected() {
        let peer = endpoint("[fe80::abcd%1]:5540");
        assert!(peer.is_ok());
        if let Ok(peer) = peer {
            assert_eq!(address(peer), "[fe80::abcd%1]:5540");
        }
        assert!(endpoint("[fe80::abcd%42]:5540").is_err());
        if let Ok(peer) = endpoint("[fd11::abcd]:5540") {
            assert_eq!(address(peer), "[fd11::abcd]:5540");
        } else {
            assert!(endpoint("[fd11::abcd]:5540").is_ok());
        }
    }
}
