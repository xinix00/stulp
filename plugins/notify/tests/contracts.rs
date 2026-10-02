//! Alleen nagemaakte pushdiensten; er wordt geen bericht naar een echt toestel gestuurd.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
use stulp_core::json::{self, Value};
use stulp_notify::Notify;
use stulp_protocol::token::base64;
use stulp_sdk::{Client, Plugin};
use wire::{Wire, value};
fn wire() -> Wire {
    let public = base64(&stulp_webpush::public(&[3; 32]).unwrap()).unwrap();
    let auth = base64(&[4; 16]).unwrap();
    let d = format!(
        r#"[{{"id":"phone","appId":"com.stulp.notify","driverId":"phone","name":"Test","data":{{"endpoint":"https://push.example/test","p256dh":"{public}","auth":"{auth}"}},"capabilities":["onoff"]}}]"#
    );
    Wire::client("com.stulp.notify", Notify::default().manifest(), &d, "null").into_transport()
}
fn handle(p: &mut Notify, c: &mut Client<Wire>, method: &str, s: &str) -> stulp_sdk::Result<Value> {
    hostnet::block_on(p.handle(c, method, &value(s)))
}
#[test]
fn mute_stops_delivery_and_expired_subscription_is_unavailable() {
    let mut w = wire();
    w.reply(410, "", false);
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
    let call =
        r#"{"kind":"action","id":"send","args":{"device":{"$device":"phone"},"message":"Test"}}"#;
    assert!(handle(&mut p, &mut c, "flow.run", call).is_err());
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"phone","capability":"onoff","value":true}"#,
    )
    .unwrap();
    assert!(handle(&mut p, &mut c, "flow.run", call).is_err());
    let d = c.state().device("phone").unwrap();
    assert!(!json::boolean(d, "available"));
    assert!(json::text(d, "unavailableMessage").contains("opnieuw"));
    let w = c.into_transport();
    assert_eq!(w.http.len(), 1);
    assert!(
        w.http[0]
            .headers
            .iter()
            .any(|(k, v)| k == "TTL" && v == "600")
    );
}
#[test]
fn pairing_reuses_persistent_vapid_identity() {
    let w = wire();
    let mut c = w.reconnect("com.stulp.notify", Notify::default().manifest());
    let mut p = Notify::default();
    handle(
        &mut p,
        &mut c,
        "pair.start",
        r#"{"sessionId":"p","driverId":"phone"}"#,
    )
    .unwrap();
    let a = handle(
        &mut p,
        &mut c,
        "pair.emit",
        r#"{"sessionId":"p","event":"publicKey"}"#,
    )
    .unwrap();
    let w = c.into_transport();
    let mut c = w.reconnect("com.stulp.notify", Notify::default().manifest());
    let mut p = Notify::default();
    handle(
        &mut p,
        &mut c,
        "pair.start",
        r#"{"sessionId":"p","driverId":"phone"}"#,
    )
    .unwrap();
    let b = handle(
        &mut p,
        &mut c,
        "pair.emit",
        r#"{"sessionId":"p","event":"publicKey"}"#,
    )
    .unwrap();
    assert_eq!(json::text(&a, "publicKey"), json::text(&b, "publicKey"));
    assert!(c.into_transport().http.is_empty());
}
