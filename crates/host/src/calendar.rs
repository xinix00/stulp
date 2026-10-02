//! Go reference vectors also exercise the portable calendar.
#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]
    use crate::timezone::{self};
    use stulp_core::json::{self, Value};
    use stulp_runtime::{calendar::Civil, schedule::Clock};
    fn number(v: &Value, k: &str) -> f64 {
        match json::get(v, k).unwrap() {
            Value::Number(n) => n.as_f64(),
            _ => panic!("missing number"),
        }
    }
    #[test]
    fn offsets_dst_and_solar_match_go_reference() {
        let reference = json::parse(include_bytes!("../data/calendar.json")).unwrap();
        for row in json::array(&reference, "local") {
            let clock = timezone::load(json::text(row, "zone")).unwrap();
            let got = clock.local(number(row, "unix") as i64).unwrap();
            let want = Civil {
                year: number(row, "year") as i32,
                month: number(row, "month") as u8,
                day: number(row, "day") as u8,
                hour: number(row, "hour") as u8,
                minute: number(row, "minute") as u8,
                offset: number(row, "offset") as i32,
            };
            assert_eq!(
                got,
                want,
                "{} {}",
                json::text(row, "zone"),
                number(row, "unix")
            );
        }
        for row in json::array(&reference, "solar") {
            let clock = timezone::load(json::text(row, "zone")).unwrap();
            let date = Civil {
                year: number(row, "year") as i32,
                month: number(row, "month") as u8,
                day: number(row, "day") as u8,
                hour: 12,
                minute: 0,
                offset: 0,
            };
            let got = clock
                .solar(
                    date,
                    number(row, "lat"),
                    number(row, "lon"),
                    json::boolean(row, "rise"),
                )
                .unwrap();
            let want = match json::get(row, "unix").unwrap() {
                Value::Number(n) => Some(n.as_f64() as i64),
                _ => None,
            };
            assert_eq!(got, want, "{} {:?}", json::text(row, "zone"), date);
        }
    }
}
