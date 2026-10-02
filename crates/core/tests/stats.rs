//! Statistiek houdt waarden, tellers en tijdsfracties uit elkaar en blijft begrensd.
#![allow(clippy::unwrap_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    stats::Statistics,
    store::{Memory, Store},
};
fn state(text: &str) -> Value {
    json::parse(text.as_bytes()).unwrap()
}
fn n(v: &Value, k: &str) -> f64 {
    match json::get(v, k).unwrap() {
        Value::Number(n) => n.as_f64(),
        _ => panic!("missing number"),
    }
}
const AT: u64 = 1_780_200_000_000;
#[test]
fn extremes_counter_resets_fraction_duration_and_fixed_memory() {
    let mut stats = Statistics::default();
    stats.tick(AT);
    for (i, v) in [20, 25, 30, 21].iter().enumerate() {
        stats
            .observe(
                "d",
                "House",
                &state(&format!(r#"{{"measure_temperature":{v}}}"#)),
                AT + i as u64 * 60_000,
            )
            .unwrap();
    }
    let gauge = stats.window("d", "measure_temperature", "day").unwrap();
    let slot = &json::array(&gauge, "slots")[0];
    assert_eq!(
        (n(slot, "average"), n(slot, "min"), n(slot, "max")),
        (24.0, 20.0, 30.0)
    );
    stats
        .observe(
            "d",
            "House",
            &state(r#"{"meter_power":5000,"onoff":true}"#),
            AT,
        )
        .unwrap();
    stats
        .observe("d", "House", &state(r#"{"meter_power":3}"#), AT + 60_000)
        .unwrap();
    let counter = stats.window("d", "meter_power", "day").unwrap();
    assert_eq!(n(&json::array(&counter, "slots")[0], "used"), 0.0);
    stats
        .observe("d", "House", &state(r#"{"onoff":false}"#), AT + 300_000)
        .unwrap();
    stats.tick(AT + 600_000);
    let fraction = stats.window("d", "onoff", "day").unwrap();
    assert_eq!(n(&json::array(&fraction, "slots")[0], "on"), 0.5);
    let size = stats.bytes();
    assert_eq!(size, 3 * 372 * 20);
    for i in 1..=400 {
        stats
            .observe(
                "d",
                "House",
                &state(r#"{"measure_temperature":22}"#),
                AT + i * 600_000,
            )
            .unwrap();
    }
    assert_eq!(stats.bytes(), size);
    assert_eq!(
        json::array(
            &stats.window("d", "measure_temperature", "0").unwrap(),
            "slots"
        )
        .len(),
        144
    );
}
#[test]
fn optional_collection_never_persists_observations_and_disable_releases_memory() {
    let mut s=Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"devices":[{"id":"d","appId":"a","name":"Room","capabilities":["measure_temperature"]}]}"#,Memory).unwrap();
    s.tick(AT);
    s.observe("a", "d", state(r#"{"measure_temperature":20}"#), true, "")
        .unwrap();
    assert_eq!(s.statistics().bytes(), 0);
    s.system(state(r#"{"statistics":true}"#)).unwrap();
    let before = s.document().encode().unwrap();
    s.observe("a", "d", state(r#"{"measure_temperature":21}"#), true, "")
        .unwrap();
    assert_eq!(s.statistics().bytes(), 7440);
    assert_eq!(s.document().encode().unwrap(), before);
    s.system(state(r#"{"statistics":false}"#)).unwrap();
    assert_eq!(s.statistics().bytes(), 0);
    assert!(json::array(&s.statistics().list().unwrap(), "series").is_empty());
}
