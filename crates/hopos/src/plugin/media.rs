//! Bounded camera output, preserving the native GOP and viewer ownership rules.
use alloc::{collections::VecDeque, format, string::String, vec::Vec};
use applib::appnet::{self, TcpListener, TcpStream};
use core::time::Duration;
use stulp_core::json;
use stulp_sdk::{Error, MediaCommand as Command, MediaEvent as Event, Result};
const MAX_BYTES: usize = 32 << 20;
const MAX_GOP: usize = 8 << 20;
const MAX_VIEWS: usize = 8;
struct Instant(u64);
impl Instant {
    fn now() -> Self {
        Self(applib::clock::now_ns())
    }
    fn elapsed(&self) -> Duration {
        Duration::from_nanos(applib::clock::now_ns().saturating_sub(self.0))
    }
}
pub(super) struct Worker {
    listener: TcpListener,
    owner: Owner,
    events: VecDeque<Event>,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        Ok(Self {
            listener: TcpListener::bind(0).map_err(|_| Error::Transport("media bind failed"))?,
            owner: Owner {
                sources: Vec::new(),
                views: Vec::new(),
                depot: Depot::new(),
            },
            events: VecDeque::new(),
        })
    }
    pub(super) fn url(&self, token: &str) -> Result<String> {
        if !valid(token) {
            return Err(Error::Invalid("invalid media token"));
        }
        let ip = appnet::net()
            .ok_or(Error::Transport("network unavailable"))?
            .ip();
        let port = self
            .listener
            .port()
            .map_err(|_| Error::Transport("media port unavailable"))?;
        Ok(format!(
            "http://{}.{}.{}.{}:{port}/{token}",
            ip[0], ip[1], ip[2], ip[3]
        ))
    }
    pub(super) fn send(&mut self, c: Command) -> Result {
        match &c {
            Command::Register {
                id,
                token,
                mime,
                header,
            } if *id == 0
                || !valid(token)
                || !(mime.starts_with("video/mp4") || mime.starts_with("image/"))
                || mime.len() > 128
                || mime.bytes().any(|b| b < 32 || b == 127)
                || header.len()
                    > if mime.starts_with("image/") {
                        4 << 20
                    } else {
                        65536
                    }
                || header.is_empty() =>
            {
                return Err(Error::Invalid("invalid media registration"));
            }
            Command::Frame { bytes, .. } if bytes.is_empty() || bytes.len() > MAX_GOP => {
                return Err(Error::Invalid("media frame too large"));
            }
            _ => (),
        }
        self.owner.command(c)
    }
    pub(super) fn poll(&mut self) -> Option<Event> {
        self.events.pop_front()
    }
    pub(super) fn tick(&mut self) -> Result {
        let o = &mut self.owner;
        for _ in 0..2 {
            match crate::poll::once(self.listener.accept()) {
                Some(Ok(socket)) => {
                    if o.views.len() >= MAX_VIEWS {
                        continue;
                    }
                    if json::push(
                        &mut o.views,
                        View {
                            socket,
                            request: Vec::new(),
                            source: None,
                            intro: Vec::new(),
                            offset: 0,
                            frames: VecDeque::new(),
                            keyframe: true,
                            image: false,
                            last: Instant::now(),
                        },
                        MAX_VIEWS,
                    )
                    .is_err()
                    {
                        return Err(Error::Transport("media viewer capacity failed"));
                    }
                }
                None => break,
                Some(Err(_)) => return Err(Error::Transport("media accept failed")),
            }
        }
        let mut i = 0;
        while i < o.views.len() {
            let v = &mut o.views[i];
            let result = v
                .request(&mut o.sources, &mut o.depot)
                .and_then(|()| v.flush(&mut o.depot));
            if result.is_err()
                || v.last.elapsed() > Duration::from_secs(if v.source.is_some() { 30 } else { 5 })
            {
                let mut v = o.views.swap_remove(i);
                if let Some(s) = o.sources.iter_mut().find(|s| Some(s.id) == v.source) {
                    s.idle = Instant::now();
                }
                v.clear(&mut o.depot);
            } else {
                i += 1;
            }
        }
        o.sources
            .retain(|s| !s.mime.starts_with("image/") || !s.header.is_empty());
        let mut i = 0;
        while i < o.sources.len() {
            let s = &o.sources[i];
            if !o.views.iter().any(|v| v.source == Some(s.id))
                && s.idle.elapsed() > Duration::from_secs(30)
            {
                let id = s.id;
                o.close(id);
                if self.events.len() < 16 {
                    self.events
                        .try_reserve(1)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    self.events.push_back(Event::Idle(id));
                }
            } else {
                i += 1;
            }
        }
        Ok(())
    }
}
fn valid(s: &str) -> bool {
    (32..=128).contains(&s.len())
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
}
struct Frame {
    id: u64,
    bytes: Vec<u8>,
    refs: usize,
}
struct Depot {
    frames: Vec<Frame>,
    bytes: usize,
    next: u64,
}
impl Depot {
    fn new() -> Self {
        Self {
            frames: Vec::new(),
            bytes: 0,
            next: 0,
        }
    }
    fn put(&mut self, bytes: Vec<u8>) -> Result<u64> {
        if bytes.len() > MAX_BYTES.saturating_sub(self.bytes) || self.frames.len() >= 2048 {
            return Err(Error::Invalid("media buffer capacity reached"));
        }
        let id = self
            .next
            .checked_add(1)
            .ok_or(Error::Invalid("media buffer ids exhausted"))?;
        self.frames
            .try_reserve(1)
            .map_err(|_| stulp_core::Error::Memory)?;
        self.bytes += bytes.len();
        self.frames.push(Frame { id, bytes, refs: 1 });
        self.next = id;
        Ok(id)
    }
    fn retain(&mut self, id: u64) -> Result {
        let f = self
            .frames
            .iter_mut()
            .find(|f| f.id == id)
            .ok_or(Error::Invalid("media frame missing"))?;
        f.refs = f
            .refs
            .checked_add(1)
            .ok_or(Error::Invalid("media reference overflow"))?;
        Ok(())
    }
    fn release(&mut self, id: u64) {
        if let Some(i) = self.frames.iter().position(|f| f.id == id) {
            self.frames[i].refs -= 1;
            if self.frames[i].refs == 0 {
                let f = self.frames.swap_remove(i);
                self.bytes -= f.bytes.len();
            }
        }
    }
    fn get(&self, id: u64) -> Result<&[u8]> {
        self.frames
            .iter()
            .find(|f| f.id == id)
            .map(|f| f.bytes.as_slice())
            .ok_or(Error::Invalid("media frame missing"))
    }
}
struct Source {
    id: u64,
    token: String,
    mime: String,
    header: Vec<u8>,
    gop: Vec<u64>,
    bytes: usize,
    idle: Instant,
    valid: bool,
}
impl Source {
    fn clear(&mut self, d: &mut Depot) {
        for id in self.gop.drain(..) {
            d.release(id);
        }
        self.bytes = 0;
        self.valid = false;
    }
}
struct View {
    socket: TcpStream,
    request: Vec<u8>,
    source: Option<u64>,
    intro: Vec<u8>,
    offset: usize,
    frames: VecDeque<u64>,
    keyframe: bool,
    image: bool,
    last: Instant,
}
impl View {
    fn clear(&mut self, d: &mut Depot) {
        for id in self.frames.drain(..) {
            d.release(id);
        }
    }
    fn enqueue(&mut self, d: &mut Depot, id: u64) -> Result {
        self.frames
            .try_reserve(1)
            .map_err(|_| stulp_core::Error::Memory)?;
        d.retain(id)?;
        self.frames.push_back(id);
        Ok(())
    }
    fn flush(&mut self, d: &mut Depot) -> Result {
        for _ in 0..4 {
            let intro = !self.intro.is_empty();
            let data = if intro {
                self.intro.as_slice()
            } else if let Some(id) = self.frames.front() {
                d.get(*id)?
            } else {
                break;
            };
            let slice = data
                .get(self.offset..)
                .ok_or(Error::Invalid("media write offset"))?;
            let n = match crate::poll::once(self.socket.write(&slice[..slice.len().min(16384)])) {
                Some(Ok(0)) => return Err(Error::Transport("media viewer disconnected")),
                Some(Ok(n)) => n,
                None => break,
                Some(Err(_)) => return Err(Error::Transport("media write failed")),
            };
            self.offset += n;
            self.last = Instant::now();
            if self.offset == data.len() {
                self.offset = 0;
                if intro {
                    self.intro.clear();
                    if self.image {
                        return Err(Error::Transport("image completed"));
                    }
                } else if let Some(id) = self.frames.pop_front() {
                    d.release(id);
                }
            }
        }
        Ok(())
    }
    fn request(&mut self, sources: &mut [Source], d: &mut Depot) -> Result {
        if self.source.is_some() {
            return Ok(());
        }
        let mut buf = [0; 2048];
        match crate::poll::once(self.socket.read(&mut buf)) {
            Some(Ok(0)) => return Err(Error::Transport("media viewer disconnected")),
            Some(Ok(n)) => {
                if self.request.len() + n > 8192 {
                    return Err(Error::Invalid("media request too large"));
                }
                self.request
                    .try_reserve(n)
                    .map_err(|_| stulp_core::Error::Memory)?;
                self.request.extend_from_slice(&buf[..n]);
            }
            None => return Ok(()),
            Some(Err(_)) => return Err(Error::Transport("media request failed")),
        }
        if !self.request.windows(4).any(|w| w == b"\r\n\r\n") {
            return Ok(());
        }
        let text = core::str::from_utf8(&self.request)
            .map_err(|_| Error::Invalid("invalid media request"))?;
        let mut line = text.lines().next().unwrap_or("").split_whitespace();
        if line.next() != Some("GET") {
            return Err(Error::Invalid("media requires GET"));
        }
        let token = line.next().and_then(|s| s.strip_prefix('/')).unwrap_or("");
        let s = sources
            .iter_mut()
            .find(|s| s.token == token && !s.header.is_empty())
            .ok_or(Error::Invalid("media source not found"))?;
        use core::fmt::Write;
        let mut head = String::new();
        head.try_reserve(s.mime.len() + 128)
            .map_err(|_| stulp_core::Error::Memory)?;
        write!(head,"HTTP/1.1 200 OK\r\nContent-Type: {}\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n",s.mime).map_err(|_|Error::Invalid("media header formatting failed"))?;
        self.image = s.mime.starts_with("image/");
        if self.image {
            self.intro = core::mem::take(&mut s.header);
            let n = self.intro.len();
            self.intro
                .try_reserve(head.len())
                .map_err(|_| stulp_core::Error::Memory)?;
            self.intro.resize(n + head.len(), 0);
            self.intro.copy_within(..n, head.len());
            self.intro[..head.len()].copy_from_slice(head.as_bytes());
        } else {
            self.intro = head.into_bytes();
            self.intro
                .try_reserve(s.header.len())
                .map_err(|_| stulp_core::Error::Memory)?;
            self.intro.extend_from_slice(&s.header);
        }
        self.source = Some(s.id);
        self.request.clear();
        s.idle = Instant::now();
        // Een nieuwe kijker ontvangt de volledige GOP; daarna geldt de kleine livewachtrij.
        if s.valid {
            for id in &s.gop {
                self.enqueue(d, *id)?;
            }
            self.keyframe = false;
        }
        Ok(())
    }
}
struct Owner {
    sources: Vec<Source>,
    views: Vec<View>,
    depot: Depot,
}
impl Owner {
    fn close(&mut self, id: u64) {
        let mut i = 0;
        while i < self.views.len() {
            if self.views[i].source == Some(id) {
                let mut v = self.views.swap_remove(i);
                v.clear(&mut self.depot);
            } else {
                i += 1;
            }
        }
        if let Some(i) = self.sources.iter().position(|s| s.id == id) {
            self.sources.swap_remove(i).clear(&mut self.depot);
        }
    }
    fn command(&mut self, c: Command) -> Result {
        match c {
            Command::Register {
                id,
                token,
                mime,
                header,
            } => {
                if self.sources.iter().any(|s| s.id == id || s.token == token)
                    || self.sources.len() >= 8
                    || self
                        .sources
                        .iter()
                        .filter(|s| s.mime.starts_with("image/") == mime.starts_with("image/"))
                        .count()
                        >= 4
                {
                    return Err(Error::Invalid("media source capacity or duplicate"));
                }
                json::push(
                    &mut self.sources,
                    Source {
                        id,
                        token,
                        mime,
                        header,
                        gop: Vec::new(),
                        bytes: 0,
                        idle: Instant::now(),
                        valid: false,
                    },
                    8,
                )?;
            }
            Command::Close { id } => self.close(id),
            Command::Frame {
                id,
                keyframe,
                bytes,
            } => {
                let Some(s) = self.sources.iter_mut().find(|s| s.id == id) else {
                    return Err(Error::Invalid("media source missing"));
                };
                if keyframe {
                    s.clear(&mut self.depot);
                    s.valid = true;
                }
                if s.bytes + bytes.len() > MAX_GOP || s.gop.len() >= 512 {
                    s.clear(&mut self.depot);
                }
                let size = bytes.len();
                let frame = self.depot.put(bytes)?;
                let result = (|| -> Result {
                    if s.valid {
                        s.gop
                            .try_reserve(1)
                            .map_err(|_| stulp_core::Error::Memory)?;
                        self.depot.retain(frame)?;
                        s.gop.push(frame);
                        s.bytes += size;
                    }
                    for v in self.views.iter_mut().filter(|v| v.source == Some(id)) {
                        if v.frames.len() >= 8 {
                            v.keyframe = true;
                            continue;
                        }
                        if v.keyframe && !keyframe {
                            continue;
                        }
                        v.enqueue(&mut self.depot, frame)?;
                        v.keyframe = false;
                    }
                    Ok(())
                })();
                self.depot.release(frame);
                result?;
            }
        }
        Ok(())
    }
}
