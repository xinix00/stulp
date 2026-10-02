//! Gemeten waarden blijven canoniek; alleen de gebruikersrand rekent om.
use crate::{
    Error, Result,
    json::{self, Value},
};
use alloc::vec::Vec;
struct Choice {
    quantity: &'static str,
    unit: &'static str,
    aliases: &'static [&'static str],
    factor: f64,
    offset: f64,
    step: f64,
    decimals: usize,
}
const CHOICES: &[Choice] = &[
    Choice {
        quantity: "temperature",
        unit: "°C",
        aliases: &["C", "c", "celsius"],
        factor: 1.0,
        offset: 0.0,
        step: 0.5,
        decimals: 1,
    },
    Choice {
        quantity: "temperature",
        unit: "°F",
        aliases: &["F", "fahrenheit"],
        factor: 1.8,
        offset: 32.0,
        step: 1.0,
        decimals: 1,
    },
    Choice {
        quantity: "wind",
        unit: "Bft",
        aliases: &[],
        factor: 0.0,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "wind",
        unit: "m/s",
        aliases: &["ms", "m/sec"],
        factor: 1.0,
        offset: 0.0,
        step: 0.5,
        decimals: 1,
    },
    Choice {
        quantity: "wind",
        unit: "km/h",
        aliases: &["kmh", "kph"],
        factor: 3.6,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "wind",
        unit: "mph",
        aliases: &[],
        factor: 2.2369362920544,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "wind",
        unit: "kn",
        aliases: &["kt", "knots"],
        factor: 1.9438444924406,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "rain",
        unit: "mm",
        aliases: &[],
        factor: 1.0,
        offset: 0.0,
        step: 0.5,
        decimals: 1,
    },
    Choice {
        quantity: "rain",
        unit: "in",
        aliases: &["inch", "\""],
        factor: 0.03937007874015748,
        offset: 0.0,
        step: 0.05,
        decimals: 2,
    },
    Choice {
        quantity: "distance",
        unit: "km",
        aliases: &[],
        factor: 1.0,
        offset: 0.0,
        step: 0.1,
        decimals: 1,
    },
    Choice {
        quantity: "distance",
        unit: "mi",
        aliases: &["mile", "miles"],
        factor: 0.621371192237334,
        offset: 0.0,
        step: 0.1,
        decimals: 1,
    },
    Choice {
        quantity: "pressure",
        unit: "hPa",
        aliases: &["hpa", "mbar", "mBar"],
        factor: 1.0,
        offset: 0.0,
        step: 1.0,
        decimals: 1,
    },
    Choice {
        quantity: "pressure",
        unit: "inHg",
        aliases: &["inhg"],
        factor: 0.02952998307144475,
        offset: 0.0,
        step: 0.1,
        decimals: 2,
    },
    Choice {
        quantity: "pressure",
        unit: "mmHg",
        aliases: &["mmhg", "torr"],
        factor: 0.7500616827041845,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "power",
        unit: "W",
        aliases: &["J/s", "watt", "Watt", "VA"],
        factor: 1.0,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "power",
        unit: "kW",
        aliases: &["kw"],
        factor: 0.001,
        offset: 0.0,
        step: 0.1,
        decimals: 3,
    },
    Choice {
        quantity: "power",
        unit: "BTU/h",
        aliases: &["btu/h", "BTU/hr"],
        factor: 3.412141633,
        offset: 0.0,
        step: 100.0,
        decimals: 0,
    },
    Choice {
        quantity: "energy",
        unit: "kWh",
        aliases: &["kwh"],
        factor: 1.0,
        offset: 0.0,
        step: 0.1,
        decimals: 3,
    },
    Choice {
        quantity: "energy",
        unit: "Wh",
        aliases: &["wh"],
        factor: 1000.0,
        offset: 0.0,
        step: 1.0,
        decimals: 0,
    },
    Choice {
        quantity: "energy",
        unit: "MJ",
        aliases: &["mj"],
        factor: 3.6,
        offset: 0.0,
        step: 0.1,
        decimals: 2,
    },
];
const BEAUFORT: [f64; 12] = [
    0.3, 1.6, 3.4, 5.5, 8.0, 10.8, 13.9, 17.2, 20.8, 24.5, 28.5, 32.7,
];
const QUANTITIES: &[(&str, &str, &str, &str)] = &[
    ("temperature", "Temperatuur", "°C", "°C"),
    ("wind", "Wind", "m/s", "Bft"),
    ("rain", "Neerslag", "mm", "mm"),
    ("distance", "Afstand", "km", "km"),
    ("pressure", "Luchtdruk", "hPa", "hPa"),
    ("power", "Vermogen", "W", ""),
    ("energy", "Energie", "kWh", ""),
];

