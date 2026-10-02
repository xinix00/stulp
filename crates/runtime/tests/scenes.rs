//! Scenecontracten spiegelen de Go-activator: herstel vóór I/O en fouten per stand.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_runtime::scenes::Scene;
fn v(s: &str) -> Value {
    json::parse(s.as_bytes()).unwrap()
}
fn setup() -> Store<Memory> {
    let mut s=Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"devices":[{"id":"lamp","appId":"a","driverId":"d","name":"Lamp","data":{"id":1},"capabilities":["dim","onoff"]}],"scenes":[{"id":"movie","name":"Movie","kind":"switch","states":[{"deviceId":"lamp","capabilityId":"dim","value":0.3},{"deviceId":"lamp","capabilityId":"onoff","value":true}]}]}"#,Memory).unwrap();
    s.observe("a", "lamp", v(r#"{"dim":0.8,"onoff":false}"#), true, "")
        .unwrap();
    s
}
#[test]
fn grouped_order_original_baseline_and_partial_restore_retry() {
    let mut s = setup();
    let mut run = Scene::start(&mut s, "movie", true).unwrap();
    assert_eq!(
        json::array(s.document().record("scenes", "movie").unwrap(), "previous").len(),
        2
    );
    let (_, p) = run.next_group().unwrap().unwrap();
    assert_eq!(
        json::text(&json::array(&p, "commands")[0], "capability"),
        "onoff"
    );
    run.complete("lamp", &v("{}"), None).unwrap();
    assert!(json::boolean(&run.finish(&mut s).unwrap(), "success"));
    s.observe("a", "lamp", v(r#"{"dim":0.2992,"onoff":true}"#), true, "")
        .unwrap();
    let mut again = Scene::start(&mut s, "movie", true).unwrap();
    assert!(again.next_group().unwrap().is_none());
    assert!(json::boolean(&again.finish(&mut s).unwrap(), "success"));
    let mut off = Scene::start(&mut s, "movie", false).unwrap();
    let (_, p) = off.next_group().unwrap().unwrap();
    assert_eq!(
        json::text(&json::array(&p, "commands")[0], "capability"),
        "dim"
    );
    off.complete("lamp", &v(r#"{"onoff":"device asleep"}"#), None)
        .unwrap();
    let result = off.finish(&mut s).unwrap();
    assert!(json::boolean(&result, "active"));
    assert_eq!(json::uint(&result, "failed"), 1);
    let previous = json::array(s.document().record("scenes", "movie").unwrap(), "previous");
    assert_eq!(previous.len(), 1);
    assert_eq!(json::text(&previous[0], "capabilityId"), "onoff");
    let mut retry = Scene::start(&mut s, "movie", false).unwrap();
    let (_, p) = retry.next_group().unwrap().unwrap();
    assert_eq!(json::array(&p, "commands").len(), 1);
    retry.complete("lamp", &v("{}"), None).unwrap();
    assert!(!json::boolean(&retry.finish(&mut s).unwrap(), "active"));
}
#[test]
fn unknown_baseline_prevents_writes_and_button_needs_none() {
    let mut s = setup();
    s.observe("a", "lamp", v("{}"), false, "").unwrap();
    let mut run = Scene::start(&mut s, "movie", true).unwrap();
    assert!(run.next_group().unwrap().is_none());
    let result = run.finish(&mut s).unwrap();
    assert_eq!(json::uint(&result, "failed"), 2);
    assert!(!json::boolean(&result, "active"));
    s.put("scenes",v(r#"{"id":"button","name":"Press","kind":"button","states":[{"deviceId":"lamp","capabilityId":"onoff","value":true}]}"#),true,None,"now").unwrap();
    let mut run = Scene::start(&mut s, "button", true).unwrap();
    assert!(run.next_group().unwrap().is_some());
    run.complete("lamp", &v("{}"), None).unwrap();
    assert!(json::boolean(&run.finish(&mut s).unwrap(), "success"));
    assert!(Scene::start(&mut s, "button", false).is_err());
}
#[test]
fn scene_reports_only_trip_after_reaching_target() {
    let mut s = setup();
    let mut run = Scene::start(&mut s, "movie", true).unwrap();
    run.next_group().unwrap();
    run.complete("lamp", &v("{}"), None).unwrap();
    run.finish(&mut s).unwrap();
    for dim in [0.6, 0.4, 0.2992] {
        s.observe(
            "a",
            "lamp",
            v(&format!(r#"{{"dim":{dim},"onoff":true}}"#)),
            true,
            "",
        )
        .unwrap();
        assert!(s.take_scene_trip().is_none());
    }
    s.observe("a", "lamp", v(r#"{"dim":0.6,"onoff":true}"#), true, "")
        .unwrap();
    let trip = s.take_scene_trip().unwrap();
    assert_eq!(json::text(&trip, "sceneId"), "movie");
    assert_eq!(json::text(&trip, "capabilityId"), "dim");
}
