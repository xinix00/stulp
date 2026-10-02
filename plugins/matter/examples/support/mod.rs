//! Uitsluitend testtransport: echte UDP-werker, synthetische controller-ACKs en verse testsleutels.
#[path = "../../../../crates/plugin-host/src/udp.rs"]
mod udp;
use std::{
    collections::VecDeque,
    time::{Duration, Instant},
};
use stulp_core::json::Value;
use stulp_protocol::{Frame, Kind};
use stulp_sdk::{Error, Event, Result, Transport, UdpCommand, UdpEvent};
pub(crate) struct Adapter {
    udp: udp::Worker,
    clock: Instant,
    inbox: VecDeque<Frame>,
    pub(crate) pings: usize,
    pub(crate) saved: Value,
}
impl Transport for Adapter {
    fn udp(&mut self, c: UdpCommand) -> Result {
        self.udp.send(c)
    }
    fn poll_udp(&mut self) -> Option<UdpEvent> {
        self.udp.poll()
    }
    async fn send(&mut self, v: &Value) -> Result {
        let f = Frame::decode(
            stulp_core::json::to_string(v)
                .map_err(stulp_core::Error::from)?
                .as_bytes(),
        )?;
        if f.kind != Kind::Request {
            return Err(Error::Invalid("unexpected controller frame"));
        }
        match f.method() {
            "$appproto.ping" => self.pings += 1,
            "state.set" => {
                self.saved = stulp_sdk::clone(stulp_sdk::util::field(
                    stulp_sdk::util::field(&f.value, "p"),
                    "state",
                ))?
            }
            _ => return Err(Error::Invalid("unexpected controller request")),
        }
        self.inbox.push_back(Frame::decode(
            stulp_core::json::to_string(&Frame::response(f.id, Ok(Value::Null))?)
                .map_err(stulp_core::Error::from)?
                .as_bytes(),
        )?);
        Ok(())
    }
    async fn next(&mut self) -> Result<Event> {
        if let Some(f) = self.inbox.pop_front() {
            return Ok(Event::Frame(f));
        }
        std::thread::sleep(Duration::from_millis(2));
        Ok(Event::Tick)
    }
    fn now(&self) -> u64 {
        self.clock.elapsed().as_millis() as u64
    }
    fn wall_time(&self) -> Result<u64> {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .map_err(|_| Error::Transport("test clock"))
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        hostnet::entropy()
            .map_err(|_| Error::Transport("test entropy"))?
            .get(..32)
            .ok_or(Error::Invalid("entropy width"))?
            .try_into()
            .map_err(|_| Error::Invalid("entropy width"))
    }
}
impl Adapter {
    pub(crate) fn new() -> Result<Self> {
        Ok(Self {
            udp: udp::Worker::new()?,
            clock: Instant::now(),
            inbox: VecDeque::new(),
            pings: 0,
            saved: Value::Null,
        })
    }
}
