//! Een timer volgt de werkelijke capabilitywijzigingen, ook tussen twee polls in.
#![allow(clippy::unwrap_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_runtime::{flows::Effect, stability::Stability};
const DATE: &str = "2026-10-01T12:00:00Z";
fn parse(s: &str) -> Value {
    json::parse(s.as_bytes()).unwrap()
}
fn store() -> Store<Memory> {
    Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"devices":[{"id":"d","appId":"a","name":"Lamp","capabilities":["onoff","measure_power"]}],"flows":[{"id":"f","name":"Stable","enabled":true,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"capability.onoff.on_for","args":{"device":{"$device":"d"},"seconds":1}}},{"id":"other","step":{"appId":"stulp","cardType":"trigger","cardId":"time_at","args":{"time":"12:00"}}},{"id":"notify","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"{{device}} stable for {{seconds}}"}}},{"id":"wrong","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"WRONG"}}}],"edges":[{"from":"t","to":"notify"},{"from":"other","to":"wrong"}]}]}"#,Memory).unwrap()
}
fn observe(s: &mut Store<Memory>, on: bool, power: u64) {
    s.observe(
        "a",
        "d",
        parse(&format!(r#"{{"onoff":{on},"measure_power":{power}}}"#)),
        true,
        "",
    )
    .unwrap();
}
#[test]
fn stays_resets_for_brief_deviation_but_not_unrelated_reports_or_flow_renames() {
    let mut s = store();
    let mut timer = Stability::default();
    observe(&mut s, true, 1);
    timer.reconcile(&s, 0).unwrap();
    observe(&mut s, true, 2);
    timer.reconcile(&s, 300).unwrap();
    let mut f = s
        .document()
        .record("flows", "f")
        .unwrap()
        .try_clone()
        .unwrap();
    json::set(&mut f, "name", json::string("Renamed").unwrap()).unwrap();
    s.put("flows", f, false, None, DATE).unwrap();
    timer.reconcile(&s, 500).unwrap();
    assert!(timer.due(&s, 999, DATE).unwrap().is_none());
    let mut run = timer.due(&s, 1000, DATE).unwrap().unwrap();
    let Effect::Notification(text) = run.advance(&s, 1000).unwrap() else {
        panic!("no notification")
    };
    assert_eq!(text, "Lamp stable for 1");
    assert!(timer.due(&s, 1001, DATE).unwrap().is_none());
    observe(&mut s, false, 2);
    observe(&mut s, true, 2);
    timer.reconcile(&s, 1200).unwrap();
    assert!(timer.due(&s, 2199, DATE).unwrap().is_none());
    assert!(timer.due(&s, 2200, DATE).unwrap().is_some());
    observe(&mut s, false, 2);
    timer.reconcile(&s, 2400).unwrap();
    assert!(timer.due(&s, 10000, DATE).unwrap().is_none());
}
#[test]
fn disabling_or_removing_a_trigger_cancels_its_pending_timer() {
    let mut s = store();
    let mut timer = Stability::default();
    observe(&mut s, true, 1);
    timer.reconcile(&s, 0).unwrap();
    let mut f = s
        .document()
        .record("flows", "f")
        .unwrap()
        .try_clone()
        .unwrap();
    json::set(&mut f, "enabled", Value::Bool(false)).unwrap();
    s.put("flows", f, false, None, DATE).unwrap();
    timer.reconcile(&s, 900).unwrap();
    assert!(timer.due(&s, 2000, DATE).unwrap().is_none());
}
use json::TryClone;
