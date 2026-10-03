#![allow(clippy::unwrap_used)]
use super::*;

struct Wire {
    now: u64,
    frames: VecDeque<Frame>,
    send_delay: u64,
}
impl Transport for Wire {
    fn now(&self) -> u64 {
        self.now
    }
    fn wall_time(&self) -> Result<u64> {
        Ok(0)
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        Ok([0; 32])
    }
    async fn send(&mut self, _: &Value) -> Result {
        self.now += self.send_delay;
        Ok(())
    }
    async fn next(&mut self) -> Result<Event> {
        Ok(self
            .frames
            .pop_front()
            .map(Event::Frame)
            .unwrap_or(Event::Tick))
    }
}
fn client() -> Client<Wire> {
    Client::new(Wire {
        now: 10_001,
        frames: VecDeque::new(),
        send_delay: 0,
    })
}
fn frame(value: Value) -> Frame {
    Frame::decode(&stulp_protocol::encode(&value).unwrap()[4..]).unwrap()
}

#[test]
fn queued_heartbeat_reply_survives_a_delayed_executor_turn() {
    let mut c = client();
    c.heartbeat = Some((7, 10_000));
    // A controller callback may already precede the heartbeat in the TCP stream.
    c.transport
        .frames
        .push_back(frame(Frame::request(9, "test", &Value::Null).unwrap()));
    c.transport
        .frames
        .push_back(frame(Frame::response(7, Ok(Value::Null)).unwrap()));
    assert_eq!(hostnet::block_on(c.pump()).unwrap().unwrap().id, 9);
    assert!(hostnet::block_on(c.pump()).unwrap().is_none());
    assert!(c.heartbeat.is_none());
}

#[test]
fn a_missed_heartbeat_is_slow_not_dead() {
    let mut c = client();
    c.heartbeat = Some((7, 10_000));
    assert!(hostnet::block_on(c.pump()).unwrap().is_none());
    assert!(c.heartbeat.is_none(), "a new ping goes out next round");
    assert!(c.slow);
}

#[test]
fn silent_controller_still_times_out() {
    let mut c = client();
    c.heartbeat = Some((7, 10_000));
    c.last_heard = 0;
    c.transport.now = super::SILENCE_LIMIT_MS + 1;
    assert!(matches!(hostnet::block_on(c.pump()), Err(Error::Timeout)));
}

#[test]
fn heartbeat_reply_budget_starts_after_the_write_completes() {
    let mut c = client();
    c.transport.send_delay = 6000;
    assert!(hostnet::block_on(c.pump()).unwrap().is_none());
    assert_eq!(
        c.heartbeat.unwrap().1,
        c.now() + super::HEARTBEAT_DEADLINE_MS
    );
}
