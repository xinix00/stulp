#![allow(clippy::unwrap_used, clippy::panic)]
use super::*;
#[path = "../../../tests/wire.rs"]
mod wire;
use wire::{Wire, value};
fn handle(p: &mut Unifi, c: &mut Client<Wire>, method: &str, params: &str) -> Result<Value> {
    hostnet::block_on(p.handle(c, method, &value(params)))
}
fn setup(driver: &str, data: &str, store: &str, settings: &str) -> (Unifi, Client<Wire>) {
    let mut p = Unifi::default();
    let devices = alloc::format!(
        r#"[{{"id":"d","appId":"com.stulp.unifi","driverId":"{driver}","name":"test","data":{data},"store":{store},"settings":{settings},"capabilities":["onoff","dim","volume_set","alarm_motion","alarm_contact","alarm_tamper","measure_temperature","measure_humidity","measure_luminance","measure_battery","alarm_battery"]}}]"#
    );
    let mut c = Wire::client("com.stulp.unifi", p.manifest(), &devices, "{}");
    hostnet::block_on(c.setting("host", json::string("192.0.2.1").unwrap())).unwrap();
    hostnet::block_on(c.setting("apiKey", json::string("synthetic").unwrap())).unwrap();
    handle(&mut p, &mut c, "app.init", "{}").unwrap();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"d"}"#).unwrap();
    (p, c)
}
#[test]
fn sparse_sensor_updates_preserve_missing_values_and_accept_zero() {
    let patch = observe::values(
        "sensor",
        "",
        &value(
            r#"{"stats":{"temperature":{"value":0},"humidity":{"value":null}},"isOpened":false}"#,
        ),
        false,
    )
    .unwrap();
    assert_eq!(number(field(&patch, "measure_temperature")), Some(0.));
    assert_eq!(field(&patch, "alarm_contact").as_bool(), Some(false));
    assert!(json::get(&patch, "measure_humidity").is_none());
    assert!(json::get(&patch, "alarm_battery").is_none());
    assert!(json::get(&patch, "alarm_motion").is_none());
    assert_eq!(observe::audio("alrmGlassBreak"), "glass_break");
    assert_eq!(observe::audio("newKind"), "newKind");
}
#[test]
fn light_writes_minimal_body_and_does_not_predict_state() {
    let (mut p, c) = setup("light", r#"{"id":"light/1"}"#, "{}", "{}");
    let mut w = c.into_transport();
    w.reply(200, "{}", false);
    w.delay = 6000;
    let mut c = w.reconnect("com.stulp.unifi", p.manifest());
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"d","capability":"dim","value":0.5}"#,
    )
    .unwrap();
    assert!(json::get(field(c.state().device("d").unwrap(), "state"), "dim").is_none());
    let w = c.into_transport();
    assert_eq!(w.http.len(), 1);
    assert_eq!(w.http[0].method, "PATCH");
    assert!(w.http[0].url.ends_with("lights/light%2F1"));
    assert!(w.http[0].device_certificate);
    assert_eq!(
        json::to_string(&json::parse(&w.http[0].body).unwrap()).unwrap(),
        r#"{"lightDeviceSettings":{"ledLevel":4}}"#
    );
    assert!(
        w.sent
            .iter()
            .any(|v| json::text(v, "m") == "$appproto.ping")
    );
}
#[test]
fn relay_read_modify_write_preserves_other_outputs_and_never_retries() {
    let (mut p, c) = setup("relay", r#"{"id":"module","output":"a"}"#, "{}", "{}");
    let mut w = c.into_transport();
    w.reply(
        200,
        r#"{"outputs":[{"id":"a","state":"off","name":"Door"},{"id":"b","state":"on"}]}"#,
        false,
    );
    w.reply(503, "{}", false);
    let mut c = w.reconnect("com.stulp.unifi", p.manifest());
    assert!(
        handle(
            &mut p,
            &mut c,
            "capability.invoke",
            r#"{"deviceId":"d","capability":"onoff","value":true}"#
        )
        .is_err()
    );
    let w = c.into_transport();
    assert_eq!(w.http.len(), 2);
    assert_eq!(w.http[0].method, "GET");
    assert_eq!(w.http[1].method, "PATCH");
    assert_eq!(
        json::to_string(&json::parse(&w.http[1].body).unwrap()).unwrap(),
        r#"{"outputs":[{"id":"a","state":"on"},{"id":"b","state":"on"}]}"#
    );
}
#[test]
fn chime_unmute_restores_durable_volume_only_after_success() {
    let (mut p, c) = setup("chime", r#"{"id":"chime"}"#, r#"{"lastVolume":37}"#, "{}");
    let mut w = c.into_transport();
    w.reply(200, "{}", false);
    let mut c = w.reconnect("com.stulp.unifi", p.manifest());
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"d","capability":"onoff","value":true}"#,
    )
    .unwrap();
    assert_eq!(
        number(field(
            field(c.state().device("d").unwrap(), "state"),
            "volume_set"
        )),
        Some(0.37)
    );
    let w = c.into_transport();
    assert_eq!(
        json::to_string(&json::parse(&w.http[0].body).unwrap()).unwrap(),
        r#"{"volume":37}"#
    );
}
#[test]
fn pairing_splits_module_outputs_and_rejects_unowned_control() {
    let list=protect::pairs("relay",&value(r#"[{"id":"r","name":"Garage","modelKey":"relay","outputs":[{"id":"1","name":"Door"},{"id":"2"}]}]"#)).unwrap();
    let list = list.as_array().unwrap();
    assert_eq!(list.len(), 2);
    assert_eq!(json::text(&list[1], "name"), "Garage 2");
    assert_eq!(json::text(field(&list[0], "data"), "output"), "1");
    let (mut p, mut c) = setup("light", r#"{"id":"l"}"#, "{}", "{}");
    assert!(
        handle(
            &mut p,
            &mut c,
            "capability.invoke",
            r#"{"deviceId":"foreign","capability":"onoff","value":true}"#
        )
        .is_err()
    );
    assert!(c.into_transport().http.is_empty());
}

#[test]
fn camera_setting_rejection_does_not_persist_or_retry_the_write() {
    let (mut p, c) = setup(
        "camera",
        r#"{"id":"camera/1"}"#,
        "{}",
        r#"{"mic_volume":10}"#,
    );
    let mut w = c.into_transport();
    w.reply(503, "{}", false);
    let mut c = w.reconnect("com.stulp.unifi", p.manifest());
    assert!(hostnet::block_on(p.settings(&mut c, "d", &value(r#"{"mic_volume":101}"#))).is_err());
    assert!(hostnet::block_on(p.settings(&mut c, "d", &value(r#"{"mic_volume":30}"#))).is_err());
    assert_eq!(
        json::uint(
            field(c.state().device("d").unwrap(), "settings"),
            "mic_volume"
        ),
        10
    );
    let w = c.into_transport();
    assert_eq!(w.http.len(), 1);
    assert_eq!(w.http[0].method, "PATCH");
    assert_eq!(
        json::uint(&json::parse(&w.http[0].body).unwrap(), "micVolume"),
        30
    );
}
