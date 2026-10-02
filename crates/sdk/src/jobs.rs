//! Een lange protocoltaak en korte UI-callbacks delen één executor via vaste rijen.
//! De taak bezit zijn protocolstaat; alleen de pomp bezit het echte transport.
use crate::{
    Client, Datagram, DatagramRequest, Error, Event, Result, Transport, UdpCommand, UdpEvent,
    clone, message,
};
use alloc::{collections::VecDeque, vec::Vec};
use core::ops::AsyncFnOnce;
use hop_sync::{
    Either, select,
    spsc::{Channel, Receiver, Sender},
};
use rand_chacha::ChaCha20Rng;
use rand_core::{RngCore, SeedableRng};
use stulp_core::json::{self, Value};
use stulp_protocol::{Frame, Kind};
const CAP: usize = 64;
/// Beslissing van de korte callbackbaan; uitgestelde callbacks krijgen nog geen succesantwoord.
pub enum Action {
    /// Nu beantwoorden, ook wanneer dit een gebruikersfout is.
    Reply(Result<Value>),
    /// In wirevolgorde uitvoeren nadat de lopende protocoltaak klaar is.
    Defer,
    /// Bewaar de callback zonder antwoord en laat uitsluitend de huidige achtergrondtaak opruimen.
    Preempt,
    /// Beantwoord het annuleringsverzoek en laat de protocoltaak opruimen.
    Cancel(Value),
}
/// Alleen korte, lokale callbacks. Netwerkopdrachten blijven van de protocoltaak.
pub trait Gate {
    /// De snapshot komt van de echte controller; het gate mag geen netwerk-I/O uitvoeren.
    fn handle(
        &mut self,
        snapshot: &Value,
        wall: u64,
        method: &str,
        params: &Value,
    ) -> Result<Action>;
}
enum Out {
    Frame(Frame),
    Udp(UdpCommand),
    Browse(DatagramRequest),
    Resolve(alloc::string::String),
    Log(alloc::string::String, alloc::string::String),
}
enum In {
    Frame(Frame),
    Udp(UdpEvent),
    Browse(Result<Vec<Datagram>>),
    Resolved(Result<Vec<alloc::string::String>>),
    Tick {
        now: u64,
        wall: u64,
        entropy: [u8; 32],
    },
    Cancel,
}
/// Lokaal transport van precies één taak. Geen sockets, threads of gedeelde appstaat.
pub struct Port<'a> {
    tx: Sender<'a, Out, CAP>,
    rx: Receiver<'a, In, CAP>,
    frames: VecDeque<Frame>,
    udp: VecDeque<UdpEvent>,
    browse: Option<Result<Vec<Datagram>>>,
    random: ChaCha20Rng,
    now: u64,
    wall: u64,
    cancelled: bool,
    browsing: bool,
    resolving: bool,
    resolved: Option<Result<Vec<alloc::string::String>>>,
}
impl Drop for Port<'_> {
    fn drop(&mut self) {
        self.random = ChaCha20Rng::from_seed([0; 32]);
    }
}
impl Port<'_> {
    fn check_cancelled(&mut self) -> Result {
        while let Some(input) = self.rx.try_recv() {
            self.accept(input)?;
        }
        if self.cancelled {
            Err(Error::Cancelled)
        } else {
            Ok(())
        }
    }
    fn accept(&mut self, input: In) -> Result {
        match input {
            In::Frame(f) => {
                reserve(&mut self.frames)?;
                self.frames.push_back(f);
            }
            In::Udp(e) => {
                reserve(&mut self.udp)?;
                self.udp.push_back(e);
            }
            In::Browse(r) => {
                self.browsing = false;
                self.browse = Some(r);
            }
            In::Resolved(r) => {
                self.resolving = false;
                self.resolved = Some(r);
            }
            In::Tick {
                now,
                wall,
                mut entropy,
            } => {
                self.now = now;
                self.wall = wall;
                // Each task has a separately seeded stream; fresh platform entropy
                // rekeys it, without a consumable budget that breaks nested workers.
                let mut seed = [0; 32];
                self.random.fill_bytes(&mut seed);
                for (byte, fresh) in seed.iter_mut().zip(&entropy) {
                    *byte ^= *fresh;
                }
                self.random = ChaCha20Rng::from_seed(seed);
                seed.fill(0);
                entropy.fill(0);
            }
            In::Cancel => self.cancelled = true,
        }
        Ok(())
    }
}
fn reserve<T>(q: &mut VecDeque<T>) -> Result {
    if q.len() >= CAP {
        return Err(Error::Invalid("local protocol inbox full"));
    }
    q.try_reserve(1).map_err(|_| stulp_core::Error::Memory)?;
    Ok(())
}
impl Transport for Port<'_> {
    fn start_resolve(&mut self, address: alloc::string::String) -> Result {
        self.check_cancelled()?;
        self.tx
            .try_send(Out::Resolve(address))
            .map_err(|_| Error::Transport("protocol DNS output full"))?;
        self.resolving = true;
        Ok(())
    }
    fn poll_resolve(&mut self) -> Option<Result<Vec<alloc::string::String>>> {
        self.resolved.take()
    }
    fn log(&mut self, level: &str, message: &str) -> Result {
        self.tx
            .try_send(Out::Log(json::copy(level)?, json::copy(message)?))
            .map_err(|_| Error::Transport("protocol job output full"))
    }

    async fn send(&mut self, value: &Value) -> Result {
        // A queued cancellation wins before another controller mutation is emitted.
        // UDP close/ACK cleanup remains available through its separate synchronous path.
        self.check_cancelled()?;
        let frame = decode(clone(value)?)?;
        if frame.method() == "$appproto.ping" {
            reserve(&mut self.frames)?;
            self.frames
                .push_back(decode(Frame::response(frame.id, Ok(Value::Null))?)?);
        } else {
            self.tx.send(Out::Frame(frame)).await;
        }
        Ok(())
    }
    async fn next(&mut self) -> Result<Event> {
        if self.cancelled {
            return Err(Error::Cancelled);
        }
        if let Some(frame) = self.frames.pop_front() {
            return Ok(Event::Frame(frame));
        }
        let input = self.rx.recv().await;
        self.accept(input)?;
        if self.cancelled {
            return Err(Error::Cancelled);
        }
        Ok(self
            .frames
            .pop_front()
            .map(Event::Frame)
            .unwrap_or(Event::Tick))
    }
    fn now(&self) -> u64 {
        self.now
    }
    fn wall_time(&self) -> Result<u64> {
        Ok(self.wall)
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        self.check_cancelled()?;
        let mut bytes = [0; 32];
        self.random.fill_bytes(&mut bytes);
        Ok(bytes)
    }
    fn udp(&mut self, command: UdpCommand) -> Result {
        self.tx
            .try_send(Out::Udp(command))
            .map_err(|_| Error::Transport("local UDP outbox full"))
    }
    fn poll_udp(&mut self) -> Option<UdpEvent> {
        self.udp.pop_front()
    }
    fn start_datagrams(&mut self, request: DatagramRequest) -> Result {
        self.check_cancelled()?;
        self.tx
            .try_send(Out::Browse(request))
            .map_err(|_| Error::Transport("local discovery outbox full"))?;
        self.browsing = true;
        Ok(())
    }
    fn poll_datagrams(&mut self) -> Option<Result<Vec<Datagram>>> {
        self.browse.take()
    }
}
fn decode(value: Value) -> Result<Frame> {
    Ok(Frame::decode(
        json::to_string(&value)
            .map_err(stulp_core::Error::from)?
            .as_bytes(),
    )?)
}
async fn relay<T: Transport>(c: &mut Client<T>, tx: &mut Sender<'_, In, CAP>, out: Out) -> Result {
    match out {
        Out::Log(level, message) => c.log(&level, &message)?,
        Out::Udp(command) => c.udp(command)?,
        Out::Browse(request) => {
            if let Err(e) = c.start_datagrams(request) {
                tx.send(In::Browse(Err(e))).await;
            }
        }
        Out::Resolve(address) => {
            if let Err(e) = c.start_resolve(address) {
                tx.send(In::Resolved(Err(e))).await;
            }
        }
        Out::Frame(f) => {
            if f.kind != Kind::Request {
                return Err(Error::Invalid("local task must send requests"));
            }
            let result = if f.method() == "state.set" {
                c.app_state(clone(crate::util::field(
                    crate::util::field(&f.value, "p"),
                    "state",
                ))?)
                .await
                .map(|()| Value::Null)
            } else {
                c.call(f.method(), crate::util::field(&f.value, "p")).await
            };
            let response = match result {
                Ok(v) => Frame::response(f.id, Ok(v))?,
                Err(e) => Frame::response(f.id, Err(&message(&e)?))?,
            };
            tx.send(In::Frame(decode(Frame::request(
                0,
                "state.snapshot",
                c.state().root(),
            )?)?))
            .await;
            tx.send(In::Frame(decode(response)?)).await;
        }
    }
    Ok(())
}
/// Voert een begrensde taak uit terwijl voortgang, annulering, assets en heartbeats blijven werken.
/// De closure krijgt een eigen snapshot en bezit alle geleende protocolstaat tot hij terugkeert.
pub async fn run<T: Transport, G: Gate, R>(
    c: &mut Client<T>,
    gate: &mut G,
    work: impl for<'a> AsyncFnOnce(&mut Client<Port<'a>>) -> Result<R>,
) -> Result<R> {
    let outgoing = Channel::<Out, CAP>::new();
    let incoming = Channel::<In, CAP>::new();
    let (tx, mut from_work) = outgoing
        .split()
        .ok_or(Error::Invalid("local channel already split"))?;
    let (mut to_work, rx) = incoming
        .split()
        .ok_or(Error::Invalid("local channel already split"))?;
    let random = ChaCha20Rng::from_seed(c.random()?);
    let port = Port {
        tx,
        rx,
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
    let mut client = Client::from_snapshot(port, clone(c.state().root())?)?;
    let mut deferred = Vec::new();
    let (result, interrupted) = {
        let pump = async {
            loop {
                for _ in 0..16 {
                    let Some(out) = from_work.try_recv() else {
                        break;
                    };
                    relay(c, &mut to_work, out).await?;
                }
                for _ in 0..16 {
                    let Some(event) = c.poll_udp() else { break };
                    to_work.send(In::Udp(event)).await;
                }
                if let Some(result) = c.poll_datagrams() {
                    to_work.send(In::Browse(result)).await;
                }
                if let Some(result) = c.poll_resolve() {
                    to_work.send(In::Resolved(result)).await;
                }
                if to_work.free() == CAP {
                    to_work
                        .send(In::Tick {
                            now: c.now(),
                            wall: c.wall_time()?,
                            entropy: c.random()?,
                        })
                        .await;
                }
                let frame = if c.inbox.is_empty() {
                    c.pump().await?
                } else {
                    Some(c.inbox.remove(0))
                };
                if let Some(f) = frame.filter(|f| f.kind == Kind::Request) {
                    let params = crate::util::field(&f.value, "p");
                    let action = gate
                        .handle(c.state().root(), c.wall_time()?, f.method(), params)
                        .unwrap_or_else(|e| Action::Reply(Err(e)));
                    let (result, cancel) = match action {
                        Action::Defer | Action::Preempt => {
                            let preempt = matches!(action, Action::Preempt);
                            json::push(&mut deferred, f, super::MAX_INBOX)?;
                            if preempt {
                                to_work.send(In::Cancel).await;
                            }
                            continue;
                        }
                        Action::Reply(r) => (r, false),
                        Action::Cancel(v) => (Ok(v), true),
                    };
                    let response = match result {
                        Ok(v) => Frame::response(f.id, Ok(v))?,
                        Err(e) => Frame::response(f.id, Err(&message(&e)?))?,
                    };
                    c.transport.send(&response).await?;
                    if cancel {
                        to_work.send(In::Cancel).await;
                    }
                }
                // Ook adapters met direct gereedstaande ticks moeten de protocoltaak laten lopen.
                hop_sync::yield_now().await;
            }
            #[allow(unreachable_code)]
            Ok::<(), Error>(())
        };
        match select(work(&mut client), pump).await {
            Either::Left(result) => (result, false),
            Either::Right(result) => (
                Err(result
                    .err()
                    .unwrap_or(Error::Transport("protocol pump stopped"))),
                true,
            ),
        }
    };
    c.interrupted_job |= interrupted;
    let cleaned = (|| -> Result {
        let mut port = client.into_transport();
        while let Some(input) = port.rx.try_recv() {
            port.accept(input)?;
        }
        while let Some(event) = port.udp.pop_front() {
            if c.udp_inbox.len() >= CAP {
                return Err(Error::Invalid("returned UDP inbox full"));
            }
            c.udp_inbox
                .try_reserve(1)
                .map_err(|_| stulp_core::Error::Memory)?;
            c.udp_inbox.push_back(event);
        }
        c.discard_datagrams |= port.browsing;
        c.discard_resolve |= port.resolving;
        // Laatste ACKs/close-opdrachten mogen niet met de voltooide future verdwijnen.
        while let Some(out) = from_work.try_recv() {
            match out {
                Out::Log(level, message) => c.log(&level, &message)?,
                Out::Udp(command) => c.udp(command)?,
                _ => return Err(Error::Invalid("protocol job finished with pending request")),
            }
        }
        for frame in c.inbox.drain(..) {
            json::push(&mut deferred, frame, super::MAX_INBOX)?;
        }
        c.inbox = deferred;
        Ok(())
    })();
    if cleaned.is_err() {
        c.interrupted_job = true;
    }
    cleaned?;
    result
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic)]
mod tests {
    extern crate std;
    use super::*;
    use std::vec;
    struct Wire {
        now: u64,
        incoming: VecDeque<Frame>,
        callbacks: VecDeque<(u64, Frame)>,
        replies: Vec<u64>,
        pings: usize,
        closes: usize,
        saved: Value,
        fail_at: Option<u64>,
        dns_ready: Option<u64>,
        dns_delay: u64,
    }
    impl Transport for Wire {
        fn start_resolve(&mut self, _: alloc::string::String) -> Result {
            assert!(self.dns_ready.is_none());
            self.dns_ready = Some(self.now + self.dns_delay);
            Ok(())
        }
        fn poll_resolve(&mut self) -> Option<Result<Vec<alloc::string::String>>> {
            if self.dns_ready.is_some_and(|at| self.now >= at) {
                self.dns_ready = None;
                Some(Ok(vec!["127.0.0.1:5540".into()]))
            } else {
                None
            }
        }

