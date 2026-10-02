//! Overgezette weerscontracten: canonieke waarden, nachtverwachting en drempelovergangen.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::json;
use stulp_weather::{
    condition,
    data::{self, Weather},
    forecast_url, trigger_filter,
};
fn value(text: &str) -> json::Value {
    json::parse(text.as_bytes()).unwrap()
}
fn weather() -> Weather {
    Weather::decode(br#"{
"utc_offset_seconds":7200,
"current":{"time":"2026-08-10T23:45","temperature_2m":20.7,"relative_humidity_2m":78,"is_day":1,"precipitation":0.4,"weather_code":61,"cloud_cover":72,"wind_speed_10m":11,"wind_direction_10m":282,"wind_gusts_10m":18},
"daily":{"temperature_2m_max":[24,25],"temperature_2m_min":[16,-2],"precipitation_sum":[2.5],"et0_fao_evapotranspiration":[4]},
"minutely_15":{"time":["2026-08-11T00:00","2026-08-11T00:15","2026-08-11T00:30","2026-08-11T00:45"],"precipitation":[1,2,3,4],"cape":[20,500],"lightning_potential":[6]}
}"#).unwrap()
}
#[test]
fn forecasts_cross_midnight_and_tonight_uses_tomorrow() {
    let w = weather();
    assert_eq!(w.rain_within(15), 1.0);
    assert_eq!(w.rain_within(30), 3.0);
    assert_eq!(w.rain_within(60), 10.0);
    assert_eq!(w.tonight(), -2.0);
    assert_eq!(w.irrigation(), 1.5);
    assert_eq!(w.thunder(), 600.0);
    assert!(condition("frost_tonight", &value("{}"), &w).unwrap());
    assert!(!condition("is_sunny", &value("{}"), &w).unwrap());
}
#[test]
fn wind_values_and_queries_remain_canonical() {
    let w = weather();
    let values = w.values().unwrap();
    assert_eq!(
        data::number(json::get(&values, "measure_wind_strength")),
        Some(11.0)
    );
    assert_eq!(data::compass(w.current("wind_direction_10m")), "WNW");
    let url = forecast_url(52.1, 5.18).unwrap();
    assert!(url.contains("wind_speed_unit=ms"));
    assert!(url.contains("forecast_days=2"));
    assert!(forecast_url(f64::NAN, 0.0).is_err());
    assert!(forecast_url(91.0, 0.0).is_err());
}
#[test]
fn every_existing_wmo_code_has_description_and_unknown_keeps_number() {
    for code in [
        0, 1, 2, 3, 45, 48, 51, 53, 55, 56, 57, 61, 63, 65, 66, 67, 71, 73, 75, 77, 80, 81, 82, 85,
        86, 95, 96, 99,
    ] {
        assert_ne!(data::state(code), "unknown");
        assert!(!data::describe(code).unwrap().contains("Onbekend"));
    }
    assert!(data::describe(123).unwrap().contains("123"));
    for code in [56, 57, 66, 67] {
        assert_eq!(data::state(code), "freezing");
    }
}
#[test]
fn thresholds_fire_once_and_accept_numeric_selections() {
    let args = value(r#"{"speed":"10.8"}"#);
    assert!(trigger_filter("wind_changed", &args, &value(r#"{"was":10.7,"now":10.8}"#)).unwrap());
    assert!(!trigger_filter("wind_changed", &args, &value(r#"{"was":10.8,"now":11.5}"#)).unwrap());
    assert!(
        trigger_filter(
            "temperature_fell",
            &value(r#"{"celsius":0}"#),
            &value(r#"{"was":1,"now":0}"#)
        )
        .unwrap()
    );
    assert!(
        trigger_filter(
            "rain_expected",
            &value(r#"{"within":"30"}"#),
            &value(r#"{"was30":0,"in30":1}"#)
        )
        .unwrap()
    );
    assert!(
        !trigger_filter(
            "rain_expected",
            &value(r#"{"within":"30"}"#),
            &value(r#"{"was30":1,"in30":2}"#)
        )
        .unwrap()
    );
    assert!(condition("wind_above", &args, &weather()).unwrap());
}
#[test]
fn compass_wraps_and_rounding_matches_go() {
    for (degrees, want) in [
        (0.0, "N"),
        (11.0, "N"),
        (12.0, "NNO"),
        (348.0, "NNW"),
        (349.0, "N"),
        (370.0, "N"),
        (-90.0, "W"),
    ] {
        assert_eq!(data::compass(degrees), want);
    }
    assert_eq!(data::round(-1.25), -1.3);
    assert_eq!(data::round(1.25), 1.3);
}
