//! De operationele eigenaar van fabric, sessies en subscriptions.
use crate::{
    commissioning::{Administrator, Commissioning},
    discovery::{self, Browse},
    fabric::Fabric,
    interaction::{Interaction, Reports},
    model,
    mrp::{Event, Timing},
    network::{Network, Peer},
    onboarding::Payload,
    reports,
};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, Transport, clone,
    util::{field, join},
};
pub(crate) struct Node {
    id: u64,
    session: Option<u16>,
    subscription: Option<u32>,
    expires: u64,
    next: u64,
    backoff: u64,
    model_failures: u64,
    /// Mislukte onderhoudspogingen op rij; bepaalt [`retry_delay`].
    failures: u32,
}
/// Na de eerste mislukte achtergrondpoging een minuut wachten, daarna
/// verdubbelen tot een half uur. Een uitgeschakeld apparaat kostte anders
/// elke minuut een CASE-handshake van 40 s op de trage RISC-V-core.
const RETRY_FIRST_MS: u64 = 60_000;
const RETRY_LIMIT_MS: u64 = 1_800_000;
/// Wachttijd na `failures` mislukte achtergrondpogingen op rij.
fn retry_delay(failures: u32) -> u64 {
    match failures {
        0 => 0,
        n => 1u64
            .checked_shl(n - 1)
            .map_or(RETRY_LIMIT_MS, |f| RETRY_FIRST_MS.saturating_mul(f))
            .min(RETRY_LIMIT_MS),
    }
}
impl Node {
    /// Een mislukte achtergrondpoging: de node wacht langer; geeft de wachttijd terug.
    fn failed(&mut self, now: u64) -> u64 {
        self.failures = self.failures.saturating_add(1);
        let delay = retry_delay(self.failures);
        self.next = now.saturating_add(delay);
        delay
    }
    /// Bereikbaar langs welk pad dan ook: de wachttijd vervalt meteen.
    fn reachable(&mut self, now: u64) {
        if self.failures > 0 {
            self.failures = 0;
            if self.subscription.is_none() {
                self.next = self.next.min(now);
            }
        }
    }
}
/// De oudste vervallen deadline gaat voor; een vroege offline node mag latere nodes niet verdringen.
/// De pauze tussen twee verbindingspogingen; zie [`Engine::settle`].
const SETTLE_MS: u64 = 250;
#[cfg(test)]
fn next_node(nodes: &[Node], now: u64, route: u64) -> Option<usize> {
    next_node_where(nodes, now, route, |_| true)
}
fn next_node_where(
    nodes: &[Node],
    now: u64,
    route: u64,
    keep: impl Fn(&Node) -> bool,
) -> Option<usize> {
    nodes
        .iter()
        .enumerate()
        .filter(|(_, node)| keep(node))
        .filter_map(|(index, node)| {
            let deadline = if node.subscription.is_some() {
                node.expires
            } else {
                node.next.max(route)
            };
            (deadline <= now).then_some((index, deadline))
        })
        .min_by_key(|(_, deadline)| *deadline)
        .map(|(index, _)| index)
}
/// Welke nodes een engine onderhoudt (abonnement, model, herverbinden).
///
/// Sinds 03-10 hoort elke node bij één vaste commandowerker
/// ([`jobs::pool::owner`]), die zowel het abonnement als de commando's doet:
/// één CASE-sessie per apparaat, zoals de Go-versie. Daarvoor deed de
/// hoofdwerker het onderhoud en had elke commandowerker eigen sessies, zodat
/// het eerste commando naar een lamp op een werker eerst een handshake van
/// 0,7 tot 1,7 s deed, en de apparaten elkaars sessies verdrongen.
#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) enum Scope {
    /// Alles, met discovery (tests en een plugin zonder pool).
    All,
    /// De hoofdwerker: discovery en levenscyclus, geen abonnementen.
    Lifecycle,
    /// Commandowerker `k`: abonnementen en commando's van zijn eigen nodes.
    Owner(usize),
}
pub(crate) struct Engine {
    pub(crate) fabric: Fabric,
    pub(crate) network: Network,
    pub(crate) scope: Scope,
    /// De staatrevisie van de laatste [`Engine::sync`] vanuit een tick.
    synced: Option<u64>,
    nodes: Vec<Node>,
    next_discovery: u64,
    /// Niet vóór dit moment aan de volgende node beginnen: tussen twee
    /// handshakes krijgen de buren op het gedeelde core een paar beurten.
    settle: u64,
    pending: Option<Event>,
    route: crate::route::Recovery,
}
fn maintains(scope: Scope, node: u64) -> bool {
    match scope {
        Scope::All => true,
        Scope::Lifecycle => false,
        Scope::Owner(k) => stulp_sdk::jobs::pool::owner(node) == k,
    }
}
impl Engine {
    /// Alleen de hoofdwerker zoekt adressen; de werkers lezen ze uit de staat.
    fn discovers(&self) -> bool {
        !matches!(self.scope, Scope::Owner(_))
    }
}
pub(crate) fn node_id(d: &Value) -> Result<u64> {
    u64::from_str_radix(json::text(field(d, "store"), "matter.nodeId"), 16)
        .ok()
        .filter(|n| *n != 0)
        .ok_or(Error::Invalid("Matter-apparaat heeft geen geldig node-id."))
}
fn node_devices(snapshot: &Value, id: u64) -> Result<Vec<Value>> {
    let mut out = Vec::new();
    if let Some(devices) = field(snapshot, "devices").as_object() {
        for (_, d) in devices.iter() {
            if node_id(d).ok() == Some(id) {
                json::push(&mut out, clone(d)?, 256)?;
            }
        }
    }
    Ok(out)
}
fn stored_timing(store: &Value) -> Timing {
    let n = |key| json::uint(store, key).min(60000);
    Timing {
        idle: n("matter.mrpIdleInterval"),
        active: n("matter.mrpActiveInterval"),
        threshold: n("matter.mrpActiveThreshold"),
    }
}
fn timing_patch(t: Timing) -> Result<Value> {
    Ok(json::fields(&[
        ("matter.mrpIdleInterval", Value::uint(t.idle)),
        ("matter.mrpActiveInterval", Value::uint(t.active)),
        ("matter.mrpActiveThreshold", Value::uint(t.threshold)),
    ])?)
}
pub(crate) fn address(host: &str, port: u16) -> Result<String> {
    let port = crate::settings::decimal(u64::from(if port == 0 { 5540 } else { port }))?;
    join(&[
        if host.contains(':') { "[" } else { "" },
        host,
        if host.contains(':') { "]:" } else { ":" },
        &port,
    ])
}
fn manual_address(s: &str) -> Result<String> {
    if s.parse::<core::net::SocketAddr>().is_ok() {
        return Ok(json::copy(s)?);
    }
    if s.parse::<core::net::IpAddr>().is_ok() {
        return address(s, 5540);
    }
    Err(Error::Invalid("Geen numeriek Matter-adres."))
}
fn lookup_address(s: &str) -> Result<String> {
    let s = s.trim();
    if s.is_empty() || s.len() > 300 || s.chars().any(char::is_whitespace) {
        return Err(Error::Invalid("Geef een geldig Matter-adres op."));
    }
    if let Ok(numeric) = manual_address(s) {
        return Ok(numeric);
    }
    if s.starts_with('[') {
        let end = s.find(']').ok_or(Error::Invalid("Ongeldig IPv6-adres."))?;
        return if end + 1 == s.len() {
            join(&[s, ":5540"])
        } else if s.get(end + 1..end + 2) == Some(":") {
            Ok(json::copy(s)?)
        } else {
            Err(Error::Invalid("Ongeldig IPv6-adres."))
        };
    }
    match s.bytes().filter(|b| *b == b':').count() {
        0 => join(&[s, ":5540"]),
        1 => Ok(json::copy(s)?),
        _ => address(s, 5540),
    }
}
async fn resolve_address<T: Transport>(c: &mut Client<T>, supplied: &str) -> Result<String> {
    let addresses = c.resolve(&lookup_address(supplied)?).await?;
    // Go ResolveUDPAddr kiest IPv4 wanneer de naam beide families aanbiedt.
    let usable = |s: &&String| {
        s.parse::<core::net::SocketAddr>()
            .is_ok_and(|a| a.port() != 0 && !a.ip().is_unspecified() && !a.ip().is_multicast())
    };
    addresses
        .iter()
        .filter(usable)
        .find(|s| {
            s.parse::<core::net::SocketAddr>()
                .is_ok_and(|a| a.is_ipv4())
        })
        .or_else(|| addresses.iter().find(usable))
        .map(|s| json::copy(s).map_err(Error::from))
        .unwrap_or(Err(Error::Invalid(
            "Matter-adres heeft geen bruikbare IP-adressen.",
        )))
}
async fn discovered_address<T: Transport>(
    c: &mut Client<T>,
    node: &discovery::Node,
) -> Result<String> {
    if let Some(ip) = node.addresses.first() {
        return address(ip, node.port);
    }
    if node.host.is_empty() {
        return Err(Error::Invalid("Matter DNS-SD record has no host"));
    }
    let mut addresses = c.resolve(&address(&node.host, node.port)?).await?;
    addresses.retain(|s| {
        s.parse::<core::net::SocketAddr>()
            .is_ok_and(|a| a.port() != 0 && !a.ip().is_unspecified() && !a.ip().is_multicast())
    });
    addresses.sort_by_key(|s| match s.parse::<core::net::SocketAddr>() {
        Ok(core::net::SocketAddr::V6(a)) if !a.ip().is_unicast_link_local() => 0,
        Ok(core::net::SocketAddr::V4(_)) => 1,
        _ => 2,
    });
    addresses
        .into_iter()
        .next()
        .ok_or(Error::Invalid("Matter DNS-SD host has no usable address"))
}
impl Engine {
    pub(crate) async fn open<T: Transport>(c: &mut Client<T>) -> Result<Self> {
        Ok(Self {
            fabric: Fabric::load(c).await?,
            network: Network::open(c).await?,
            scope: Scope::All,
            synced: None,
            nodes: Vec::new(),
            next_discovery: c.now().saturating_add(1000),
            settle: 0,
            pending: None,
            route: crate::route::Recovery::default(),
        })
    }
    /// [`Engine::sync`] alleen als de staat veranderde. Een tick liep eerder bij
    /// elke ronde alle apparaten langs, en sinds er negen engines zijn (de
    /// hoofdwerker en acht node-eigenaren) kostte dat het plugin-slot een
    /// flink deel van zijn tijd (04-10).
    fn follow<T: Transport>(&mut self, c: &Client<T>) -> Result {
        let revision = c.state().revision();
        if self.synced != Some(revision) {
            self.sync(c.state().root(), c.now())?;
            self.synced = Some(revision);
        }
        Ok(())
    }
    pub(crate) fn sync(&mut self, root: &Value, now: u64) -> Result {
        let mut wanted = Vec::new();
        if let Some(devices) = field(root, "devices").as_object() {
            for (_, d) in devices.iter() {
                let Ok(id) = node_id(d) else {
                    continue;
                };
                // Commando's lopen in eigen werkers met eigen sessies; hun
                // succes zien we hier alleen als `available` op het apparaat.
                // Het onderhoud markeert alle apparaten van een mislukte node
                // onbeschikbaar, dus beschikbaar betekent: weer bereikbaar.
                if json::boolean(d, "available")
                    && let Some(n) = self.nodes.iter_mut().find(|n| n.id == id)
                {
                    n.reachable(now);
                }
                if !wanted.contains(&id) {
                    json::push(&mut wanted, id, 64)?;
                }
            }
        }
        for n in &self.nodes {
            if !wanted.contains(&n.id)
                && let Some(session) = n.session
            {
                self.network.remove(session)?;
            }
        }
        self.nodes.retain(|n| wanted.contains(&n.id));
        for id in wanted {
            if !self.nodes.iter().any(|n| n.id == id) {
                json::push(
                    &mut self.nodes,
                    Node {
                        id,
                        session: None,
                        subscription: None,
                        expires: 0,
                        next: now.saturating_add(1000),
                        backoff: 1000,
                        model_failures: 0,
                        failures: 0,
                    },
                    64,
                )?;
            }
        }
        Ok(())
    }
    pub(crate) fn ready<T: Transport>(&mut self, c: &mut Client<T>) -> Result<bool> {
        self.follow(c)?;
        self.network.tick(c)?;
        for _ in 0..16 {
            if self.pending.is_some() {
                break;
            }
            let Some(event) = self.network.event() else {
                break;
            };
            match event {
                Event::Accepted(_) => self.pending = Some(event),
                Event::Message(h, _) => {
                    self.network.acknowledge(c, h)?;
                    self.network.close(h);
                }
                // De StatusResponse van een rapport is bevestigd of opgegeven.
                Event::Acknowledged(h, _) | Event::Failed(h) => self.network.close(h),
            }
        }
        Ok(self.pending.is_some() || self.maintenance_due(c.now()))
    }
    /// Een rapport wacht op verwerking (gevuld door [`Engine::ready`]).
    pub(crate) fn has_report(&self) -> bool {
        self.pending.is_some()
    }
    /// Discovery, of een eigen node die (opnieuw) verbonden of geabonneerd moet worden.
    pub(crate) fn maintenance_due(&self, now: u64) -> bool {
        let scope = self.scope;
        (self.discovers() && !self.nodes.is_empty() && now >= self.next_discovery)
            || self
                .nodes
                .iter()
                .filter(|n| maintains(scope, n.id))
                .any(|n| {
                    if n.subscription.is_some() {
                        now >= n.expires
                    } else {
                        now >= n.next.max(self.route.next)
                    }
                })
    }
    pub(crate) async fn browse<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        services: &[&str],
        window: u64,
    ) -> Result<Vec<discovery::Node>> {
        let mut b = Browse::start(c, services, window)?;
        loop {
            if let Some(result) = b.poll(c) {
                return result;
            }
            self.network.tick(c)?;
            c.idle().await?;
        }
    }
    pub(crate) async fn commission<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        mut payload: Payload,
        supplied: &str,
    ) -> Result<Vec<Value>> {
        let find_until = c.now().saturating_add(60000);
        let mut timing = Timing::default();
        let initial_address = if !supplied.is_empty() {
            resolve_address(c, supplied).await?
        } else {
            loop {
                let found = self.browse(c, &[discovery::COMMISSIONABLE], 4000).await?;
                let matching: Vec<_> = {
                    let mut matches = Vec::new();
                    for n in found {
                        if n.matches(&payload) {
                            json::push(&mut matches, n, 128)?;
                        }
                    }
                    matches
                };
                if matching.len() > 1 {
                    return Err(Error::Invalid(
                        "Meerdere Matter-apparaten passen bij deze code; geef een adres op.",
                    ));
                }
                if let Some(n) = matching.first() {
                    timing = n.timing();
                    break discovered_address(c, n).await?;
                }
                if c.now() >= find_until {
                    return Err(Error::Invalid(
                        "Geen passend apparaat gevonden. Open het Matter-koppelvenster.",
                    ));
                }
            }
        };
        let deadline = c.now().saturating_add(120000);
        let result = self
            .network
            .pase(c, &initial_address, payload.passcode, timing, deadline)
            .await;
        zeroize::Zeroize::zeroize(&mut payload.passcode);
        let pase = result?;
        let issued = async {
            let mut commissioner = Commissioning {
                im: Interaction {
                    network: &mut self.network,
                    address: &initial_address,
                    session: pase.session,
                },
                endpoint: 0,
            };
            commissioner.arm(c, 120, 1, deadline).await?;
            commissioner.configure(c, "XX", 2, deadline).await?;
            let verified = commissioner
                .attest(
                    c,
                    &pase.challenge,
                    payload.vendor,
                    payload.product,
                    deadline,
                )
                .await?;
            payload.vendor = verified.vendor;
            payload.product = verified.product;
            if let Some(index) = commissioner.stale(c, self.fabric.id(), deadline).await? {
                commissioner.remove(c, index, deadline).await?;
            }
            let key = commissioner
                .csr(c, &verified, &pase.challenge, deadline)
                .await?;
            commissioner
                .add_root(c, &self.fabric.root()?, deadline)
                .await?;
            let node = self.fabric.allocate(c).await?;
            let mut serial = [0; 16];
            serial.copy_from_slice(&c.random()?[..16]);
            let noc = self.fabric.issue(&key, node, serial, c.wall_time()?)?;
            let index = commissioner
                .add_noc(
                    c,
                    &noc,
                    Administrator {
                        ipk: &self.fabric.case().epoch_ipk(),
                        subject: self.fabric.case().node(),
                        vendor: 0xfff1,
                    },
                    deadline,
                )
                .await?
                .index()?;
            Ok::<_, Error>((node, noc, index))
        }
        .await;
        self.network.remove(pase.session)?;
        drop(pase);
        let (node, noc, index) = issued?;
        let mut operational = initial_address;
        let first = self
            .network
            .case(
                c,
                self.fabric.case(),
                Peer {
                    address: &operational,
                    node,
                    noc: &noc,
                    timing,
                },
                deadline.min(c.now().saturating_add(12000)),
            )
            .await;
        let session = match first {
            Ok(s) => s,
            Err(Error::Core(e)) => return Err(Error::Core(e)),
            Err(e) => {
                let found = self.browse(c, &[discovery::OPERATIONAL], 4000).await?;
                let compressed = u64::from_be_bytes(self.fabric.case().compressed_id()?);
                let n = found
                    .iter()
                    .find(|n| n.operational() == Some((compressed, node)))
                    .ok_or(e)?;
                operational = address(
                    n.addresses
                        .first()
                        .ok_or(Error::Invalid("Operationeel Matter-adres ontbreekt."))?,
                    n.port,
                )?;
                timing = n.timing();
                self.network
                    .case(
                        c,
                        self.fabric.case(),
                        Peer {
                            address: &operational,
                            node,
                            noc: &noc,
                            timing,
                        },
                        deadline,
                    )
                    .await?
            }
        };
        let result = async {
            let mut im = Interaction {
                network: &mut self.network,
                address: &operational,
                session,
            };
            let mut devices = model::inspect(
                &mut im,
                c,
                &model::Identity {
                    node,
                    noc: &noc,
                    fabric: index,
                    address: &operational,
                    vendor: payload.vendor,
                    product: payload.product,
                },
                deadline,
            )
            .await?;
            Commissioning { im, endpoint: 0 }
                .complete(c, deadline)
                .await?;
            for d in &mut devices {
                let mut store = clone(field(d, "store"))?;
                if let Some(patch) = timing_patch(timing)?.as_object() {
                    for (k, v) in patch.iter() {
                        json::set(&mut store, k, clone(v)?)?;
                    }
                }
                json::set(d, "store", store)?;
            }
            Ok(devices)
        }
        .await;
        // De controller bewaart pas later de gekozen kandidaten. Init opent daarna een nieuwe CASE.
        self.network.remove(session)?;
        result
    }
    async fn connect<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        device: &Value,
    ) -> Result<(usize, u16, String)> {
        self.connect_until(c, device, c.now().saturating_add(40000))
            .await
    }
    async fn connect_until<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        device: &Value,
        limit: u64,
    ) -> Result<(usize, u16, String)> {
        let id = node_id(device)?;
        self.sync(c.state().root(), c.now())?;
        let i = self
            .nodes
            .iter()
            .position(|n| n.id == id)
            .ok_or(Error::Invalid("Matter-node ontbreekt."))?;
        let store = field(device, "store");
        let address = resolve_address(c, json::text(store, "matter.address")).await?;
        let session = if let Some(s) = self.nodes[i].session {
            s
        } else {
            let noc = stulp_protocol::token::decode(json::text(store, "matter.noc"))?;
            let deadline = limit.min(c.now().saturating_add(40000));
            let s = self
                .network
                .case(
                    c,
                    self.fabric.case(),
                    Peer {
                        address: &address,
                        node: id,
                        noc: &noc,
                        timing: stored_timing(store),
                    },
                    deadline,
                )
                .await?;
            self.nodes[i].session = Some(s);
            s
        };
        Ok((i, session, address))
    }
    fn expire(&mut self, index: usize, now: u64) -> Result {
        let n = &mut self.nodes[index];
        if let Some(s) = n.session.take() {
            self.network.remove(s)?;
        }
        n.subscription = None;
        n.expires = 0;
        n.next = now.saturating_add(n.backoff);
        n.backoff = (n.backoff * 2).min(60000);
        Ok(())
    }
    pub(crate) async fn command<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        values: &Value,
    ) -> Result<Value> {
        let device = clone(c.state().device(id)?)?;
        let mut plan = crate::commands::plan(&device, values)?;
        if plan.commands.is_empty() {
            return Ok(plan.errors);
        }
        let start = c.now();
        let node = node_id(&device)?;
        let handshake = !self
            .nodes
            .iter()
            .any(|n| n.id == node && n.session.is_some());
        let connection = self.connect(c, &device).await;
        let connected = c.now();
        let (index, session, address) = match connection {
            Ok(v) => v,
            Err(e) => {
                let text = stulp_sdk::message(&e)?;
                for p in plan.commands {
                    if let Some(values) = p.values.as_object() {
                        for (k, _) in values.iter() {
                            json::set(&mut plan.errors, k, json::string(&text)?)?;
                        }
                    }
                }
                c.unavailable(id, &text).await?;
                return Ok(plan.errors);
            }
        };
        let mut failed = None;
        let (mut invoke_ms, mut report_ms) = (0, 0);
        for p in plan.commands {
            let sent = c.now();
            let result = if failed.is_some() {
                Err(Error::Transport("Matter session failed"))
            } else {
                p.invoke(
                    &mut Interaction {
                        network: &mut self.network,
                        address: &address,
                        session,
                    },
                    c,
                    c.now().saturating_add(30000),
                )
                .await
            };
            let answered = c.now();
            invoke_ms += answered.saturating_sub(sent);
            match result {
                Ok(()) => {
                    c.values(id, p.values).await?;
                    c.available(id, true).await?;
                    report_ms += c.now().saturating_sub(answered);
                }
                Err(e) => {
                    if failed.is_none() {
                        failed = Some(stulp_sdk::message(&e)?);
                    }
                    let text = failed.as_deref().unwrap_or("Matter command failed");
                    if let Some(values) = p.values.as_object() {
                        for (k, _) in values.iter() {
                            json::set(&mut plan.errors, k, json::string(text)?)?;
                        }
                    }
                }
            }
        }
        if let Some(error) = &failed {
            self.expire(index, c.now())?;
            c.unavailable(id, error).await?;
        }
        // Waar de tijd van een lampcommando zit: handshake (connect), het
        // Matter-bericht heen en terug (invoke), en het melden aan Stulp (report).
        let mut line = String::new();
        if line.try_reserve(160).is_ok() {
            let _ = write!(
                line,
                "MATTER_COMMAND node={node:016X} handshake={} connect_ms={} invoke_ms={invoke_ms} report_ms={report_ms} total_ms={}{}",
                u8::from(handshake),
                connected.saturating_sub(start),
                c.now().saturating_sub(start),
                if failed.is_some() { " failed" } else { "" }
            );
            c.log("info", &line)?;
        }
        Ok(plan.errors)
    }
    async fn refresh_model<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        device: &Value,
        session: u16,
        address: &str,
        deadline: u64,
    ) -> Result {
        let node = node_id(device)?;
        let existing = node_devices(c.state().root(), node)?;
        if !crate::maintenance::required(&existing) {
            return Ok(());
        }
        let store = field(device, "store");
        let noc = stulp_protocol::token::decode(json::text(store, "matter.noc"))?;
        let data = field(device, "data");
        let identity = model::Identity {
            node,
            noc: &noc,
            fabric: u8::try_from(json::uint(store, "matter.fabricIndex"))
                .map_err(|_| Error::Invalid("Matter fabric index"))?,
            address,
            vendor: u16::try_from(json::uint(data, "vendorId")).unwrap_or(0),
            product: u16::try_from(json::uint(data, "productId")).unwrap_or(0),
        };
        let prototypes = model::inspect(
            &mut Interaction {
                network: &mut self.network,
                address,
                session,
            },
            c,
            &identity,
            deadline,
        )
        .await?;
        crate::maintenance::refreshed(c, &existing, prototypes).await
    }
    pub(crate) async fn diagnose<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        deadline: u64,
    ) -> Result<Value> {
        let device = clone(c.state().device(id)?)?;
        let (_, session, address) = self.connect_until(c, &device, deadline).await?;
        let refresh = self
            .refresh_model(c, &device, session, &address, deadline)
            .await;
        if let Err(Error::Core(e)) = refresh {
            return Err(Error::Core(e));
        }
        let device = clone(c.state().device(id)?)?;
        let mut result = crate::diagnostics::inspect(
            &mut Interaction {
                network: &mut self.network,
                address: &address,
                session,
            },
            c,
            &device,
            deadline,
        )
        .await?;
        if let Err(e) = refresh {
            let mut errors = match field(&result, "errors") {
                Value::Array(a) => {
                    let mut out = Vec::new();
                    for v in a {
                        json::push(&mut out, clone(v)?, 256)?;
                    }
                    out
                }
                _ => Vec::new(),
            };
            json::push(
                &mut errors,
                json::string(&join(&[
                    "Apparaatmodel verversen: ",
                    &stulp_sdk::message(&e)?,
                ])?)?,
                256,
            )?;
            json::set(&mut result, "errors", Value::Array(errors))?;
        }
        Ok(result)
    }
    pub(crate) async fn remove<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
    ) -> Result<Value> {
        let device = clone(c.state().device(id)?)?;
        let node = node_id(&device)?;
        // Bridgebroers blijven werken zolang er nog een endpoint van deze node bestaat.
        if node_devices(c.state().root(), node)?.len() > 1 {
            return Ok(Value::Null);
        }
        let fabric = u8::try_from(json::uint(field(&device, "store"), "matter.fabricIndex"))
            .ok()
            .filter(|v| *v != 0)
            .ok_or(Error::Invalid("Matter fabric-index ontbreekt."))?;
        let (index, session, address) = self.connect(c, &device).await?;
        let deadline = c.now().saturating_add(30000);
        let result = Commissioning {
            im: Interaction {
                network: &mut self.network,
                address: &address,
                session,
            },
            endpoint: 0,
        }
        .remove(c, fabric, deadline)
        .await;
        self.expire(index, c.now())?;
        result?;
        Ok(Value::Null)
    }
    pub(crate) async fn settings<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        patch: &Value,
    ) -> Result<Value> {
        let device = clone(c.state().device(id)?)?;
        let plans = crate::settings::plan(crate::settings::metadata(&device), patch)?;
        if plans.is_empty() {
            return Ok(Value::Null);
        }
        let (index, session, address) = self.connect(c, &device).await?;
        for (setting, value) in plans {
            let deadline = c.now().saturating_add(30000);
            if let Err(e) = setting
                .write(
                    &mut Interaction {
                        network: &mut self.network,
                        address: &address,
                        session,
                    },
                    c,
                    value,
                    deadline,
                )
                .await
            {
                self.expire(index, c.now())?;
                return Err(e);
            }
            c.merge(
                id,
                "settings",
                json::fields(&[(&setting.id, Value::uint(u64::from(value)))])?,
            )
            .await?;
        }
        Ok(Value::Null)
    }
    /// Werkt de apparaten bij en start hun flowtriggers; geeft het aantal
    /// gestarte triggers. `flows` is onwaar voor het eerste rapport van een
    /// abonnement: dat speelt de eventbuffer van het apparaat af, met
    /// knopdrukken van voor de (her)start. De eventmarker schuift wel op.
    /// (03-10: na een restore wisselde "knop 2" vijftien keer een lamp.)
    async fn publish<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        node: u64,
        reports: &Reports,
        flows: bool,
    ) -> Result<usize> {
        let mut started = 0;
        for d in node_devices(c.state().root(), node)? {
            let id = json::text(&d, "id");
            if let Some(update) = reports::apply(&d, reports)? {
                // Marker eerst opslaan: een herhaald UDP-event start een flow nooit tweemaal.
                if field(&d, "store") != field(&update.device, "store") {
                    c.store(id, clone(field(&update.device, "store"))?).await?;
                }
                if field(&d, "settings") != field(&update.device, "settings") {
                    c.merge(id, "settings", clone(field(&update.device, "settings"))?)
                        .await?;
                }
                c.values(id, clone(field(&update.device, "state"))?).await?;
                for event in update.events.into_iter().filter(|_| flows) {
                    started += 1;
                    c.call(
                        "flow.trigger",
                        &json::fields(&[
                            ("kind", json::string("trigger")?),
                            ("system", Value::Bool(true)),
                            ("id", json::string(&event.card)?),
                            ("tokens", event.tokens),
                            ("state", event.state),
                        ])?,
                    )
                    .await?;
                }
            }
            if !json::boolean(&d, "available") {
                c.available(id, true).await?;
            }
        }
        Ok(started)
    }
    pub(crate) async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.reports(c).await?;
        self.maintain(c).await
    }
    /// Verwerkt binnengekomen rapporten: kort werk, dat een werker direct doet,
    /// zonder achtergrondtaak (die begint met een kopie van de hele staat).
    pub(crate) async fn reports<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        self.follow(c)?;
        self.network.tick(c)?;
        for _ in 0..16 {
            let Some(event) = self.pending.take().or_else(|| self.network.event()) else {
                break;
            };
            if let Event::Acknowledged(h, _) | Event::Failed(h) = event {
                self.network.close(h);
                continue;
            }
            if let Event::Accepted(h) = event {
                let (_, session, protocol, _) = self.network.peer(h)?;
                let owner = self
                    .nodes
                    .iter()
                    .position(|n| n.session == Some(session) && n.subscription.is_some());
                if protocol != 1 || owner.is_none() {
                    self.network.acknowledge(c, h)?;
                    self.network.close(h);
                    continue;
                }
                let i = owner.ok_or(Error::Invalid("subscription owner"))?;
                let report0 = c.now();
                let id = self.nodes[i]
                    .subscription
                    .ok_or(Error::Invalid("subscription id"))?;
                let peer = json::copy(self.network.peer(h)?.0)?;
                let result = Interaction {
                    network: &mut self.network,
                    address: &peer,
                    session,
                }
                .report(c, h, id, c.now().saturating_add(30000))
                .await;
                match result {
                    Ok(r) => {
                        self.nodes[i].expires = c.now().saturating_add(self.nodes[i].next);
                        let received = c.now();
                        let started = self.publish(c, self.nodes[i].id, &r, true).await?;
                        // De sensorkant van "beweging naar lamp": ontvangen en
                        // doorgegeven, tot en met de flowtrigger naar Stulp.
                        let total = c.now().saturating_sub(report0);
                        if started > 0 || total >= 500 {
                            let mut line = String::new();
                            if line.try_reserve(128).is_ok() {
                                let _ = write!(
                                    line,
                                    "MATTER_REPORT node={:016X} triggers={started} receive_ms={} publish_ms={} total_ms={total}",
                                    self.nodes[i].id,
                                    received.saturating_sub(report0),
                                    c.now().saturating_sub(received),
                                );
                                c.log("info", &line)?;
                            }
                        }
                    }
                    Err(Error::Cancelled) => return Err(Error::Cancelled),
                    Err(Error::Core(e)) => return Err(Error::Core(e)),
                    Err(_) => self.expire(i, c.now())?,
                }
            }
        }
        Ok(())
    }
    /// Discovery en (her)verbinden van de eigen nodes: lang werk met handshakes,
    /// dat bij een werker in een afbreekbare achtergrondtaak loopt.
    pub(crate) async fn maintain<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        if self.discovers() && !self.nodes.is_empty() && c.now() >= self.next_discovery {
            self.next_discovery = c.now().saturating_add(300000);
            match self.browse(c, &[discovery::OPERATIONAL], 4000).await {
                Ok(nodes) => self.refresh(c, &nodes).await?,
                Err(Error::Cancelled) => return Err(Error::Cancelled),
                Err(Error::Core(e)) => return Err(Error::Core(e)),
                Err(_) => (), // Een opgeslagen adres blijft geldig wanneer multicast geblokkeerd is.
            }
            return Ok(());
        }
        let now = c.now();
        if now < self.settle {
            return Ok(());
        }
        let scope = self.scope;
        let Some(i) = next_node_where(&self.nodes, now, self.route.next, |n| {
            maintains(scope, n.id)
        }) else {
            return Ok(());
        };
        self.settle = now.saturating_add(SETTLE_MS);
        if self.nodes[i].subscription.is_some() {
            self.expire(i, now)?;
        }
        if now < self.route.next {
            return Ok(());
        }
        let devices = node_devices(c.state().root(), self.nodes[i].id)?;
        let Some(device) = devices.first() else {
            return Ok(());
        };
        let result = async {
            let (_, session, address) = self.connect(c, device).await?;
            match self
                .refresh_model(c, device, session, &address, c.now().saturating_add(40000))
                .await
            {
                Ok(()) => self.nodes[i].model_failures = 0,
                Err(Error::Cancelled) => return Err(Error::Cancelled),
                Err(Error::Core(e)) => return Err(Error::Core(e)),
                Err(e) if crate::route::missing(&e) => return Err(e),
                Err(e) => {
                    self.nodes[i].model_failures = self.nodes[i].model_failures.saturating_add(1);
                    if self.nodes[i].model_failures == 1 {
                        c.log(
                            "warn",
                            &join(&[
                                "Matter device-model refresh failed; subscribing to stored model: ",
                                &stulp_sdk::message(&e)?,
                            ])?,
                        )?;
                    }
                }
            }
            let current = node_devices(c.state().root(), self.nodes[i].id)?;
            let request = reports::subscription(&current)?;
            Interaction {
                network: &mut self.network,
                address: &address,
                session,
            }
            .subscribe(c, &request, c.now().saturating_add(60000))
            .await
        }
        .await;
        if matches!(result, Err(Error::Cancelled)) {
            // The authenticated session can still serve foreground commands. The unfinished
            // background exchange cleaned itself up; retry maintenance on a later tick.
            self.nodes[i].next = c.now().saturating_add(1000);
            return Err(Error::Cancelled);
        }
        if result.as_ref().is_err_and(crate::route::missing) {
            let now = c.now();
            let first = self.route.failed(now, c.random()?[0]);
            if first {
                c.log("info", "MATTER_ROUTE_WAIT grace_ms=120000")?;
            }
            self.expire(i, now)?;
            self.nodes[i].next = self.route.next;
            if self.route.loud(now) {
                for node in &self.nodes {
                    if node.subscription.is_some() {
                        continue;
                    }
                    for device in node_devices(c.state().root(), node.id)? {
                        if manual_address(json::text(field(&device, "store"), "matter.address"))
                            .ok()
                            .and_then(|s| s.parse::<core::net::SocketAddr>().ok())
                            .is_some_and(|a| a.is_ipv6())
                        {
                            c.unavailable(json::text(&device, "id"), "no IPv6 route")
                                .await?;
                        }
                    }
                }
            }
            return Ok(());
        }
        if self.route.recovered() {
            c.log("info", "MATTER_ROUTE_RECOVERED")?;
            for node in &mut self.nodes {
                if node.subscription.is_none() {
                    node.next = c.now();
                }
            }
        }
        match result {
            Ok(s) => {
                self.nodes[i].subscription = Some(s.id);
                self.nodes[i].next = reports::watchdog(s.maximum);
                self.nodes[i].expires = c.now().saturating_add(self.nodes[i].next);
                self.nodes[i].backoff = 1000;
                self.nodes[i].reachable(c.now());
                self.publish(c, self.nodes[i].id, &s.reports, false).await?;
            }
            Err(Error::Core(e)) => return Err(Error::Core(e)),
            Err(e) => {
                let text = stulp_sdk::message(&e)?;
                self.expire(i, c.now())?;
                let delay = self.nodes[i].failed(c.now());
                if self.nodes[i].failures == 1 {
                    c.log(
                        "warn",
                        &join(&[
                            "MATTER_RECONNECT_FAILED node=",
                            json::text(field(device, "store"), "matter.nodeId"),
                            " error=",
                            &text,
                        ])?,
                    )?;
                }
                // Eén regel per wachtperiode; opdrachten van de gebruiker
                // lopen via de werkers en wachten hier niet op.
                let mut line = String::new();
                if line.try_reserve(64).is_ok() {
                    let _ = write!(
                        line,
                        "MATTER_BACKOFF node={:016X} seconds={}",
                        self.nodes[i].id,
                        delay / 1000
                    );
                    c.log("info", &line)?;
                }
                for d in devices {
                    c.unavailable(json::text(&d, "id"), &text).await?;
                }
            }
        }
        Ok(())
    }
    async fn refresh<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        found: &[discovery::Node],
    ) -> Result {
        let compressed = u64::from_be_bytes(self.fabric.case().compressed_id()?);
        for candidate in found {
            let Some((fabric, node)) = candidate.operational() else {
                continue;
            };
            if fabric != compressed {
                continue;
            }
            if !self.nodes.iter().any(|n| n.id == node) {
                continue;
            }
            let address = match discovered_address(c, candidate).await {
                Ok(a) => a,
                Err(Error::Cancelled) => return Err(Error::Cancelled),
                Err(Error::Core(e)) => return Err(Error::Core(e)),
                Err(_) => continue,
            };
            for d in node_devices(c.state().root(), node)? {
                let mut patch = timing_patch(candidate.timing())?;
                json::set(&mut patch, "matter.address", json::string(&address)?)?;
                if json::text(field(&d, "store"), "matter.address") != address
                    && let Some(i) = self.nodes.iter().position(|n| n.id == node)
                {
                    self.expire(i, c.now())?;
                }
                c.store(json::text(&d, "id"), patch).await?;
            }
        }
        Ok(())
    }
    pub(crate) fn topology(&self, root: &Value, wall: u64) -> Result<Value> {
        let mut nodes = Vec::new();
        let mut sorted = Vec::new();
        for n in &self.nodes {
            json::push(&mut sorted, n, 64)?;
        }
        sorted.sort_unstable_by_key(|n| n.id);
        for n in sorted {
            let mut devices = Vec::new();
            let mut address = String::new();
            let mut credentialed = false;
            for d in node_devices(root, n.id)? {
                let store = field(&d, "store");
                if address.is_empty() {
                    address = json::copy(json::text(store, "matter.address"))?;
                }
                credentialed |= !json::text(store, "matter.noc").is_empty();
                json::push(
                    &mut devices,
                    json::fields(&[
                        ("id", clone(field(&d, "id"))?),
                        ("name", clone(field(&d, "name"))?),
                        (
                            "endpoint",
                            json::string(&crate::settings::decimal(u64::from(
                                crate::devices::primary(&d),
                            ))?)?,
                        ),
                        ("available", Value::Bool(json::boolean(&d, "available"))),
                        (
                            "unavailableMessage",
                            clone(field(&d, "unavailableMessage"))?,
                        ),
                    ])?,
                    256,
                )?;
            }
            json::push(
                &mut nodes,
                json::fields(&[
                    ("nodeId", json::string(&model::node_id(n.id)?)?),
                    ("address", json::string(&address)?),
                    ("credentialed", Value::Bool(credentialed)),
                    ("sessionOpen", Value::Bool(n.session.is_some())),
                    ("subscribed", Value::Bool(n.subscription.is_some())),
                    ("devices", Value::Array(devices)),
                ])?,
                64,
            )?;
        }
        Ok(json::fields(&[
            ("generatedAt", json::string(&crate::ui::date(wall)?)?),
            (
                "fabric",
                json::fields(&[
                    (
                        "fabricId",
                        json::string(&model::node_id(self.fabric.id())?)?,
                    ),
                    (
                        "controllerId",
                        json::string(&model::node_id(self.fabric.case().node())?)?,
                    ),
                    ("nodes", Value::uint(nodes.len() as u64)),
                ])?,
            ),
            ("nodes", Value::Array(nodes)),
        ])?)
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod address_tests {
    use super::*;
    fn offline(id: u64) -> Node {
        Node {
            id,
            session: None,
            subscription: None,
            expires: 0,
            next: 1000,
            backoff: 1000,
            model_failures: 0,
            failures: 0,
        }
    }
    #[test]
    fn each_node_is_maintained_by_exactly_one_command_worker() {
        let nodes: Vec<_> = (0x10000..0x10030).map(offline).collect();
        for node in &nodes {
            let owners: Vec<_> = (1..=stulp_sdk::jobs::pool::COMMAND_WORKERS)
                .filter(|k| maintains(Scope::Owner(*k), node.id))
                .collect();
            assert_eq!(owners, [stulp_sdk::jobs::pool::owner(node.id)]);
            assert!(!maintains(Scope::Lifecycle, node.id));
            assert!(maintains(Scope::All, node.id));
        }
        // Een werker kiest alleen uit zijn eigen nodes; de hoofdwerker uit geen.
        let k = stulp_sdk::jobs::pool::owner(nodes[3].id);
        let picked =
            next_node_where(&nodes, 2000, 0, |n| maintains(Scope::Owner(k), n.id)).unwrap();
        assert_eq!(stulp_sdk::jobs::pool::owner(nodes[picked].id), k);
        assert_eq!(
            next_node_where(&nodes, 2000, 0, |n| maintains(Scope::Lifecycle, n.id)),
            None
        );
    }
    #[test]
    fn slow_offline_nodes_do_not_starve_the_rest_of_a_restored_home() {
        let mut nodes: Vec<_> = (1..=34).map(offline).collect();
        let mut now = 1000;
        for expected in 0..nodes.len() {
            let index = next_node(&nodes, now, 0).unwrap();
            assert_eq!(index, expected);
            // CASE kan veertig seconden duren, ruim langer dan de eerste retryvertraging.
            now += 40000;
            nodes[index].next = now + 1000;
        }
        assert_eq!(next_node(&nodes, now, 0), Some(0));
    }
    #[test]
    fn unreachable_node_backs_off_from_a_minute_to_half_an_hour() {
        assert_eq!(retry_delay(0), 0);
        let mut node = offline(1);
        let mut now = 1000;
        for expected in [60, 120, 240, 480, 960, 1800, 1800] {
            assert_eq!(node.failed(now), expected * 1000);
            assert_eq!(node.next, now + expected * 1000);
            // Het onderhoud slaat de node over tot de wachttijd verstreken is.
            assert_eq!(
                next_node(core::slice::from_ref(&node), node.next - 1, 0),
                None
            );
            now = node.next;
            assert_eq!(next_node(core::slice::from_ref(&node), now, 0), Some(0));
            now += 40000;
        }
        assert_eq!(retry_delay(u32::MAX), RETRY_LIMIT_MS);
        // Succes langs welk pad dan ook: meteen weer aan de beurt, en de
        // volgende mislukking begint opnieuw bij een minuut.
        node.reachable(now);
        assert_eq!(node.failures, 0);
        assert_eq!(next_node(core::slice::from_ref(&node), now, 0), Some(0));
        assert_eq!(node.failed(now), 60000);
    }
    #[test]
    fn route_backoff_blocks_reconnects_but_still_expires_subscriptions() {
        let mut nodes = [offline(1), offline(2)];
        nodes[1].subscription = Some(7);
        nodes[1].expires = 2000;
        assert_eq!(next_node(&nodes, 1500, 10000), None);
        assert_eq!(next_node(&nodes, 2000, 10000), Some(1));
        nodes[1].subscription = None;
        assert_eq!(next_node(&nodes, 2000, 10000), None);
        assert_eq!(next_node(&nodes, 10000, 10000), Some(0));
    }
    #[test]
    fn names_default_port_and_ipv6_interfaces() {
        for (input, expected) in [
            (" light.local ", "light.local:5540"),
            ("light.local:5555", "light.local:5555"),
            ("192.0.2.1", "192.0.2.1:5540"),
            ("::1", "[::1]:5540"),
            ("[::1]", "[::1]:5540"),
            ("fe80::1%en0", "[fe80::1%en0]:5540"),
            ("[fe80::1%en0]:5555", "[fe80::1%en0]:5555"),
        ] {
            assert_eq!(lookup_address(input).unwrap(), expected);
        }
        for input in ["", "[::1", "[::1]garbage", "light local"] {
            assert!(lookup_address(input).is_err());
        }
    }
}
