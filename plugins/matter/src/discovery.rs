//! Begrensde DNS-SD-codec en tijdelijke mDNS-cache, onafhankelijk van platforminterfaces.
use crate::{mrp::Timing, onboarding::Payload};
use alloc::{string::String, vec::Vec};
use core::{
    fmt::Write,
    net::{IpAddr, Ipv4Addr, Ipv6Addr},
};
use stulp_core::json;
use stulp_sdk::{Error, Result, util::join};
/// Matter-apparaten met een geopend commissioning window.
pub const COMMISSIONABLE: &str = "_matterc._udp.local.";
/// Reeds gekoppelde operationele nodes; _tcp is de advertentienaam, verkeer gebruikt UDP.
pub const OPERATIONAL: &str = "_matter._tcp.local.";
/// Thread border routers publiceren dit service-type.
pub const BORDER_ROUTER: &str = "_meshcop._udp.local.";
const MAX_RECORDS: usize = 1024;
fn invalid<T>() -> Result<T> {
    Err(Error::Invalid("invalid mDNS packet"))
}
fn append(out: &mut String, text: &str) -> Result {
    if text.len() > 1024usize.saturating_sub(out.len()) {
        return invalid();
    }
    out.try_reserve(text.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    out.push_str(text);
    Ok(())
}
fn octets<'a>(packet: &'a [u8], at: &mut usize, len: usize) -> Result<&'a [u8]> {
    let end = at
        .checked_add(len)
        .ok_or(Error::Invalid("DNS length overflow"))?;
    let b = packet
        .get(*at..end)
        .ok_or(Error::Invalid("truncated DNS field"))?;
    *at = end;
    Ok(b)
}
fn u16be(packet: &[u8], at: &mut usize) -> Result<u16> {
    let b = octets(packet, at, 2)?;
    Ok(u16::from_be_bytes([b[0], b[1]]))
}
fn u32be(packet: &[u8], at: &mut usize) -> Result<u32> {
    let b = octets(packet, at, 4)?;
    Ok(u32::from_be_bytes([b[0], b[1], b[2], b[3]]))
}
// Escapes voorkomen dat een punt in een DNS-label met een labelgrens verward wordt.
fn name(packet: &[u8], at: &mut usize) -> Result<String> {
    let mut out = String::new();
    let mut pos = *at;
    let mut jumped = false;
    let mut length = 1usize;
    for _ in 0..128 {
        let here = pos;
        let first = *packet
            .get(pos)
            .ok_or(Error::Invalid("truncated DNS name"))?;
        pos += 1;
        if first & 0xc0 == 0xc0 {
            let next = *packet
                .get(pos)
                .ok_or(Error::Invalid("truncated DNS pointer"))?;
            pos += 1;
            let target = (usize::from(first & 63) << 8) | usize::from(next);
            if target >= here {
                return Err(Error::Invalid("DNS compression pointer is not backwards"));
            }
            if !jumped {
                *at = pos;
                jumped = true;
            }
            pos = target;
            continue;
        }
        if first & 0xc0 != 0 {
            return invalid();
        }
        if first == 0 {
            if !jumped {
                *at = pos;
            }
            if out.is_empty() {
                append(&mut out, ".")?;
            }
            return Ok(out);
        }
        length += usize::from(first) + 1;
        if length > 255 {
            return invalid();
        }
        let label = octets(packet, &mut pos, usize::from(first))?;
        for b in label {
            if !(32..=126).contains(b) || *b == b'.' || *b == b'\\' {
                let digits = [b'\\', b'0' + b / 100, b'0' + b / 10 % 10, b'0' + b % 10];
                append(
                    &mut out,
                    core::str::from_utf8(&digits).map_err(|_| Error::Invalid("DNS escape"))?,
                )?;
            } else {
                let text = [*b];
                append(
                    &mut out,
                    core::str::from_utf8(&text).map_err(|_| Error::Invalid("DNS label"))?,
                )?;
            }
        }
        append(&mut out, ".")?;
    }
    Err(Error::Invalid("too many DNS compression steps"))
}
fn unescape(name: &str) -> Result<String> {
    let mut bytes = Vec::new();
    bytes
        .try_reserve(name.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    let mut i = 0;
    let input = name.as_bytes();
    while i < input.len() {
        if input[i] == b'\\'
            && i + 3 < input.len()
            && input[i + 1..i + 4].iter().all(u8::is_ascii_digit)
        {
            let n = u16::from(input[i + 1] - b'0') * 100
                + u16::from(input[i + 2] - b'0') * 10
                + u16::from(input[i + 3] - b'0');
            if n > 255 {
                return invalid();
            }
            bytes.push(n as u8);
            i += 4;
        } else {
            bytes.push(input[i]);
            i += 1;
        }
    }
    String::from_utf8(bytes).map_err(|_| Error::Invalid("DNS-SD instance name is not UTF-8"))
}
/// Een legacy multicastquery krijgt vanaf een ephemeral UDP-poort een unicastantwoord.
pub fn query(services: &[&str]) -> Result<Vec<u8>> {
    let mut questions = Vec::new();
    for service in services {
        json::push(&mut questions, (*service, PTR), 12)?;
    }
    query_records(&questions)
}
const PTR: u16 = 12;
const TXT: u16 = 16;
const AAAA: u16 = 28;
const SRV: u16 = 33;
/// Hoogstens zoveel namen per vervolgvraag; past ruim in één datagram.
const FOLLOW_UP_NAMES: usize = 24;
/// Het grootste aantal vragen, van voren af, dat nog in één datagram past;
/// SRV en TXT van dezelfde instantie blijven bij elkaar.
fn fitting(questions: &[(&str, u16)]) -> Result<usize> {
    let mut n = questions.len().min(FOLLOW_UP_NAMES);
    while n > 0 {
        if query_records(&questions[..n]).is_ok() {
            return Ok(n);
        }
        n -= if n >= 2 && questions[n - 1].1 == TXT && questions[n - 2].1 == SRV {
            2
        } else {
            1
        };
    }
    Err(Error::Invalid("no DNS question fits the datagram budget"))
}
/// Eén query met per naam een eigen recordtype: PTR voor de dienst, daarna
/// SRV/TXT per instantie en AAAA per host. De HomePod-proxy beantwoordt een
/// PTR-vraag zonder die records erbij (14-10: na een prefix-wissel kende
/// Stulp daardoor geen enkel nieuw adres), dus die worden apart gevraagd.
pub fn query_records(questions: &[(&str, u16)]) -> Result<Vec<u8>> {
    if questions.is_empty() || questions.len() > FOLLOW_UP_NAMES {
        return Err(Error::Invalid("DNS query requires 1..24 questions"));
    }
    let mut out = Vec::new();
    crate::append(&mut out, &[0; 12])?;
    out[5] = questions.len() as u8;
    for (service, kind) in questions {
        let text = service.trim_end_matches('.');
        if text.is_empty() || text.len() > 253 {
            return invalid();
        }
        for label in text.split('.') {
            if label.is_empty()
                || label.len() > 63
                || !label.is_ascii()
                || label.bytes().any(|b| b < 33 || b == b'\\')
            {
                return invalid();
            }
            crate::append(&mut out, &[label.len() as u8])?;
            crate::append(&mut out, label.as_bytes())?;
        }
        crate::append(&mut out, &[0])?;
        crate::append(&mut out, &kind.to_be_bytes())?;
        crate::append(&mut out, &[0, 1])?;
    }
    if out.len() > 1232 {
        return Err(Error::Invalid("mDNS query exceeds datagram budget"));
    }
    Ok(out)
}
enum Record {
    Ptr(String),
    Srv { host: String, port: u16 },
    Txt(Vec<(String, Vec<u8>)>),
    Address(IpAddr, u32),
}
struct Entry {
    owner: String,
    record: Record,
    expires: u64,
}
fn same(a: &Entry, b: &Entry) -> bool {
    if !a.owner.eq_ignore_ascii_case(&b.owner) {
        return false;
    }
    match (&a.record, &b.record) {
        (Record::Ptr(a), Record::Ptr(b)) => a.eq_ignore_ascii_case(b),
        (Record::Srv { .. }, Record::Srv { .. }) | (Record::Txt(_), Record::Txt(_)) => true,
        (Record::Address(a, x), Record::Address(b, y)) => a == b && x == y,
        _ => false,
    }
}
/// Cache per zoekronde; malformed packets worden volledig afgewezen vóór mutatie.
#[derive(Default)]
pub struct Collector {
    records: Vec<Entry>,
}
impl Collector {
    /// Behoudt link-local IPv6-scopes uit de ontvangende interface; tijden zijn monotone milliseconden.
    pub fn consume(&mut self, packet: &[u8], scope: u32, now: u64) -> Result {
        if packet.len() < 12 || packet.len() > 9000 {
            return invalid();
        }
        let flags = u16::from_be_bytes([packet[2], packet[3]]);
        if flags & 0x8000 == 0 || flags & 0x780f != 0 {
            return invalid();
        }
        let mut at = 4;
        let questions = u16be(packet, &mut at)?;
        let answers = u16be(packet, &mut at)?;
        let authorities = u16be(packet, &mut at)?;
        let additionals = u16be(packet, &mut at)?;
        let count = usize::from(answers) + usize::from(authorities) + usize::from(additionals);
        if questions > 128 || count > 256 {
            return Err(Error::Invalid("DNS record limit"));
        }
        for _ in 0..questions {
            name(packet, &mut at)?;
            octets(packet, &mut at, 4)?;
        }
        let mut parsed = Vec::new();
        parsed
            .try_reserve(count)
            .map_err(|_| stulp_core::Error::Memory)?;
        for _ in 0..count {
            let owner = name(packet, &mut at)?;
            let kind = u16be(packet, &mut at)?;
            let class = u16be(packet, &mut at)?;
            let ttl = u32be(packet, &mut at)?;
            let length = usize::from(u16be(packet, &mut at)?);
            let start = at;
            let data = octets(packet, &mut at, length)?;
            let end = at;
            if class & 0x7fff != 1 {
                continue;
            }
            let record = match kind {
                12 => {
                    let mut cursor = start;
                    let target = name(packet, &mut cursor)?;
                    if cursor != end {
                        return invalid();
                    }
                    Record::Ptr(target)
                }
                33 => {
                    let mut cursor = start;
                    octets(packet, &mut cursor, 4)?;
                    let port = u16be(packet, &mut cursor)?;
                    let host = name(packet, &mut cursor)?;
                    if cursor != end {
                        return invalid();
                    }
                    Record::Srv { host, port }
                }
                16 => {
                    let mut text = Vec::new();
                    let mut cursor = 0;
                    while cursor < data.len() {
                        let length = usize::from(data[cursor]);
                        cursor += 1;
                        let b = octets(data, &mut cursor, length)?;
                        if b.is_empty() {
                            continue;
                        }
                        let split = b.iter().position(|b| *b == b'=');
                        let (key, value) = match split {
                            Some(at) => (&b[..at], &b[at + 1..]),
                            None => (b, &[][..]),
                        };
                        let key = core::str::from_utf8(key)
                            .map_err(|_| Error::Invalid("DNS TXT key is not ASCII"))?;
                        if key.is_empty() || !key.is_ascii() {
                            return invalid();
                        }
                        if let Some(i) = text
                            .iter()
                            .position(|(k, _): &(String, Vec<u8>)| k.eq_ignore_ascii_case(key))
                        {
                            text.remove(i);
                        }
                        json::push(&mut text, (json::copy(key)?, crate::copy(value)?), 64)?;
                    }
                    Record::Txt(text)
                }
                1 if data.len() == 4 => Record::Address(
                    IpAddr::V4(Ipv4Addr::new(data[0], data[1], data[2], data[3])),
                    0,
                ),
                28 if data.len() == 16 => {
                    let octets: [u8; 16] =
                        data.try_into().map_err(|_| Error::Invalid("AAAA width"))?;
                    let ip = Ipv6Addr::from(octets);
                    Record::Address(
                        IpAddr::V6(ip),
                        if ip.is_unicast_link_local() { scope } else { 0 },
                    )
                }
                1 | 28 => return invalid(),
                _ => continue,
            };
            parsed.push(Entry {
                owner,
                record,
                expires: now.saturating_add(u64::from(ttl.min(86400)) * 1000),
            });
        }
        if at != packet.len() {
            return invalid();
        }
        let retained = self
            .records
            .iter()
            .filter(|e| e.expires > now && !parsed.iter().any(|p| same(e, p)))
            .count();
        let added = parsed
            .iter()
            .enumerate()
            .filter(|(i, e)| e.expires > now && !parsed[i + 1..].iter().any(|p| same(e, p)))
            .count();
        if added > MAX_RECORDS.saturating_sub(retained) {
            return Err(Error::Invalid("mDNS cache full"));
        }
        self.records
            .try_reserve(parsed.len())
            .map_err(|_| stulp_core::Error::Memory)?;
        self.records.retain(|e| e.expires > now);
        for entry in parsed {
            self.records.retain(|e| !same(e, &entry));
            if entry.expires > now {
                self.records.push(entry);
            }
        }
        Ok(())
    }
    /// De vervolgvragen die de gevonden instanties compleet maken: eerst SRV
    /// (en TXT) voor instanties zonder SRV, daarna AAAA voor hosts zonder adres.
    /// Leeg als alles compleet is.
    pub fn follow_up(&self, services: &[&str], now: u64) -> Result<Vec<(String, u16)>> {
        let live = |e: &&Entry| e.expires > now;
        let mut questions = Vec::new();
        let mut hosts = Vec::new();
        for ptr in self.records.iter().filter(live) {
            let Record::Ptr(instance) = &ptr.record else {
                continue;
            };
            if !services.iter().any(|s| s.eq_ignore_ascii_case(&ptr.owner)) {
                continue;
            }
            let srv = self
                .records
                .iter()
                .filter(live)
                .find_map(|e| match &e.record {
                    Record::Srv { host, .. } if e.owner.eq_ignore_ascii_case(instance) => {
                        Some(host)
                    }
                    _ => None,
                });
            match srv {
                None => {
                    if questions.len() + 2 > FOLLOW_UP_NAMES {
                        break;
                    }
                    json::push(
                        &mut questions,
                        (json::copy(instance)?, SRV),
                        FOLLOW_UP_NAMES,
                    )?;
                    json::push(
                        &mut questions,
                        (json::copy(instance)?, TXT),
                        FOLLOW_UP_NAMES,
                    )?;
                }
                Some(host) => {
                    let known = self.records.iter().filter(live).any(|e| {
                        matches!(e.record, Record::Address(..))
                            && e.owner.eq_ignore_ascii_case(host)
                    });
                    if !known && !hosts.iter().any(|h: &String| h.eq_ignore_ascii_case(host)) {
                        json::push(&mut hosts, json::copy(host)?, 128)?;
                    }
                }
            }
        }
        if questions.is_empty() {
            for host in hosts.into_iter().take(FOLLOW_UP_NAMES) {
                json::push(&mut questions, (host, AAAA), FOLLOW_UP_NAMES)?;
            }
        }
        Ok(questions)
    }
    /// Bouwt alleen de gevraagde services; onvolledige antwoorden blijven herkenbaar als onvolledig.
    pub fn nodes(&self, services: &[&str], now: u64) -> Result<Vec<Node>> {
        let mut nodes = Vec::new();
        for ptr in self.records.iter().filter(|e| e.expires > now) {
            let Record::Ptr(instance) = &ptr.record else {
                continue;
            };
            if !services.iter().any(|s| s.eq_ignore_ascii_case(&ptr.owner)) {
                continue;
            }
            let suffix = join(&[".", &ptr.owner])?;
            if instance.len() <= suffix.len()
                || !instance[instance.len() - suffix.len()..].eq_ignore_ascii_case(&suffix)
            {
                continue;
            }
            let instance_label = &instance[..instance.len() - suffix.len()];
            let kind = if ptr.owner.eq_ignore_ascii_case(COMMISSIONABLE) {
                "commissionable"
            } else if ptr.owner.eq_ignore_ascii_case(OPERATIONAL) {
                "operational"
            } else {
                ptr.owner.trim_end_matches("local.").trim_end_matches('.')
            };
            let mut n = Node {
                kind: json::copy(kind)?,
                service: json::copy(ptr.owner.trim_end_matches("local.").trim_end_matches('.'))?,
                instance: unescape(instance_label)?,
                host: String::new(),
                port: 0,
                addresses: Vec::new(),
                text: Vec::new(),
            };
            for entry in self
                .records
                .iter()
                .filter(|e| e.expires > now && e.owner.eq_ignore_ascii_case(instance))
            {
                match &entry.record {
                    Record::Srv { host, port } => {
                        n.host = json::copy(host.trim_end_matches('.'))?;
                        n.port = *port;
                        for address in self
                            .records
                            .iter()
                            .filter(|e| e.expires > now && e.owner.eq_ignore_ascii_case(host))
                        {
                            if let Record::Address(ip, scope) = address.record {
                                if ip.is_unspecified() || ip.is_multicast() {
                                    continue;
                                }
                                let mut addr = String::new();
                                addr.try_reserve(64)
                                    .map_err(|_| stulp_core::Error::Memory)?;
                                write!(&mut addr, "{ip}")
                                    .map_err(|_| Error::Invalid("DNS address formatting"))?;
                                if scope != 0 {
                                    write!(&mut addr, "%{scope}")
                                        .map_err(|_| Error::Invalid("DNS scope formatting"))?;
                                }
                                if !n.addresses.contains(&addr) {
                                    json::push(&mut n.addresses, addr, 32)?;
                                }
                            }
                        }
                    }
                    Record::Txt(text) => {
                        for (k, v) in text {
                            json::push(&mut n.text, (json::copy(k)?, crate::copy(v)?), 64)?;
                        }
                    }
                    _ => (),
                }
            }
            n.addresses.sort_unstable_by_key(|s| rank(s));
            json::push(&mut nodes, n, 128)?;
        }
        nodes.sort_unstable_by(|a, b| a.kind.cmp(&b.kind).then(a.instance.cmp(&b.instance)));
        Ok(nodes)
    }
}
fn rank(address: &str) -> u8 {
    let host = address.split('%').next().unwrap_or(address);
    match host.parse::<IpAddr>() {
        Ok(IpAddr::V6(ip)) if !ip.is_unicast_link_local() => 0,
        Ok(IpAddr::V4(_)) => 1,
        Ok(IpAddr::V6(_)) => 2,
        _ => 3,
    }
}
/// Eén gevonden service; TXT-velden behouden hun originele sleutel en waarde.
pub struct Node {
    /// Commissionable, operational of een ander service-type.
    pub kind: String,
    /// DNS-SD-service zonder local-suffix.
    pub service: String,
    /// Leesbare instancenaam, inclusief spaties en Unicode.
    pub instance: String,
    /// Doelhost zonder afsluitende punt.
    pub host: String,
    /// Geadverteerde poort.
    pub port: u16,
    /// IPv6 ULA/global eerst, daarna IPv4 en scoped link-local.
    pub addresses: Vec<String>,
    /// TXT-eigenschappen, inclusief vendor/product en MRP-timing.
    pub text: Vec<(String, Vec<u8>)>,
}
impl Node {
    /// TXT-keyvergelijking is case-insensitief volgens DNS-SD.
    pub fn text(&self, key: &str) -> Option<&str> {
        self.raw_text(key)
            .and_then(|v| core::str::from_utf8(v).ok())
    }
    /// Binaire TXT-waarden zoals Thread's extended PAN ID blijven byte voor byte behouden.
    pub fn raw_text(&self, key: &str) -> Option<&[u8]> {
        self.text
            .iter()
            .find(|(k, _)| k.eq_ignore_ascii_case(key))
            .map(|(_, v)| v.as_slice())
    }
    /// Alleen een open commissioning window met een passende lange/korte discriminator.
    pub fn matches(&self, payload: &Payload) -> bool {
        if self.kind != "commissionable"
            || self
                .text("CM")
                .and_then(|v| v.parse::<u8>().ok())
                .unwrap_or(0)
                == 0
        {
            return false;
        }
        let Some(d) = self
            .text("D")
            .and_then(|s| s.parse::<u16>().ok())
            .filter(|d| *d <= 4095)
        else {
            return false;
        };
        if payload.short {
            d >> 8 == payload.discriminator >> 8
        } else {
            d == payload.discriminator
        }
    }
    /// MRP-intervals in milliseconden; onbekende of absurd grote TXT-waarden vallen terug op defaults.
    pub fn timing(&self) -> Timing {
        let n = |key| {
            self.text(key)
                .and_then(|s| s.trim().parse::<u64>().ok())
                .filter(|n| *n > 0 && *n <= 60000)
                .unwrap_or(0)
        };
        Timing {
            idle: n("SII"),
            active: n("SAI"),
            threshold: n("SAT"),
        }
    }
    /// Operationele instance-identiteit is altijd twee volledige 64-bit hexvelden.
    pub fn operational(&self) -> Option<(u64, u64)> {
        if self.kind != "operational" {
            return None;
        }
        let (fabric, node) = self.instance.split_once('-')?;
        if fabric.len() != 16 || node.len() != 16 {
            return None;
        }
        Some((
            u64::from_str_radix(fabric, 16).ok()?,
            u64::from_str_radix(node, 16).ok()?,
        ))
    }
}

/// Zoveel vervolgrondes na de PTR-ronde; genoeg voor ruim honderd instanties.
const FOLLOW_UP_ROUNDS: u8 = 8;
/// Een vervolgvraag krijgt unicastantwoorden; die zijn er binnen een seconde.
const FOLLOW_UP_WINDOW_MS: u64 = 1500;
/// Eén asynchrone LAN-zoekronde; pollen kan tussen MRP- en subscriptionverwerking gebeuren.
/// Na de PTR-ronde volgen zo nodig rondes voor SRV/TXT en AAAA (zie
/// [`Collector::follow_up`]); wat na de laatste ronde nog ontbreekt, blijft
/// een onvolledige node, zoals voorheen.
pub struct Browse {
    services: Vec<String>,
    deadline: u64,
    finished: bool,
    collector: Collector,
    rounds: u8,
    /// Open vragen van de vorige ronde; levert een ronde niets op, dan stopt het.
    outstanding: usize,
}
impl Browse {
    /// Start legacy mDNS op alle bruikbare IPv4/IPv6 LAN-interfaces via de platformadapter.
    pub fn start<T: stulp_sdk::Transport>(
        c: &mut stulp_sdk::Client<T>,
        services: &[&str],
        window_ms: u64,
    ) -> Result<Self> {
        if window_ms == 0 || window_ms > 30000 {
            return Err(Error::Invalid("Matter browse window must be 1..30000 ms"));
        }
        let payload = query(services)?;
        let mut owned = Vec::new();
        for service in services {
            json::push(&mut owned, json::copy(service)?, 12)?;
        }
        let deadline = c.now().saturating_add(window_ms).saturating_add(1000);
        c.start_datagrams(stulp_sdk::DatagramRequest {
            target: stulp_sdk::DatagramTarget::Mdns,
            payload,
            timeout_ms: window_ms,
        })?;
        Ok(Self {
            services: owned,
            deadline,
            finished: false,
            collector: Collector::default(),
            rounds: 0,
            outstanding: 0,
        })
    }
    fn services(&self) -> Result<Vec<&str>> {
        let mut services = Vec::new();
        for service in &self.services {
            json::push(&mut services, service.as_str(), 12)?;
        }
        Ok(services)
    }
    fn complete<T: stulp_sdk::Transport>(
        &mut self,
        c: &mut stulp_sdk::Client<T>,
    ) -> Result<Vec<Node>> {
        self.finished = true;
        self.collector.nodes(&self.services()?, c.now())
    }
    /// Foute losse DNS-pakketten verstoren de zoekronde niet; allocatie- en transportfouten wel.
    pub fn poll<T: stulp_sdk::Transport>(
        &mut self,
        c: &mut stulp_sdk::Client<T>,
    ) -> Option<Result<Vec<Node>>> {
        if self.finished {
            return Some(Err(Error::Invalid("Matter browse already completed")));
        }
        if let Some(result) = c.poll_datagrams() {
            let packets = match result {
                Ok(packets) => packets,
                // Een mislukte vervolgronde laat de nodes van de PTR-ronde staan.
                Err(e) if self.rounds == 0 => {
                    self.finished = true;
                    return Some(Err(e));
                }
                Err(_) => return Some(self.complete(c)),
            };
            for packet in packets {
                if let Err(Error::Core(e)) =
                    self.collector
                        .consume(&packet.payload, packet.interface, c.now())
                {
                    self.finished = true;
                    return Some(Err(Error::Core(e)));
                }
            }
            let next = match self
                .services()
                .and_then(|s| self.collector.follow_up(&s, c.now()))
            {
                Ok(next) => next,
                Err(e) => {
                    self.finished = true;
                    return Some(Err(e));
                }
            };
            if next.is_empty()
                || self.rounds >= FOLLOW_UP_ROUNDS
                || (self.rounds > 0 && next.len() >= self.outstanding)
            {
                return Some(self.complete(c));
            }
            self.outstanding = next.len();
            let mut questions = Vec::new();
            for (name, kind) in &next {
                if json::push(&mut questions, (name.as_str(), *kind), FOLLOW_UP_NAMES).is_err() {
                    break;
                }
            }
            // Zoveel vragen als in één datagram passen: Matter-instantienamen
            // zijn lang (14-10: 24 stuks waren 1,4 kB, en dan ging er niets uit).
            let payload = match fitting(&questions).and_then(|n| query_records(&questions[..n])) {
                Ok(p) => p,
                Err(_) => return Some(self.complete(c)),
            };
            if c.start_datagrams(stulp_sdk::DatagramRequest {
                target: stulp_sdk::DatagramTarget::Mdns,
                payload,
                timeout_ms: FOLLOW_UP_WINDOW_MS,
            })
            .is_err()
            {
                return Some(self.complete(c));
            }
            self.rounds += 1;
            self.deadline = c
                .now()
                .saturating_add(FOLLOW_UP_WINDOW_MS)
                .saturating_add(1000);
            return None;
        }
        if c.now() >= self.deadline {
            if self.rounds > 0 {
                return Some(self.complete(c));
            }
            self.finished = true;
            return Some(Err(Error::Timeout));
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn macos_zone_fallback_uses_the_same_collector_and_retains_binary_txt() -> Result {
        let mut zone = b"4C17FC0E930A269F._matterc._udp SRV 0 0 5540 B6E87FB8481098C3.local. ; comment\n4C17FC0E930A269F._matterc._udp TXT \"VP=4447+4104\" \"D=2813\" \"CM=2\" \"DN=Aqara switch\" \"xp=".to_vec();
        zone.extend_from_slice(&[0xb4, 0x44, 0x83, 0x6c, 0x32, 0x60, 0x4a, 0x7f]);
        zone.extend_from_slice(b"\"\n");
        let services = stulp_sdk::dnssd::services(&query(&[COMMISSIONABLE, OPERATIONAL])?)?;
        assert_eq!(services, [COMMISSIONABLE, OPERATIONAL]);
        let mut collector = Collector::default();
        for packet in stulp_sdk::dnssd::zone(COMMISSIONABLE, &zone)? {
            collector.consume(&packet.payload, packet.interface, 1000)?;
        }
        let nodes = collector.nodes(&[COMMISSIONABLE], 1000)?;
        assert_eq!(nodes.len(), 1);
        assert_eq!(nodes[0].host, "B6E87FB8481098C3.local");
        assert_eq!(nodes[0].port, 5540);
        assert_eq!(nodes[0].text("DN"), Some("Aqara switch"));
        assert_eq!(
            nodes[0].raw_text("xp"),
            Some([0xb4, 0x44, 0x83, 0x6c, 0x32, 0x60, 0x4a, 0x7f].as_slice())
        );
        assert!(nodes[0].addresses.is_empty()); // Resolutie gebeurt pas voor onze node.
        Ok(())
    }
    #[test]
    fn compressed_go_packet_keeps_scopes_timing_and_short_discriminators() -> Result {
        let mut c = Collector::default();
        let packet = include_bytes!("../tests/fixtures/mdns.bin");
        c.consume(packet, 42, 1000)?;
        let nodes = c.nodes(&[COMMISSIONABLE, OPERATIONAL], 1000)?;
        assert_eq!(nodes.len(), 2);
        let n = &nodes[0];
        assert_eq!(n.host, "bedroom-sensor.local");
        assert_eq!(n.port, 5540);
        assert_eq!(n.text("dn"), Some("Bedroom Sensor"));
        assert_eq!(
            n.addresses.as_slice(),
            ["fd11::1", "192.0.2.50", "fe80::2%42"]
        );
        assert_eq!(n.timing().idle, 5000);
        assert_eq!(n.timing().active, 300);
        assert_eq!(n.timing().threshold, 4000);
        assert!(n.matches(&Payload::parse("MT:-24J0AFN00KA0648G00")?));
        assert!(n.matches(&Payload::parse("34970112332")?));
        assert_eq!(nodes[1].operational(), Some((0xabcdef1234567890, 1)));
        c.consume(packet, 42, 2000)?;
        assert_eq!(c.nodes(&[COMMISSIONABLE, OPERATIONAL], 121000)?.len(), 2);
        assert!(c.nodes(&[COMMISSIONABLE, OPERATIONAL], 122000)?.is_empty());
        let routers = c.nodes(&[BORDER_ROUTER], 2000)?;
        assert_eq!(routers.len(), 1);
        assert_eq!(
            routers[0].raw_text("xp"),
            Some(&[0, 255, 128, 1, 2, 3, 4, 5][..])
        );
        assert_eq!(routers[0].text("xp"), None);
        // Geen enkele prefix van een pakket mag een halve cache publiceren.
        for size in 0..packet.len() {
            let mut c = Collector::default();
            assert!(c.consume(&packet[..size], 42, 1000).is_err());
            assert!(c.records.is_empty());
        }
        Ok(())
    }
    #[test]
    fn dns_names_never_confuse_label_dots_or_follow_cyclic_pointers() -> Result {
        let raw = b"\x0bcaf\xc3\xa9.lamp\x00";
        // Lengteveld omvat alleen de labelbytes, niet de terminator.
        let mut raw = crate::copy(raw)?;
        raw[0] = (raw.len() - 2) as u8;
        let mut at = 0;
        let escaped = name(&raw, &mut at)?;
        assert_eq!(unescape(escaped.trim_end_matches('.'))?, "café.lamp");
        assert!(escaped.contains("\\046"));
        assert!(name(&[0xc0, 0], &mut 0).is_err());
        assert!(name(&[0xc0, 2, 0], &mut 0).is_err());
        let q = query(&[COMMISSIONABLE, OPERATIONAL, BORDER_ROUTER])?;
        assert_eq!(q[5], 3);
        let mut at = 12;
        assert_eq!(name(&q, &mut at)?, COMMISSIONABLE);
        assert_eq!(&q[at..at + 4], &[0, 12, 0, 1]);
        assert!(query(&["bad..local."]).is_err());
        Ok(())
    }
    struct Round {
        pending: bool,
        follow_up: bool,
        now: u64,
    }
    impl stulp_sdk::Transport for Round {
        async fn send(&mut self, _: &json::Value) -> Result {
            Err(Error::Invalid("unexpected test controller write"))
        }
        async fn next(&mut self) -> Result<stulp_sdk::Event> {
            Ok(stulp_sdk::Event::Tick)
        }
        fn now(&self) -> u64 {
            self.now
        }
        fn random(&mut self) -> Result<[u8; 32]> {
            Err(Error::Invalid("discovery must not need entropy"))
        }
        fn start_datagrams(&mut self, request: stulp_sdk::DatagramRequest) -> Result {
            assert!(matches!(request.target, stulp_sdk::DatagramTarget::Mdns));
            if request.timeout_ms == FOLLOW_UP_WINDOW_MS {
                // Een vervolgronde voor wat het vaste pakket niet compleet
                // maakt; dit harnas heeft er geen antwoord op.
                self.follow_up = true;
                return Ok(());
            }
            assert_eq!(request.timeout_ms, 4000);
            assert_eq!(request.payload, query(&[COMMISSIONABLE, OPERATIONAL])?);
            self.pending = true;
            Ok(())
        }
        fn poll_datagrams(&mut self) -> Option<Result<Vec<stulp_sdk::Datagram>>> {
            if self.follow_up {
                self.follow_up = false;
                return Some(Ok(Vec::new()));
            }
            if !self.pending {
                return None;
            }
            self.pending = false;
            Some((|| {
                let mut out = Vec::new();
                for bytes in [
                    &[0u8, 1, 2][..],
                    include_bytes!("../tests/fixtures/mdns.bin").as_slice(),
                ] {
                    json::push(
                        &mut out,
                        stulp_sdk::Datagram {
                            source: json::copy("192.0.2.50:5353")?,
                            interface: 42,
                            payload: crate::copy(bytes)?,
                        },
                        2,
                    )?;
                }
                Ok(out)
            })())
        }
    }
    /// Een antwoordpakket zoals de HomePod-proxy ze stuurt: alleen de records
    /// waar om gevraagd is, zonder additionals.
    fn answer(records: &[(&str, u16, Vec<u8>)]) -> Vec<u8> {
        fn name(out: &mut Vec<u8>, text: &str) {
            for label in text.trim_end_matches('.').split('.') {
                out.push(label.len() as u8);
                out.extend_from_slice(label.as_bytes());
            }
            out.push(0);
        }
        let mut out = alloc::vec![0, 0, 0x84, 0, 0, 0, 0, records.len() as u8, 0, 0, 0, 0];
        for (owner, kind, data) in records {
            name(&mut out, owner);
            out.extend_from_slice(&kind.to_be_bytes());
            out.extend_from_slice(&[0, 1, 0, 0, 0, 120]);
            out.extend_from_slice(&(data.len() as u16).to_be_bytes());
            out.extend_from_slice(data);
        }
        out
    }
    fn dns_name(text: &str) -> Vec<u8> {
        let mut out = Vec::new();
        for label in text.trim_end_matches('.').split('.') {
            out.push(label.len() as u8);
            out.extend_from_slice(label.as_bytes());
        }
        out.push(0);
        out
    }
    /// Beantwoordt per ronde alleen het gevraagde type, zoals de proxy.
    struct Proxy {
        now: u64,
        pending: Option<Vec<u8>>,
        asked: Vec<Vec<u16>>,
    }
    impl stulp_sdk::Transport for Proxy {
        async fn send(&mut self, _: &json::Value) -> Result {
            Err(Error::Invalid("unexpected test controller write"))
        }
        async fn next(&mut self) -> Result<stulp_sdk::Event> {
            Ok(stulp_sdk::Event::Tick)
        }
        fn now(&self) -> u64 {
            self.now
        }
        fn random(&mut self) -> Result<[u8; 32]> {
            Err(Error::Invalid("discovery must not need entropy"))
        }
        fn start_datagrams(&mut self, request: stulp_sdk::DatagramRequest) -> Result {
            // De vraagtypen uit de query lezen: na elke naam volgen type en klasse.
            let q = &request.payload;
            let count = usize::from(q[5]);
            let mut at = 12;
            let mut kinds = Vec::new();
            for _ in 0..count {
                while q[at] != 0 {
                    at += 1 + usize::from(q[at]);
                }
                at += 1;
                kinds.push(u16::from_be_bytes([q[at], q[at + 1]]));
                at += 4;
            }
            self.asked.push(kinds);
            self.pending = Some(request.payload);
            Ok(())
        }
        fn poll_datagrams(&mut self) -> Option<Result<Vec<stulp_sdk::Datagram>>> {
            let query = self.pending.take()?;
            let kind = u16::from_be_bytes([query[query.len() - 4], query[query.len() - 3]]);
            let instance = "ABCDEF0123456789-0000000000000001._matter._tcp.local.";
            let host = "DEADBEEFCAFEF00D.local.";
            let payload = match kind {
                PTR => answer(&[(OPERATIONAL, PTR, dns_name(instance))]),
                SRV | TXT => {
                    let mut srv = alloc::vec![0, 0, 0, 0, 0x15, 0xa4];
                    srv.extend_from_slice(&dns_name(host));
                    answer(&[(instance, SRV, srv), (instance, TXT, b"\x05SII=1".to_vec())])
                }
                AAAA => {
                    let mut ip = alloc::vec![0xfd, 0x5f, 0x71, 0xe5, 0xf7, 0x52, 0, 0];
                    ip.extend_from_slice(&[0, 0, 0, 0, 0, 0, 0, 1]);
                    answer(&[(host, AAAA, ip)])
                }
                _ => answer(&[]),
            };
            Some(Ok(alloc::vec![stulp_sdk::Datagram {
                source: "192.0.2.50:5353".into(),
                interface: 7,
                payload,
            }]))
        }
    }
    #[test]
    fn follow_up_batches_shrink_to_the_datagram_budget() -> Result {
        let mut names = Vec::new();
        for i in 0..12u64 {
            names.push(alloc::format!(
                "93F4E4B2D343FD28-{i:016X}._matter._tcp.local."
            ));
        }
        let mut questions = Vec::new();
        for name in &names {
            questions.push((name.as_str(), SRV));
            questions.push((name.as_str(), TXT));
        }
        assert!(
            query_records(&questions).is_err(),
            "24 lange namen passen niet"
        );
        let n = fitting(&questions)?;
        assert!(n > 0 && n < 24 && n % 2 == 0, "n={n}");
        assert!(query_records(&questions[..n])?.len() <= 1232);
        Ok(())
    }
    #[test]
    fn a_proxy_that_answers_only_what_is_asked_gets_follow_up_rounds() -> Result {
        let mut c = stulp_sdk::Client::new(Proxy {
            now: 1000,
            pending: None,
            asked: Vec::new(),
        });
        let mut browse = Browse::start(&mut c, &[OPERATIONAL], 4000)?;
        let mut nodes = None;
        for _ in 0..8 {
            if let Some(result) = browse.poll(&mut c) {
                nodes = Some(result?);
                break;
            }
        }
        let nodes = nodes.ok_or(Error::Invalid("browse did not finish"))?;
        assert_eq!(nodes.len(), 1);
        assert_eq!(nodes[0].host, "DEADBEEFCAFEF00D.local");
        assert_eq!(nodes[0].port, 5540);
        assert_eq!(nodes[0].addresses, ["fd5f:71e5:f752::1"]);
        assert_eq!(nodes[0].operational(), Some((0xABCDEF0123456789, 1)));
        let asked = c.into_transport().asked;
        assert_eq!(
            asked,
            [alloc::vec![PTR], alloc::vec![SRV, TXT], alloc::vec![AAAA]]
        );
        Ok(())
    }
    #[test]
    fn browse_uses_all_lan_adapter_and_preserves_scope_even_for_ipv4_replies() -> Result {
        let mut c = stulp_sdk::Client::new(Round {
            pending: false,
            follow_up: false,
            now: 4000,
        });
        let mut browse = Browse::start(&mut c, &[COMMISSIONABLE, OPERATIONAL], 4000)?;
        let mut result = None;
        for _ in 0..4 {
            if let Some(r) = browse.poll(&mut c) {
                result = Some(r);
                break;
            }
        }
        let nodes = result.ok_or(Error::Invalid("missing browse result"))??;
        assert_eq!(nodes.len(), 2);
        assert!(nodes[0].addresses.iter().any(|a| a == "fe80::2%42"));
        assert!(matches!(browse.poll(&mut c), Some(Err(_))));
        let mut timeout = Browse {
            services: Vec::new(),
            deadline: 4000,
            finished: false,
            collector: Collector::default(),
            rounds: 0,
            outstanding: 0,
        };
        assert!(matches!(timeout.poll(&mut c), Some(Err(Error::Timeout))));
        Ok(())
    }
}
