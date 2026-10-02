//! Existing controller documents migrate without device I/O or a delete RPC.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use std::cell::Cell;
use stulp_core::{
    Error,
    json::{self, TryClone, Value},
    store::{Memory, Storage, Store},
};
use stulp_matter::devices::reconcile;

const APP: &str = "com.stulp.matter";
const NOW: &str = "2026-10-02T00:00:00Z";
const SOURCE: &[u8] = br#"{
 "version":2,
 "apps":[{"id":"com.stulp.matter","enabled":true},{"id":"foreign","enabled":true}],
 "deviceGroups":[{"id":"room","name":"Room"}],
 "devices":[
  {"id":"a","appId":"com.stulp.matter","driverId":"matter","name":"Custom name - 1","groupId":"room","class":"light","data":{"id":"primary","endpoint":1},"capabilities":["onoff"],"store":{"matter.nodeId":"10","matter.endpoint":1,"matter.noc":"synthetic-key","matter.lastEventNumber":"4","custom":"kept"}},
  {"id":"b","appId":"com.stulp.matter","driverId":"matter","name":"Secondary","class":"sensor","data":{"id":"secondary","endpoint":2},"capabilities":["onoff"],"settings":{"secondary":true},"store":{"matter.nodeId":"10","matter.endpoint":2,"matter.lastEventNumber":"7"}},
  {"id":"bridge","appId":"com.stulp.matter","driverId":"matter","name":"Bridge child","data":{"id":"bridge"},"capabilities":["onoff"],"store":{"matter.nodeId":"10","matter.endpoint":3,"matter.bridged":true}},
  {"id":"alone","appId":"com.stulp.matter","driverId":"matter","name":"Other node","data":{"id":"alone"},"capabilities":["onoff"],"store":{"matter.nodeId":"11","matter.endpoint":1}},
  {"id":"foreign","appId":"foreign","driverId":"matter","name":"Other app","data":{"id":"foreign"},"capabilities":["onoff"],"store":{"matter.nodeId":"10","matter.endpoint":4}}
 ],
 "flows":[{"id":"f","name":"Keep","enabled":true,"revision":7,"nodes":[
  {"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"capability.onoff.on","args":{"device":{"$device":"b"},"capability":"onoff"},"state":{"deviceId":"b"}}},
  {"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"b"}}}
 ],"edges":[{"from":"t","to":"a"}]}],
 "scenes":[{"id":"s","name":"Keep active","kind":"switch","revision":4,"active":true,"states":[{"deviceId":"b","capabilityId":"onoff","value":false}],"previous":[{"deviceId":"b","capabilityId":"onoff","value":true}]}]
}"#;

struct Disk<'a> {
    writes: &'a Cell<usize>,
    fail: bool,
}
impl Storage for Disk<'_> {
    fn save(&mut self, _: &[u8]) -> stulp_core::Result {
        self.writes.set(self.writes.get() + 1);
        if self.fail {
            Err(Error::Storage)
        } else {
            Ok(())
        }
    }
}
fn field<'a>(v: &'a Value, key: &str) -> &'a Value {
    json::get(v, key).unwrap()
}
fn setup<S: Storage>(s: &mut Store<S>) {
    let on = || json::fields(&[("onoff", Value::Bool(true))]).unwrap();
    s.observe(APP, "a", on(), true, "").unwrap();
    s.observe(
        APP,
        "b",
        json::fields(&[("onoff", Value::Bool(false))]).unwrap(),
        true,
        "",
    )
    .unwrap();
    s.observe(APP, "b", on(), true, "").unwrap();
}

