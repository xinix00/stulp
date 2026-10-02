//! Eén eigenaar voor sessies en exchanges; retries bewaren exact dezelfde ciphertext.
use crate::message::{Header, MAX_UDP, Message, Protocol};
use alloc::{collections::VecDeque, string::String, vec::Vec};
use stulp_core::json;
use stulp_sdk::{Error, Result};
use zeroize::Zeroize;
const MAX_SESSIONS: usize = 64;
const MAX_EXCHANGES: usize = 128;
const MAX_EVENTS: usize = 64;
const MAX_OUT: usize = 64;
/// Peer-timing uit DNS-SD of sessieonderhandeling, in milliseconden.
#[derive(Clone, Copy, Default)]
pub struct Timing {
    /// Interval wanneer de peer kan slapen.
    pub idle: u64,
    /// Interval nadat geauthenticeerd verkeer de peer wakker bewees.
    pub active: u64,
    /// Hoe lang het actieve interval geldt.
    pub threshold: u64,
}
impl Timing {
    fn base(&self, last: Option<u64>, now: u64) -> u64 {
        if self.active > 0
            && self.threshold > 0
            && last.is_some_and(|last| now.saturating_sub(last) < self.threshold)
        {
            self.active
        } else if self.idle > 0 {
            self.idle
        } else if self.active > 0 {
            self.active
        } else {
            500
        }
    }
}
/// Een nieuw onderhandelde sessie; Drop wist beide sleutels.
pub struct Session {
    /// Lokale ontvangende ID, uniek binnen de node.
    pub local: u16,
    /// Ontvangende ID van de peer.
    pub peer: u16,
    /// Operationele bron voor encryptie; nul bij PASE.
    pub local_node: u64,
    /// Operationele bron voor decryptie; nul bij PASE.
    pub peer_node: u64,
    /// Canoniek socketadres, inclusief IPv6-scope wanneer nodig.
    pub address: String,
    /// Uitgaande sessiesleutel.
    pub outgoing: [u8; 16],
    /// Inkomende sessiesleutel.
    pub incoming: [u8; 16],
    /// MRP-timing van deze peer.
    pub timing: Timing,
}
impl Drop for Session {
    fn drop(&mut self) {
        self.outgoing.zeroize();
        self.incoming.zeroize();
    }
}
struct Secure {
    config: Session,
    counter: u32,
    replay: Replay,
    last: Option<u64>,
}
/// Exact één datagram; alleen de platformadapter bezit de UDP-socket.
pub struct Outgoing {
    /// Lokale sessie-ID voor intrekken van nog niet verzonden pakketten.
    pub session: u16,
    /// Canoniek peeradres.
    pub address: String,
    /// Hoogstens de Matter UDP-MTU.
    pub bytes: Vec<u8>,
}
/// Een exchange is privé benoemd, los van het herbruikbare wire-ID.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Handle(u64);
/// Berichten voor commissioning, CASE en het Interaction Model.
pub enum Event {
    /// De peer opende een nieuwe exchange.
    Accepted(Handle),
    /// Eenmaal bezorgde protocolinhoud.
    Message(Handle, Message),
    /// Een betrouwbare zending is bevestigd.
    Acknowledged(Handle, u32),
    /// De sessie verviel of de retrylimiet werd bereikt.
    Failed(Handle),
}
struct Pending {
    counter: u32,
    bytes: Vec<u8>,
    attempt: u8,
    next: u64,
}
struct Exchange {
    handle: Handle,
    remote: String,
    session: u16,
    id: u16,
    initiator: bool,
    protocol: u16,
    ephemeral: u64,
    replay: Replay,
    ack: Option<u32>,
    pending: Option<Pending>,
    retry: u64,
}
/// Begrenst oude en dubbele berichten op 64 counters; alleen na authenticatie gebruiken.
#[derive(Default)]
pub struct Replay {
    maximum: Option<u32>,
    seen: u64,
}
impl Replay {
    /// True betekent dubbel of te oud; de sessie moet vóór counter-wrap verlopen.
    pub fn mark(&mut self, counter: u32) -> bool {
        let Some(max) = self.maximum else {
            self.maximum = Some(counter);
            self.seen = 1;
            return false;
        };
        if counter > max {
            let d = counter - max;
            self.seen = if d >= 64 { 1 } else { self.seen << d | 1 };
            self.maximum = Some(counter);
            return false;
        }
        let d = max - counter;
        if d >= 64 {
            return true;
        }
        let bit = 1u64 << d;
        if self.seen & bit != 0 {
            return true;
        }
        self.seen |= bit;
        false
    }
}
/// Bevat alle protocolstaat, zonder locks of taken per exchange.
pub struct Node {
    sessions: Vec<Secure>,
    exchanges: Vec<Exchange>,
    out: VecDeque<Outgoing>,
    events: VecDeque<Event>,
    counter: u32,
    next_handle: u64,
    next_exchange: u16,
}
fn queue<T>(q: &mut VecDeque<T>, item: T, max: usize) -> Result {
    if q.len() >= max {
        return Err(Error::Invalid("Matter transport queue full"));
    }
    q.try_reserve(1).map_err(|_| stulp_core::Error::Memory)?;
    q.push_back(item);
    Ok(())
}
fn increment(counter: &mut u32) -> Result<u32> {
    *counter = counter.checked_add(1).ok_or(Error::Invalid(
        "Matter counter exhausted; establish a new session",
    ))?;
    Ok(*counter)
}
fn interval(base: u64, attempt: u8, jitter: u32) -> u64 {
    // 1.1 marge, exponent pas na de eerste retry, maximaal 25% jitter.
    let mut n = base.saturating_mul(1100) / 1000;
    for _ in 1..attempt {
        n = n.saturating_mul(16) / 10;
    }
    n.saturating_add(n.saturating_mul(u64::from(jitter)) / u64::from(u32::MAX) / 4)
        .max(1)
}
impl Node {
    /// Verse seeds komen van platformentropy; de hoge counterbits blijven vrij.
    pub fn new(counter_seed: u32, exchange_seed: u16) -> Self {
        Self {
            sessions: Vec::new(),
            exchanges: Vec::new(),
            out: VecDeque::new(),
            events: VecDeque::new(),
            counter: counter_seed & 0x0fff_ffff,
            next_handle: 0,
            next_exchange: exchange_seed,
        }
    }
    /// Installeert een sessie pas nadat de handshake die bevestigde.
    pub fn register(&mut self, config: Session, counter_seed: u32) -> Result {
        if config.local == 0
            || config.peer == 0
            || config.address.is_empty()
            || self.sessions.iter().any(|s| s.config.local == config.local)
        {
            return Err(Error::Invalid("invalid or duplicate Matter session"));
        }
        json::push(
            &mut self.sessions,
            Secure {
                config,
                counter: counter_seed & 0x0fff_ffff,
                replay: Replay::default(),
                last: None,
            },
            MAX_SESSIONS,
        )?;
        Ok(())
    }
    /// Verlopen sleutels en alle bijbehorende exchanges worden samen ingetrokken.
    pub fn remove(&mut self, local: u16) -> Result {
        let count = self.exchanges.iter().filter(|e| e.session == local).count();
        let belongs = |event: &Event| {
            let handle = match event {
                Event::Accepted(h)
                | Event::Message(h, _)
                | Event::Failed(h)
                | Event::Acknowledged(h, _) => h,
            };
            self.exchanges
                .iter()
                .any(|e| e.handle == *handle && e.session == local)
        };
        let retained = self.events.iter().filter(|e| !belongs(e)).count();
        if count > MAX_EVENTS - retained {
            return Err(Error::Invalid("Matter event queue full"));
        }
        self.events
            .try_reserve(count)
            .map_err(|_| stulp_core::Error::Memory)?;
        self.events.retain(|e| !belongs(e));
        self.sessions.retain(|s| s.config.local != local);
        self.out.retain(|p| p.session != local);
        for i in (0..self.exchanges.len()).rev() {
            if self.exchanges[i].session == local {
                let e = self.exchanges.remove(i);
                self.events.push_back(Event::Failed(e.handle));
            }
        }
        Ok(())
    }
    fn handle(&mut self) -> Result<Handle> {
        self.next_handle = self
            .next_handle
            .checked_add(1)
            .ok_or(Error::Invalid("Matter handles exhausted"))?;
        Ok(Handle(self.next_handle))
    }
    fn position(&self, h: Handle) -> Result<usize> {
        self.exchanges
            .iter()
            .position(|e| e.handle == h)
            .ok_or(Error::Invalid("Matter exchange closed"))
    }
    /// Controleert of een lokaal sessie-ID vrij is vóór het onderhandelen.
    pub fn has_session(&self, local: u16) -> bool {
        self.sessions.iter().any(|s| s.config.local == local)
    }
    /// Opent een lokale exchange. Session 0 vereist een verse operationele ephemeral-ID.
    pub fn initiate(
        &mut self,
        address: &str,
        session: u16,
        protocol: u16,
        ephemeral: u64,
        retry_ms: u64,
    ) -> Result<Handle> {
        if session == 0 && (ephemeral == 0 || ephemeral > 0xffff_ffef_ffff_ffff) {
            return Err(Error::Invalid("invalid ephemeral initiator ID"));
        }
        if session != 0
            && !self
                .sessions
                .iter()
                .any(|s| s.config.local == session && s.config.address == address)
        {
            return Err(Error::Invalid("Matter session missing for peer"));
        }
        let mut id = None;
        for _ in 0..=MAX_EXCHANGES {
            self.next_exchange = self.next_exchange.wrapping_add(1);
            if !self.exchanges.iter().any(|e| {
                e.initiator
                    && e.id == self.next_exchange
                    && e.remote == address
                    && e.session == session
            }) {
                id = Some(self.next_exchange);
                break;
            }
        }
        let id = id.ok_or(Error::Invalid("Matter exchange IDs unavailable"))?;
        let handle = self.handle()?;
        json::push(
            &mut self.exchanges,
            Exchange {
                handle,
                remote: json::copy(address)?,
                session,
                id,
                initiator: true,
                protocol,
                ephemeral,
                replay: Replay::default(),
                ack: None,
                pending: None,
                retry: retry_ms,
            },
            MAX_EXCHANGES,
        )?;
        Ok(handle)
    }
    /// Nog niet bevestigde betrouwbare zending binnen één bestaande exchange.
    pub fn awaiting(&self, h: Handle) -> Result<bool> {
        Ok(self.exchanges[self.position(h)?].pending.is_some())
    }
    /// Laat de eigenaar weten welke peer en sessie een inkomende exchange gebruikt.
    pub fn peer(&self, h: Handle) -> Result<(&str, u16, u16, u64)> {
        let e = &self.exchanges[self.position(h)?];
        let node = self
            .sessions
            .iter()
            .find(|s| s.config.local == e.session)
            .map(|s| s.config.peer_node)
            .unwrap_or(0);
        Ok((&e.remote, e.session, e.protocol, node))
    }
    /// Sluiten bevestigt geen nog onbevestigde zending.
    pub fn close(&mut self, h: Handle) {
        self.exchanges.retain(|e| e.handle != h);
    }
    /// Neemt één datagram over; de adapter meldt eventuele socketfouten aan de eigenaar.
    pub fn outgoing(&mut self) -> Option<Outgoing> {
        self.out.pop_front()
    }
    /// Neemt één protocolgebeurtenis over.
    pub fn event(&mut self) -> Option<Event> {
        self.events.pop_front()
    }
    fn build(
        &mut self,
        index: usize,
        protocol: u16,
        opcode: u8,
        payload: &[u8],
        reliable: bool,
    ) -> Result<(Vec<u8>, u32)> {
        let e = &self.exchanges[index];
        let secure = self
            .sessions
            .iter_mut()
            .find(|s| s.config.local == e.session);
        let counter = if let Some(s) = secure.as_ref() {
            s.counter
                .checked_add(1)
                .ok_or(Error::Invalid("Matter counter exhausted"))?
        } else {
            self.counter
                .checked_add(1)
                .ok_or(Error::Invalid("Matter counter exhausted"))?
        };
        let mut m = Message {
            header: Header {
                counter,
                ..Header::default()
            },
            protocol: Protocol {
                initiator: e.initiator,
                reliable,
                ack: e.ack,
                opcode,
                exchange: e.id,
                protocol,
                ..Protocol::default()
            },
            payload: crate::copy(payload)?,
        };
        let frame = if let Some(s) = secure {
            increment(&mut s.counter)?;
            m.header.session = s.config.peer;
            m.seal(&s.config.outgoing, s.config.local_node)?
        } else {
            if e.session != 0 {
                return Err(Error::Invalid("Matter session expired"));
            }
            increment(&mut self.counter)?;
            if e.initiator {
                m.header.source = Some(e.ephemeral);
            } else {
                m.header.destination = Some(e.ephemeral);
            }
            m.encode()?
        };
        if frame.len() > MAX_UDP {
            return Err(Error::Invalid("Matter datagram exceeds IPv6 UDP budget"));
        }
        Ok((frame, counter))
    }
    fn base(&self, index: usize, now: u64) -> u64 {
        let e = &self.exchanges[index];
        if e.retry > 0 {
            return e.retry;
        }
        self.sessions
            .iter()
            .find(|s| s.config.local == e.session)
            .map(|s| s.config.timing.base(s.last, now))
            .unwrap_or(500)
    }
    /// Bewaart betrouwbare bytes voor maximaal vijf zendingen met dezelfde counter.
    pub fn send(
        &mut self,
        h: Handle,
        opcode: u8,
        payload: &[u8],
        reliable: bool,
        now: u64,
        jitter: u32,
    ) -> Result {
        let i = self.position(h)?;
        if self.exchanges[i].pending.is_some() {
            return Err(Error::Invalid(
                "Matter exchange already awaits acknowledgement",
            ));
        }
        let protocol = self.exchanges[i].protocol;
        let (bytes, counter) = self.build(i, protocol, opcode, payload, reliable)?;
        let pending = if reliable {
            Some(Pending {
                counter,
                bytes: crate::copy(&bytes)?,
                attempt: 0,
                next: now.saturating_add(interval(self.base(i, now), 0, jitter)),
            })
        } else {
            None
        };
        queue(
            &mut self.out,
            Outgoing {
                session: self.exchanges[i].session,
                address: json::copy(&self.exchanges[i].remote)?,
                bytes,
            },
            MAX_OUT,
        )?;
        self.exchanges[i].pending = pending;
        self.exchanges[i].ack = None;
        Ok(())
    }
    /// MRP-acks gebruiken SecureChannel, ook op een InteractionModel-exchange.
    pub fn acknowledge(&mut self, h: Handle) -> Result {
        let i = self.position(h)?;
        if self.exchanges[i].ack.is_none() {
            return Ok(());
        }
        let (bytes, _) = self.build(i, 0, 0x10, &[], false)?;
        queue(
            &mut self.out,
            Outgoing {
                session: self.exchanges[i].session,
                address: json::copy(&self.exchanges[i].remote)?,
                bytes,
            },
            MAX_OUT,
        )?;
        self.exchanges[i].ack = None;
        Ok(())
    }
    /// Verwerkt één UDP-pakket; mislukte authenticatie verandert geen replay- of timingstaat.
    pub fn receive(&mut self, address: &str, bytes: &[u8], now: u64) -> Result {
        if bytes.len() > MAX_UDP {
            return Err(Error::Invalid("oversized Matter datagram"));
        }
        if self.events.len() > MAX_EVENTS - 3 || self.out.len() >= MAX_OUT {
            return Err(Error::Invalid("Matter transport backpressure"));
        }
        self.events
            .try_reserve(3)
            .map_err(|_| stulp_core::Error::Memory)?;
        let (header, _) = Header::peek(bytes)?;
        if header.kind != 0 || header.control {
            return Err(Error::Invalid(
                "Matter group and counter-control messages unsupported",
            ));
        }
        let local = header.session;
        let secure = self
            .sessions
            .iter()
            .position(|s| s.config.local == local && s.config.address == address);
        let message = if local == 0 {
            Message::parse(bytes)?
        } else {
            let s = &self.sessions
                [secure.ok_or(Error::Invalid("unknown Matter secure session or address"))?];
            let m = Message::open(bytes, &s.config.incoming, s.config.peer_node)?;
            if m.header
                .destination
                .is_some_and(|n| n != s.config.local_node)
            {
                return Err(Error::Invalid("Matter destination differs from session"));
            }
            m
        };
        let p = &message.protocol;
        if p.vendor.is_some() {
            return Err(Error::Invalid(
                "vendor-specific Matter protocols unsupported",
            ));
        }
        if local == 0 && message.header.source.is_some() == message.header.destination.is_some() {
            return Err(Error::Invalid(
                "unsecured Matter message needs one ephemeral ID",
            ));
        }
        let found = self.exchanges.iter().position(|e| {
            e.remote == address
                && e.session == local
                && e.id == p.exchange
                && e.initiator != p.initiator
        });
        // Reserveren vóór replay-publicatie: een volle tabel mag een later herhaald
        // bericht niet als reeds bezorgd vastleggen.
        let new_address =
            if found.is_none() && p.initiator && !(p.opcode == 0x10 && p.protocol == 0) {
                if self.exchanges.len() >= MAX_EXCHANGES {
                    return Err(Error::Invalid("Matter exchange table full"));
                }
                self.exchanges
                    .try_reserve(1)
                    .map_err(|_| stulp_core::Error::Memory)?;
                Some(json::copy(address)?)
            } else {
                None
            };
        let duplicate = if let Some(i) = secure {
            let s = &mut self.sessions[i];
            let duplicate = s.replay.mark(message.header.counter);
            s.last = Some(now);
            duplicate
        } else {
            false
        };
        let mut accepted = false;
        let i = if let Some(i) = found {
            i
        } else {
            if duplicate || !p.initiator || p.opcode == 0x10 && p.protocol == 0 {
                return self.late_ack(address, &message);
            }
            let ephemeral = message.header.source.unwrap_or(0);
            if local == 0 && (ephemeral == 0 || ephemeral > 0xffff_ffef_ffff_ffff) {
                return Err(Error::Invalid("invalid peer ephemeral ID"));
            }
            let handle = self.handle()?;
            json::push(
                &mut self.exchanges,
                Exchange {
                    handle,
                    remote: new_address
                        .ok_or(Error::Invalid("Matter new exchange address missing"))?,
                    session: local,
                    id: p.exchange,
                    initiator: false,
                    protocol: p.protocol,
                    ephemeral,
                    replay: Replay::default(),
                    ack: None,
                    pending: None,
                    retry: 0,
                },
                MAX_EXCHANGES,
            )?;
            accepted = true;
            self.exchanges.len() - 1
        };
        let e = &mut self.exchanges[i];
        if p.protocol != e.protocol && !(p.protocol == 0 && p.opcode == 0x10) {
            return Err(Error::Invalid("message belongs to another Matter protocol"));
        }
        if local == 0 {
            let addressed = if e.initiator {
                message.header.destination
            } else {
                message.header.source
            };
            if addressed != Some(e.ephemeral) {
                return Err(Error::Invalid(
                    "unsecured message addresses another initiator",
                ));
            }
        }
        if accepted {
            self.events.push_back(Event::Accepted(e.handle));
        }
        if let Some(counter) = p.ack
            && e.pending.as_ref().is_some_and(|w| w.counter == counter)
        {
            e.pending = None;
            self.events
                .push_back(Event::Acknowledged(e.handle, counter));
        }
        let duplicate = if local == 0 {
            e.replay.mark(message.header.counter)
        } else {
            duplicate
        };
        if p.reliable {
            e.ack = Some(message.header.counter);
        }
        let handle = e.handle;
        if p.protocol == 0 && p.opcode == 0x10 {
            return Ok(());
        }
        if duplicate {
            self.acknowledge(handle)?;
        } else {
            self.events.push_back(Event::Message(handle, message));
        }
        Ok(())
    }
    fn late_ack(&mut self, address: &str, m: &Message) -> Result {
        if m.header.session == 0 || !m.protocol.reliable {
            return Ok(());
        }
        let s = self
            .sessions
            .iter_mut()
            .find(|s| s.config.local == m.header.session && s.config.address == address)
            .ok_or(Error::Invalid("Matter session missing"))?;
        let ack = Message {
            header: Header {
                session: s.config.peer,
                counter: increment(&mut s.counter)?,
                ..Header::default()
            },
            protocol: Protocol {
                initiator: !m.protocol.initiator,
                ack: Some(m.header.counter),
                opcode: 0x10,
                exchange: m.protocol.exchange,
                protocol: 0,
                ..Protocol::default()
            },
            payload: Vec::new(),
        };
        let bytes = ack.seal(&s.config.outgoing, s.config.local_node)?;
        queue(
            &mut self.out,
            Outgoing {
                session: m.header.session,
                address: json::copy(address)?,
                bytes,
            },
            MAX_OUT,
        )
    }
    /// Stuurt hoogstens één retry per ronde en herberekent of een slaperige peer wakker is.
    pub fn tick(&mut self, now: u64, jitter: u32) -> Result {
        let Some(i) = self
            .exchanges
            .iter()
            .position(|e| e.pending.as_ref().is_some_and(|p| p.next <= now))
        else {
            return Ok(());
        };
        let e = &self.exchanges[i];
        let p = e
            .pending
            .as_ref()
            .ok_or(Error::Invalid("Matter retry missing"))?;
        if p.attempt >= 4 {
            let h = e.handle;
            queue(&mut self.events, Event::Failed(h), MAX_EVENTS)?;
            self.exchanges.remove(i);
            return Ok(());
        }
        let next = now.saturating_add(interval(self.base(i, now), p.attempt + 1, jitter));
        queue(
            &mut self.out,
            Outgoing {
                session: e.session,
                address: json::copy(&e.remote)?,
                bytes: crate::copy(&p.bytes)?,
            },
            MAX_OUT,
        )?;
        if let Some(p) = self.exchanges[i].pending.as_mut() {
            p.attempt += 1;
            p.next = next;
        }
        Ok(())
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    fn pop(n: &mut Node) -> Result<Outgoing> {
        n.outgoing().ok_or(Error::Invalid("test datagram missing"))
    }
    fn accepted(n: &mut Node) -> Result<Handle> {
        match n.event() {
            Some(Event::Accepted(h)) => Ok(h),
            _ => Err(Error::Invalid("test acceptance missing")),
        }
    }
    fn secure_pair() -> Result<(Node, Node)> {
        let mut a = Node::new(1, 0);
        let mut b = Node::new(2, 0);
        a.register(
            Session {
                local: 1,
                peer: 2,
                local_node: 11,
                peer_node: 22,
                address: json::copy("b")?,
                outgoing: [1; 16],
                incoming: [2; 16],
                timing: Timing {
                    idle: 17000,
                    active: 300,
                    threshold: 4000,
                },
            },
            3,
        )?;
        b.register(
            Session {
                local: 2,
                peer: 1,
                local_node: 22,
                peer_node: 11,
                address: json::copy("a")?,
                outgoing: [2; 16],
                incoming: [1; 16],
                timing: Timing::default(),
            },
            4,
        )?;
        Ok((a, b))
    }
    #[test]
    fn unsecured_duplicate_is_acked_once_without_redelivery() -> Result {
        let mut a = Node::new(123, 0);
        let mut b = Node::new(456, 0);
        let h = a.initiate("b", 0, 0, 99, 10)?;
        a.send(h, 0x20, b"request", true, 0, 0)?;
        let packet = pop(&mut a)?;
        b.receive("a", &packet.bytes, 0)?;
        let peer = accepted(&mut b)?;
        assert!(matches!(b.event(),Some(Event::Message(_,m)) if m.payload==b"request"));
        b.receive("a", &packet.bytes, 1)?;
        assert!(b.event().is_none());
        let ack = pop(&mut b)?;
        let decoded = Message::parse(&ack.bytes)?;
        assert_eq!(decoded.header.destination, Some(99));
        assert_eq!(decoded.protocol.protocol, 0);
        assert_eq!(decoded.protocol.opcode, 0x10);
        a.receive("b", &ack.bytes, 2)?;
        assert!(matches!(a.event(),Some(Event::Acknowledged(id,_)) if id==h));
        a.tick(100, 0)?;
        assert!(a.outgoing().is_none());
        b.close(peer);
        Ok(())
    }
    #[test]
    fn ciphertext_retries_are_identical_and_stop_after_five_transmissions() -> Result {
        let (mut a, _) = secure_pair()?;
        let h = a.initiate("b", 1, 1, 0, 1)?;
        a.send(h, 2, b"read", true, 0, 0)?;
        let initial = pop(&mut a)?.bytes;
        for t in [10, 20, 30, 40] {
            a.tick(t, 0)?;
            assert_eq!(pop(&mut a)?.bytes, initial);
        }
        a.tick(50, 0)?;
        assert!(matches!(a.event(),Some(Event::Failed(id)) if id==h));
        assert!(a.outgoing().is_none());
        assert!(a.send(h, 2, b"read", true, 60, 0).is_err());
        Ok(())
    }
    #[test]
    fn authentication_precedes_replay_and_timing_and_late_replies_get_secure_ack() -> Result {
        let (mut a, mut b) = secure_pair()?;
        let h = a.initiate("b", 1, 1, 0, 0)?;
        a.send(h, 2, b"read", true, 0, 0)?;
        let packet = pop(&mut a)?;
        b.receive("a", &packet.bytes, 0)?;
        let hb = accepted(&mut b)?;
        b.event();
        b.send(hb, 5, b"report", true, 10, 0)?;
        let mut reply = pop(&mut b)?.bytes;
        reply[4] ^= 0x40;
        assert!(a.receive("b", &reply, 10).is_err());
        assert!(a.sessions[0].last.is_none());
        assert!(a.sessions[0].replay.maximum.is_none());
        reply[4] ^= 0x40;
        a.receive("b", &reply, 11)?;
        assert!(matches!(a.event(), Some(Event::Acknowledged(_, _))));
        assert!(matches!(a.event(),Some(Event::Message(_,m)) if m.payload==b"report"));
        assert_eq!(
            a.sessions[0].config.timing.base(a.sessions[0].last, 12),
            300
        );
        a.close(h);
        a.receive("b", &reply, 12)?;
        let ack = pop(&mut a)?;
        let decoded = Message::open(&ack.bytes, &[1; 16], 11)?;
        assert_eq!(decoded.protocol.opcode, 0x10);
        assert_eq!(decoded.protocol.protocol, 0);
        assert!(decoded.protocol.ack.is_some());
        assert!(a.event().is_none());
        Ok(())
    }
    #[test]
    fn peer_roles_separate_equal_exchange_ids_and_expiry_revokes_queued_bytes() -> Result {
        let (mut a, mut b) = secure_pair()?;
        let ha = a.initiate("b", 1, 1, 0, 0)?;
        let hb = b.initiate("a", 2, 1, 0, 0)?;
        a.send(ha, 2, b"a", true, 0, 0)?;
        b.send(hb, 2, b"b", true, 0, 0)?;
        let pa = pop(&mut a)?;
        let pb = pop(&mut b)?;
        a.receive("b", &pb.bytes, 1)?;
        b.receive("a", &pa.bytes, 1)?;
        let peer = accepted(&mut a)?;
        assert_ne!(peer, ha);
        assert_eq!(a.exchanges.len(), 2);
        a.send(peer, 5, b"answer", true, 2, 0)?;
        a.remove(1)?;
        assert!(a.outgoing().is_none());
        assert!(a.sessions.is_empty());
        assert!(a.exchanges.is_empty());
        while let Some(event) = a.event() {
            assert!(matches!(event, Event::Failed(_)));
        }
        let mut r = Replay::default();
        assert!(!r.mark(100));
        assert!(!r.mark(99));
        assert!(r.mark(99));
        assert!(r.mark(35));
        assert!(!r.mark(200));
        assert!(r.mark(100));
        Ok(())
    }
}
