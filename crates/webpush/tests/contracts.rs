//! Sleutelvalidatie en grenzen die de browser vóór aflevering nodig heeft.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::json::{self, Value};
use stulp_protocol::token::base64;
use stulp_webpush as push;
fn sub() -> push::Subscription {
    push::Subscription::read(
        &json::fields(&[
            ("endpoint", Value::string("https://push.example/p").unwrap()),
            (
                "p256dh",
                Value::string(&base64(&push::public(&[3; 32]).unwrap()).unwrap()).unwrap(),
            ),
            ("auth", Value::string(&base64(&[4; 16]).unwrap()).unwrap()),
        ])
        .unwrap(),
    )
    .unwrap()
}
#[test]
fn each_salt_and_key_changes_the_payload() {
    let s = sub();
    let a = push::encrypt(&s, &[5; 32], &[6; 16], b"hello").unwrap();
    let b = push::encrypt(&s, &[7; 32], &[8; 16], b"hello").unwrap();
    assert_ne!(a, b);
    assert_eq!(&a[16..20], &4096u32.to_be_bytes());
    assert_eq!(a[20], 65);
    assert_eq!(a.len(), 108);
}
#[test]
fn full_record_fits_and_overflow_fails() {
    let s = sub();
    assert_eq!(
        push::encrypt(&s, &[5; 32], &[6; 16], &vec![b'a'; 3993])
            .unwrap()
            .len(),
        4096
    );
    assert!(push::encrypt(&s, &[5; 32], &[6; 16], &vec![b'a'; 3994]).is_err());
    assert!(push::public(&[0; 32]).is_err());
    assert!(push::origin("http://push.example/a").is_err());
}
#[test]
fn both_base64_alphabets_decode() {
    assert_eq!(push::decode(" +/8= ").unwrap(), vec![251, 255]);
    assert_eq!(push::decode("-_8").unwrap(), vec![251, 255]);
    assert!(push::decode("ab%").is_err());
}
