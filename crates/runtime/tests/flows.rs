//! Flow-contracten: echte callbackgrenzen, vertakkingen, snapshots en monotone deadlines.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_runtime::flows::{Effect, Run, TIMEOUT_MS};
const NOW: &str = "2026-10-01T12:00:00Z";
fn value(text: &str) -> Value {
    json::parse(text.as_bytes()).unwrap()
}
fn flow(nodes: &str, edges: &str) -> Value {
    value(&format!(
        r#"{{"id":"f","name":"Test","enabled":false,"nodes":[{{"id":"t","step":{{"appId":"stulp","cardType":"trigger","cardId":"time_at"}}}},{nodes}],"edges":[{edges}]}}"#
    ))
}
fn store() -> Store<Memory> {
    Store::open(b"", Memory).unwrap()
}

#[test]
fn manual_run_waits_for_real_callback_and_rejects_non_boolean_condition() {
    let definition = flow(
        r#"{"id":"a","step":{"appId":"plugin","cardType":"condition","cardId":"ready","args":{"threshold":12}}},{"id":"b","step":{"appId":"plugin","cardType":"action","cardId":"send"}}"#,
        r#"{"from":"t","to":"a"},{"from":"a","to":"b"}"#,
    );
    let mut run = Run::manual(&definition, 0, NOW).unwrap();
    let Effect::Call {
        app,
        method,
        params,
    } = run.advance(&store(), 0).unwrap()
    else {
        panic!("missing callback");
    };
    assert_eq!((app.as_str(), method), ("plugin", "flow.run"));
    assert_eq!(json::text(&params, "kind"), "condition");
    assert_eq!(
        json::uint(json::get(&params, "args").unwrap(), "threshold"),
        12
    );
    assert!(matches!(
        run.advance(&store(), 10).unwrap(),
        Effect::Waiting
    ));
    run.complete(Value::uint(1), 20).unwrap();
    assert!(matches!(
        run.advance(&store(), 21).unwrap(),
        Effect::Finished
    ));
    let result = run.result().unwrap();
    assert!(!json::boolean(&result, "success"));
    assert!(json::array(&result, "actions").is_empty());
    assert_eq!(json::array(&result, "conditions").len(), 1);
    assert!(run.error().contains("boolean"));
}

#[test]
fn false_condition_blocks_only_its_branch_and_shared_successor_runs_once() {
    let definition = flow(
        r#"{"id":"a","step":{"appId":"plugin","cardType":"condition","cardId":"ready"}},{"id":"b","step":{"appId":"plugin","cardType":"action","cardId":"independent"}},{"id":"c","step":{"appId":"plugin","cardType":"action","cardId":"shared"}}"#,
        r#"{"from":"t","to":"a"},{"from":"t","to":"b"},{"from":"a","to":"c"},{"from":"b","to":"c"}"#,
    );
    for passed in [false, true] {
        let mut run = Run::manual(&definition, 0, NOW).unwrap();
        assert!(matches!(
            run.advance(&store(), 0).unwrap(),
            Effect::Call { .. }
        ));
        run.complete(Value::Bool(passed), 1).unwrap();
        for expected in ["independent", "shared"] {
            let Effect::Call { params, .. } = run.advance(&store(), 2).unwrap() else {
                panic!("missing action");
            };
            assert_eq!(json::text(&params, "id"), expected);
            run.complete(Value::Null, 3).unwrap();
        }
        assert!(matches!(
            run.advance(&store(), 4).unwrap(),
            Effect::Finished
        ));
        assert_eq!(json::array(&run.result().unwrap(), "actions").len(), 2);
        assert!(!json::boolean(&run.result().unwrap(), "stopped"));
    }
}

