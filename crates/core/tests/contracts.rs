//! Overgezette regressiecontracten uit internal/store en internal/flow.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    Error,
    document::Document,
    json::{self, TryClone, Value},
    store::{Memory, Storage, Store},
};
const NOW: &str = "2026-10-01T12:00:00Z";
fn value(s: &str) -> Value {
    json::parse(s.as_bytes()).unwrap()
}
fn put<S: Storage>(s: &mut Store<S>, collection: &str, raw: &str) {
    s.put(collection, value(raw), true, None, NOW).unwrap();
}
fn base() -> Store<Memory> {
    let mut s = Store::open(b"", Memory).unwrap();
    put(&mut s, "apps", r#"{"id":"app","enabled":true}"#);
    put(
        &mut s,
        "devices",
        r#"{"id":"d","appId":"app","driverId":"lamp","name":"Hardware","data":{"id":1},"capabilities":["onoff"]}"#,
    );
    s
}

#[test]
fn installed_app_metadata_is_not_limited_by_the_active_connection_pool() {
    let mut apps = Vec::new();
    for n in 0..160 {
        apps.push(
            json::fields(&[
                ("id", json::string(&format!("fixture.{n}")).unwrap()),
                ("enabled", Value::Bool(false)),
            ])
            .unwrap(),
        );
    }
    let source = json::fields(&[("apps", Value::Array(apps))]).unwrap();
    let mut store = Store::open(json::to_string(&source).unwrap().as_bytes(), Failing).unwrap();
    let before = store.document().encode().unwrap();
    for n in 0..160 {
        let id = format!("fixture.{n}");
        store.set_app_status(&id, "waiting").unwrap();
        store
            .announce(
                &id,
                json::fields(&[
                    ("id", json::string(&id).unwrap()),
                    ("sdk", Value::uint(3)),
                    ("version", json::string("1.0").unwrap()),
                ])
                .unwrap(),
            )
            .unwrap();
        store.set_app_status(&id, "stopped").unwrap();
        assert!(store.manifest(&id).is_some());
        assert_eq!(store.app_status(&id), "stopped");
    }
    assert_eq!(store.document().encode().unwrap(), before);
}

#[test]
fn version_unknown_fields_and_integer_precision() {
    assert!(Document::decode(br#"{"version":3}"#).is_err());
    let doc =
        Document::decode(br#"{"version":1,"extension":{"counter":18446744073709551615}}"#).unwrap();
    let again = Document::decode(doc.encode().unwrap().as_bytes()).unwrap();
    assert_eq!(json::uint(again.root(), "version"), 2);
    assert_eq!(
        json::uint(json::get(again.root(), "extension").unwrap(), "counter"),
        u64::MAX
    );
}

struct Failing;
impl Storage for Failing {
    fn save(&mut self, _: &[u8]) -> stulp_core::Result {
        Err(Error::Storage)
    }
}
#[test]
fn failed_write_publishes_nothing_and_changes_nothing() {
    let mut s = Store::open(b"", Failing).unwrap();
    let before = s.document().encode().unwrap();
    assert_eq!(
        s.put(
            "deviceGroups",
            value(r#"{"id":"g","name":"Kitchen"}"#),
            true,
            None,
            NOW
        ),
        Err(Error::Storage)
    );
    assert_eq!(s.document().encode().unwrap(), before);
    assert_eq!(s.sequence(), 0);
}

#[test]
fn observations_do_not_touch_storage_or_survive_restart() {
    let s = base();
    let bytes = s.document().encode().unwrap();
    let mut s = Store::open(bytes.as_bytes(), Failing).unwrap();
    s.observe("app", "d", value(r#"{"onoff":true}"#), true, "")
        .unwrap();
    assert!(json::boolean(&s.device("d").unwrap(), "available"));
    assert_eq!(s.document().encode().unwrap(), bytes);
    let restarted = Store::open(bytes.as_bytes(), Memory).unwrap();
    assert!(!json::boolean(&restarted.device("d").unwrap(), "available"));
    assert_eq!(
        json::get(&restarted.device("d").unwrap(), "state").unwrap(),
        &json::object()
    );
    assert!(s.observe("foreign", "d", json::object(), true, "").is_err());
}

#[test]
fn hardware_name_survives_rename() {
    let mut s = base();
    let mut d = s
        .document()
        .record("devices", "d")
        .unwrap()
        .try_clone()
        .unwrap();
    json::set(&mut d, "name", json::string("Friendly").unwrap()).unwrap();
    s.put("devices", d, false, None, NOW).unwrap();
    assert_eq!(
        json::text(
            json::get(&s.device("d").unwrap(), "store").unwrap(),
            "__stulp.hardwareName"
        ),
        "Hardware"
    );
}

#[test]
fn duplicate_pairing_and_group_cycles_are_rejected() {
    let mut s = base();
    assert!(
        s.put(
            "devices",
            value(r#"{"id":"other","appId":"app","driverId":"lamp","data":{"id":1}}"#),
            true,
            None,
            NOW
        )
        .is_err()
    );
    put(&mut s, "deviceGroups", r#"{"id":"a","name":"A"}"#);
    put(
        &mut s,
        "deviceGroups",
        r#"{"id":"b","name":"B","parentId":"a"}"#,
    );
    assert!(
        s.put(
            "deviceGroups",
            value(r#"{"id":"a","name":"A","parentId":"b"}"#),
            false,
            None,
            NOW
        )
        .is_err()
    );
    s.delete("deviceGroups", "a").unwrap();
    assert_eq!(
        json::text(
            s.document().record("deviceGroups", "b").unwrap(),
            "parentId"
        ),
        ""
    );
}

#[test]
fn reorder_requires_every_member_exactly_once() {
    let mut s = base();
    assert!(s.reorder_devices("", &[]).is_err());
    assert!(s.reorder_devices("", &["d", "d"]).is_err());
    s.reorder_devices("", &["d"]).unwrap();
    assert_eq!(json::uint(&s.device("d").unwrap(), "sortOrder"), 10);
}

const FLOW: &str = r#"{"id":"f","name":"Lights","enabled":true,"nodes":[{"id":"t","step":{"appId":"app","cardId":"trigger","cardType":"trigger"}},{"id":"a","step":{"appId":"app","cardId":"action","cardType":"action"}}],"edges":[{"id":"e","from":"t","to":"a"}]}"#;
#[test]
fn optimistic_revision_and_snapshot_ownership() {
    let mut s = base();
    put(&mut s, "flows", FLOW);
    s.put("flows", value(FLOW), false, Some(1), NOW).unwrap();
    assert_eq!(
        s.put("flows", value(FLOW), false, Some(1), NOW),
        Err(Error::Changed)
    );
    assert_eq!(
        json::uint(s.document().record("flows", "f").unwrap(), "revision"),
        2
    );
}

#[test]
fn flow_graph_executes_only_connected_actions() {
    use stulp_core::flow::{self, Execution};
    let f = value(FLOW);
    assert_eq!(flow::runnable(&f), (true, ""));
    let mut execution = Execution::start(&f, "t").unwrap();
    assert_eq!(json::text(execution.next(&f).unwrap().unwrap(), "id"), "a");
    assert!(execution.next(&f).is_err());
    execution.complete(&f, true).unwrap();
    assert!(execution.next(&f).unwrap().is_none());
    let mut cyclic = f.try_clone().unwrap();
    json::set(
        &mut cyclic,
        "edges",
        value(r#"[{"from":"t","to":"a"},{"from":"a","to":"t"}]"#),
    )
    .unwrap();
    assert!(flow::validate(&cyclic).is_err());
}

#[test]
fn scene_restore_snapshot_is_durable_and_protected() {
    let mut s = base();
    put(
        &mut s,
        "scenes",
        r#"{"id":"s","name":"Evening","states":[{"deviceId":"d","capabilityId":"onoff","value":true}]}"#,
    );
    let previous = vec![value(
        r#"{"deviceId":"d","capabilityId":"onoff","value":false}"#,
    )];
    assert!(s.begin_scene("s", previous).unwrap());
    assert!(!s.begin_scene("s", vec![]).unwrap());
    assert!(s.delete("scenes", "s").is_err());
    let mut restarted = Store::open(s.document().encode().unwrap().as_bytes(), Memory).unwrap();
    assert!(json::boolean(
        json::get(&restarted.device("scene:s").unwrap(), "state").unwrap(),
        "onoff"
    ));
    restarted.scene_remaining("s", vec![]).unwrap();
    restarted.delete("scenes", "s").unwrap();
    assert!(restarted.device("scene:s").is_err());
}

#[test]
fn uninstall_removes_secrets_and_disables_dependent_flows() {
    let mut s = base();
    put(&mut s, "flows", FLOW);
    s.setting("app", "secret", Some(json::string("hidden").unwrap()))
        .unwrap();
    s.app_state("app", value(r#"{"privateKey":"hidden"}"#))
        .unwrap();
    s.delete("apps", "app").unwrap();
    assert!(s.device("d").is_err());
    assert!(!json::boolean(
        s.document().record("flows", "f").unwrap(),
        "enabled"
    ));
    assert!(!s.document().encode().unwrap().contains("hidden"));
}

#[test]
fn event_overflow_requires_authoritative_reload() {
    let mut s = base();
    for i in 0..70 {
        s.observe(
            "app",
            "d",
            json::fields(&[("value", Value::uint(i))]).unwrap(),
            true,
            "",
        )
        .unwrap();
    }
    assert!(matches!(s.events_after(0), Err(Error::Changed)));
    assert_eq!(s.events_after(s.sequence() - 1).unwrap().count(), 1);
}

#[test]
fn transient_store_keys_never_reach_the_document() {
    let mut s = base();
    let mut d = s.device("d").unwrap();
    json::set(
        &mut d,
        "store",
        value(r#"{"~cache":"discard","persist":"keep"}"#),
    )
    .unwrap();
    s.put("devices", d, false, None, NOW).unwrap();
    let text = s.document().encode().unwrap();
    assert!(!text.contains("discard"));
    assert!(text.contains("keep"));
    assert_eq!(
        json::text(
            json::get(&s.device("d").unwrap(), "store").unwrap(),
            "~cache"
        ),
        "discard"
    );
    let mut renamed = s
        .document()
        .record("devices", "d")
        .unwrap()
        .try_clone()
        .unwrap();
    json::set(&mut renamed, "name", json::string("Renamed").unwrap()).unwrap();
    s.put("devices", renamed, false, None, NOW).unwrap();
    assert_eq!(
        json::text(
            json::get(&s.device("d").unwrap(), "store").unwrap(),
            "~cache"
        ),
        "discard"
    );
    let restarted = Store::open(s.document().encode().unwrap().as_bytes(), Memory).unwrap();
    assert!(
        json::get(
            json::get(&restarted.device("d").unwrap(), "store").unwrap(),
            "~cache"
        )
        .is_none()
    );
}

#[test]
fn older_flow_completion_does_not_replace_newer_history() {
    let mut s = base();
    put(
        &mut s,
        "flows",
        r#"{"id":"f","name":"Test","nodes":[],"edges":[]}"#,
    );
    s.flow_result("f", "2026-10-01T12:00:00.1Z", "newer failure", NOW)
        .unwrap();
    let before = s.document().encode().unwrap();
    let sequence = s.sequence();
    s.flow_result("f", "2026-10-01T12:00:00Z", "", NOW).unwrap();
    assert_eq!(s.document().encode().unwrap(), before);
    assert_eq!(s.sequence(), sequence);
}

#[test]
fn numeric_equality_preserves_large_identity_values() {
    assert!(json::equal(&value("20"), &value("20.0")));
    assert!(json::equal(&value("-0.0"), &value("0")));
    assert!(json::equal(
        &value(r#"{"a":1,"b":2}"#),
        &value(r#"{"b":2.0,"a":1.0}"#)
    ));
    assert!(!json::equal(
        &value("9007199254740993"),
        &value("9007199254740992.0")
    ));
    assert!(!json::equal(
        &value("18446744073709551615"),
        &value("18446744073709551616.0")
    ));
    assert!(!json::equal(
        &value("9223372036854775807"),
        &value("9223372036854775808.0")
    ));
}

#[test]
fn flow_events_are_owned_transient_bounded_and_capability_changes_are_deduplicated() {
    let mut s = base();
    s.observe("app", "d", value(r#"{"onoff":false}"#), true, "")
        .unwrap();
    assert!(s.take_trigger().is_none());
    s.observe("app", "d", value(r#"{"onoff":true}"#), true, "")
        .unwrap();
    let generic = s.take_trigger().unwrap();
    let specific = s.take_trigger().unwrap();
    assert_eq!(json::text(&generic, "id"), "device_capability_changed");
    assert_eq!(json::text(&specific, "id"), "capability.onoff.on");
    assert_eq!(json::text(&specific, "appId"), "stulp");
    s.observe("app", "d", value(r#"{"onoff":true}"#), false, "offline")
        .unwrap();
    assert!(s.take_trigger().is_none());
    let p = value(
        r#"{"id":"button","kind":"device-trigger","state":{"deviceId":"d"},"tokens":{"number":18446744073709551615}}"#,
    );
    put(&mut s, "apps", r#"{"id":"foreign","enabled":true}"#);
    assert!(s.trigger("foreign", &p).is_err());
    let before = s.document().encode().unwrap();
    for _ in 0..stulp_core::store::MAX_TRIGGERS {
        s.trigger("app", &p).unwrap();
    }
    assert!(s.trigger("app", &p).is_err());
    assert_eq!(s.document().encode().unwrap(), before);
    assert!(
        Store::open(before.as_bytes(), Memory)
            .unwrap()
            .take_trigger()
            .is_none()
    );
}

#[test]
fn media_ownership_lazy_tickets_expiry_and_restart() {
    let mut s = base();
    s.announce(
        "app",
        value(r#"{"id":"app","sdk":3,"version":"1","drivers":[{"id":"lamp"}]}"#),
    )
    .unwrap();
    s.set_app_status("app", "running").unwrap();
    let media = value(
        r#"{"deviceId":"d","media":[{"slot":"snapshot","kind":"image","title":"Front door","resourceId":"snapshot"}]}"#,
    );
    assert!(s.register_media("other", &media).is_err());
    let before = s.document().encode().unwrap();
    s.register_media("app", &media).unwrap();
    assert_eq!(s.image_sources().unwrap().as_array().unwrap().len(), 1);
    let ticket = "abcdef0123456789abcdef0123456789aa";
    assert_eq!(
        json::text(&s.share_image(ticket, "d", "", 1000).unwrap(), "url"),
        format!("/image/{ticket}")
    );
    assert!(s.image_source(ticket, 900999).is_ok());
    assert!(s.image_source(ticket, 901000).is_err());
    assert!(s.image_source("unknown", 1001).is_err());
    assert_eq!(s.document().encode().unwrap(), before);
    assert!(
        Store::open(before.as_bytes(), Memory)
            .unwrap()
            .image_source(ticket, 1001)
            .is_err()
    );
    for n in 0..8 {
        s.share_image(
            &format!("0123456789abcdef0123456789abcdef{n:02}"),
            "d",
            "snapshot",
            2000 + n,
        )
        .unwrap();
    }
    assert!(s.image_source(ticket, 2100).is_err());
    s.set_app_status("app", "waiting").unwrap();
    s.set_app_status("app", "running").unwrap();
    assert!(s.device_media("d").unwrap().as_array().unwrap().is_empty());
}

#[test]
fn app_references_rewrite_flows_and_active_scene_baselines_atomically() {
    let source = r#"{"version":2,"devices":[{"id":"old","appId":"a","capabilities":["button"]},{"id":"new","appId":"a","capabilities":["button.2"]},{"id":"foreign","appId":"b","capabilities":["onoff"]}],"flows":[{"id":"f","name":"Keep","enabled":true,"revision":7,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"capability.button.on","args":{"device":{"$device":"old"},"capability":"button"},"state":{"deviceId":"old"}}},{"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"old"}}}],"edges":[{"from":"t","to":"a"}]}],"scenes":[{"id":"s","name":"Keep active","kind":"switch","revision":4,"active":true,"states":[{"deviceId":"old","capabilityId":"button","value":true}],"previous":[{"deviceId":"old","capabilityId":"button","value":false}]}]}"#;
    let replacements = value(r#"{"old":{"deviceId":"new","capabilities":{"button":"button.2"}}}"#);
    let mut s = Store::open(source.as_bytes(), Memory).unwrap();
    assert!(s.replace_references("b", &replacements, NOW).is_err());
    assert!(
        s.replace_references("a", &value(r#"{"old":{"deviceId":"foreign"}}"#), NOW)
            .is_err()
    );
    s.replace_references("a", &replacements, NOW).unwrap();
    let flow = s.document().record("flows", "f").unwrap();
    assert_eq!(json::uint(flow, "revision"), 8);
    assert!(json::boolean(flow, "enabled"));
    let step = json::get(&json::array(flow, "nodes")[0], "step").unwrap();
    assert_eq!(json::text(step, "cardId"), "capability.button.2.on");
    assert_eq!(
        json::text(json::get(step, "state").unwrap(), "deviceId"),
        "new"
    );
    let action = json::get(&json::array(flow, "nodes")[1], "step").unwrap();
    assert_eq!(
        json::text(json::get(action, "args").unwrap(), "excerpt"),
        "old"
    );
    let scene = s.document().record("scenes", "s").unwrap();
    assert!(json::boolean(scene, "active"));
    assert_eq!(json::uint(scene, "revision"), 5);
    for key in ["states", "previous"] {
        let state = &json::array(scene, key)[0];
        assert_eq!(json::text(state, "deviceId"), "new");
        assert_eq!(json::text(state, "capabilityId"), "button.2");
    }
    assert_eq!(
        json::get(&json::array(scene, "previous")[0], "value"),
        Some(&Value::Bool(false))
    );
    let before = s.document().encode().unwrap();
    let sequence = s.sequence();
    s.replace_references("a", &replacements, NOW).unwrap();
    assert_eq!(s.sequence(), sequence);
    assert_eq!(s.document().encode().unwrap(), before);
    let mut failed = Store::open(source.as_bytes(), Failing).unwrap();
    let before = failed.document().encode().unwrap();
    assert!(failed.replace_references("a", &replacements, NOW).is_err());
    assert_eq!(failed.document().encode().unwrap(), before);
    assert_eq!(failed.sequence(), 0);
}
