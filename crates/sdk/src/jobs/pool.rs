//! Two command owners plus the main lifecycle owner, scoped to one controller attach.
use super::*;
use crate::Plugin;
const OWNERS: usize = 3;
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
    if !(1..=6).contains(id) {
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
/// Serve lifecycle/UI and two independent physical nodes on the same executor.
/// No command is canceled or replayed to make room for another node.
pub async fn serve<T: Transport, P: Plugin, W: Plugin, K: Key>(
    c: &mut Client<T>,
    main: &mut P,
    mut first: W,
    mut second: W,
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
    let [mut controller, mut a, mut b]: [Client<Port<'_>>; OWNERS] = clients
        .try_into()
        .map_err(|_| Error::Invalid("worker count mismatch"))?;
    let pump = async {
        let mut leases: [Option<Lease>; OWNERS] = [None, None, None];
        let mut sticky = [0; OWNERS];
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
            if c.state.revision != revision {
                for tx in &mut writers {
                    snapshot(c, tx).await?;
                }
                revision = c.state.revision;
            }
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
                    let worker = (1..OWNERS)
                        .find(|i| leases[*i].is_none() && sticky[*i] == node)
                        .or_else(|| (1..OWNERS).find(|i| leases[*i].is_none()));
                    let Some(worker) = worker else {
                        at += 1;
                        continue;
                    };
                    let frame = pending.remove(at);
                    leases[worker] = Some(Lease {
                        request: frame.id,
                        key: node,
                    });
                    sticky[worker] = node;
                    snapshot(c, &mut writers[worker]).await?;
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
                    snapshot(c, &mut writers[0]).await?;
                    writers[0].send(In::Frame(frame)).await;
                }
            }
            hop_sync::yield_now().await;
        }
        #[allow(unreachable_code)]
        Ok::<(), Error>(())
    };
    let workers = async {
        match select(
            controller.serve_serial(main),
            select(a.serve_serial(&mut first), b.serve_serial(&mut second)),
        )
        .await
        {
            Either::Left(r) | Either::Right(Either::Left(r)) | Either::Right(Either::Right(r)) => r,
        }
    };
    match select(pump, workers).await {
        Either::Left(r) | Either::Right(r) => r,
    }
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
        let result = hostnet::block_on(serve(&mut c, &mut Worker, Worker, Worker, Keys));
        assert!(matches!(result, Err(Error::Transport("finished"))));
        let out = c.into_transport().answers;
        assert_eq!(out.iter().map(|v| v.0).collect::<Vec<_>>(), [101, 100, 102]);
        assert!(out[0].1 < 500);
        assert!(out[1].1 >= 1500);
        assert!(out[2].1 >= out[1].1);
    }
}
