//! Tijdkaarten starten alleen hun eigen tak; dubbele wintertijd houdt beide UTC-offsets.
#![allow(clippy::unwrap_used, clippy::panic)]
use stulp_core::{
    Result, json,
    store::{Memory, Store},
};
use stulp_runtime::{
    calendar::{self, Civil},
    flows::Effect,
    schedule::{Clock, Schedule},
};
struct Zone;
impl Clock for Zone {
    fn local(&self, unix: i64) -> Result<Civil> {
        let change = calendar::midnight(2026, 10, 25)? + 3600;
        calendar::civil(unix, if unix < change { 7200 } else { 3600 })
    }
    fn solar(&self, date: Civil, _lat: f64, _lon: f64, _rise: bool) -> Result<Option<i64>> {
        Ok(Some(
            calendar::midnight(date.year, date.month, date.day)? + 1800,
        ))
    }
}
#[test]
fn same_minute_is_deduplicated_but_second_dst_occurrence_runs() {
    let store=Store::open(br#"{"version":2,"flows":[{"id":"f","name":"Clock","enabled":true,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"time_at","args":{"time":"02:30"}}},{"id":"sun","step":{"appId":"stulp","cardType":"trigger","cardId":"sunrise","args":{"latitude":52,"longitude":4,"offset":0}}},{"id":"no","step":{"appId":"stulp","cardType":"trigger","cardId":"time_at","args":{"time":"05:00"}}},{"id":"a","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"{{date}} {{time}}"}}},{"id":"wrong","step":{"appId":"stulp","cardType":"action","cardId":"notification","args":{"excerpt":"Wrong time"}}}],"edges":[{"from":"t","to":"a"},{"from":"sun","to":"a"},{"from":"no","to":"wrong"}]}]}"#,Memory).unwrap();
    let first = calendar::midnight(2026, 10, 25).unwrap() + 1800;
    let mut scheduler = Schedule::default();
    for unix in [first, first + 3600] {
        let mut run = scheduler
            .due(&store, &Zone, unix, 0, "2026-10-25T00:30:00Z")
            .unwrap()
            .unwrap();
        let Effect::Notification(text) = run.advance(&store, 0).unwrap() else {
            panic!("missing scheduled action")
        };
        assert_eq!(text, "2026-10-25 02:30");
        run.complete(json::Value::Bool(true), 1).unwrap();
        assert!(matches!(run.advance(&store, 2).unwrap(), Effect::Finished));
        assert!(
            scheduler
                .due(&store, &Zone, unix + 59, 60_000, "2026-10-25T00:30:59Z")
                .unwrap()
                .is_none()
        );
    }
    assert!(
        scheduler
            .due(&store, &Zone, first + 3660, 70_000, "2026-10-25T01:31:00Z")
            .unwrap()
            .is_none()
    );
}
#[test]
fn calendar_roundtrips_cover_gregorian_century_rules() {
    for y in [
        1, 100, 400, 1600, 1900, 1969, 1970, 2000, 2026, 2100, 2400, 9999,
    ] {
        for m in 1..=12 {
            for d in 1..=calendar::month_days(y, m) {
                let t = calendar::midnight(y, m, d).unwrap();
                let c = calendar::civil(t, 0).unwrap();
                assert_eq!((c.year, c.month, c.day, c.hour, c.minute), (y, m, d, 0, 0));
            }
        }
    }
    assert_eq!(calendar::midnight(1970, 1, 1).unwrap(), 0);
    assert!(calendar::midnight(1900, 2, 29).is_err());
}
