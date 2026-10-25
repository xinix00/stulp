//! Alleen een nagemaakte pushdienst; er wordt geen bericht naar een echt toestel gestuurd.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
use stulp_core::json::{self, Value};
use stulp_notify::Notify;
use stulp_sdk::{Client, Plugin};
use wire::{Wire, value};
// Een wegwerpsleutel, alleen voor deze test (dezelfde als in stulp-apns).
const P8: &str = "-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgzRteCB4XmtDpgVBO
7K8buoOvibGr2+nL9G64fzpBVnihRANCAASC0GkbvAr1imZsQi1l8MdMhkfWH+Rq
Vhiy3U8AKNG9+YkzASqCdH3vnXFZuzueTR7iBQkAMJDvuHZKUskavti3
-----END PRIVATE KEY-----
";
fn wire(store: &str) -> Wire {
    let d = format!(
        r#"[{{"id":"phone","appId":"com.stulp.notify","driverId":"phone","name":"Test","data":{{"kind":"app"}},"store":{store},"capabilities":["onoff"]}}]"#
    );
    let mut c = Wire::client("com.stulp.notify", Notify::default().manifest(), &d, "null");
    for (k, v) in [("apnsKey", P8), ("apnsKeyId", "ABCDEFGHIJ")] {
        stulp_platform::host::block_on(c.setting(k, json::string(v).unwrap())).unwrap();
    }
    c.into_transport()
}
fn handle(p: &mut Notify, c: &mut Client<Wire>, method: &str, s: &str) -> stulp_sdk::Result<Value> {
    stulp_platform::host::block_on(p.handle(c, method, &value(s)))
}
const SEND: &str = r#"{"kind":"action","id":"send","args":{"device":{"$device":"phone"},"message":"Test","level":"time-sensitive"}}"#;
#[test]
fn mute_stops_delivery_and_an_unregistered_app_is_unavailable() {
    let mut w = wire(r#"{"apns.token":"abcdef0123","apns.environment":"development"}"#);
    w.reply(410, r#"{"reason":"Unregistered"}"#, false);
    let mut c = w.reconnect("com.stulp.notify", Notify::default().manifest());
    let mut p = Notify::default();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"phone"}"#).unwrap();
    assert!(json::boolean(
        c.state().device("phone").unwrap(),
        "available"
    ));
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"phone","capability":"onoff","value":false}"#,
    )
    .unwrap();
    assert!(handle(&mut p, &mut c, "flow.run", SEND).is_err());
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"phone","capability":"onoff","value":true}"#,
    )
    .unwrap();
    let error = handle(&mut p, &mut c, "flow.run", SEND).unwrap_err();
    let text = stulp_sdk::message(&error).unwrap();
    assert!(text.contains("Unregistered"), "{text}");
    let d = c.state().device("phone").unwrap();
    assert!(!json::boolean(d, "available"));
    assert!(json::text(d, "unavailableMessage").contains("opnieuw"));
    let w = c.into_transport();
    assert_eq!(w.http.len(), 1);
    let r = &w.http[0];
    assert!(r.http2);
    assert_eq!(
        r.url,
        "https://api.sandbox.push.apple.com/3/device/abcdef0123"
    );
    let header = |name: &str| {
        r.headers
            .iter()
            .find(|(k, _)| k == name)
            .map(|(_, v)| v.as_str())
    };
    assert_eq!(header("apns-topic"), Some("software.haas.stulp"));
    assert!(header("authorization").unwrap().starts_with("bearer "));
    let body = value(core::str::from_utf8(&r.body).unwrap());
    assert_eq!(
        json::text(json::get(&body, "aps").unwrap(), "interruption-level"),
        "time-sensitive"
    );
}
#[test]
fn a_phone_without_the_app_gets_no_notification_and_no_pairing() {
    let w = wire("{}");
    let mut c = w.reconnect("com.stulp.notify", Notify::default().manifest());
    let mut p = Notify::default();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"phone"}"#).unwrap();
    let error = handle(&mut p, &mut c, "flow.run", SEND).unwrap_err();
    assert!(stulp_sdk::message(&error).unwrap().contains("Stulp-app"));
    assert!(
        handle(
            &mut p,
            &mut c,
            "pair.start",
            r#"{"sessionId":"p","driverId":"phone"}"#,
        )
        .is_err()
    );
    assert!(c.into_transport().http.is_empty());
}