/// Beaufort is een schaal met drempels, geen vermenigvuldigingsfactor.
pub fn beaufort(value: f64) -> usize {
    BEAUFORT.iter().take_while(|&&limit| value >= limit).count()
}
/// Een kracht correspondeert bij terugrekenen met de laagste bijbehorende snelheid.
pub fn beaufort_floor(force: usize) -> f64 {
    if force == 0 {
        0.0
    } else {
        BEAUFORT.get(force.min(12) - 1).copied().unwrap_or(0.0)
    }
}
fn round(value: f64, decimals: usize) -> f64 {
    let factor = [1.0, 10.0, 100.0, 1000.0, 10000.0]
        .get(decimals)
        .copied()
        .unwrap_or(1.0);
    let scaled = value * factor;
    if !scaled.is_finite() || scaled.abs() >= 9.0e18 {
        return value;
    }
    let rounded = (if scaled < 0.0 {
        scaled - 0.5
    } else {
        scaled + 0.5
    }) as i64;
    rounded as f64 / factor
}
fn from(choice: &Choice, value: f64) -> f64 {
    if choice.unit == "Bft" {
        beaufort_floor(round(value, 0).clamp(0.0, 12.0) as usize)
    } else {
        (value - choice.offset) / choice.factor
    }
}
fn to(choice: &Choice, value: f64) -> f64 {
    if choice.unit == "Bft" {
        beaufort(value) as f64
    } else {
        value * choice.factor + choice.offset
    }
}
fn resolve<'a>(settings: &Value, declared: &str) -> Option<(&'a Choice, &'a Choice)> {
    let source = CHOICES
        .iter()
        .find(|c| c.unit == declared || c.aliases.contains(&declared))?;
    let mut wanted = json::text(settings, source.quantity);
    if wanted.is_empty() {
        wanted = QUANTITIES.iter().find(|q| q.0 == source.quantity)?.3;
    }
    let chosen = CHOICES
        .iter()
        .find(|c| c.quantity == source.quantity && c.unit == wanted)
        .unwrap_or(source);
    Some((source, chosen))
}
/// Toont zonder keuze exact de originele precisie, behalve de standaard windschaal.
pub fn show<'a>(settings: &Value, value: f64, declared: &'a str) -> (f64, &'a str) {
    let Some((source, chosen)) = resolve(settings, declared) else {
        return (value, declared);
    };
    if source.unit == chosen.unit {
        return (value, source.unit);
    }
    (
        round(to(chosen, from(source, value)), chosen.decimals),
        chosen.unit,
    )
}
/// Rekent gebruikersinvoer terug naar de eenheid die de app declareerde.
pub fn canonical(settings: &Value, value: f64, declared: &str) -> f64 {
    let Some((source, chosen)) = resolve(settings, declared) else {
        return value;
    };
    if source.unit == chosen.unit {
        return value;
    }
    round(to(source, from(chosen, value)), source.decimals + 1)
}
/// Een getypte stap hoort bij de gekozen eenheid, zonder omgerekende fractiestappen.
pub fn step(settings: &Value, declared: &str) -> Option<f64> {
    let (source, chosen) = resolve(settings, declared)?;
    (source.unit != chosen.unit).then_some(chosen.step)
}
/// Alleen aangeboden keuzes mogen in het document landen; aliases zijn declaraties.
pub fn valid(quantity: &str, unit: &str) -> bool {
    (matches!(quantity, "power" | "energy") && unit.is_empty())
        || CHOICES
            .iter()
            .any(|c| c.quantity == quantity && c.unit == unit)
}
/// Past een deelpatch toe zonder andere eenheden te resetten.
pub fn update(settings: &mut Value, patch: &Value) -> Result {
    for (quantity, value) in patch
        .as_object()
        .ok_or(Error::Invalid("units must be an object"))?
        .iter()
    {
        let unit = value
            .as_str()
            .ok_or(Error::Invalid("unit must be a string"))?;
        if !valid(quantity, unit) {
            return Err(Error::Invalid("unit does not belong to quantity"));
        }
        json::set(settings, quantity, json::string(unit)?)?;
    }
    Ok(())
}
/// Vult alleen de impliciete defaults voor de instellingenpagina in.
pub fn filled(settings: &Value) -> Result<Value> {
    use json::TryClone;
    let mut out = settings.try_clone()?;
    for (quantity, _, _, default) in QUANTITIES {
        if json::text(&out, quantity).is_empty() {
            json::set(&mut out, quantity, json::string(default)?)?;
        }
    }
    Ok(out)
}
/// De gebruikersinterface bouwt de lijst uit dezelfde tabel als de berekening.
pub fn offer() -> Result<Value> {
    let mut result = Vec::new();
    for (name, title, canonical, default) in QUANTITIES {
        let mut options = Vec::new();
        if default.is_empty() {
            json::push(
                &mut options,
                json::fields(&[
                    ("unit", json::string("")?),
                    ("title", json::string("zoals de app meldt")?),
                ])?,
                8,
            )?;
        }
        for choice in CHOICES.iter().filter(|c| c.quantity == *name) {
            json::push(
                &mut options,
                json::fields(&[
                    ("unit", json::string(choice.unit)?),
                    ("title", json::string(choice.unit)?),
                ])?,
                8,
            )?;
        }
        json::push(
            &mut result,
            json::fields(&[
                ("name", json::string(name)?),
                ("title", json::string(title)?),
                ("canonical", json::string(canonical)?),
                ("default", json::string(default)?),
                ("options", Value::Array(options)),
            ])?,
            7,
        )?;
    }
    Ok(Value::Array(result))
}

/// Een meting leest als een zin; procenten en graden sluiten direct op het getal aan.
pub fn text(settings: &Value, value: f64, declared: &str) -> Result<alloc::string::String> {
    use core::fmt::Write;
    let (shown, label) = show(settings, value, declared);
    if !shown.is_finite() {
        return Err(Error::Invalid("measurement must be finite"));
    }
    let mut out = alloc::string::String::new();
    // Een eindige f64 in vaste notatie past met beide extremen binnen 1.100 bytes.
    let size = 1100_usize.checked_add(label.len()).ok_or(Error::Full)?;
    out.try_reserve(size).map_err(|_| Error::Memory)?;
    write!(out, "{shown}").map_err(|_| Error::Full)?;
    if !label.is_empty() {
        if !matches!(label, "%" | "°") {
            out.push(' ');
        }
        out.push_str(label);
    }
    Ok(out)
}
