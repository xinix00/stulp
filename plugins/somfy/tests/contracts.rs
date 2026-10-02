//! Contrasten uit de Go-contracttests, met een nagebouwde TaHoma-cloud.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Plugin};
use stulp_somfy::{Somfy, covering};
use wire::{Wire, value};
const DEVICE: &str = r#"[{"id":"blind","appId":"com.stulp.somfy","driverId":"io_roller_shutter","data":{"deviceURL":"io://test/1","label":"Living"},"store":{"executionId":"old/exec"},"capabilities":["windowcoverings_set","windowcoverings_state"]}]"#;
fn client() -> Client<Wire> {
    Wire::client(
        "com.stulp.somfy",
        Somfy::default().manifest(),
        DEVICE,
        r#"{"username":"u+test","password":"p&test"}"#,
    )
}
fn handle(
    p: &mut Somfy,
    c: &mut Client<Wire>,
    method: &str,
    params: &str,
) -> stulp_sdk::Result<Value> {
    hostnet::block_on(p.handle(c, method, &value(params)))
}
fn ready() -> (Somfy, Client<Wire>) {
    let mut p = Somfy::default();
    let mut c = client();
    handle(&mut p, &mut c, "app.init", "{}").unwrap();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"blind"}"#).unwrap();
    (p, c)
}
#[test]
fn direction_and_position_match_original() {
    assert_eq!(covering::position(40.0), 0.6);
    assert_eq!(covering::closure(0.6), 40);
    assert_eq!(covering::closure(-2.0), 100);
    assert_eq!(covering::closure(2.0), 0);
    assert_eq!(
        covering::command("io_horizontal_awning", "up").unwrap(),
        "close"
    );
    assert_eq!(
        covering::command("io_roller_shutter", "up").unwrap(),
        "open"
    );
}
#[test]
fn login_cookie_reauthentication_and_command_do_not_invent_position() {
    let (p, c) = ready();
    let mut w = c.into_transport();
    w.reply(200, r#"{"success":true}"#, true);
    w.reply(401, "{}", false);
    w.reply(200, r#"{"success":true}"#, true);
    w.reply(200, r#"{"execId":"new-exec"}"#, false);
    w.delay = 6000;
    let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
    let mut p = p;
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"blind","capability":"windowcoverings_set","value":0.6}"#,
    )
    .unwrap();
    let d = c.state().device("blind").unwrap();
    assert_eq!(
        json::text(json::get(d, "store").unwrap(), "executionId"),
        "new-exec"
    );
    assert!(json::get(json::get(d, "state").unwrap(), "windowcoverings_set").is_none());
    let w = c.into_transport();
    assert_eq!(w.http.len(), 4);
    assert!(
        String::from_utf8_lossy(&w.http[0].body).contains("userId=u%2Btest&userPassword=p%26test")
    );
    assert!(
        w.http[1]
            .headers
            .iter()
            .any(|(k, v)| k == "Cookie" && v == "JSESSIONID=session")
    );
    let sent = json::parse(&w.http[3].body).unwrap();
    let action = &json::array(&sent, "actions")[0];
    assert_eq!(json::text(action, "deviceURL"), "io://test/1");
    let command = &json::array(action, "commands")[0];
    assert_eq!(json::array(command, "parameters")[0].as_u64(), Some(40));
    assert!(
        w.sent
            .iter()
            .filter(|v| json::text(v, "m") == "$appproto.ping")
            .count()
            >= 3
    );
}
#[test]
fn rejected_login_never_sends_commands() {
    let (mut p, c) = ready();
    let mut w = c.into_transport();
    w.reply(200, r#"{"success":false}"#, false);
    let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
    assert!(
        handle(
            &mut p,
            &mut c,
            "capability.invoke",
            r#"{"deviceId":"blind","capability":"windowcoverings_state","value":"up"}"#
        )
        .is_err()
    );
    assert_eq!(c.into_transport().http.len(), 1);
}
#[test]
fn persistence_failure_preserves_private_account() {
    let (mut p, c) = ready();
    let mut w = c.into_transport();
    w.fail_state = true;
    let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
    assert!(
        handle(
            &mut p,
            &mut c,
            "api.invoke",
            r#"{"handler":"save","body":{"username":"new","password":"new"}}"#
        )
        .is_err()
    );
    let status = handle(&mut p, &mut c, "api.invoke", r#"{"handler":"status"}"#).unwrap();
    assert_eq!(json::text(&status, "username"), "u+test");
    assert!(json::get(&status, "password").is_none());
    assert!(c.into_transport().http.is_empty());
}
#[test]
fn cancel_is_scoped_to_durable_execution() {
    let (mut p, c) = ready();
    let mut w = c.into_transport();
    w.reply(200, r#"{"success":true}"#, true);
    w.reply(204, "", false);
    let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"blind","capability":"windowcoverings_state","value":"idle"}"#,
    )
    .unwrap();
    let d = c.state().device("blind").unwrap();
    assert_eq!(
        json::text(json::get(d, "store").unwrap(), "executionId"),
        ""
    );
    assert!(
        handle(
            &mut p,
            &mut c,
            "capability.invoke",
            r#"{"deviceId":"blind","capability":"windowcoverings_state","value":"idle"}"#
        )
        .is_err()
    );
    let w = c.into_transport();
    assert_eq!(w.http.len(), 2);
    assert!(w.http[1].url.ends_with("/exec/current/setup/old%2Fexec"));
    assert_eq!(w.http[1].method, "DELETE");
}

fn advance(c: &mut Client<Wire>, milliseconds: u64) {
    let until = c.now() + milliseconds;
    while c.now() < until {
        hostnet::block_on(c.idle()).unwrap();
    }
}

#[test]
fn cloud_throttling_waits_without_relogin_and_recovers_automatically() {
    for status in [401, 429] {
        let (mut p, c) = ready();
        let mut w = c.into_transport();
        w.reply(status, r#"{"errorCode":"AUTHENTICATION_ERROR","error":"Too many requests, try again later : login with private@example.test"}"#, false);
        w.reply(200, r#"{"success":true}"#, true);
        w.reply(200, r#"{"devices":[]}"#, false);
        let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
        advance(&mut c, 2_000);
        hostnet::block_on(p.tick(&mut c)).unwrap();
        let s = handle(&mut p, &mut c, "api.invoke", r#"{"handler":"status"}"#).unwrap();
        assert!(json::text(&s, "error").contains("beperkt tijdelijk"));
        assert!(
            !json::to_string(&s)
                .unwrap()
                .contains("private@example.test")
        );
        assert!(json::uint(&s, "retryAfter") >= 899);
        assert_eq!(
            json::get(&s, "connected").and_then(Value::as_bool),
            Some(false)
        );
        assert!(
            json::text(c.state().device("blind").unwrap(), "unavailableMessage")
                .contains("beperkt tijdelijk")
        );
        for _ in 0..80 {
            advance(&mut c, 10_000);
            hostnet::block_on(p.tick(&mut c)).unwrap();
        }
        // Ook een handmatige leesopdracht respecteert dezelfde wachttijd.
        assert!(
            handle(
                &mut p,
                &mut c,
                "pair.list",
                r#"{"driverId":"io_roller_shutter"}"#
            )
            .is_err()
        );
        advance(&mut c, 101_000);
        hostnet::block_on(p.tick(&mut c)).unwrap();
        let s = handle(&mut p, &mut c, "api.invoke", r#"{"handler":"status"}"#).unwrap();
        assert_eq!(
            json::get(&s, "connected").and_then(Value::as_bool),
            Some(true)
        );
        assert_eq!(json::uint(&s, "retryAfter"), 0);
        assert!(json::text(&s, "error").is_empty());
        let w = c.into_transport();
        assert_eq!(w.http.len(), 3);
        assert!(
            w.http[2]
                .headers
                .iter()
                .any(|(k, v)| k == "Cookie" && v == "JSESSIONID=session")
        );
    }
}

#[test]
fn setup_throttle_honors_retry_after_without_refreshing_session() {
    let (mut p, c) = ready();
    let mut w = c.into_transport();
    w.reply(200, r#"{"success":true}"#, true);
    w.reply(429, "{}", false);
    w.replies
        .back_mut()
        .unwrap()
        .headers
        .push(("Retry-After".into(), "1800".into()));
    let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
    assert!(
        handle(
            &mut p,
            &mut c,
            "pair.list",
            r#"{"driverId":"io_roller_shutter"}"#
        )
        .is_err()
    );
    let s = handle(&mut p, &mut c, "api.invoke", r#"{"handler":"status"}"#).unwrap();
    assert_eq!(json::uint(&s, "retryAfter"), 1800);
    advance(&mut c, 901_000);
    assert!(
        handle(
            &mut p,
            &mut c,
            "pair.list",
            r#"{"driverId":"io_roller_shutter"}"#
        )
        .is_err()
    );
    assert_eq!(c.into_transport().http.len(), 2);
}

#[test]
fn failed_logins_back_off_instead_of_retrying_every_poll() {
    let (mut p, c) = ready();
    let mut w = c.into_transport();
    w.reply(401, r#"{"error":"Bad credentials"}"#, false);
    w.reply(401, r#"{"error":"Bad credentials"}"#, false);
    let mut c = w.reconnect("com.stulp.somfy", Somfy::default().manifest());
    for delay in [60, 120] {
        assert!(
            handle(
                &mut p,
                &mut c,
                "pair.list",
                r#"{"driverId":"io_roller_shutter"}"#
            )
            .is_err()
        );
        let s = handle(&mut p, &mut c, "api.invoke", r#"{"handler":"status"}"#).unwrap();
        assert_eq!(json::uint(&s, "retryAfter"), delay);
        advance(&mut c, 10_000);
        assert!(
            handle(
                &mut p,
                &mut c,
                "pair.list",
                r#"{"driverId":"io_roller_shutter"}"#
            )
            .is_err()
        );
        advance(&mut c, (delay - 10) * 1000);
    }
    assert_eq!(c.into_transport().http.len(), 2);
}
