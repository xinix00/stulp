//! De echte plugin praat tegen de echte controllerlogica met een deterministisch transport.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_protocol::Frame;
use stulp_runtime::App;
use stulp_sdk::{Client, Error, Event, Plugin, Result, Transport};
use stulp_virtualdevices::VirtualDevices;
struct Wire {
    store: Store<Memory>,
    app: App,
    inbox: std::collections::VecDeque<Frame>,
    operations: Vec<String>,
    fail_store: bool,
    entropy: u8,
}
impl Wire {
    fn new(stored: &str, fail_store: bool) -> Self {
        let text = format!(
            r#"{{"version":2,"apps":[{{"id":"com.stulp.virtualdevices","enabled":true}}],"devices":[{{"id":"d","appId":"com.stulp.virtualdevices","driverId":"switch","name":"Virtual","data":{{"id":"virtual-test"}},"store":{stored},"capabilities":["onoff"]}}]}}"#
        );
        Self {
            store: Store::open(text.as_bytes(), Memory).unwrap(),
            app: App::new(
                "com.stulp.virtualdevices",
                json::parse(VirtualDevices::default().manifest()).unwrap(),
            )
            .unwrap(),
            inbox: std::collections::VecDeque::new(),
            operations: Vec::new(),
            fail_store,
            entropy: 0,
        }
    }
}
impl Transport for Wire {
    async fn send(&mut self, value: &Value) -> Result {
        let frame = Frame::decode(json::to_string(value).unwrap().as_bytes()).unwrap();
        let params = json::get(&frame.value, "p").unwrap();
        if frame.method() == "device.merge" {
            self.operations.push(json::text(params, "field").into());
            if self.fail_store && json::text(params, "field") == "store" {
                self.inbox.push_back(
                    Frame::decode(
                        json::to_string(&Frame::response(frame.id, Err("disk full")).unwrap())
                            .unwrap()
                            .as_bytes(),
                    )
                    .unwrap(),
                );
                return Ok(());
            }
        }
        if frame.method() == "device.set" {
            self.operations.push(json::text(params, "field").into());
        }
        let result = self
            .app
            .receive(&mut self.store, frame, 0, "2026-10-01T12:00:00Z", "")
            .unwrap();
        for frame in result.outgoing {
            self.inbox
                .push_back(Frame::decode(json::to_string(&frame).unwrap().as_bytes()).unwrap());
        }
        Ok(())
    }
    async fn next(&mut self) -> Result<Event> {
        self.inbox
            .pop_front()
            .map(Event::Frame)
            .ok_or(Error::Transport("unexpected empty test inbox"))
    }
    fn now(&self) -> u64 {
        0
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        self.entropy += 1;
        Ok([self.entropy; 32])
    }
}
fn value(s: &str) -> Value {
    json::parse(s.as_bytes()).unwrap()
}
fn client(stored: &str, fail: bool) -> Client<Wire> {
    let mut c = Client::new(Wire::new(stored, fail));
    hostnet::block_on(c.hello("com.stulp.virtualdevices")).unwrap();
    c
}

#[test]
fn restore_does_not_overwrite_durable_state_and_marks_available() {
    let mut c = client(r#"{"onoff":true}"#, false);
    hostnet::block_on(VirtualDevices::default().handle(
        &mut c,
        "device.init",
        &value(r#"{"deviceId":"d","driverId":"switch"}"#),
    ))
    .unwrap();
    assert!(json::boolean(
        json::get(c.state().device("d").unwrap(), "state").unwrap(),
        "onoff"
    ));
    assert!(json::boolean(c.state().device("d").unwrap(), "available"));
    assert_eq!(c.into_transport().operations, ["state", "available"]);
}
#[test]
fn absent_or_invalid_state_is_persisted_before_publishing() {
    for stored in ["{}", r#"{"onoff":"aan"}"#] {
        let mut c = client(stored, false);
        hostnet::block_on(VirtualDevices::default().handle(
            &mut c,
            "device.init",
            &value(r#"{"deviceId":"d","driverId":"switch"}"#),
        ))
        .unwrap();
        assert_eq!(
            c.into_transport().operations,
            ["store", "state", "available"]
        );
    }
}
#[test]
fn rejected_store_write_never_publishes_a_new_value() {
    let mut c = client(r#"{"onoff":false}"#, true);
    let error = hostnet::block_on(VirtualDevices::default().handle(
        &mut c,
        "capability.invoke",
        &value(r#"{"deviceId":"d","capability":"onoff","value":true}"#),
    ))
    .unwrap_err();
    assert!(error.to_string().contains("disk full"));
    assert_eq!(c.into_transport().operations, ["store"]);
}
#[test]
fn accepted_write_survives_controller_restart() {
    let mut c = client(r#"{"onoff":false}"#, false);
    hostnet::block_on(VirtualDevices::default().handle(
        &mut c,
        "capability.invoke",
        &value(r#"{"deviceId":"d","capability":"onoff","value":true}"#),
    ))
    .unwrap();
    let wire = c.into_transport();
    assert_eq!(wire.operations, ["store", "state"]);
    let restarted =
        Store::open(wire.store.document().encode().unwrap().as_bytes(), Memory).unwrap();
    assert!(json::boolean(
        json::get(&restarted.device("d").unwrap(), "store").unwrap(),
        "onoff"
    ));
}
#[test]
fn pair_sessions_keep_separate_candidates_and_retry_identity() {
    let mut c = client("{}", false);
    let mut plugin = VirtualDevices::default();
    for id in ["first", "second"] {
        hostnet::block_on(plugin.handle(
            &mut c,
            "pair.start",
            &value(&format!(r#"{{"driverId":"switch","sessionId":"{id}"}}"#)),
        ))
        .unwrap();
    }
    let list = value(r#"{"sessionId":"first","event":"list_devices"}"#);
    assert!(hostnet::block_on(plugin.handle(&mut c, "pair.emit", &list)).is_err());
    let mut ids = Vec::new();
    for id in ["first", "second"] {
        let params = value(&format!(
            r#"{{"sessionId":"{id}","event":"create","data":{{"name":"  Alarm  "}}}}"#
        ));
        let candidate = hostnet::block_on(plugin.handle(&mut c, "pair.emit", &params)).unwrap();
        assert_eq!(json::text(&candidate, "name"), "Alarm");
        ids.push(json::text(json::get(&candidate, "data").unwrap(), "id").to_string());
    }
    assert_ne!(ids[0], ids[1]);
    let one = hostnet::block_on(plugin.handle(&mut c, "pair.emit", &list)).unwrap();
    let two = hostnet::block_on(plugin.handle(&mut c, "pair.emit", &list)).unwrap();
    assert_eq!(one, two);
}
