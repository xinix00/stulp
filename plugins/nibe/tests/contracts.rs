//! Oude nummering, energiebalans en OAuth-rotatie blijven aantoonbaar hetzelfde.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
use stulp_core::json::{self, Value};
use stulp_nibe::{Nibe, code_from_redirect, energy::Split};
use stulp_sdk::{Client, Plugin};
use wire::{Wire, value};
fn configured(state: &str) -> Wire {
    let c = Wire::client("com.stulp.nibe", Nibe::default().manifest(), "[]", state);
    let mut w = c.into_transport();
    for (k, v) in [
        ("clientId", "client"),
        ("clientSecret", "secret"),
        ("redirectUri", "https://localhost/callback"),
    ] {
        w.store
            .setting("com.stulp.nibe", k, Some(Value::string(v).unwrap()))
            .unwrap();
    }
    w
}
fn api(p: &mut Nibe, c: &mut Client<Wire>, s: &str) -> stulp_sdk::Result<Value> {
    hostnet::block_on(p.handle(c, "api.invoke", &value(s)))
}
#[test]
fn redirect_state_and_encoding() {
    assert_eq!(
        code_from_redirect("https://localhost/?code=a%2Bb&state=known", "known").unwrap(),
        "a+b"
    );
    assert!(code_from_redirect("https://localhost/?code=x&state=other", "known").is_err());
    assert!(code_from_redirect("?code=x&code=y", "known").is_err());
    assert!(code_from_redirect("?code=%GG", "known").is_err());
    assert_eq!(
        code_from_redirect(" bare-code ", "known").unwrap(),
        "bare-code"
    );
}
#[test]
fn energy_preserves_sum_and_ignores_resets_and_gaps() {
    let mut s = Split::default();
    assert!(!s.anchor(1000.0));
    s.power(0, 1000.0, 3);
    s.power(1_200_000, 1000.0, 3);
    s.power(1_200_000, 1000.0, 1);
    s.power(2_400_000, 1000.0, 1);
    assert!(s.anchor(1002.0));
    let (h, w) = s.meters();
    assert!((h - 1.0).abs() < 1e-9 && (w - 1.0).abs() < 1e-9);
    assert!(!s.anchor(3.0));
    assert!(!s.anchor(100.0));
    let restored = Split::restore(&s.store().unwrap());
    assert_eq!(restored.meters(), s.meters());
}
#[test]
fn rotating_token_is_saved_before_system_query() {
    let state = r#"{"tokens":{"accessToken":"expired","refreshToken":"old-refresh","expiry":"2020-01-01T00:00:00Z"}}"#;
    let mut w = configured(state);
    w.reply(
        200,
        r#"{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}"#,
        false,
    );
    w.reply(200, r#"{"systems":[]}"#, false);
    let mut c = w.reconnect("com.stulp.nibe", Nibe::default().manifest());
    api(&mut Nibe::default(), &mut c, r#"{"handler":"check"}"#).unwrap();
    let stored = json::get(json::get(c.state().root(), "appState").unwrap(), "tokens").unwrap();
    assert_eq!(json::text(stored, "refreshToken"), "new-refresh");
    let w = c.into_transport();
    assert_eq!(w.http.len(), 2);
    assert!(String::from_utf8_lossy(&w.http[0].body).contains("refresh_token=old-refresh"));
    assert!(
        w.http[1]
            .headers
            .iter()
            .any(|(k, v)| k == "Authorization" && v == "Bearer new-access")
    );
}
#[test]
fn failed_persistence_does_not_use_new_token() {
    let mut w = configured("null");
    w.fail_state = true;
    w.reply(
        200,
        r#"{"access_token":"new-access","expires_in":3600}"#,
        false,
    );
    let mut c = w.reconnect("com.stulp.nibe", Nibe::default().manifest());
    assert!(api(&mut Nibe::default(), &mut c, r#"{"handler":"connect"}"#).is_err());
    assert!(json::get(c.state().root(), "appState").unwrap().is_null());
    assert_eq!(c.into_transport().http.len(), 1);
}
#[test]
fn authorization_has_pkce_and_status_never_returns_secret() {
    let w = configured("null");
    let mut c = w.reconnect("com.stulp.nibe", Nibe::default().manifest());
    let mut p = Nibe::default();
    let a = api(&mut p, &mut c, r#"{"handler":"authorize"}"#).unwrap();
    let url = json::text(&a, "url");
    assert!(url.contains("code_challenge_method=S256"));
    assert!(!url.contains("secret"));
    let status = api(&mut p, &mut c, r#"{"handler":"status"}"#).unwrap();
    assert!(json::boolean(&status, "hasSecret"));
    assert!(json::get(&status, "clientSecret").is_none());
}
