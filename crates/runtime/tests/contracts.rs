//! Overgezette lifecycle-, isolatie- en read-your-own-writes-contracten.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_protocol::Frame;
use stulp_runtime::{
    App,
    supervisor::{Mode, State, Supervisor},
};
fn fixture() -> (Store<Memory>, App) {
    let store=Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true},{"id":"b","enabled":true}],"devices":[{"id":"own","appId":"a","driverId":"lamp","name":"Own","data":{"id":1},"capabilities":["onoff"]},{"id":"foreign","appId":"b","driverId":"lamp","name":"Foreign","data":{"id":2}}],"appState":{"a":{"secret":"mine"},"b":{"secret":"theirs"}}}"#,Memory).unwrap();
    let manifest =
        json::parse(br#"{"id":"a","sdk":3,"version":"1","drivers":[{"id":"lamp"}]}"#).unwrap();
    (store, App::new("a", manifest).unwrap())
}
fn request(id: u64, method: &str, params: &str) -> Frame {
    let v = Frame::request(id, method, &json::parse(params.as_bytes()).unwrap()).unwrap();
    Frame::decode(json::to_string(&v).unwrap().as_bytes()).unwrap()
}
fn response(id: u64) -> Frame {
    Frame::decode(
        json::to_string(&Frame::response(id, Ok(Value::Null)).unwrap())
            .unwrap()
            .as_bytes(),
    )
    .unwrap()
}