#[test]
fn migration_preserves_identity_routes_flows_scenes_and_queued_events_in_one_write() {
    let writes = Cell::new(0);
    let mut store = Store::open(
        SOURCE,
        Disk {
            writes: &writes,
            fail: false,
        },
    )
    .unwrap();
    setup(&mut store);
    let revision = store.capability_revision("b", "onoff");
    let old = store.document().record("devices", "a").unwrap();
    let data = json::to_string(field(old, "data")).unwrap();
    reconcile(&mut store, NOW).unwrap();
    assert_eq!(writes.get(), 1);
    assert!(store.device("b").is_err());
    let device = store.device("a").unwrap();
    assert_eq!(json::text(&device, "name"), "Custom name - 1");
    assert_eq!(json::text(&device, "groupId"), "room");
    assert_eq!(json::to_string(field(&device, "data")).unwrap(), data);
    assert_eq!(
        json::text(field(&device, "store"), "matter.noc"),
        "synthetic-key"
    );
    assert_eq!(json::text(field(&device, "store"), "custom"), "kept");
    assert_eq!(
        json::text(field(&device, "store"), "matter.lastEventNumber"),
        "7"
    );
    assert!(json::boolean(field(&device, "settings"), "secondary"));
    for cap in ["onoff.1", "onoff.2"] {
        assert!(json::boolean(field(&device, "state"), cap));
    }
    assert_eq!(store.capability_revision("a", "onoff.2"), revision);
    assert_eq!(stulp_matter::devices::endpoint(&device, "onoff.1"), 1);
    assert_eq!(stulp_matter::devices::endpoint(&device, "onoff.2"), 2);
    for id in ["bridge", "alone", "foreign"] {
        assert_eq!(
            json::array(&store.device(id).unwrap(), "capabilities")[0].as_str(),
            Some("onoff")
        );
    }
    let flow = store.document().record("flows", "f").unwrap();
    assert_eq!(json::uint(flow, "revision"), 8);
    let step = field(&json::array(flow, "nodes")[0], "step");
    assert_eq!(json::text(step, "cardId"), "capability.onoff.2.on");
    assert_eq!(
        json::text(field(field(step, "args"), "device"), "$device"),
        "a"
    );
    let action = field(&json::array(flow, "nodes")[1], "step");
    assert_eq!(json::text(field(action, "args"), "excerpt"), "b");
    let scene = store.document().record("scenes", "s").unwrap();
    assert!(json::boolean(scene, "active"));
    for key in ["states", "previous"] {
        let state = &json::array(scene, key)[0];
        assert_eq!(json::text(state, "deviceId"), "a");
        assert_eq!(json::text(state, "capabilityId"), "onoff.2");
        assert_eq!(json::boolean(state, "value"), key == "previous");
    }
    let trip = store.take_scene_trip().unwrap();
    assert_eq!(json::text(&trip, "deviceId"), "a");
    assert_eq!(json::text(&trip, "capabilityId"), "onoff.2");
    let mut capability_trigger = false;
    while let Some(trigger) = store.take_trigger() {
        assert_eq!(json::text(field(&trigger, "state"), "deviceId"), "a");
        assert_eq!(
            json::text(field(&trigger, "tokens"), "capability"),
            "onoff.2"
        );
        capability_trigger |= json::text(&trigger, "id") == "capability.onoff.2.on";
    }
    assert!(capability_trigger);
    let encoded = store.document().encode().unwrap();
    let sequence = store.sequence();
    reconcile(&mut store, NOW).unwrap();
    assert_eq!(writes.get(), 1);
    assert_eq!(store.sequence(), sequence);
    assert_eq!(store.document().encode().unwrap(), encoded);
    let mut rebooted = Store::open(encoded.as_bytes(), Memory).unwrap();
    reconcile(&mut rebooted, NOW).unwrap();
    assert!(rebooted.device("b").is_err());
    assert_eq!(
        stulp_matter::devices::endpoint(&rebooted.device("a").unwrap(), "onoff.2"),
        2
    );
}

#[test]
fn failed_migration_changes_neither_records_nor_live_routes_or_event_cursor() {
    let writes = Cell::new(0);
    let mut store = Store::open(
        SOURCE,
        Disk {
            writes: &writes,
            fail: true,
        },
    )
    .unwrap();
    setup(&mut store);
    let before = store.document().encode().unwrap();
    let a = json::to_string(&store.device("a").unwrap()).unwrap();
    let b = json::to_string(&store.device("b").unwrap()).unwrap();
    let sequence = store.sequence();
    assert!(reconcile(&mut store, NOW).is_err());
    assert_eq!(writes.get(), 1);
    assert_eq!(store.document().encode().unwrap(), before);
    assert_eq!(json::to_string(&store.device("a").unwrap()).unwrap(), a);
    assert_eq!(json::to_string(&store.device("b").unwrap()).unwrap(), b);
    assert_eq!(store.sequence(), sequence);
    assert_eq!(
        json::text(&store.take_scene_trip().unwrap(), "deviceId"),
        "b"
    );
    assert_eq!(
        json::text(field(&store.take_trigger().unwrap(), "state"), "deviceId"),
        "b"
    );
}

#[test]
fn newly_supported_lux_capabilities_keep_their_original_endpoints_and_commit_with_merge() {
    let mut source = json::parse(SOURCE).unwrap();
    let mut devices = Vec::new();
    for device in json::array(&source, "devices") {
        let mut device = device.try_clone().unwrap();
        if matches!(json::text(&device, "id"), "a" | "b") {
            let mut store = field(&device, "store").try_clone().unwrap();
            json::set(
                &mut store,
                "matter.serverClusters",
                json::parse(br#"["0x400"]"#).unwrap(),
            )
            .unwrap();
            json::set(&mut device, "store", store).unwrap();
        }
        devices.push(device);
    }
    json::set(&mut source, "devices", Value::Array(devices)).unwrap();
    let bytes = json::to_string(&source).unwrap();
    for fail in [true, false] {
        let writes = Cell::new(0);
        let mut store = Store::open(
            bytes.as_bytes(),
            Disk {
                writes: &writes,
                fail,
            },
        )
        .unwrap();
        let before = store.document().encode().unwrap();
        assert_eq!(reconcile(&mut store, NOW).is_err(), fail);
        assert_eq!(writes.get(), 1);
        if fail {
            assert_eq!(store.document().encode().unwrap(), before);
            assert_eq!(store.sequence(), 0);
        } else {
            let lamp = store.device("a").unwrap();
            assert_eq!(json::array(&lamp, "capabilities").len(), 4);
            assert_eq!(
                stulp_matter::devices::endpoint(&lamp, "measure_luminance.1"),
                1
            );
            assert_eq!(
                stulp_matter::devices::endpoint(&lamp, "measure_luminance.2"),
                2
            );
            assert!(store.device("b").is_err());
            reconcile(&mut store, NOW).unwrap();
            assert_eq!(writes.get(), 1);
        }
    }
}
