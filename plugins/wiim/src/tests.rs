#![allow(clippy::unwrap_used, clippy::panic)]
use super::*;
use alloc::format;
const DESCRIPTION: &str = include_str!("../tests/fixtures/descriptionXML.xml");
fn status(metadata: &str, state: &str, mode: &str) -> String {
    let escaped = metadata
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;");
    format!(
        "<s:Envelope xmlns:s='soap'><s:Body><u:GetInfoExResponse xmlns:u='av'><CurrentTransportState>{state}</CurrentTransportState><LoopMode>{mode}</LoopMode><TrackDuration>00:03:21.500</TrackDuration><RelTime>00:01:05</RelTime><CurrentVolume>40</CurrentVolume><CurrentMute>0</CurrentMute><TrackMetaData>{escaped}</TrackMetaData></u:GetInfoExResponse></s:Body></s:Envelope>"
    )
}
#[test]
fn upnp_service_url_and_identity_come_from_description() {
    let d = wire::Description::parse(
        "http://192.0.2.1:49152/description.xml",
        DESCRIPTION.as_bytes(),
    )
    .unwrap();
    assert!(d.player);
    assert_eq!(d.name, "Büro Audio");
    assert_eq!(d.uuid, "FF98F09C-74E9-46A5-07FF-155EFF98F09C");
    assert_eq!(
        d.control,
        "http://192.0.2.1:49152/upnp/control/rendertransport1"
    );
    let modified = DESCRIPTION
        .replace(
            "<device>",
            "<URLBase>http://192.0.2.2:1234/root/</URLBase><device>",
        )
        .replace("/upnp/control/rendertransport1", "../control");
    let d = wire::Description::parse("http://192.0.2.1:49152/d", modified.as_bytes()).unwrap();
    assert_eq!(d.control, "http://192.0.2.2:1234/control");
}
#[test]
fn reported_track_radio_and_time_match_original() {
    let values = wire::parse_status(
        status(
            include_str!("../tests/fixtures/trackDIDL.xml"),
            "PLAYING",
            "2",
        )
        .as_bytes(),
    )
    .unwrap();
    assert_eq!(
        json::text(&values, "speaker_artist"),
        "Massive Attack, Mezzanine"
    );
    assert_eq!(json::text(&values, "speaker_track"), "Teardrop");
    assert_eq!(number(field(&values, "speaker_duration")), Some(201.5));
    assert_eq!(number(field(&values, "speaker_position")), Some(65.));
    assert_eq!(number(field(&values, "volume_set")), Some(0.4));
    assert!(json::boolean(&values, "speaker_shuffle"));
    let values = wire::parse_status(
        status(
            include_str!("../tests/fixtures/radioDIDL.xml"),
            "PLAYING",
            "0",
        )
        .as_bytes(),
    )
    .unwrap();
    assert_eq!(json::text(&values, "speaker_artist"), "SWR3");
    assert_eq!(
        json::text(&values, "speaker_track"),
        "David Guetta & Sia - Floating through space"
    );
    let values =
        wire::parse_status(status("broken ignored metadata", "NO_MEDIA_PRESENT", "9").as_bytes())
            .unwrap();
    assert_eq!(json::text(&values, "speaker_track"), "");
    assert!(json::get(&values, "speaker_shuffle").is_none());
}
#[test]
fn malformed_responses_fail_with_useful_errors() {
    let missing = status("", "STOPPED", "0").replace("<CurrentVolume>40</CurrentVolume>", "");
    assert!(
        stulp_sdk::message(&wire::parse_status(missing.as_bytes()).err().unwrap())
            .unwrap()
            .contains("CurrentVolume")
    );
    let fault=br#"<Envelope><Body><Fault><detail><UPnPError><errorCode>701</errorCode><errorDescription>Transition not available</errorDescription></UPnPError></detail></Fault></Body></Envelope>"#;
    let error = stulp_sdk::message(&wire::parse_status(fault).err().unwrap()).unwrap();
    assert!(error.contains("701") && error.contains("Transition not available"));
    for bad in ["00:03", "NaN:00:00", "-1:00:00", "00:00:inf"] {
        assert!(wire::seconds(bad).is_err());
    }
    assert_eq!(wire::seconds("NOT_IMPLEMENTED").unwrap(), None);
}
#[test]
fn loop_modes_roundtrip_and_addresses_reject_url_injection() {
    for i in 0..6 {
        let s = decimal(i).unwrap();
        let (shuffle, repeat) = wire::loop_mode(&s).unwrap();
        assert_eq!(wire::encode_loop(shuffle, repeat).unwrap(), s);
    }
    assert!(wire::encode_loop(true, "sometimes").is_err());
    for bad in [
        "",
        "http://host",
        "host:443",
        "host@other",
        "host\r\n",
        "host&reboot",
        "host\\other",
    ] {
        assert!(wire::address(bad).is_err());
    }
    assert!(wire::address("wiim-pro.local").is_ok());
    assert_eq!(
        wire::location(b"HTTP/1.1 200 OK\r\nlocation: http://192.0.2.1/d\r\n"),
        Some("http://192.0.2.1/d")
    );
}
