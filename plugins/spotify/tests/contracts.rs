//! De bediening gebruikt echte API-vormen zonder afspelen of volume te voorspellen.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Plugin};
use stulp_spotify::{Spotify, code_from_redirect, uri};
use wire::{Wire, value};
const DEVICE: &str = r#"[{"id":"speaker","appId":"com.stulp.spotify","driverId":"player","name":"Room","data":{"id":"original"},"store":{"spotifyId":"current"},"capabilities":["speaker_playing","speaker_next","speaker_prev","volume_set","speaker_track","speaker_artist","speaker_album"]}]"#;
fn wire() -> Wire {
    Wire::client("com.stulp.spotify",Spotify::default().manifest(),DEVICE,r#"{"tokens":{"accessToken":"access","refreshToken":"refresh","expiry":"2030-01-01T00:00:00Z"}}"#).into_transport()
}
fn handle(
    p: &mut Spotify,
    c: &mut Client<Wire>,
    method: &str,
    s: &str,
) -> stulp_sdk::Result<Value> {
    hostnet::block_on(p.handle(c, method, &value(s)))
}
#[test]
fn state_is_required_and_search_terms_are_not_uris() {
    assert!(code_from_redirect("?code=c", "s").is_err());
    assert!(code_from_redirect("?code=c&state=bad", "s").is_err());
    assert_eq!(
        code_from_redirect("https://localhost/#code=c%2B1&state=s", "s").unwrap(),
        "c+1"
    );
    assert!(uri("blue monday", "track").is_err());
    assert_eq!(
        uri(
            "https://open.spotify.com/playlist/0123456789ABCDEFGHIJKL?si=a",
            "playlist"
        )
        .unwrap(),
        "spotify:playlist:0123456789ABCDEFGHIJKL"
    );
}
#[test]
fn commands_target_restored_id_without_optimistic_state() {
    let mut w = wire();
    w.reply(204, "", false);
    let mut c = w.reconnect("com.stulp.spotify", Spotify::default().manifest());
    let mut p = Spotify::default();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"speaker"}"#).unwrap();
    handle(
        &mut p,
        &mut c,
        "capability.invoke",
        r#"{"deviceId":"speaker","capability":"volume_set","value":0.346}"#,
    )
    .unwrap();
    assert!(
        json::get(
            json::get(c.state().device("speaker").unwrap(), "state").unwrap(),
            "volume_set"
        )
        .is_none()
    );
    let w = c.into_transport();
    assert_eq!(w.http.len(), 1);
    assert!(
        w.http[0]
            .url
            .ends_with("/me/player/volume?device_id=current&volume_percent=35")
    );
}
#[test]
fn search_has_measured_limit_and_skips_null_playlists() {
    let mut w = wire();
    w.reply(200,r#"{"playlists":{"items":[null,{"uri":"spotify:playlist:chosen","name":"P","owner":{"display_name":"Maker"},"images":[{"url":"large","width":640},{"url":"small","width":64}]}]}}"#,false);
    let mut c = w.reconnect("com.stulp.spotify", Spotify::default().manifest());
    let answer = handle(
        &mut Spotify::default(),
        &mut c,
        "flow.autocomplete",
        r#"{"kind":"action","id":"play_playlist","argument":"playlist","query":"a & b"}"#,
    )
    .unwrap();
    let item = &answer.as_array().unwrap()[0];
    assert_eq!(json::text(item, "image"), "small");
    assert_eq!(json::text(item, "description"), "Maker");
    assert!(
        c.into_transport().http[0]
            .url
            .ends_with("type=playlist&limit=10&q=a%20%26%20b")
    );
}
#[test]
fn playlist_and_track_use_different_payloads() {
    let mut w = wire();
    w.reply(204, "", false);
    w.reply(204, "", false);
    let mut c = w.reconnect("com.stulp.spotify", Spotify::default().manifest());
    let mut p = Spotify::default();
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"speaker"}"#).unwrap();
    for (kind, id) in [("track", "play_track"), ("playlist", "play_playlist")] {
        let s = format!(
            r#"{{"kind":"action","id":"{id}","args":{{"device":{{"$device":"speaker"}},"{kind}":{{"id":"spotify:{kind}:chosen","name":"Chosen"}}}}}}"#
        );
        handle(&mut p, &mut c, "flow.run", &s).unwrap();
    }
    let w = c.into_transport();
    assert_eq!(
        json::array(&json::parse(&w.http[0].body).unwrap(), "uris").len(),
        1
    );
    assert_eq!(
        json::text(&json::parse(&w.http[1].body).unwrap(), "context_uri"),
        "spotify:playlist:chosen"
    );
}
