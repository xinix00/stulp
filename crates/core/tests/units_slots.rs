//! Pariteitsgevallen voor eenheden en herstel na afgebroken documentwrites.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    Error, json,
    slots::{self, Backend, Slot},
    store::Storage,
    units,
};

#[test]
fn canonical_values_remain_exact_until_user_chooses() {
    let empty = json::object();
    for unit in ["°C", "mm", "km", "hPa", "%", "W", "kWh"] {
        let value = 20.700000000000003;
        assert_eq!(units::show(&empty, value, unit), (value, unit));
    }
    for (choice, unit, value, want) in [
        (r#"{"temperature":"°F"}"#, "°C", 22.1, 71.8),
        (r#"{"rain":"in"}"#, "mm", 25.4, 1.0),
        (r#"{"pressure":"inHg"}"#, "hPa", 1013.25, 29.92),
        (r#"{"power":"kW"}"#, "W", 1500.0, 1.5),
    ] {
        let settings = json::parse(choice.as_bytes()).unwrap();
        assert_eq!(units::show(&settings, value, unit).0, want);
    }
}

#[test]
fn beaufort_uses_lower_bounds_and_round_trips() {
    let choices = json::object();
    for force in 0..=12 {
        let ms = units::canonical(&choices, f64::from(force), "m/s");
        assert_eq!(units::show(&choices, ms, "m/s").0, f64::from(force));
    }
    assert_eq!(units::canonical(&choices, 6.0, "m/s"), 10.8);
    assert_eq!(units::show(&choices, 12.0, "m/s"), (6.0, "Bft"));
    assert!(!units::valid("temperature", "km/h"));
    assert!(units::valid("power", ""));
}

#[test]
fn every_truncated_slot_falls_back_to_previous_generation() {
    let a = slots::encode(1, b"old").unwrap();
    let b = slots::encode(2, b"new").unwrap();
    for end in 0..b.len() {
        let (slot, record) = slots::select(Some(&a), Some(&b[..end]), None).unwrap();
        assert_eq!(slot, Slot::A);
        assert_eq!(record.payload, b"old");
    }
    let (slot, record) = slots::select(Some(&a), Some(&b), None).unwrap();
    assert_eq!(slot, Slot::B);
    assert_eq!(record.payload, b"new");
    let conflict = slots::encode(1, b"different").unwrap();
    assert!(slots::select(Some(&a), Some(&conflict), None).is_err());
    assert_eq!(slots::checksum(b"", b"123456789"), 0xcbf4_3926);
}

struct LostReply {
    a: Option<Vec<u8>>,
    b: Option<Vec<u8>>,
    truncate: bool,
}
impl Backend for LostReply {
    fn read(&mut self, slot: Slot) -> stulp_core::Result<Option<Vec<u8>>> {
        Ok(match slot {
            Slot::A => self.a.clone(),
            Slot::B => self.b.clone(),
            Slot::Legacy => None,
        })
    }
    fn write(&mut self, slot: Slot, bytes: &[u8]) -> stulp_core::Result {
        let bytes = if self.truncate {
            bytes[..bytes.len() / 2].to_vec()
        } else {
            bytes.to_vec()
        };
        match slot {
            Slot::A => self.a = Some(bytes),
            Slot::B => self.b = Some(bytes),
            Slot::Legacy => panic!("legacy must never be overwritten"),
        };
        Err(Error::Storage)
    }
}
#[test]
fn lost_reply_reads_back_before_deciding_commit() {
    let (mut full, _) = slots::Files::open(LostReply {
        a: None,
        b: None,
        truncate: false,
    })
    .unwrap();
    full.save(b"committed despite lost response").unwrap();
    let (mut partial, _) = slots::Files::open(LostReply {
        a: None,
        b: None,
        truncate: true,
    })
    .unwrap();
    assert_eq!(partial.save(b"not fully committed"), Err(Error::Storage));
}
