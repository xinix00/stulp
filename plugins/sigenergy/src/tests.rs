#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
extern crate std;
#[path = "../../../tests/wire.rs"]
pub(super) mod wire;
use super::*;
use alloc::{format, vec};
use wire::{Wire, value};
const ID: &str = "com.stulp.sigenergy";
#[test]
fn full_scan_reaches_last_unit_after_ten_minutes_and_keeps_heartbeats() {
    let mut p = Sigenergy::default();
    let mut wire = Wire::client(ID, p.manifest(), "[]", "{}").into_transport();
    wire.tcp_delay = 500;
    wire.tcp_bodies.push_back(vec![247, 3, 4, 0, 0, 0, 1]);
    for unit in 1..=255 {
        for driver in ["plant", "energy", "inverter", "battery", "evaccharger"] {
            if unit == 255 && driver == "inverter" {
                wire.tcp_bodies.push_back(vec![unit, 3, 2, 0, 2]);
            } else {
                wire.tcp_bodies.push_back(vec![unit, 0x83, 2]);
            }
        }
    }
    let mut c = wire.reconnect(ID, p.manifest());
    handle(&mut p, &mut c, "app.init", "{}").unwrap();
    let result = handle(
        &mut p,
        &mut c,
        "api.invoke",
        r#"{"handler":"test","body":{"host":"127.0.0.1","units":"1-255"}}"#,
    )
    .unwrap();
    assert!(c.now() > 600_000 && c.now() < 900_000);
    assert_eq!(json::uint(&result, "units"), 255);
    let found = json::array(&result, "found");
    assert_eq!(found.len(), 1);
    assert_eq!(json::uint(&found[0], "unit"), 255);
    let wire = c.into_transport();
    assert_eq!(wire.tcp.len(), 1276);
    assert!(wire.tcp_bodies.is_empty());
    assert!(
        wire.sent
            .iter()
            .filter(|v| json::text(v, "m") == "$appproto.ping")
            .count()
            > 100
    );
}
const GATEWAY: &str = r#"[{"id":"gateway","appId":"com.stulp.sigenergy","driverId":"gateway","data":{"stationId":"12345"},"capabilities":["off_grid","grid_status"]}]"#;
const STATE: &str = r#"{"version":1,"region":"eu","username":"owner@example.test","tokens":{"accessToken":"access","refreshToken":"refresh","expiresAt":"2030-01-01T00:00:00Z"}}"#;
fn handle(p: &mut Sigenergy, c: &mut Client<Wire>, m: &str, s: &str) -> Result<Value> {
    hostnet::block_on(p.handle(c, m, &value(s)))
}
fn step(c: &mut Client<Wire>) {
    for _ in 0..15 {
        hostnet::block_on(c.call("$appproto.ping", &Value::Null)).unwrap();
    }
}
fn gateway_wire() -> Wire {
    Wire::client(ID, Sigenergy::default().manifest(), GATEWAY, STATE).into_transport()
}
fn ready(w: Wire) -> (Sigenergy, Client<Wire>) {
    let mut p = Sigenergy::default();
    let mut c = w.reconnect(ID, p.manifest());
    handle(&mut p, &mut c, "app.init", "{}").unwrap();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"gateway"}"#).unwrap();
    (p, c)
}
fn settings(w: &mut Wire) {
    w.reply(
        200,
        r#"{"code":0,"data":{"stationId":12345,"offGridEnable":true}}"#,
        false,
    );
}
fn status(w: &mut Wire, grid: i64, button: bool) {
    w.reply(200,&format!(r#"{{"code":0,"data":{{"onOffGridStatus":{grid},"manualOffGridStatus":0,"showButton":{button}}}}}"#),false);
}
#[test]
fn password_encoding_matches_original_go_vector() {
    assert_eq!(
        cloud::password("correct horse").unwrap(),
        "2XgqjWOYS8wesGPH8+9zTA=="
    );
    assert!(cloud::region("http://example.test").is_err());
}
#[test]
fn gateway_revalidates_and_posts_once_then_reports_actual_state() {
    let mut w = gateway_wire();
    settings(&mut w);
    status(&mut w, 0, true);
    settings(&mut w);
    status(&mut w, 0, true);
    w.reply(503, r#"{"code":123,"msg":"uncertain server error"}"#, false);
    status(&mut w, 2, true);
    let (mut p, mut c) = ready(w);
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"gateway","capability":"off_grid","value":true}"#,
    )
    .unwrap();
    assert!(
        field(
            field(c.state().device("gateway").unwrap(), "state"),
            "off_grid"
        )
        .is_null()
    );
    hostnet::block_on(p.tick(&mut c)).unwrap();
    step(&mut c);
    hostnet::block_on(p.tick(&mut c)).unwrap();
    assert!(json::boolean(
        field(c.state().device("gateway").unwrap(), "state"),
        "off_grid"
    ));
    step(&mut c);
    hostnet::block_on(p.tick(&mut c)).unwrap();
    let w = c.into_transport();
    let posts: Vec<_> = w.http.iter().filter(|r| r.method == "POST").collect();
    assert_eq!(posts.len(), 1);
    assert_eq!(
        value(core::str::from_utf8(&posts[0].body).unwrap())
            .as_object()
            .unwrap()
            .len(),
        2
    );
    assert_eq!(
        json::uint(&json::parse(&posts[0].body).unwrap(), "onGridState"),
        1
    );
}
#[test]
fn withdrawn_button_and_disconnect_invalidate_prepared_command() {
    let mut w = gateway_wire();
    settings(&mut w);
    status(&mut w, 0, true);
    settings(&mut w);
    status(&mut w, 0, false);
    let (mut p, mut c) = ready(w);
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"gateway","capability":"off_grid","value":true}"#,
    )
    .unwrap();
    hostnet::block_on(p.tick(&mut c)).unwrap();
    assert!(c.into_transport().http.iter().all(|r| r.method != "POST"));
    let mut w = gateway_wire();
    settings(&mut w);
    status(&mut w, 0, true);
    let (mut p, mut c) = ready(w);
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"gateway","capability":"off_grid","value":true}"#,
    )
    .unwrap();
    handle(
        &mut p,
        &mut c,
        "api.invoke",
        r#"{"handler":"cloud_disconnect"}"#,
    )
    .unwrap();
    hostnet::block_on(p.tick(&mut c)).unwrap();
    assert!(c.into_transport().http.iter().all(|r| r.method != "POST"));
}
#[test]
fn refresh_is_persisted_before_authenticated_reads_and_failure_stops_them() {
    let state = STATE.replace("2030-01-01T00:00:00Z", "2020-01-01T00:00:00Z");
    let mut w = Wire::client(ID, Sigenergy::default().manifest(), "[]", &state).into_transport();
    w.reply(
        200,
        r#"{"code":0,"data":{"access_token":"new","refresh_token":"rotated","expires_in":"180"}}"#,
        false,
    );
    w.reply(200, r#"{"code":0,"data":{"stationList":[]}}"#, false);
    let mut p = Sigenergy::default();
    let mut c = w.reconnect(ID, p.manifest());
    handle(&mut p, &mut c, "app.init", "{}").unwrap();
    handle(&mut p, &mut c, "api.invoke", r#"{"handler":"cloud_check"}"#).unwrap();
    assert_eq!(
        json::text(
            field(field(c.state().root(), "appState"), "tokens"),
            "refreshToken"
        ),
        "rotated"
    );
    let w = c.into_transport();
    assert!(
        w.http[1]
            .headers
            .iter()
            .any(|(k, v)| k == "Authorization" && v == "Bearer new")
    );
    let mut w = Wire::client(ID, Sigenergy::default().manifest(), "[]", &state).into_transport();
    w.fail_state = true;
    w.reply(
        200,
        r#"{"code":0,"data":{"access_token":"new","expires_in":180}}"#,
        false,
    );
    let mut p = Sigenergy::default();
    let mut c = w.reconnect(ID, p.manifest());
    handle(&mut p, &mut c, "app.init", "{}").unwrap();
    assert!(handle(&mut p, &mut c, "api.invoke", r#"{"handler":"cloud_check"}"#).is_err());
    assert_eq!(c.into_transport().http.len(), 1);
}
#[test]
fn charger_writes_function_six_and_never_publishes_predicted_charging() {
    let device = r#"[{"id":"charger","appId":"com.stulp.sigenergy","driverId":"evaccharger","settings":{"modbus_unitId":2},"capabilities":["evcharger_charging","evcharger_charging_state","measure_power","meter_power.charged"]}]"#;
    let mut w = Wire::client(ID, Sigenergy::default().manifest(), device, "{}").into_transport();
    w.tcp_bodies.push_back(vec![2, 6, 0xa4, 0x10, 0, 0]);
    w.tcp_bodies.push_back(vec![2, 6, 0xa4, 0x10, 0, 1]);
    let mut p = Sigenergy::default();
    let mut c = w.reconnect(ID, p.manifest());
    hostnet::block_on(c.setting("host", json::string("192.0.2.44").unwrap())).unwrap();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"charger"}"#).unwrap();
    for on in [true, false] {
        handle(
            &mut p,
            &mut c,
            "capability.invoke",
            &format!(r#"{{"deviceId":"charger","capability":"evcharger_charging","value":{on}}}"#),
        )
        .unwrap();
    }
    assert!(
        field(
            field(c.state().device("charger").unwrap(), "state"),
            "evcharger_charging"
        )
        .is_null()
    );
    let w = c.into_transport();
    assert_eq!(w.tcp.len(), 2);
    assert_eq!(&w.tcp[0].frame[6..], &[2, 6, 0xa4, 0x10, 0, 0]);
    assert_eq!(&w.tcp[1].frame[6..], &[2, 6, 0xa4, 0x10, 0, 1]);
    assert_eq!(w.tcp[0].generation, w.tcp[1].generation);
}
#[test]
fn sparse_scan_and_exact_charger_selection_preserve_ranges() {
    assert_eq!(scan::units("1-3,8,2,247").unwrap(), vec![1, 2, 3, 8, 247]);
    let (a, b, exact) = scan::charger_plan("1-3,247", "").unwrap();
    assert_eq!(a, vec![1, 2, 3]);
    assert!(!exact);
    assert_eq!(b.len(), 243);
    assert_eq!(b[0], 4);
    assert_eq!(b[242], 246);
    let (a, b, exact) = scan::charger_plan("1-32,247", "199").unwrap();
    assert_eq!(a, vec![199]);
    assert!(b.is_empty() && exact);
    for invalid in ["0", "256", "4-1", "1-999999999", "wat"] {
        assert!(scan::units(invalid).is_err());
    }
}
