//! Eight fixed stream workers; backpressure and generation-scoped command routing.
use alloc::{collections::VecDeque, vec::Vec};
use applib::{App, EXEC};
use core::{cell::Cell, time::Duration};
use hop_sync::{Either, Local, mpsc::Mailbox, select};
use leanhttp::{AsyncRead, AsyncWrite, Dial as _};
use stulp_sdk::{Error, Result, StreamCommand as Command, StreamEvent as Event};
pub(super) struct Queues {
    generation: Local<Cell<u64>>,
    started: Local<Cell<u8>>,
    input: [Local<Mailbox<(u64, Command), 32>>; 8],
    output: Local<Mailbox<(u64, Event), 64>>,
}
impl Queues {
    const fn new() -> Self {
        Self {
            generation: Local::new(Cell::new(0)),
            started: Local::new(Cell::new(0)),
            input: [const { Local::new(Mailbox::new()) }; 8],
            output: Local::new(Mailbox::new()),
        }
    }
}
static QUEUES: [Queues; super::BUNDLE_CAP] = [const { Queues::new() }; super::BUNDLE_CAP];
pub(super) struct Streams {
    queues: &'static Queues,
    generation: u64,
    ids: [u64; 8],
    highest: u64,
}
impl Streams {
    pub(super) fn new(index: usize) -> Result<Self> {
        let queues = QUEUES
            .get(index)
            .ok_or(Error::Invalid("plugin bundle index out of range"))?;
        let generation = queues.generation.get().get().wrapping_add(1);
        queues.generation.get().set(generation);
        Ok(Self {
            queues,
            generation,
            ids: [0; 8],
            highest: 0,
        })
    }
    pub(super) fn start(
        &self,
        app: &'static App,
        env: &mut crate::environment::Environment,
    ) -> Result {
        match self.queues.started.get().get() {
            2 => return Ok(()),
            0 => (),
            _ => return Err(Error::Transport("stream worker startup failed")),
        }
        // Een gedeeltelijke start mag nooit een tweede consument op dezelfde rij starten.
        self.queues.started.get().set(1);
        start(self.queues, app, env)?;
        self.queues.started.get().set(2);
        Ok(())
    }
    pub(super) fn command(&mut self, c: Command) -> Result {
        let id = match &c {
            Command::Open { id, .. } | Command::Write { id, .. } | Command::Close { id } => *id,
        };
        let index = match &c {
            Command::Open { host, port, .. } => {
                if id == 0
                    || id <= self.highest
                    || host.is_empty()
                    || host.len() > 253
                    || *port == 0
                    || host
                        .bytes()
                        .any(|b| b <= 32 || b >= 127 || b"/@\\?#".contains(&b))
                {
                    return Err(Error::Invalid("invalid stream target or reused id"));
                }
                self.ids
                    .iter()
                    .position(|i| *i == 0)
                    .ok_or(Error::Transport("stream capacity reached"))?
            }
            Command::Write { bytes, .. } if bytes.is_empty() || bytes.len() > 65536 => {
                return Err(Error::Invalid("stream write exceeds bounds"));
            }
            _ => self
                .ids
                .iter()
                .position(|i| *i == id)
                .ok_or(Error::Invalid("stream missing"))?,
        };
        let open = matches!(&c, Command::Open { .. });
        self.queues.input[index]
            .try_send((self.generation, c))
            .map_err(|_| Error::Transport("stream command queue full"))?;
        if open {
            self.ids[index] = id;
            self.highest = id;
        }
        Ok(())
    }
    pub(super) fn poll(&mut self) -> Option<Event> {
        while let Some((g, e)) = self.queues.output.try_recv() {
            if g == self.generation {
                if let Event::Closed(id, _) = &e {
                    for slot in &mut self.ids {
                        if *slot == *id {
                            *slot = 0;
                        }
                    }
                }
                return Some(e);
            }
        }
        None
    }
}
impl Drop for Streams {
    fn drop(&mut self) {
        if self.queues.generation.get().get() == self.generation {
            self.queues
                .generation
                .get()
                .set(self.generation.wrapping_add(1));
        }
    }
}
async fn emit(owner: &Queues, g: u64, e: Event) -> bool {
    let mut value = (g, e);
    loop {
        if owner.generation.get().get() != g {
            return false;
        }
        match owner.output.try_send(value) {
            Ok(()) => return true,
            Err(full) => value = full.0,
        }
        EXEC.get().after(Duration::from_millis(2)).await;
    }
}
fn start(
    owner: &'static Queues,
    app: &'static App,
    env: &mut crate::environment::Environment,
) -> Result {
    for input in &owner.input {
        let mut dial = crate::network::Dial::new(app, &env.random());
        EXEC.get()
            .spawn(async move {
                loop {
                    let (g, c) = input.recv().await;
                    if g != owner.generation.get().get() {
                        continue;
                    }
                    let Command::Open {
                        id,
                        host,
                        port,
                        tls,
                        device_certificate,
                    } = c
                    else {
                        continue;
                    };
                    dial.device(device_certificate);
                    let opened = select(
                        dial.dial(leanhttp::Target {
                            https: tls,
                            host: &host,
                            port,
                        }),
                        EXEC.get().after(Duration::from_secs(15)),
                    )
                    .await;
                    let mut conn = match opened {
                        Either::Left(Ok(c)) => c,
                        _ => {
                            emit(
                                owner,
                                g,
                                Event::Closed(id, Error::Transport("stream connect failed")),
                            )
                            .await;
                            continue;
                        }
                    };
                    let _ = conn.set_read_timeout(None);
                    let _ = conn.set_write_timeout(None);
                    if !emit(owner, g, Event::Opened(id)).await {
                        continue;
                    }
                    let result = active(owner, g, id, &mut conn, input).await;
                    emit(
                        owner,
                        g,
                        Event::Closed(
                            id,
                            result.err().unwrap_or(Error::Transport("stream closed")),
                        ),
                    )
                    .await;
                }
            })
            .map_err(|_| Error::Transport("stream worker unavailable"))?;
    }
    Ok(())
}
async fn active(
    owner: &Queues,
    g: u64,
    id: u64,
    conn: &mut crate::network::Connection,
    input: &Mailbox<(u64, Command), 32>,
) -> Result {
    let mut writes = VecDeque::<Vec<u8>>::new();
    let mut offset = 0;
    let mut queued = 0;
    let mut last = applib::clock::now_ns();
    loop {
        if owner.generation.get().get() != g {
            return Ok(());
        }
        for _ in 0..4 {
            let Some((generation, c)) = input.try_recv() else {
                break;
            };
            if generation != g {
                continue;
            }
            match c {
                Command::Write { id: target, bytes } if target == id => {
                    if queued + bytes.len() > 1 << 20 {
                        return Err(Error::Transport("stream output capacity reached"));
                    }
                    writes
                        .try_reserve(1)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    if writes.is_empty() {
                        last = applib::clock::now_ns();
                    }
                    queued += bytes.len();
                    writes.push_back(bytes);
                }
                Command::Close { id: target } if target == id => return Ok(()),
                _ => return Err(Error::Invalid("stream ownership changed")),
            }
        }
        if let Some(bytes) = writes.front() {
            match core::future::poll_fn(|cx| {
                core::task::Poll::Ready(conn.poll_write(cx, &bytes[offset..]))
            })
            .await
            {
                core::task::Poll::Ready(Ok(0)) => {
                    return Err(Error::Transport("stream write closed"));
                }
                core::task::Poll::Ready(Ok(n)) => {
                    offset += n;
                    last = applib::clock::now_ns();
                    if offset == bytes.len() {
                        queued -= bytes.len();
                        offset = 0;
                        writes.pop_front();
                    }
                }
                core::task::Poll::Ready(Err(_)) => {
                    return Err(Error::Transport("stream write failed"));
                }
                core::task::Poll::Pending => (),
            };
            if applib::clock::now_ns().saturating_sub(last) > 10_000_000_000 {
                return Err(Error::Timeout);
            }
        }
        if owner.output.len() < 63 {
            let mut buf = [0; 16384];
            match core::future::poll_fn(|cx| core::task::Poll::Ready(conn.poll_read(cx, &mut buf)))
                .await
            {
                core::task::Poll::Ready(Ok(0)) => return Ok(()),
                core::task::Poll::Ready(Ok(n)) => {
                    let mut data = Vec::new();
                    data.try_reserve_exact(n)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    data.extend_from_slice(&buf[..n]);
                    if !emit(owner, g, Event::Data(id, data)).await {
                        return Ok(());
                    }
                }
                core::task::Poll::Ready(Err(_)) => {
                    return Err(Error::Transport("stream read failed"));
                }
                core::task::Poll::Pending => (),
            }
        }
        EXEC.get().after(Duration::from_millis(2)).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn reconnect_of_one_bundle_plugin_preserves_other_streams() -> Result {
        let a = Streams::new(8)?;
        let mut b = Streams::new(9)?;
        b.ids[0] = 42;
        b.queues
            .output
            .try_send((b.generation, Event::Opened(42)))
            .map_err(|_| Error::Transport("test queue full"))?;
        drop(a);
        let mut next = Streams::new(8)?;
        assert!(next.poll().is_none());
        assert!(matches!(b.poll(), Some(Event::Opened(42))));
        assert_eq!(b.ids[0], 42);
        assert_eq!(b.queues.generation.get().get(), b.generation);
        Ok(())
    }
}
