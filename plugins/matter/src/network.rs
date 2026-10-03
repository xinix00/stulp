//! Eén eigenaar verbindt MRP/PASE/CASE aan de SDK, met coöperatieve deadlines.
use crate::{
    case,
    message::Message,
    mrp::{self, Event, Handle, Node, Timing},
    pase,
};
use alloc::{collections::VecDeque, string::String, vec::Vec};
use core::{fmt::Write, net::SocketAddr};
use stulp_core::json;
use stulp_sdk::{Client, Error, Result, Transport, UdpCommand, UdpEvent};
fn canonical(address: &str) -> Result<(String, bool)> {
    let parsed = address.parse::<SocketAddr>().map_err(|_| {
        Error::Invalid("Matter requires an IP address, port and numeric IPv6 scope")
    })?;
    if parsed.port() == 0 || parsed.ip().is_unspecified() || parsed.ip().is_multicast() {
        return Err(Error::Invalid("invalid Matter peer address"));
    }
    let mut out = String::new();
    out.try_reserve(64).map_err(|_| stulp_core::Error::Memory)?;
    write!(&mut out, "{parsed}").map_err(|_| Error::Invalid("Matter address formatting"))?;
    Ok((out, parsed.is_ipv6()))
}
/// MRP en sockets blijven bij dezelfde eigenaar; unsolicited berichten staan in een kleine inbox.
pub struct Network {
    node: Node,
    ipv4: Option<u64>,
    ipv6: Option<u64>,
    inbox: VecDeque<Event>,
    jitter: u32,
    invalid_packets: u64,
    no_route: bool,
}
impl Network {
    /// Opent IPv4 en IPv6; één werkende familie is voldoende, deadlines houden de hartslag actief.
    pub async fn open<T: Transport>(c: &mut Client<T>) -> Result<Self> {
        let seed = c.random()?;
        let mut n = Self {
            node: Node::new(
                u32::from_le_bytes([seed[0], seed[1], seed[2], seed[3]]),
                u16::from_le_bytes([seed[4], seed[5]]),
            ),
            ipv4: None,
            ipv6: None,
            inbox: VecDeque::new(),
            jitter: u32::from_le_bytes([seed[6], seed[7], seed[8], seed[9]]) | 1,
            invalid_packets: 0,
            no_route: false,
        };
        c.udp(UdpCommand::Bind {
            id: 1,
            address: json::copy("0.0.0.0:0")?,
        })?;
        if let Err(e) = c.udp(UdpCommand::Bind {
            id: 2,
            address: json::copy("[::]:0")?,
        }) {
            let _ = c.udp(UdpCommand::Close { id: 1 });
            return Err(e);
        }
        let deadline = c.now().saturating_add(3000);
        let mut answered = 0u8;
        let result = async {
            while answered != 3 {
                if c.now() >= deadline {
                    return Err(Error::Timeout);
                }
                if let Some(event) = c.poll_udp() {
                    match event {
                        UdpEvent::Bound(1, _) => {
                            n.ipv4 = Some(1);
                            answered |= 1;
                        }
                        UdpEvent::Bound(2, _) => {
                            n.ipv6 = Some(2);
                            answered |= 2;
                        }
                        UdpEvent::Error(1, _) => answered |= 1,
                        UdpEvent::Error(2, _) => answered |= 2,
                        _ => (),
                    }
                } else {
                    c.idle().await?;
                }
            }
            if n.ipv4.is_none() && n.ipv6.is_none() {
                return Err(Error::Transport("Matter has no usable UDP socket"));
            }
            Ok(())
        }
        .await;
        if let Err(e) = result {
            let _ = c.udp(UdpCommand::Close { id: 1 });
            let _ = c.udp(UdpCommand::Close { id: 2 });
            return Err(e);
        }
        Ok(n)
    }
    fn jitter(&mut self) -> u32 {
        let mut x = self.jitter;
        x ^= x << 13;
        x ^= x >> 17;
        x ^= x << 5;
        self.jitter = x;
        x
    }
    fn reserve_inbox(&mut self) -> Result {
        if self.inbox.len() >= 64 {
            return Err(Error::Invalid("Matter unsolicited event queue full"));
        }
        self.inbox
            .try_reserve(1)
            .map_err(|_| stulp_core::Error::Memory)?;
        Ok(())
    }
    /// Ongeldige of niet-authentieke datagrams zijn diagnostiek, geen reden alle apparaten te stoppen.
    pub fn invalid_packets(&self) -> u64 {
        self.invalid_packets
    }
    /// Eén korte netwerkstap; callbacks gebruiken `receive` om coöperatief verder te wachten.
    pub fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        let now = c.now();
        let jitter = self.jitter();
        self.node.tick(now, jitter)?;
        for _ in 0..16 {
            let Some(event) = c.poll_udp() else {
                break;
            };
            match event {
                UdpEvent::Data { id, address, bytes }
                    if Some(id) == self.ipv4 || Some(id) == self.ipv6 =>
                {
                    match self.node.receive(&address, &bytes, now) {
                        Ok(()) => {
                            if Some(id) == self.ipv6 {
                                self.no_route = false;
                            }
                        }
                        Err(Error::Core(e)) => return Err(Error::Core(e)),
                        Err(_) => self.invalid_packets = self.invalid_packets.saturating_add(1),
                    }
                }
                UdpEvent::Error(id, error)
                    if Some(id) == self.ipv6 && crate::route::missing(&error) =>
                {
                    self.no_route = true;
                }
                UdpEvent::Error(id, _) if Some(id) == self.ipv4 || Some(id) == self.ipv6 => {
                    self.invalid_packets = self.invalid_packets.saturating_add(1)
                }
                UdpEvent::Closed(id) => {
                    if self.ipv4 == Some(id) {
                        self.ipv4 = None;
                    }
                    if self.ipv6 == Some(id) {
                        self.ipv6 = None;
                    }
                }
                _ => (),
            }
        }
        self.flush(c)
    }
    fn flush<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        while let Some(packet) = self.node.outgoing() {
            let (address, v6) = canonical(&packet.address)?;
            let id = if v6 { self.ipv6 } else { self.ipv4 }.ok_or(Error::Transport(
                "Matter peer address family is unavailable",
            ))?;
            c.udp(UdpCommand::Send {
                id,
                address,
                bytes: packet.bytes,
            })?;
        }
        Ok(())
    }
    /// Metadata voor een unsolicited exchange en diens beveiligde sessie.
    pub fn peer(&self, h: Handle) -> Result<(&str, u16, u16, u64)> {
        self.node.peer(h)
    }
    /// Levert één event aan de eigenaar; gebruik dit elke idle-tick voor subscriptions.
    pub fn event(&mut self) -> Option<Event> {
        self.inbox.pop_front().or_else(|| self.node.event())
    }
    /// Een gecanoniseerde nieuwe exchange; onversleuteld krijgt hij een verse ephemeral-ID.
    pub fn initiate<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        address: &str,
        session: u16,
        protocol: u16,
        retry: u64,
    ) -> Result<Handle> {
        let (address, v6) = canonical(address)?;
        if (v6 && self.ipv6.is_none()) || (!v6 && self.ipv4.is_none()) {
            return Err(Error::Transport("Matter address family unavailable"));
        }
        let ephemeral = if session == 0 {
            let r = c.random()?;
            let mut b = [0; 8];
            b.copy_from_slice(&r[..8]);
            (u64::from_le_bytes(b) & (i64::MAX as u64)).max(1)
        } else {
            0
        };
        self.node
            .initiate(&address, session, protocol, ephemeral, retry)
    }
    /// Verstuur betrouwbaar zonder synchronisch op de ACK te wachten.
    pub fn send<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        h: Handle,
        opcode: u8,
        payload: &[u8],
    ) -> Result {
        let jitter = self.jitter();
        self.node.send(h, opcode, payload, true, c.now(), jitter)?;
        self.flush(c)
    }
    /// Een volledig geauthenticeerd antwoord; andere exchanges blijven in de inbox behouden.
    pub async fn receive<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        h: Handle,
        deadline: u64,
    ) -> Result<Message> {
        loop {
            if c.now() >= deadline {
                return Err(Error::Timeout);
            }
            if let Some(i) = self
                .inbox
                .iter()
                .position(|e| matches!(e,Event::Message(id,_)|Event::Failed(id) if *id==h))
            {
                match self.inbox.remove(i) {
                    Some(Event::Message(_, m)) => return Ok(m),
                    Some(Event::Failed(_)) => return Err(Error::Timeout),
                    _ => return Err(Error::Invalid("Matter inbox mismatch")),
                }
            }
            self.tick(c)?;
            if self.no_route && canonical(self.node.peer(h)?.0)?.1 {
                self.no_route = false;
                return Err(Error::Transport("no IPv6 route"));
            }
            while let Some(event) = self.node.event() {
                match event {
                    Event::Message(id, m) if id == h => return Ok(m),
                    Event::Failed(id) if id == h => return Err(Error::Timeout),
                    Event::Acknowledged(id, _) if id == h => (),
                    e => {
                        self.reserve_inbox()?;
                        self.inbox.push_back(e);
                    }
                }
            }
            c.idle().await?;
        }
    }
    /// Wacht op de ACK vóór afsluiten; vroege antwoorden en andere exchanges blijven behouden.
    pub async fn wait_ack<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        h: Handle,
        deadline: u64,
    ) -> Result {
        while self.node.awaiting(h)? {
            if c.now() >= deadline {
                return Err(Error::Timeout);
            }
            self.tick(c)?;
            if self.no_route && canonical(self.node.peer(h)?.0)?.1 {
                self.no_route = false;
                return Err(Error::Transport("no IPv6 route"));
            }
            while let Some(event) = self.node.event() {
                match event {
                    Event::Failed(id) if id == h => return Err(Error::Timeout),
                    Event::Acknowledged(id, _) if id == h => (),
                    e => {
                        self.reserve_inbox()?;
                        self.inbox.push_back(e);
                    }
                }
            }
            if self.node.awaiting(h)? {
                c.idle().await?;
            }
        }
        Ok(())
    }
    /// Alleen een MRP-bevestiging. Het Interaction Model heeft soms daarnaast status nodig.
    pub fn acknowledge<T: Transport>(&mut self, c: &mut Client<T>, h: Handle) -> Result {
        self.node.acknowledge(h)?;
        self.flush(c)
    }
    /// Sluit de exchange, ook na fouten. Reeds gequeue-de ACKs blijven verzendbaar.
    pub fn close(&mut self, h: Handle) {
        self.node.close(h);
        self.inbox.retain(|e| match e {
            Event::Accepted(id)
            | Event::Message(id, _)
            | Event::Failed(id)
            | Event::Acknowledged(id, _) => *id != h,
        });
    }
    /// Trekt een sessie en alle bijbehorende uitstaande exchanges in.
    pub fn remove(&mut self, session: u16) -> Result {
        self.node.remove(session)?;
        let node = &self.node;
        self.inbox.retain(|event| {
            let h = match event {
                Event::Accepted(h)
                | Event::Message(h, _)
                | Event::Failed(h)
                | Event::Acknowledged(h, _) => *h,
            };
            node.peer(h).is_ok()
        });
        Ok(())
    }
    fn session_id<T: Transport>(&self, c: &mut Client<T>) -> Result<u16> {
        let r = c.random()?;
        let start = u16::from_le_bytes([r[0], r[1]]);
        for offset in 0..=65 {
            let id = start.wrapping_add(offset);
            if id != 0 && !self.node.has_session(id) {
                return Ok(id);
            }
        }
        Err(Error::Invalid("Matter session IDs unavailable"))
    }
    fn register<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        address: &str,
        session: pase::Session,
        local_node: u64,
        peer_node: u64,
        timing: Timing,
    ) -> Result<u16> {
        let seed = c.random()?;
        let id = session.local;
        self.node.register(
            mrp::Session {
                local: id,
                peer: session.peer,
                local_node,
                peer_node,
                address: canonical(address)?.0,
                outgoing: session.keys.i2r,
                incoming: session.keys.r2i,
                timing,
            },
            u32::from_le_bytes([seed[0], seed[1], seed[2], seed[3]]),
        )?;
        Ok(id)
    }
    /// On-network PASE; de caller bewaakt de volledige commissioning-deadline.
    pub async fn pase<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        address: &str,
        mut passcode: u32,
        timing: Timing,
        deadline: u64,
    ) -> Result<CommissioningSession> {
        let start = pase::Start::new(c.random()?, self.session_id(c)?)?;
        let h = self.initiate(c, address, 0, 0, timing.idle)?;
        let result = async {
            self.send(c, h, 0x20, start.request())?;
            let response = self.expected(c, h, 0x21, deadline).await?;
            self.acknowledge(c, h)?;
            let prepared = start.prepare(&response)?;
            let work = crate::spake::Pbkdf::new(passcode, &prepared.salt, prepared.iterations);
            zeroize::Zeroize::zeroize(&mut passcode);
            let mut work = work?;
            let mut last = c.now();
            let mut rounds = 0usize;
            while !work.step(1024)? {
                rounds += 1;
                if c.now() >= deadline {
                    return Err(Error::Timeout);
                }
                if rounds.is_multiple_of(4) || c.now().saturating_sub(last) >= 20 {
                    self.tick(c)?;
                    c.idle().await?;
                    last = c.now();
                }
            }
            let (proving, pake1) = prepared.prove(work.finish()?, c.random()?)?;

            self.send(c, h, 0x22, &pake1)?;
            let response = self.expected(c, h, 0x23, deadline).await?;
            let (confirming, pake3) = proving.finish(&response)?;
            self.send(c, h, 0x24, &pake3)?;
            let status = self.expected(c, h, 0x40, deadline).await?;
            self.acknowledge(c, h)?;
            let session = confirming.finish(&status)?;
            let challenge = zeroize::Zeroizing::new(session.keys.challenge);
            let session = self.register(c, address, session, 0, 0, timing)?;
            Ok(CommissioningSession { session, challenge })
        }
        .await;
        let _ = self.acknowledge(c, h);
        self.close(h);
        result
    }
    /// Operationele CASE gebruikt uitsluitend een bekende, door ons uitgegeven NOC.
    pub async fn case<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        fabric: &case::Fabric,
        peer: Peer<'_>,
        deadline: u64,
    ) -> Result<u16> {
        let start = case::Start::new(
            fabric,
            peer.node,
            peer.noc,
            self.session_id(c)?,
            c.random()?,
            c.random()?,
        )?;
        let h = self.initiate(c, peer.address, 0, 0, peer.timing.idle)?;
        let case0 = c.now();
        let result = async {
            // Elke P-256-stap krijgt een adempauze (`idle`: één beurt van het
            // transport, met hartslag), zodat het slot zijn core tussendoor
            // loslaat; één handshake in één poll hield op de LicheeRV de
            // controller honderden milliseconden stil (HopOS docs/apps.md).
            c.idle().await?;
            self.send(c, h, 0x30, start.request())?;
            let response = self.expected(c, h, 0x31, deadline).await?;
            let shared = start.shared(&response)?;
            c.idle().await?;
            shared.verify()?;
            c.idle().await?;
            let (confirming, sigma3) = shared.sign()?;
            self.send(c, h, 0x32, &sigma3)?;
            let status = self.expected(c, h, 0x40, deadline).await?;
            self.acknowledge(c, h)?;
            self.register(
                c,
                peer.address,
                confirming.finish(&status)?,
                fabric.node(),
                peer.node,
                peer.timing,
            )
        }
        .await;
        let _ = self.acknowledge(c, h);
        self.close(h);
        // De meetlat van de handshake op de node: duur per node en uitkomst.
        let mut line = String::new();
        if line.try_reserve(160).is_ok() {
            let _ = write!(
                line,
                "MATTER_CASE node={:016X} ms={} result={}",
                peer.node,
                c.now().saturating_sub(case0),
                match &result {
                    Ok(_) => String::from("ok"),
                    Err(e) => stulp_sdk::message(e).unwrap_or_else(|_| String::from("failed")),
                }
            );
            c.log("info", &line)?;
        }
        result
    }
    async fn expected<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        h: Handle,
        opcode: u8,
        deadline: u64,
    ) -> Result<Vec<u8>> {
        let message = self.receive(c, h, deadline).await?;
        if message.protocol.opcode != opcode {
            if message.protocol.opcode == 0x40 {
                let status = pase::Status::parse(&message.payload)?;
                let mut text = String::new();
                text.try_reserve(128)
                    .map_err(|_| stulp_core::Error::Memory)?;
                write!(text, "Matter handshake rejected: expected=0x{opcode:02x} general={} protocol={} status=0x{:04x}",
                    status.general, status.protocol, status.code)
                    .map_err(|_| Error::Invalid("Matter status formatting"))?;
                return Err(Error::Remote(text));
            }
            return Err(Error::Invalid("unexpected Matter handshake response"));
        }
        Ok(message.payload)
    }
}
/// Alles wat een bestaande pairing nodig heeft voor de operationele verbinding.
pub struct Peer<'a> {
    /// Canoniek UDP-adres.
    pub address: &'a str,
    /// Operationeel node-ID.
    pub node: u64,
    /// Exact tijdens commissioning opgeslagen NOC.
    pub noc: &'a [u8],
    /// Timing uit DNS-SD.
    pub timing: Timing,
}

/// PASE bewaart de challenge tot attestation en CSR afgerond zijn.
pub struct CommissioningSession {
    /// Lokaal beveiligd sessie-ID.
    pub session: u16,
    /// Geheime sessiebinding; Drop wist de challenge.
    pub challenge: zeroize::Zeroizing<[u8; 16]>,
}