#[test]
fn inverted_condition_and_stopped_result_match_go() {
    let definition = flow(
        r#"{"id":"a","step":{"appId":"plugin","cardType":"condition","cardId":"ready","inverted":true}},{"id":"b","step":{"appId":"plugin","cardType":"action","cardId":"send"}}"#,
        r#"{"from":"t","to":"a"},{"from":"a","to":"b"}"#,
    );
    let mut run = Run::manual(&definition, 0, NOW).unwrap();
    run.advance(&store(), 0).unwrap();
    run.complete(Value::Bool(true), 1).unwrap();
    assert!(matches!(
        run.advance(&store(), 2).unwrap(),
        Effect::Finished
    ));
    let result = run.result().unwrap();
    assert!(json::boolean(&result, "success"));
    assert!(json::boolean(&result, "stopped"));
    assert!(!json::boolean(
        &json::array(&result, "conditions")[0],
        "passed"
    ));
}

#[test]
fn delay_yields_and_deadline_wins_over_a_late_callback() {
    let definition = flow(
        r#"{"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"delay","args":{"seconds":30}}},{"id":"b","step":{"appId":"plugin","cardType":"action","cardId":"send"}}"#,
        r#"{"from":"t","to":"a"},{"from":"a","to":"b"}"#,
    );
    let mut run = Run::manual(&definition, 100, NOW).unwrap();
    assert!(matches!(
        run.advance(&store(), 100).unwrap(),
        Effect::Waiting
    ));
    assert!(matches!(
        run.advance(&store(), 30_099).unwrap(),
        Effect::Waiting
    ));
    assert!(matches!(
        run.advance(&store(), 30_100).unwrap(),
        Effect::Waiting
    ));
    assert!(matches!(
        run.advance(&store(), 30_101).unwrap(),
        Effect::Call { .. }
    ));
    run.complete(Value::Bool(true), 100 + TIMEOUT_MS).unwrap();
    assert!(run.error().contains("timed out"));
    run.complete(Value::Bool(true), 100 + TIMEOUT_MS + 1)
        .unwrap();
    assert!(!json::boolean(&run.result().unwrap(), "success"));
    assert_eq!(json::array(&run.result().unwrap(), "actions").len(), 2);
}

