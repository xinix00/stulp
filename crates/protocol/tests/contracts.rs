//! De framing- en authenticatiecontracten uit internal/appproto.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::json::{self, Value};
use stulp_protocol::{
    Decoder, Frame, Kind, MAX_FRAME, MAX_GREETING, encode,
    session::{MAX_PENDING, Session},
    token::{self, Direction},
};

#[test]
fn every_split_and_coalesced_frames_preserve_bytes() {
    let request = Frame::request(9, "device.set", &json::object()).unwrap();
    let bytes = encode(&request).unwrap();
    for split in 0..bytes.len() {
        let mut d = Decoder::new(MAX_FRAME);
        assert_eq!(d.feed(&bytes[..split]).unwrap(), split);
        assert!(d.take().is_none());
        assert_eq!(d.feed(&bytes[split..]).unwrap(), bytes.len() - split);
        let frame = Frame::decode(&d.take().unwrap()).unwrap();
        assert_eq!(frame.id, 9);
        assert_eq!(frame.kind, Kind::Request);
        assert_eq!(frame.method(), "device.set");
        d.finish().unwrap();
    }
    let mut both = bytes.clone();
    both.extend_from_slice(&bytes);
    let mut d = Decoder::new(MAX_FRAME);
    assert_eq!(d.feed(&both).unwrap(), bytes.len());
    assert!(d.take().is_some());
    assert_eq!(d.feed(&both[bytes.len()..]).unwrap(), bytes.len());
    assert!(d.take().is_some());
}

#[test]
fn oversized_prefix_poisoning_and_truncated_eof() {
    for length in [0, MAX_FRAME + 1] {
        let mut d = Decoder::new(MAX_FRAME);
        assert!(d.feed(&(length as u32).to_be_bytes()).is_err());
        assert!(d.feed(&[0, 0, 0, 1, b'a']).is_err());
    }
    let mut d = Decoder::new(MAX_GREETING);
    assert!(d.feed(&((MAX_GREETING + 1) as u32).to_be_bytes()).is_err());
    for part in [&[0_u8][..], &[0, 0, 0, 2, b'{'][..]] {
        let mut d = Decoder::new(MAX_FRAME);
        d.feed(part).unwrap();
        assert!(d.finish().is_err());
    }
}

#[test]
fn unknown_kind_bad_identifiers_and_malformed_json_fail() {
    for raw in [
        r#"{"t":"wrong"}"#,
        r#"{"t":"req","id":0,"m":"a"}"#,
        r#"{"t":"req","id":1}"#,
        r#"{"t":"err","id":1}"#,
        r#"{"t":"res","id":1,"id":2}"#,
    ] {
        assert!(Frame::decode(raw.as_bytes()).is_err(), "{raw}");
    }
    let response = Frame::response(1, Ok(Value::uint(u64::MAX))).unwrap();
    let bytes = json::to_string(&response).unwrap();
    let frame = Frame::decode(bytes.as_bytes()).unwrap();
    assert_eq!(json::uint(&frame.value, "r"), u64::MAX);
}

#[test]
fn proofs_are_bound_to_app_nonce_and_direction() {
    let t = token::token("secret", "com.stulp.weather").unwrap();
    assert_eq!(t, "7fI_A8wHCRB8A3ue7L6FNqimFefiRsucjlE8gLGNyHY");
    let p = token::proof(&t, Direction::App, "fresh-nonce", "com.stulp.weather").unwrap();
    assert!(token::check("secret", "com.stulp.weather", "fresh-nonce", &p).unwrap());
    for (secret, app, nonce) in [
        ("", "com.stulp.weather", "fresh-nonce"),
        ("secret", "com.stulp.matter", "fresh-nonce"),
        ("secret", "com.stulp.weather", "other"),
        ("secret", "com.stulp.weather", ""),
    ] {
        assert!(!token::check(secret, app, nonce, &p).unwrap());
    }
    let reflected = token::proof(&t, Direction::Stulp, "fresh-nonce", "com.stulp.weather").unwrap();
    assert!(!token::check("secret", "com.stulp.weather", "fresh-nonce", &reflected).unwrap());
}

#[test]
fn base64_has_no_padding_and_covers_partial_triplets() {
    for (bytes, want) in [
        (&b""[..], ""),
        (&b"f"[..], "Zg"),
        (&b"fo"[..], "Zm8"),
        (&b"foo"[..], "Zm9v"),
        (&[255, 255, 255][..], "____"),
    ] {
        assert_eq!(token::base64(bytes).unwrap(), want);
    }
}

#[test]
fn timeout_disconnect_and_late_answers_have_one_owner() {
    let mut s = Session::new();
    let a = s.begin(10, 100, 50).unwrap();
    let b = s.begin(20, 100, 100).unwrap();
    assert!(s.expire(149).is_none());
    assert_eq!(s.expire(150), Some((a, 10)));
    assert!(s.complete(a).is_none());
    assert_eq!(s.complete(b), Some(20));
    assert!(s.complete(b).is_none());
    for owner in 0..MAX_PENDING {
        s.begin(owner as u64, 200, 50).unwrap();
    }
    assert!(s.begin(999, 200, 50).is_err());
    assert_eq!(s.disconnect().count(), MAX_PENDING);
    assert!(s.begin(1000, 300, 50).is_ok());
}

#[test]
fn large_snapshots_use_the_frame_budget_not_the_api_json_budget() {
    let text = "x".repeat(2 * 1024 * 1024);
    let frame = Frame::request(
        1,
        "state.snapshot",
        &json::fields(&[("state", json::string(&text).unwrap())]).unwrap(),
    )
    .unwrap();
    let bytes = encode(&frame).unwrap();
    let mut decoder = Decoder::new(MAX_FRAME);
    for chunk in bytes.chunks(8093) {
        assert_eq!(decoder.feed(chunk).unwrap(), chunk.len());
    }
    let parsed = Frame::decode(&decoder.take().unwrap()).unwrap();
    assert_eq!(
        json::text(json::get(&parsed.value, "p").unwrap(), "state"),
        text
    );
}
