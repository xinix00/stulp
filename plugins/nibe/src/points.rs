//! De parameternummers en grenzen komen rechtstreeks uit de Go-tabellen.
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Error, Result,
    util::{field, float, number},
};
#[derive(Clone, Copy)]
pub(super) enum Kind {
    Number,
    Boolean,
    Enum,
}
pub(super) struct Point {
    pub(super) id: &'static str,
    pub(super) capability: &'static str,
    pub(super) kind: Kind,
    pub(super) writable: bool,
    pub(super) min: f64,
    pub(super) max: f64,
}
pub(super) const POINTS: &[Point] = &[
    Point {
        id: "4",
        capability: "measure_temperature.outdoor",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "8",
        capability: "measure_temperature.supply",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "10",
        capability: "measure_temperature.return",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "1708",
        capability: "measure_temperature.calculated_supply",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "32628",
        capability: "measure_temperature.hotwater",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "48351",
        capability: "measure_temperature",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "29972",
        capability: "hotwater_amount",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "55000",
        capability: "operating_priority",
        kind: Kind::Enum,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "1756",
        capability: "additional_heat_power",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "5927",
        capability: "compressor_frequency",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "1975",
        capability: "pump_speed",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "26945",
        capability: "airflow",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "26411",
        capability: "add_heat_time_heating",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "1865",
        capability: "add_heat_time_hotwater",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "47751",
        capability: "target_temperature",
        kind: Kind::Number,
        writable: true,
        min: 5.0,
        max: 35.0,
    },
    Point {
        id: "7086",
        capability: "hot_water_boost",
        kind: Kind::Boolean,
        writable: true,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "8121",
        capability: "ventilation_boost",
        kind: Kind::Boolean,
        writable: true,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "3830",
        capability: "ventilation_mode",
        kind: Kind::Enum,
        writable: true,
        min: 0.0,
        max: 4.0,
    },
    Point {
        id: "40004",
        capability: "measure_temperature.outdoor",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "40008",
        capability: "measure_temperature.supply",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "40012",
        capability: "measure_temperature.return",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "43009",
        capability: "measure_temperature.calculated_supply",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "40013",
        capability: "measure_temperature.hotwater",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "40033",
        capability: "measure_temperature",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "50345",
        capability: "hotwater_amount",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "49994",
        capability: "operating_priority",
        kind: Kind::Enum,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "43084",
        capability: "additional_heat_power",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "41778",
        capability: "compressor_frequency",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "43437",
        capability: "pump_speed",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "43239",
        capability: "add_heat_time_hotwater",
        kind: Kind::Number,
        writable: false,
        min: 0.0,
        max: 0.0,
    },
    Point {
        id: "47398",
        capability: "target_temperature",
        kind: Kind::Number,
        writable: true,
        min: 5.0,
        max: 35.0,
    },
    Point {
        id: "50004",
        capability: "hot_water_boost",
        kind: Kind::Boolean,
        writable: true,
        min: 0.0,
        max: 0.0,
    },
];
pub(super) const ENERGY: &[&str] = &[
    "measure_power",
    "measure_power.heating",
    "measure_power.hotwater",
    "meter_power",
    "meter_power.heating",
    "meter_power.hotwater",
];
pub(super) fn reading(points: &Value, id: &str) -> Option<f64> {
    points
        .as_array()?
        .iter()
        .find(|v| json::text(v, "parameterId") == id)
        .and_then(|v| number(field(v, "value")))
        .filter(|n| *n != -32768.0)
}
pub(super) fn present(points: &Value) -> Result<Vec<String>> {
    let mut out = Vec::new();
    for p in points
        .as_array()
        .ok_or(Error::Invalid("invalid myUplink points"))?
    {
        json::push(&mut out, json::copy(json::text(p, "parameterId"))?, 4096)?;
    }
    Ok(out)
}
pub(super) fn capability(point: &Point, n: f64) -> Result<Value> {
    match point.kind {
        Kind::Number => float(n),
        Kind::Boolean => Ok(Value::Bool(n != 0.0)),
        Kind::Enum => {
            let mut s = String::new();
            s.try_reserve(32).map_err(|_| stulp_core::Error::Memory)?;
            write!(&mut s, "{}", n as i64).map_err(|_| Error::Invalid("enum encoding failed"))?;
            Ok(json::string(&s)?)
        }
    }
}
pub(super) fn writable(point: &Point, v: &Value) -> Result<f64> {
    let n = match point.kind {
        Kind::Boolean => match v.as_bool() {
            Some(true) => 1.0,
            Some(false) => 0.0,
            None => return Err(Error::Invalid("Deze bediening verwacht aan of uit.")),
        },
        Kind::Number => number(v).ok_or(Error::Invalid("Deze bediening verwacht een getal."))?,
        Kind::Enum => v
            .as_str()
            .and_then(|s| s.parse::<i64>().ok())
            .map(|n| n as f64)
            .ok_or(Error::Invalid("Onbekende keuze voor deze bediening."))?,
    };
    if point.min != point.max && (n < point.min || n > point.max) {
        return Err(Error::Invalid(
            "Deze waarde ligt buiten het bereik van de warmtepomp.",
        ));
    }
    Ok(n)
}
pub(super) fn boost(present: &[String], hours: i64) -> Result<(&'static str, u64)> {
    if present.iter().any(|p| p == "4564") {
        if matches!(hours, 0 | 2 | 3 | 6 | 12 | 24 | 48) {
            return Ok(("4564", hours as u64));
        }
    } else if present.iter().any(|p| p == "48132") {
        let n = match hours {
            0 => 0,
            2 => 4,
            3 => 1,
            6 => 2,
            12 => 3,
            _ => return Err(Error::Invalid("Deze pomp kent die duur niet.")),
        };
        return Ok(("48132", n));
    }
    Err(Error::Invalid(
        "Deze pomp kent geen extra warm water voor die duur, of is nog niet uitgelezen.",
    ))
}
pub(super) fn feature(cap: &str) -> Option<&'static str> {
    match cap {
        "hot_water_boost" => Some("boostHotWater"),
        "ventilation_boost" => Some("boostVentilation"),
        "ventilation_mode" => Some("setVentilationMode"),
        _ => None,
    }
}
