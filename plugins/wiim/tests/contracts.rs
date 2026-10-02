//! Echte controller-RPC met een synthetische speler: geen netwerk naar het huis.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Plugin};
use stulp_wiim::Wiim;
use wire::{Wire, value};
const DEVICE: &str = r#"[{"id":"speaker","appId":"com.stulp.wiim","driverId":"player","name":"Room","data":{"id":"fixed-uuid"},"settings":{"address":"192.0.2.42"},"capabilities":["speaker_playing","speaker_next","speaker_prev","volume_set","volume_mute","speaker_track","speaker_artist","speaker_album","speaker_shuffle","speaker_repeat","speaker_position","speaker_duration","button.off","button.preset1"]}]"#;
fn handle(p: &mut Wiim, c: &mut Client<Wire>, method: &str, s: &str) -> stulp_sdk::Result<Value> {
    hostnet::block_on(p.handle(c, method, &value(s)))
}
fn setup() -> (Wiim, Client<Wire>) {
    let mut p = Wiim::default();
    let mut c = Wire::client("com.stulp.wiim", p.manifest(), DEVICE, "{}");
    handle(&mut p, &mut c, "device.init", r#"{"deviceId":"speaker"}"#).unwrap();
    (p, c)
}
#[test]
fn commands_preserve_literal_colons_and_do_not_invent_state() {
    let (mut p, c) = setup();
    let mut w = c.into_transport();
    w.reply(200, "OK", false);
    w.delay = 6000;
    let mut c = w.reconnect("com.stulp.wiim", p.manifest());
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
    assert_eq!(
        w.http[0].url,
        "https://192.0.2.42/httpapi.asp?command=setPlayerCmd:vol:35"
    );
    assert!(w.http[0].device_certificate);
    assert!(
        w.sent
            .iter()
            .any(|v| json::text(v, "m") == "$appproto.ping")
    );
}
#[test]
fn unknown_repeat_and_out_of_range_presets_never_reach_player() {
    let (mut p, mut c) = setup();
    assert!(
        handle(
            &mut p,
            &mut c,
            "capability.invoke",
            r#"{"deviceId":"speaker","capability":"speaker_shuffle","value":true}"#
        )
        .is_err()
    );
    assert!(
        handle(
            &mut p,
            &mut c,
            "flow.run",
            r#"{"id":"call_preset","args":{"device":"speaker","preset_number":"13"}}"#
        )
        .is_err()
    );
    assert!(c.into_transport().http.is_empty());
}
#[test]
fn manual_pairing_uses_actual_description_and_retains_the_candidate() {
    let mut p = Wiim::default();
    let mut w = Wire::client("com.stulp.wiim", p.manifest(), "[]", "{}").into_transport();
    w.reply(200, include_str!("fixtures/descriptionXML.xml"), false);
    let mut c = w.reconnect("com.stulp.wiim", p.manifest());
    handle(
        &mut p,
        &mut c,
        "pair.start",
        r#"{"driverId":"player","sessionId":"s"}"#,
    )
    .unwrap();
    let answer = handle(
        &mut p,
        &mut c,
        "pair.emit",
        r#"{"sessionId":"s","event":"manual","data":{"address":" 192.0.2.42 "}}"#,
    )
    .unwrap();
    assert_eq!(json::uint(&answer, "found"), 1);
    let devices = handle(
        &mut p,
        &mut c,
        "pair.emit",
        r#"{"sessionId":"s","event":"list_devices"}"#,
    )
    .unwrap();
    let d = &devices.as_array().unwrap()[0];
    assert_eq!(
        json::text(json::get(d, "settings").unwrap(), "address"),
        "192.0.2.42"
    );
    assert_eq!(
        json::text(json::get(d, "data").unwrap(), "id"),
        "FF98F09C-74E9-46A5-07FF-155EFF98F09C"
    );
    assert_eq!(c.into_transport().http.len(), 1);
}
#[test]
fn three_missed_rounds_preserve_last_values_and_recovery_refreshes_control() {
    let (mut p, c) = setup();
    let mut w = c.into_transport();
    let description = include_str!("fixtures/descriptionXML.xml");
    let status = "<Envelope><Body><GetInfoExResponse><CurrentTransportState>PLAYING</CurrentTransportState><LoopMode>4</LoopMode><TrackDuration>00:03:21</TrackDuration><RelTime>00:01:05</RelTime><CurrentVolume>40</CurrentVolume><CurrentMute>0</CurrentMute><TrackMetaData/></GetInfoExResponse></Body></Envelope>";
    w.reply(200, description, false);
    w.reply(200, status, false);
    // First failure was at the cached control URL; subsequent attempts rediscover it.
    for _ in 0..3 {
        w.reply(503, "", false);
    }
    w.reply(200, description, false);
    w.reply(200, status, false);
    let mut c = w.reconnect("com.stulp.wiim", p.manifest());
    for round in 0..5 {
        for _ in 0..55 {
            hostnet::block_on(c.call("$appproto.ping", &Value::Null)).unwrap();
        }
        hostnet::block_on(p.tick(&mut c)).unwrap();
        let d = c.state().device("speaker").unwrap();
        assert_eq!(json::boolean(d, "available"), round != 3);
        assert_eq!(
            json::to_string(json::get(json::get(d, "state").unwrap(), "volume_set").unwrap())
                .unwrap(),
            "0.4"
        );
    }
    let w = c.into_transport();
    assert_eq!(w.http.len(), 7);
    assert!(w.http[1].url.ends_with("/upnp/control/rendertransport1"));
    assert!(w.http[3].url.ends_with("/description.xml"));
}