#[test]
fn capability_toggle_uses_live_state_and_canonical_values() {
    let mut store = Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"devices":[{"id":"d","appId":"a","driverId":"lamp","capabilities":["onoff"]}]}"#,Memory).unwrap();
    store
        .observe("a", "d", value(r#"{"onoff":false}"#), true, "")
        .unwrap();
    let definition = flow(
        r#"{"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"capability.onoff.toggle","args":{"device":{"$device":"d"}}}}"#,
        r#"{"from":"t","to":"a"}"#,
    );
    let mut run = Run::manual(&definition, 0, NOW).unwrap();
    let Effect::Call {
        app,
        method,
        params,
    } = run.advance(&store, 0).unwrap()
    else {
        panic!("missing capability call");
    };
    assert_eq!((app.as_str(), method), ("a", "capability.invoke"));
    assert_eq!(json::text(&params, "deviceId"), "d");
    assert_eq!(json::get(&params, "value"), Some(&Value::Bool(true)));
    // Alleen een waarneming van de app mag een nieuwe live-waarde publiceren.
    assert!(!json::boolean(
        json::get(&store.device("d").unwrap(), "state").unwrap(),
        "onoff"
    ));
}

#[test]
fn automatic_trigger_filters_device_and_threshold_and_preserves_integer_tokens() {
    let mut definition = flow(
        r#"{"id":"a","step":{"appId":"plugin","cardType":"action","cardId":"send","args":{"exact":"{{counter}}","text":"event {{state.event}}"}}}"#,
        r#"{"from":"t","to":"a"}"#,
    );
    json::set(&mut definition, "enabled", Value::Bool(true)).unwrap();
    let input = value(
        r#"{"appId":"stulp","id":"time_at","kind":"trigger","tokens":{"counter":18446744073709551615},"state":{"event":"wake"}}"#,
    );
    let mut run = Run::triggered(&definition, &input, 0, NOW)
        .unwrap()
        .unwrap();
    let Effect::Call { params, .. } = run.advance(&store(), 0).unwrap() else {
        panic!("missing automatic action")
    };
    assert_eq!(
        json::uint(json::get(&params, "args").unwrap(), "exact"),
        u64::MAX
    );
    assert_eq!(
        json::text(json::get(&params, "args").unwrap(), "text"),
        "event wake"
    );
    json::set(&mut definition, "enabled", Value::Bool(false)).unwrap();
    assert!(
        Run::triggered(&definition, &input, 0, NOW)
            .unwrap()
            .is_none()
    );

    let definition = value(
        r#"{"id":"f","name":"Threshold","enabled":true,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"capability.measure_temperature.rose_above","args":{"device":{"$device":"d"},"value":20}}},{"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"warm"}}}],"edges":[{"from":"t","to":"a"}]}"#,
    );
    for (device, old, current, matched) in [
        ("d", 19, 21, true),
        ("other", 19, 21, false),
        ("d", 21, 22, false),
        ("d", 19, 20, false),
    ] {
        let input = value(&format!(
            r#"{{"appId":"stulp","id":"capability.measure_temperature.rose_above","kind":"trigger","state":{{"deviceId":"{device}","oldValue":{old},"value":{current}}}}}"#
        ));
        assert_eq!(
            Run::triggered(&definition, &input, 0, NOW)
                .unwrap()
                .is_some(),
            matched
        );
    }
}
#[test]
fn automatic_plugin_filter_is_awaited_before_actions() {
    let definition = value(
        r#"{"id":"f","name":"Filtered","enabled":true,"nodes":[{"id":"t","step":{"appId":"plugin","cardType":"device-trigger","cardId":"motion","args":{"device":{"$device":"d"},"threshold":"{{value}}"}}},{"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"motion"}}}],"edges":[{"from":"t","to":"a"}]}"#,
    );
    let input = value(
        r#"{"appId":"plugin","kind":"device-trigger","id":"motion","tokens":{"value":42},"state":{"deviceId":"d"}}"#,
    );
    for matched in [false, true] {
        let mut run = Run::triggered(&definition, &input, 0, NOW)
            .unwrap()
            .unwrap();
        let Effect::Call { params, .. } = run.advance(&store(), 0).unwrap() else {
            panic!("filter callback missing")
        };
        assert_eq!(json::text(&params, "kind"), "device-trigger");
        assert_eq!(
            json::uint(json::get(&params, "args").unwrap(), "threshold"),
            42
        );
        assert!(matches!(run.advance(&store(), 1).unwrap(), Effect::Waiting));
        run.complete(Value::Bool(matched), 2).unwrap();
        assert_eq!(
            matches!(run.advance(&store(), 3).unwrap(), Effect::Notification(_)),
            matched
        );
    }
}

#[test]
fn measured_tokens_are_readable_in_text_and_canonical_in_numeric_arguments() {
    let mut store = Store::open(
        br#"{"version":2,"apps":[{"id":"plugin","enabled":true}]}"#,
        Memory,
    )
    .unwrap();
    store.announce("plugin", value(r#"{"id":"plugin","sdk":3,"version":"1","flow":{"actions":[{"id":"send","args":[{"name":"threshold","type":"number"},{"name":"text","type":"text"},{"name":"standalone","type":"text"}]}]}}"#)).unwrap();
    store
        .system(value(r#"{"units":{"temperature":"°F"}}"#))
        .unwrap();
    let definition = value(
        r#"{"id":"f","name":"Temperature","enabled":true,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"capability.measure_temperature.changed"}},{"id":"a","step":{"appId":"plugin","cardType":"action","cardId":"send","args":{"threshold":"{{value}}","text":"Room {{tokens.value}} / before {{state.oldValue}}","standalone":"{{value}}"},"state":{"raw":"{{value}}"}}}],"edges":[{"from":"t","to":"a"}]}"#,
    );
    let event = value(
        r#"{"appId":"stulp","kind":"trigger","id":"capability.measure_temperature.changed","tokens":{"value":21.5},"state":{"value":21.5,"oldValue":20}}"#,
    );
    let mut run = Run::triggered(&definition, &event, 0, NOW)
        .unwrap()
        .unwrap();
    let Effect::Call { params, .. } = run.advance(&store, 0).unwrap() else {
        panic!("missing action")
    };
    let args = json::get(&params, "args").unwrap();
    assert!(json::equal(
        json::get(args, "threshold").unwrap(),
        &value("21.5")
    ));
    assert_eq!(json::text(args, "text"), "Room 70.7 °F / before 68 °F");
    assert_eq!(json::text(args, "standalone"), "70.7 °F");
    assert!(json::equal(
        json::get(json::get(&params, "state").unwrap(), "raw").unwrap(),
        &value("21.5")
    ));
}

#[test]
fn failed_action_stops_only_its_branch_and_sibling_actions_still_run() {
    // Eén trigger naar drie acties; de eerste (Spotify) faalt, haar opvolger niet uitvoeren.
    let definition = flow(
        r#"{"id":"a","step":{"appId":"com.stulp.spotify","cardType":"action","cardId":"play_playlist"}},{"id":"b","step":{"appId":"lamp","cardType":"action","cardId":"on"}},{"id":"c","step":{"appId":"stulp","cardType":"action","cardId":"capability.onoff.turn_on","args":{"device":{"$device":"missing"}}}},{"id":"d","step":{"appId":"plugin","cardType":"action","cardId":"after_spotify"}},{"id":"e","step":{"appId":"plugin","cardType":"action","cardId":"last"}}"#,
        r#"{"from":"t","to":"a"},{"from":"t","to":"b"},{"from":"t","to":"c"},{"from":"t","to":"e"},{"from":"a","to":"d"}"#,
    );
    let store = store();
    let mut run = Run::manual(&definition, 0, NOW).unwrap();
    let Effect::Call { params, .. } = run.advance(&store, 0).unwrap() else {
        panic!("missing spotify action");
    };
    assert_eq!(json::text(&params, "id"), "play_playlist");
    run.fail("app callback timed out").unwrap();
    let Effect::Call { app, params, .. } = run.advance(&store, 1).unwrap() else {
        panic!("sibling action after a failed action did not run");
    };
    assert_eq!((app.as_str(), json::text(&params, "id")), ("lamp", "on"));
    run.complete(Value::Null, 2).unwrap();
    // Een built-in actie die al bij het versturen faalt, stopt de run evenmin.
    assert!(matches!(run.advance(&store, 3).unwrap(), Effect::Waiting));
    let Effect::Call { params, .. } = run.advance(&store, 4).unwrap() else {
        panic!("last sibling action did not run");
    };
    assert_eq!(json::text(&params, "id"), "last");
    run.complete(Value::Null, 5).unwrap();
    assert!(matches!(run.advance(&store, 6).unwrap(), Effect::Finished));
    let result = run.result().unwrap();
    assert!(!json::boolean(&result, "success"));
    assert_eq!(json::text(&result, "error"), "app callback timed out");
    let actions = json::array(&result, "actions");
    let ids: Vec<_> = actions.iter().map(|a| json::text(a, "cardId")).collect();
    assert_eq!(
        ids,
        ["play_playlist", "on", "capability.onoff.turn_on", "last"]
    );
    assert_eq!(json::text(&actions[0], "error"), "app callback timed out");
    assert!(json::get(&actions[1], "error").is_none());
    assert!(!json::text(&actions[2], "error").is_empty());
}

#[test]
fn failed_condition_callback_still_stops_the_run() {
    let definition = flow(
        r#"{"id":"a","step":{"appId":"plugin","cardType":"condition","cardId":"ready"}},{"id":"b","step":{"appId":"plugin","cardType":"action","cardId":"send"}}"#,
        r#"{"from":"t","to":"a"},{"from":"t","to":"b"}"#,
    );
    let mut run = Run::manual(&definition, 0, NOW).unwrap();
    assert!(matches!(
        run.advance(&store(), 0).unwrap(),
        Effect::Call { .. }
    ));
    run.fail("app callback failed").unwrap();
    assert!(matches!(
        run.advance(&store(), 1).unwrap(),
        Effect::Finished
    ));
    let result = run.result().unwrap();
    assert!(json::array(&result, "actions").is_empty());
    assert_eq!(json::array(&result, "conditions").len(), 1);
}
