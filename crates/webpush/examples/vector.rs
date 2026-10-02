//! Synthetische vector voor de onafhankelijke Go-ontvanger, zonder netwerk.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::json::{self, Value};
use stulp_protocol::token::base64;
fn main() {
    let data = json::fields(&[
        (
            "endpoint",
            json::string("https://push.example/device").unwrap(),
        ),
        (
            "p256dh",
            json::string(&base64(&stulp_webpush::public(&[3; 32]).unwrap()).unwrap()).unwrap(),
        ),
        ("auth", json::string(&base64(&[4; 16]).unwrap()).unwrap()),
    ])
    .unwrap();
    let subscription = stulp_webpush::Subscription::read(&data).unwrap();
    let plaintext = stulp_webpush::message("Stulp", "Iemand belt aan", None).unwrap();
    let body = stulp_webpush::encrypt(&subscription, &[5; 32], &[6; 16], &plaintext).unwrap();
    let header = stulp_webpush::authorization(
        &[7; 32],
        subscription.endpoint(),
        "mailto:stulp@example.net",
        1_790_000_000,
    )
    .unwrap();
    let out = json::fields(&[
        ("body", json::string(&base64(&body).unwrap()).unwrap()),
        ("authorization", json::string(&header).unwrap()),
        ("private", json::string(&base64(&[3; 32]).unwrap()).unwrap()),
        ("auth", json::string(&base64(&[4; 16]).unwrap()).unwrap()),
        ("now", Value::uint(1_790_000_000)),
    ])
    .unwrap();
    println!("{}", json::to_string(&out).unwrap());
}