#[test]
fn hello_filters_secrets_and_initialization_waits_for_each_ack() {
    let (mut store, mut app) = fixture();
    let received = app
        .receive(
            &mut store,
            request(1, "hello", r#"{"protocol":1}"#),
            0,
            "now",
            "",
        )
        .unwrap();
    let welcome = json::to_string(&received.outgoing[0]).unwrap();
    assert!(welcome.contains("mine"));
    assert!(!welcome.contains("theirs"));
    assert!(!welcome.contains("Foreign"));
    assert_eq!(json::text(&received.outgoing[1], "m"), "app.init");
    assert!(!app.is_running());
    let duplicate = app
        .receive(
            &mut store,
            request(2, "hello", r#"{"protocol":1}"#),
            0,
            "now",
            "",
        )
        .unwrap();
    assert_eq!(duplicate.outgoing.len(), 1);
    assert_eq!(json::text(&duplicate.outgoing[0], "t"), "err");
    let next = app.receive(&mut store, response(1), 1, "now", "").unwrap();
    assert_eq!(json::text(&next.outgoing[0], "m"), "driver.init");
    let next = app.receive(&mut store, response(2), 2, "now", "").unwrap();
    assert_eq!(json::text(&next.outgoing[0], "m"), "device.init");
    app.receive(&mut store, response(3), 3, "now", "").unwrap();
    assert!(app.is_running());
}

#[test]
fn foreign_mutations_fail_and_ping_does_not_wait_for_init() {
    let (mut store, mut app) = fixture();
    app.receive(
        &mut store,
        request(1, "hello", r#"{"protocol":1}"#),
        0,
        "now",
        "",
    )
    .unwrap();
    let before = store.document().encode().unwrap();
    let failed = app
        .receive(
            &mut store,
            request(
                4,
                "device.set",
                r#"{"deviceId":"foreign","field":"name","value":"owned"}"#,
            ),
            1,
            "now",
            "",
        )
        .unwrap();
    assert_eq!(json::text(&failed.outgoing[0], "t"), "err");
    assert_eq!(store.document().encode().unwrap(), before);
    let ping = app
        .receive(
            &mut store,
            request(5, "$appproto.ping", "null"),
            2,
            "now",
            "",
        )
        .unwrap();
    assert_eq!(json::text(&ping.outgoing[0], "t"), "res");
    assert!(!app.is_running());
}

#[test]
fn mutations_push_the_snapshot_before_acknowledging() {
    let (mut store, mut app) = fixture();
    app.receive(
        &mut store,
        request(1, "hello", r#"{"protocol":1}"#),
        0,
        "now",
        "",
    )
    .unwrap();
    let updated = app
        .receive(
            &mut store,
            request(
                2,
                "device.merge",
                r#"{"deviceId":"own","field":"state","patch":{"onoff":true}}"#,
            ),
            1,
            "now",
            "",
        )
        .unwrap();
    assert_eq!(json::text(&updated.outgoing[0], "m"), "state.device");
    assert_eq!(json::text(&updated.outgoing[1], "t"), "res");
    assert!(!store.document().encode().unwrap().contains("onoff\":true"));
}

#[test]
fn attached_exit_waits_spawned_exit_retries_and_old_exit_is_ignored() {
    let mut attached = Supervisor::new(Mode::Attached);
    let first = attached.start().unwrap();
    attached.ready(first).unwrap();
    assert!(attached.exited(first, 0));
    assert_eq!(attached.state(), State::Waiting);
    assert!(!attached.retry_due(u64::MAX));
    let next = attached.start().unwrap();
    assert!(!attached.exited(first, 10));
    attached.ready(next).unwrap();
    let mut spawned = Supervisor::new(Mode::Spawned);
    let first = spawned.start().unwrap();
    spawned.exited(first, 0);
    assert!(!spawned.retry_due(999));
    assert!(spawned.retry_due(1000));
    spawned.stop();
    assert!(!spawned.retry_due(u64::MAX));
}

#[test]
fn browser_settings_and_new_devices_reach_app_before_initialization() {
    use json::TryClone;
    let (mut store, mut app) = fixture();
    app.receive(
        &mut store,
        request(1, "hello", r#"{"protocol":1}"#),
        0,
        "now",
        "",
    )
    .unwrap();
    for id in 1..=3 {
        app.receive(&mut store, response(id), id, "now", "")
            .unwrap();
    }
    store
        .setting("a", "enabled", Some(Value::Bool(true)))
        .unwrap();
    store
        .setting("b", "private", Some(Value::Bool(true)))
        .unwrap();
    let frames = app.follow(&store, 4).unwrap();
    assert_eq!(frames.len(), 1);
    assert_eq!(json::text(&frames[0], "m"), "state.settings");
    assert!(json::boolean(
        json::get(&frames[0], "p").unwrap(),
        "enabled"
    ));
    assert!(!json::to_string(&frames[0]).unwrap().contains("private"));
    let mut device = store
        .document()
        .record("devices", "own")
        .unwrap()
        .try_clone()
        .unwrap();
    json::set(&mut device, "id", json::string("new").unwrap()).unwrap();
    json::set(&mut device, "data", json::parse(br#"{"id":3}"#).unwrap()).unwrap();
    store.put("devices", device, true, None, "now").unwrap();
    let frames = app.follow(&store, 5).unwrap();
    assert_eq!(frames.len(), 2);
    assert_eq!(json::text(&frames[0], "m"), "state.device");
    assert_eq!(
        json::text(json::get(&frames[0], "p").unwrap(), "deviceId"),
        "new"
    );
    assert_eq!(json::text(&frames[1], "m"), "driver.init");
    let next = app
        .receive(
            &mut store,
            response(json::uint(&frames[1], "id")),
            6,
            "now",
            "",
        )
        .unwrap();
    assert_eq!(json::text(&next.outgoing[0], "m"), "device.init");
    assert_eq!(
        json::text(json::get(&next.outgoing[0], "p").unwrap(), "deviceId"),
        "new"
    );
    app.receive(
        &mut store,
        response(json::uint(&next.outgoing[0], "id")),
        7,
        "now",
        "",
    )
    .unwrap();
    assert!(app.follow(&store, 8).unwrap().is_empty());
    store.delete("devices", "new").unwrap();
    let frames = app.follow(&store, 9).unwrap();
    assert_eq!(
        json::get(json::get(&frames[0], "p").unwrap(), "device"),
        Some(&Value::Null)
    );
}
#[test]
fn scan_deadlines_are_scoped_to_discovery_and_bounded() {
    let test = json::parse(br#"{"handler":"test"}"#).unwrap();
    let emit = json::parse(br#"{"event":"list_devices"}"#).unwrap();
    for (app, method, params, want) in [
        ("com.stulp.sigenergy", "api.invoke", &test, 900_000),
        ("com.stulp.sigenergy", "pair.emit", &emit, 900_000),
        ("com.stulp.sigenergy", "pair.list", &Value::Null, 900_000),
        ("com.stulp.sigenergy", "api.invoke", &Value::Null, 120_000),
        ("com.stulp.sigenergy", "capability.invoke", &test, 30_000),
        ("other", "api.invoke", &test, 120_000),
    ] {
        assert_eq!(
            stulp_runtime::callback_timeout_for(app, method, params),
            want
        );
    }
}

#[test]
fn older_plugins_fall_back_in_order_and_never_repeat_uncertain_commands() {
    fn ready() -> (Store<Memory>, App) {
        let (mut store, mut app) = fixture();
        let mut received = app
            .receive(
                &mut store,
                request(1, "hello", r#"{"protocol":1}"#),
                0,
                "now",
                "",
            )
            .unwrap();
        while !app.is_running() {
            let id = json::uint(received.outgoing.last().unwrap(), "id");
            received = app.receive(&mut store, response(id), 1, "now", "").unwrap();
        }
        (store, app)
    }
    fn answer(id: u64, error: Option<&str>) -> Frame {
        let frame = Frame::response(id, error.map_or(Ok(Value::Null), Err)).unwrap();
        Frame::decode(json::to_string(&frame).unwrap().as_bytes()).unwrap()
    }
    let params = json::parse(br#"{"deviceId":"own","commands":[{"capability":"onoff","value":true},{"capability":"dim","value":0.5},{"capability":"light_hue","value":0.2}],"options":{"transition":2}}"#).unwrap();
    let (mut store, mut app) = ready();
    let original = app.call(20, "capabilities.invoke", &params, 100).unwrap();
    let mut received = app
        .receive(
            &mut store,
            answer(
                json::uint(&original, "id"),
                Some("unknown method capabilities.invoke"),
            ),
            101,
            "now",
            "",
        )
        .unwrap();
    for (index, (cap, error)) in [
        ("onoff", None),
        ("dim", Some("device refused dim")),
        ("light_hue", None),
    ]
    .into_iter()
    .enumerate()
    {
        assert!(received.completed.is_none());
        assert_eq!(received.outgoing.len(), 1);
        let command = &received.outgoing[0];
        assert_eq!(json::text(command, "m"), "capability.invoke");
        let p = json::get(command, "p").unwrap();
        assert_eq!(json::text(p, "deviceId"), "own");
        assert_eq!(json::text(p, "capability"), cap);
        assert_eq!(
            json::uint(json::get(p, "options").unwrap(), "transition"),
            2
        );
        received = app
            .receive(
                &mut store,
                answer(json::uint(command, "id"), error),
                102 + index as u64,
                "now",
                "",
            )
            .unwrap();
    }
    let done = received.completed.unwrap();
    assert_eq!(done.owner, 20);
    assert!(!done.failed);
    assert_eq!(json::text(&done.value, "dim"), "device refused dim");
    assert_eq!(done.value.as_object().unwrap().len(), 1);
    assert!(received.outgoing.is_empty());
    let original = app.call(21, "capabilities.invoke", &params, 1000).unwrap();
    let result = app
        .receive(
            &mut store,
            answer(json::uint(&original, "id"), Some("network timeout")),
            1001,
            "now",
            "",
        )
        .unwrap();
    assert!(result.completed.unwrap().failed);
    assert!(result.outgoing.is_empty());
    let original = app.call(22, "capabilities.invoke", &params, 2000).unwrap();
    let result = app
        .receive(
            &mut store,
            answer(json::uint(&original, "id"), Some("unknown method")),
            2001,
            "now",
            "",
        )
        .unwrap();
    let id = json::uint(&result.outgoing[0], "id");
    assert_eq!(app.expire(32_000), Some(22));
    let late = app
        .receive(&mut store, answer(id, None), 32_001, "now", "")
        .unwrap();
    assert!(late.outgoing.is_empty() && late.completed.is_none());
}
