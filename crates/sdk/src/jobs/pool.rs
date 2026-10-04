//! Command owners plus the main lifecycle owner, scoped to one controller attach.
use super::*;
use crate::Plugin;
use alloc::boxed::Box;
use core::{future::Future, pin::Pin, task::Poll};
/// Commando's naar verschillende nodes lopen tegelijk, tot dit aantal. Twee was
/// te weinig: vier lampen gingen in twee golven, terwijl de grote core van de
/// LicheeRV nauwelijks iets deed (03-10). Acht dekt een scène grotendeels in
/// één golf.
pub const COMMAND_WORKERS: usize = 8;
const OWNERS: usize = COMMAND_WORKERS + 1;
/// De vaste werker van een sleutel. Een node hoort altijd bij dezelfde werker,
/// zodat de plugin daar één sessie per apparaat kan houden (en er bijvoorbeeld
/// ook het abonnement kan onderhouden). Tot 03-10 ging een node naar de laatste
/// werker als die vrij was, anders naar een andere, die dan eerst een eigen
/// Matter-handshake van 0,7 tot 1,7 s deed: de Go-versie deelde er één.
pub fn owner(key: u64) -> usize {
    1 + (key % COMMAND_WORKERS as u64) as usize
}
/// Route device operations by physical-node identity. None retains the main plugin's handling.
pub trait Key {
    /// Identical keys are never executed concurrently, even for different logical endpoints.
    fn key(&self, snapshot: &Value, method: &str, params: &Value) -> Result<Option<u64>>;
    /// Lifecycle mutations wait until command workers finish and prevent later overtaking.
    fn barrier(&self, method: &str) -> bool;
}
struct Lease {
    request: u64,
    key: u64,
}
fn udp_id(command: &mut UdpCommand, worker: usize) -> Result {
    let id = match command {
        UdpCommand::Bind { id, .. }
        | UdpCommand::Send { id, .. }
        | UdpCommand::Close { id }
        | UdpCommand::JoinV4 { id, .. }
        | UdpCommand::JoinV6 { id, .. } => id,
    };
    if !(1..=2).contains(id) {
        return Err(Error::Invalid("worker UDP id outside its two-socket lease"));
    }
    *id += worker as u64 * 2;
    Ok(())
}
fn event_owner(event: &mut UdpEvent) -> Result<usize> {
    let id = match event {
        UdpEvent::Bound(id, _)
        | UdpEvent::Closed(id)
        | UdpEvent::Error(id, _)
        | UdpEvent::Data { id, .. } => id,
    };
    if !(1..=(OWNERS as u64 * 2)).contains(id) {
        return Err(Error::Invalid("UDP event outside worker leases"));
    }
    let worker = (*id - 1) / 2;
    *id = (*id - 1) % 2 + 1;
    Ok(worker as usize)
}
async fn snapshot<T: Transport>(c: &Client<T>, tx: &mut Sender<'_, In, CAP>) -> Result {
    tx.send(In::Frame(decode(Frame::request(
        0,
        "state.snapshot",
        c.state().root(),
    )?)?))
    .await;
    Ok(())
}
/// Geeft de staat-events sinds de vorige ronde door aan elke werker die bij
/// was: één apparaat per event in plaats van de hele staat als JSON heen en
/// terug (op de LicheeRV ~300 ms per kopie, twee per lampcommando, 03-10).
/// Liep het journaal over, dan is wie bij was nu verouderd; de hoofdwerker en
/// werkers met een callback krijgen meteen een kopie, de rest pas bij werk.
async fn fan_out<T: Transport>(
    c: &mut Client<T>,
    writers: &mut [Sender<'_, In, CAP>],
    sent: &mut [u64; OWNERS],
    revision: &mut u64,
    active: [bool; OWNERS],
) -> Result {
    if c.state.revision == *revision {
        return Ok(());
    }
    let events = c.journal.as_mut().map(core::mem::take).unwrap_or_default();
    let lost = core::mem::take(&mut c.journal_lost);
    for (worker, tx) in writers.iter_mut().enumerate() {
        if lost || sent[worker] != *revision {
            if active[worker] {
                snapshot(c, tx).await?;
                sent[worker] = c.state.revision;
            }
            continue;
        }
        for event in &events {
            tx.send(In::Frame(Frame {
                kind: event.kind,
                id: event.id,
                value: clone(&event.value)?,
            }))
            .await;
        }
        sent[worker] = c.state.revision;
    }
    *revision = c.state.revision;
    Ok(())
}
/// Serve lifecycle/UI and up to [`COMMAND_WORKERS`] physical nodes at once on
/// the same executor. A node always goes to its [`owner`] worker, so the plugin
/// keeps one session per device there. No command is canceled or replayed to
/// make room for another node.
pub async fn serve<T: Transport, P: Plugin, W: Plugin, K: Key>(
    c: &mut Client<T>,
    main: &mut P,
    mut workers: [W; COMMAND_WORKERS],
    key: K,
) -> Result {
    let outgoing: [Channel<Out, CAP>; OWNERS] = core::array::from_fn(|_| Channel::new());
    let incoming: [Channel<In, CAP>; OWNERS] = core::array::from_fn(|_| Channel::new());
    let mut readers = Vec::new();
    let mut writers = Vec::new();
    let mut clients = Vec::new();
    for index in 0..OWNERS {
        let (tx, rx) = outgoing[index]
            .split()
            .ok_or(Error::Invalid("worker channel already split"))?;
        let (to_work, from_pump) = incoming[index]
            .split()
            .ok_or(Error::Invalid("worker channel already split"))?;
        let random = ChaCha20Rng::from_seed(c.random()?);
        let port = Port {
            tx,
            rx: from_pump,
            frames: VecDeque::new(),
            udp: VecDeque::new(),
            browse: None,
            random,
            now: c.now(),
            wall: c.wall_time()?,
            cancelled: false,
            browsing: false,
            resolving: false,
            resolved: None,
        };
        json::push(&mut readers, rx, OWNERS)?;
        json::push(&mut writers, to_work, OWNERS)?;
        json::push(
            &mut clients,
            Client::from_snapshot(port, clone(c.state().root())?)?,
            OWNERS,
        )?;
    }
    if clients.len() != OWNERS {
        return Err(Error::Invalid("worker count mismatch"));
    }
    let mut controller = clients.remove(0);
    c.journal = Some(Vec::new());
    c.journal_lost = false;
    let mut command_clients = clients;
    let pump = async {
        let mut leases: [Option<Lease>; OWNERS] = core::array::from_fn(|_| None);
        // De revisie van de staat die elke werker het laatst kreeg. Een kopie
        // van de staat is het hele huisdocument naar JSON en terug; tot 03-10
        // ging die vóór elke callback naar de werker en bij elke wijziging
        // naar alle drie, honderden keren bij een start met ~70
        // Matter-apparaten (0,6 tot 1,7 s per callback op de LicheeRV). Nu
        // krijgt een werker een kopie alleen als de zijne verouderd is.
        let mut sent = [c.state.revision; OWNERS];
        let mut pending = Vec::new();
        let mut barrier = None;
        let mut revision = c.state.revision;
        let mut browsing = None;
        let mut resolving = None;
        let mut browse_queue = VecDeque::new();
        let mut resolve_queue = VecDeque::new();
        loop {
            for worker in 0..OWNERS {
                for _ in 0..16 {
                    let Some(out) = readers[worker].try_recv() else {
                        break;
                    };
                    match out {
                        Out::Frame(frame) if matches!(frame.kind, Kind::Response | Kind::Error) => {
                            if worker != 0 {
                                let lease = leases[worker]
                                    .take()
                                    .ok_or(Error::Invalid("worker replied without callback"))?;
                                if lease.request != frame.id {
                                    return Err(Error::Invalid(
                                        "worker callback ownership mismatch",
                                    ));
                                }
                            }
                            if barrier == Some(frame.id) {
                                barrier = None;
                            }
                            c.transport.send(&frame.value).await?;
                        }
                        Out::Udp(mut command) => {
                            udp_id(&mut command, worker)?;
                            c.udp(command)?;
                        }
                        Out::Browse(request) => {
                            reserve(&mut browse_queue)?;
                            browse_queue.push_back((worker, request));
                        }
                        Out::Resolve(address) => {
                            if address.parse::<core::net::SocketAddr>().is_ok() {
                                let mut values = Vec::new();
                                json::push(&mut values, address, 16)?;
                                writers[worker].send(In::Resolved(Ok(values))).await;
                            } else {
                                reserve(&mut resolve_queue)?;
                                resolve_queue.push_back((worker, address));
                            }
                        }
                        Out::Frame(f) => {
                            // Een aanroep van een werker naar de controller.
                            // De staat die daarbij verandert gaat als events
                            // naar iedereen, vóór het antwoord naar de vrager.
                            if f.kind != Kind::Request {
                                return Err(Error::Invalid("local task must send requests"));
                            }
                            let params = crate::util::field(&f.value, "p");
                            let result = if f.method() == "state.set" {
                                c.app_state(clone(crate::util::field(params, "state"))?)
                                    .await
                                    .map(|()| Value::Null)
                            } else {
                                c.call(f.method(), params).await
                            };
                            let response = match result {
                                Ok(v) => Frame::response(f.id, Ok(v))?,
                                Err(e) => Frame::response(f.id, Err(&message(&e)?))?,
                            };
                            let active = core::array::from_fn(|w| w == 0 || leases[w].is_some());
                            fan_out(c, &mut writers, &mut sent, &mut revision, active).await?;
                            if sent[worker] != c.state.revision {
                                snapshot(c, &mut writers[worker]).await?;
                                sent[worker] = c.state.revision;
                            }
                            writers[worker].send(In::Frame(decode(response)?)).await;
                        }
                        out => relay(c, &mut writers[worker], out).await?,
                    }
                }
            }
            if browsing.is_none()
                && let Some((worker, request)) = browse_queue.pop_front()
            {
                match c.start_datagrams(request) {
                    Ok(()) => browsing = Some(worker),
                    Err(e) => writers[worker].send(In::Browse(Err(e))).await,
                }
            }
            if resolving.is_none()
                && let Some((worker, address)) = resolve_queue.pop_front()
            {
                match c.start_resolve(address) {
                    Ok(()) => resolving = Some(worker),
                    Err(e) => writers[worker].send(In::Resolved(Err(e))).await,
                }
            }
            if let Some(result) = c.poll_datagrams()
                && let Some(worker) = browsing.take()
            {
                writers[worker].send(In::Browse(result)).await;
            }
            if let Some(result) = c.poll_resolve()
                && let Some(worker) = resolving.take()
            {
                writers[worker].send(In::Resolved(result)).await;
            }
            for _ in 0..32 {
                let Some(mut event) = c.poll_udp() else {
                    break;
                };
                let owner = event_owner(&mut event)?;
                writers[owner].send(In::Udp(event)).await;
            }
            let active = core::array::from_fn(|w| w == 0 || leases[w].is_some());
            fan_out(c, &mut writers, &mut sent, &mut revision, active).await?;
            for tx in &mut writers {
                if tx.free() == CAP {
                    tx.send(In::Tick {
                        now: c.now(),
                        wall: c.wall_time()?,
                        entropy: c.random()?,
                    })
                    .await;
                }
            }
            let frame = if c.inbox.is_empty() {
                c.pump().await?
            } else {
                Some(c.inbox.remove(0))
            };
            if let Some(frame) = frame.filter(|f| f.kind == Kind::Request) {
                json::push(&mut pending, frame, super::super::MAX_INBOX)?;
            }
            // Events van deze ronde eerst door, zodat een werker die nu een
            // commando krijgt bij is en geen volledige kopie nodig heeft.
            let active = core::array::from_fn(|w| w == 0 || leases[w].is_some());
            fan_out(c, &mut writers, &mut sent, &mut revision, active).await?;
            let mut at = 0;
            while at < pending.len() {
                let frame = &pending[at];
                let target = match key.key(
                    c.state().root(),
                    frame.method(),
                    crate::util::field(&frame.value, "p"),
                ) {
                    Ok(v) => v,
                    Err(e) => {
                        let frame = pending.remove(at);
                        c.transport
                            .send(&Frame::response(frame.id, Err(&message(&e)?))?)
                            .await?;
                        continue;
                    }
                };
                if barrier.is_some() {
                    break;
                }
                if let Some(node) = target {
                    if leases.iter().flatten().any(|l| l.key == node) {
                        at += 1;
                        continue;
                    }
                    let worker = owner(node);
                    if leases[worker].is_some() {
                        at += 1;
                        continue;
                    }
                    let frame = pending.remove(at);
                    leases[worker] = Some(Lease {
                        request: frame.id,
                        key: node,
                    });
                    if sent[worker] != c.state.revision {
                        snapshot(c, &mut writers[worker]).await?;
                        sent[worker] = c.state.revision;
                    }
                    writers[worker].send(In::Frame(frame)).await;
                } else {
                    let is_barrier = key.barrier(frame.method());
                    if is_barrier && leases.iter().any(Option::is_some) {
                        break;
                    }
                    let frame = pending.remove(at);
                    if is_barrier {
                        barrier = Some(frame.id);
                    }
                    if sent[0] != c.state.revision {
                        snapshot(c, &mut writers[0]).await?;
                        sent[0] = c.state.revision;
                    }
                    writers[0].send(In::Frame(frame)).await;
                }
            }
            hop_sync::yield_now().await;
        }
        #[allow(unreachable_code)]
        Ok::<(), Error>(())
    };
    let workers = async {
        let mut running: Vec<Pin<Box<dyn Future<Output = Result> + '_>>> = Vec::new();
        running
            .try_reserve_exact(OWNERS)
            .map_err(|_| stulp_core::Error::Memory)?;
        running.push(Box::pin(controller.serve_serial(main)));
        for (client, plugin) in command_clients.iter_mut().zip(workers.iter_mut()) {
            running.push(Box::pin(client.serve_serial(plugin)));
        }
        // Eén werker die stopt (een fout of het einde van de attach) beëindigt
        // de pool, zoals de oude select over drie werkers.
        core::future::poll_fn(|cx| {
            for future in &mut running {
                if let Poll::Ready(r) = future.as_mut().poll(cx) {
                    return Poll::Ready(r);
                }
            }
            Poll::Pending
        })
        .await
    };
    let result = match select(pump, workers).await {
        Either::Left(r) | Either::Right(r) => r,
    };
    c.journal = None;
    c.journal_lost = false;
    result
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]
    extern crate std;
    use super::*;
    struct Wire {
        now: u64,
        input: VecDeque<Frame>,
        answers: Vec<(u64, u64)>,
    }
    impl Transport for Wire {
        fn now(&self) -> u64 {
            self.now
        }
        fn wall_time(&self) -> Result<u64> {
            Ok(1_790_000_000)
        }
        fn random(&mut self) -> Result<[u8; 32]> {
            Ok([17; 32])
        }
        async fn send(&mut self, v: &Value) -> Result {
            let frame = decode(clone(v)?)?;
            if frame.kind == Kind::Request {
                self.input
                    .push_back(decode(Frame::response(frame.id, Ok(Value::Null))?)?);
            } else {
                self.answers.push((frame.id, self.now));
            }
            Ok(())
        }
        async fn next(&mut self) -> Result<crate::Event> {
            self.now += 5;
            if self.answers.len() == 3 {
                return Err(Error::Transport("finished"));
            }
            if self.now > 10000 {
                return Err(Error::Timeout);
            }
            Ok(self
                .input
                .pop_front()
                .map(crate::Event::Frame)
                .unwrap_or(crate::Event::Tick))
        }
    }
    struct Keys;
    impl Key for Keys {
        fn key(&self, _: &Value, _: &str, p: &Value) -> Result<Option<u64>> {
            Ok(Some(json::uint(p, "node")))
        }
        fn barrier(&self, _: &str) -> bool {
            false
        }
    }
    struct Worker;
    impl Plugin for Worker {
        fn manifest(&self) -> &'static [u8] {
            br#"{"id":"com.test.pool"}"#
        }
        async fn handle<T: Transport>(
            &mut self,
            c: &mut Client<T>,
            _: &str,
            p: &Value,
        ) -> Result<Value> {
            let until = c.now() + json::uint(p, "delay");
            while c.now() < until {
                c.idle().await?;
            }
            Ok(Value::Null)
        }
    }
    #[test]
    fn different_nodes_complete_independently_but_same_node_keeps_order() {
        let mut input = VecDeque::new();
        for (id, node, delay) in [(100, 1, 1500), (101, 2, 20), (102, 1, 0)] {
            input.push_back(
                decode(
                    Frame::request(
                        id,
                        "command",
                        &json::fields(&[
                            ("node", Value::uint(node)),
                            ("delay", Value::uint(delay)),
                        ])
                        .unwrap(),
                    )
                    .unwrap(),
                )
                .unwrap(),
            );
        }
        let wire = Wire {
            now: 0,
            input,
            answers: Vec::new(),
        };
        let snapshot = json::parse(
            br#"{"protocol":1,"appId":"com.test.pool","devices":{},"settings":{},"manifest":{}}"#,
        )
        .unwrap();
        let mut c = Client::from_snapshot(wire, snapshot).unwrap();
        let result = hostnet::block_on(serve(
            &mut c,
            &mut Worker,
            core::array::from_fn(|_| Worker),
            Keys,
        ));
        assert!(matches!(result, Err(Error::Transport("finished"))));
        let out = c.into_transport().answers;
        assert_eq!(out.iter().map(|v| v.0).collect::<Vec<_>>(), [101, 100, 102]);
        assert!(out[0].1 < 500);
        assert!(out[1].1 >= 1500);
        assert!(out[2].1 >= out[1].1);
    }
    use alloc::string::ToString;
    /// Een transport dat de antwoorden zelf bewaart, om te zien wat een werker zag.
    struct Seen {
        now: u64,
        input: VecDeque<Frame>,
        answers: Vec<(u64, Value)>,
    }
    impl Transport for Seen {
        fn now(&self) -> u64 {
            self.now
        }
        fn wall_time(&self) -> Result<u64> {
            Ok(1_790_000_000)
        }
        fn random(&mut self) -> Result<[u8; 32]> {
            Ok([17; 32])
        }
        async fn send(&mut self, v: &Value) -> Result {
            let frame = decode(clone(v)?)?;
            if frame.kind == Kind::Request {
                self.input
                    .push_back(decode(Frame::response(frame.id, Ok(Value::Null))?)?);
            } else {
                self.answers
                    .push((frame.id, clone(json::get(v, "r").unwrap_or(&Value::Null))?));
            }
            Ok(())
        }
        async fn next(&mut self) -> Result<crate::Event> {
            self.now += 5;
            if self.answers.len() == 3 || self.now > 10000 {
                return Err(Error::Transport("finished"));
            }
            Ok(self
                .input
                .pop_front()
                .map(crate::Event::Frame)
                .unwrap_or(crate::Event::Tick))
        }
    }
    /// Zegt welke naam het apparaat in de eigen staat van de werker heeft.
    struct Reader;
    impl Plugin for Reader {
        fn manifest(&self) -> &'static [u8] {
            br#"{"id":"com.test.pool"}"#
        }
        async fn handle<T: Transport>(
            &mut self,
            c: &mut Client<T>,
            _: &str,
            _: &Value,
        ) -> Result<Value> {
            clone(json::get(c.state().device("d")?, "name").unwrap_or(&Value::Null))
        }
    }
    fn device(name: &str) -> Frame {
        decode(
            Frame::request(
                0,
                "state.device",
                &json::fields(&[
                    ("deviceId", json::string("d").unwrap()),
                    (
                        "device",
                        json::fields(&[("name", json::string(name).unwrap())]).unwrap(),
                    ),
                ])
                .unwrap(),
            )
            .unwrap(),
        )
        .unwrap()
    }
    fn command(id: u64, node: u64) -> Frame {
        decode(
            Frame::request(
                id,
                "command",
                &json::fields(&[("node", Value::uint(node))]).unwrap(),
            )
            .unwrap(),
        )
        .unwrap()
    }
    #[test]
    fn workers_follow_state_events_without_a_full_copy() {
        // Elke werker, ook een die nog nooit werk had, ziet de staat zoals de
        // controller hem net doorgaf: de events gaan naar iedereen.
        let input = VecDeque::from([
            device("one"),
            command(100, 1),
            device("two"),
            command(101, 2),
            device("three"),
            command(102, 1),
        ]);
        let wire = Seen {
            now: 0,
            input,
            answers: Vec::new(),
        };
        let snapshot = json::parse(
            br#"{"protocol":1,"appId":"com.test.pool","devices":{"d":{"name":"zero"}},"settings":{},"manifest":{}}"#,
        )
        .unwrap();
        let mut c = Client::from_snapshot(wire, snapshot).unwrap();
        let result = hostnet::block_on(serve(
            &mut c,
            &mut Reader,
            core::array::from_fn(|_| Reader),
            Keys,
        ));
        assert!(matches!(result, Err(Error::Transport("finished"))));
        assert!(c.journal.is_none());
        let out = c.into_transport().answers;
        let names: Vec<_> = out
            .iter()
            .map(|(id, v)| (*id, v.as_str().unwrap_or("").to_string()))
            .collect();
        assert_eq!(
            names,
            [
                (100, "one".to_string()),
                (101, "two".to_string()),
                (102, "three".to_string())
            ]
        );
    }
}