        async fn send(&mut self, value: &Value) -> Result {
            let f = decode(clone(value)?)?;
            if f.kind == Kind::Response {
                self.replies.push(f.id);
                return Ok(());
            }
            if f.method() == "$appproto.ping" {
                self.pings += 1;
            } else if f.method() == "state.set" {
                self.saved = clone(crate::util::field(
                    crate::util::field(&f.value, "p"),
                    "state",
                ))?;
            } else {
                return Err(Error::Invalid("unexpected test request"));
            }
            self.incoming
                .push_back(decode(Frame::response(f.id, Ok(Value::Null))?)?);
            Ok(())
        }
        async fn next(&mut self) -> Result<Event> {
            self.now += 100;
            if self.fail_at.is_some_and(|at| self.now >= at) {
                self.fail_at = None;
                return Err(Error::Transport("synthetic pump failure"));
            }
            if let Some(f) = self.incoming.pop_front() {
                return Ok(Event::Frame(f));
            }
            if self
                .callbacks
                .front()
                .is_some_and(|(at, _)| *at <= self.now)
            {
                return Ok(Event::Frame(self.callbacks.pop_front().unwrap().1));
            }
            Ok(Event::Tick)
        }
        fn now(&self) -> u64 {
            self.now
        }
        fn wall_time(&self) -> Result<u64> {
            Ok(1_790_000_000 + self.now / 1000)
        }
        fn random(&mut self) -> Result<[u8; 32]> {
            Ok([7; 32])
        }
        fn udp(&mut self, c: UdpCommand) -> Result {
            if matches!(c, UdpCommand::Close { .. }) {
                self.closes += 1;
            }
            Ok(())
        }
    }
    struct Front {
        statuses: usize,
    }
    impl Gate for Front {
        fn handle(&mut self, _: &Value, _: u64, method: &str, _: &Value) -> Result<Action> {
            Ok(match method {
                "status" => {
                    self.statuses += 1;
                    Action::Reply(Ok(Value::Bool(true)))
                }
                "cancel" => Action::Cancel(Value::Null),
                "urgent" => Action::Preempt,
                _ => Action::Defer,
            })
        }
    }
    fn client(callbacks: &[(u64, u64, &str)]) -> Client<Wire> {
        let mut input = VecDeque::new();
        for (at, id, method) in callbacks {
            input.push_back((
                *at,
                decode(Frame::request(*id, method, &Value::Null).unwrap()).unwrap(),
            ));
        }
        Client::from_snapshot(Wire { now: 0, incoming: VecDeque::new(), callbacks: input,
            replies: vec![], pings: 0, closes: 0, saved: Value::Null, fail_at: None, dns_ready: None, dns_delay: 7000 },
            json::parse(br#"{"protocol":1,"appId":"test","manifest":{},"devices":{},"settings":{},"appState":{}}"#).unwrap()).unwrap()
    }
    #[test]
    fn progress_heartbeat_snapshot_and_deferred_order_survive_long_job() {
        let mut c = client(&[
            (200, 9001, "command"),
            (300, 9002, "status"),
            (400, 9003, "command"),
            (7000, 9004, "status"),
        ]);
        let mut gate = Front { statuses: 0 };
        let result = hostnet::block_on(run(&mut c, &mut gate, async |worker| {
            while worker.now() < 8500 {
                worker.idle().await?;
            }
            worker
                .app_state(json::fields(&[("persisted", Value::Bool(true))])?)
                .await?;
            assert!(json::boolean(
                crate::util::field(worker.state().root(), "appState"),
                "persisted"
            ));
            worker.udp(UdpCommand::Close { id: 7 })?;
            Ok(42)
        }))
        .unwrap();
        assert_eq!(result, 42);
        assert_eq!(gate.statuses, 2);
        assert_eq!(
            c.inbox.iter().map(|f| f.id).collect::<Vec<_>>(),
            [9001, 9003]
        );
        assert!(json::boolean(
            crate::util::field(c.state().root(), "appState"),
            "persisted"
        ));
        let wire = c.into_transport();
        assert_eq!(wire.replies, [9002, 9004]);
        assert!(wire.pings >= 1);
        assert_eq!(wire.closes, 1);
        assert!(json::boolean(&wire.saved, "persisted"));
    }
    #[test]
    fn cancellation_unwinds_the_job_and_flushes_cleanup() {
        let mut c = client(&[(200, 9001, "status"), (400, 9002, "cancel")]);
        let mut gate = Front { statuses: 0 };
        let result = hostnet::block_on(run(&mut c, &mut gate, async |worker| {
            let result = async {
                while worker.now() < 10000 {
                    worker.idle().await?;
                }
                Ok(())
            }
            .await;
            worker.udp(UdpCommand::Close { id: 7 })?;
            result
        }));
        assert!(result.is_err());
        assert!(c.now() < 2000);
        let wire = c.into_transport();
        assert_eq!(wire.replies, [9001, 9002]);
        assert_eq!(wire.closes, 1);
    }
    #[test]
    fn preemption_preserves_callback_and_finishes_cleanup_before_foreground_work() {
        let mut c = client(&[(200, 9001, "status"), (400, 9002, "urgent")]);
        let mut gate = Front { statuses: 0 };
        let result = hostnet::block_on(run(&mut c, &mut gate, async |worker| {
            let result = async {
                while worker.now() < 40000 {
                    worker.idle().await?;
                }
                Ok(())
            }
            .await;
            // Cleanup is sent, while a new mutation after cancellation must never escape.
            worker.udp(UdpCommand::Close { id: 7 })?;
            assert!(matches!(
                worker.app_state(json::object()).await,
                Err(Error::Cancelled)
            ));
            result
        }));
        assert!(matches!(result, Err(Error::Cancelled)));
        assert!(c.now() < 2000);
        assert!(
            !c.interrupted_job,
            "cooperative cancellation poisoned the owner"
        );
        assert_eq!(c.inbox.len(), 1);
        assert_eq!(c.inbox.remove(0).id, 9002);
        let value = hostnet::block_on(run(&mut c, &mut gate, async |worker| {
            worker
                .app_state(json::fields(&[("foreground", Value::Bool(true))])?)
                .await?;
            Ok(42)
        }))
        .unwrap();
        assert_eq!(value, 42);
        let wire = c.into_transport();
        assert_eq!(
            wire.replies,
            [9001],
            "urgent callback was answered before it was executed"
        );
        assert_eq!(wire.closes, 1);
        assert!(json::boolean(&wire.saved, "foreground"));
    }

    #[test]
    fn interrupted_protocol_owner_requires_reconnect_even_if_error_is_handled() {
        let mut c = client(&[]);
        c.transport.fail_at = Some(300);
        let mut gate = Front { statuses: 0 };
        let result = hostnet::block_on(run(&mut c, &mut gate, async |worker| {
            while worker.now() < 10000 {
                worker.idle().await?;
            }
            Ok(())
        }));
        assert!(result.is_err());
        assert!(c.interrupted_job);
        assert!(hostnet::block_on(c.idle()).is_err());
    }
    #[test]
    fn resolver_keeps_ui_heartbeat_alive_and_discards_cancelled_answers() {
        let mut c = client(&[(300, 9001, "status"), (2000, 9002, "status")]);
        let mut gate = Front { statuses: 0 };
        let found = hostnet::block_on(run(&mut c, &mut gate, async |work| {
            work.resolve("light.local:5540").await
        }))
        .unwrap();
        assert_eq!(found, ["127.0.0.1:5540"]);
        assert_eq!(gate.statuses, 2);
        assert!(c.transport.pings > 0);
        c.transport.dns_delay = 15_000;
        let timed = hostnet::block_on(run(&mut c, &mut gate, async |work| {
            work.resolve("slow.local:5540").await
        }));
        assert!(matches!(timed, Err(Error::Timeout)));
        assert!(c.discard_resolve);
        assert!(c.start_resolve("next.local:5540".into()).is_err());
        c.transport.now += 6000;
        assert!(c.poll_resolve().is_none());
        assert!(!c.discard_resolve);
        c.transport.dns_delay = 100;
        let next = hostnet::block_on(run(&mut c, &mut gate, async |work| {
            work.resolve("next.local:5540").await
        }))
        .unwrap();
        assert_eq!(next, found);
    }
}

/// Bounded keyed callback workers alongside the main plugin owner.
pub mod pool;
