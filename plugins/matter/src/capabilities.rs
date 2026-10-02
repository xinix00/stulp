//! Eén register voor Matter-metingen, initiële reads, subscriptions en bedieningsopdrachten.
use crate::{
    im,
    tlv::{Node, Tag, Value, Writer},
};
use alloc::vec::Vec;
use stulp_core::json::{self, Value as Json};
use stulp_sdk::{Error, Result};

#[derive(Clone, Copy)]
enum Decoder {
    Bool,
    Lock,
    Unsigned(f64),
    Signed(f64),
    Illuminance,
    Motion,
    Battery,
    Energy,
    Alarm,
    Concentration,
    AirQuality,
}
#[derive(Clone, Copy)]
enum Filter {
    Always,
    NoTemperature,
    Boolean(&'static str),
}
/// Een gedeelde mapping behoudt optionaliteit en de concrete clusteridentiteit.
pub struct Mapping {
    /// Stulp-capability, zonder eventueel endpointvolgnummer.
    pub capability: &'static str,
    /// Servercluster.
    pub cluster: u32,
    /// Meetattribuut.
    pub attribute: u32,
    /// UnsupportedAttribute betekent afwezig in plaats van een defect apparaat.
    pub optional: bool,
    /// Ondersteunde commands, ook voor inventarisdiagnostiek.
    pub commands: &'static [u32],
    decoder: Decoder,
    filter: Filter,
}
const fn mapping(
    capability: &'static str,
    cluster: u32,
    attribute: u32,
    optional: bool,
    decoder: Decoder,
    commands: &'static [u32],
    filter: Filter,
) -> Mapping {
    Mapping {
        capability,
        cluster,
        attribute,
        optional,
        decoder,
        commands,
        filter,
    }
}
use Decoder::*;
use Filter::*;
/// De volgorde en voorkeurscluster volgen de oorspronkelijke Go-controller.
pub static MAPPINGS: &[Mapping] = &[
    mapping("onoff", 6, 0, false, Bool, &[0, 1], Always),
    mapping("dim", 8, 0, false, Unsigned(254.0), &[4], Always),
    mapping("locked", 0x101, 0, false, Lock, &[0, 1], Always),
    mapping(
        "measure_temperature",
        0x402,
        0,
        false,
        Signed(100.0),
        &[],
        Always,
    ),
    mapping(
        "measure_temperature",
        0x201,
        0,
        false,
        Signed(100.0),
        &[],
        NoTemperature,
    ),
    mapping(
        "measure_luminance",
        0x400,
        0,
        false,
        Illuminance,
        &[],
        Always,
    ),
    mapping(
        "measure_humidity",
        0x405,
        0,
        false,
        Unsigned(100.0),
        &[],
        Always,
    ),
    mapping(
        "measure_pressure",
        0x403,
        0,
        false,
        Signed(10.0),
        &[],
        Always,
    ),
    mapping(
        "measure_water",
        0x404,
        0,
        false,
        Unsigned(10.0),
        &[],
        Always,
    ),
    mapping("alarm_motion", 0x406, 0, false, Motion, &[], Always),
    mapping(
        "alarm_contact",
        0x45,
        0,
        false,
        Bool,
        &[],
        Boolean("alarm_contact"),
    ),
    mapping(
        "alarm_motion",
        0x45,
        0,
        false,
        Bool,
        &[],
        Boolean("alarm_motion"),
    ),
    mapping(
        "alarm_water",
        0x45,
        0,
        false,
        Bool,
        &[],
        Boolean("alarm_water"),
    ),
    mapping(
        "alarm_generic",
        0x45,
        0,
        false,
        Bool,
        &[],
        Boolean("alarm_generic"),
    ),
    mapping("measure_battery", 0x2f, 0xc, true, Battery, &[], Always),
    mapping(
        "measure_voltage",
        0x90,
        4,
        true,
        Signed(1000.0),
        &[],
        Always,
    ),
    mapping(
        "measure_current",
        0x90,
        5,
        true,
        Signed(1000.0),
        &[],
        Always,
    ),
    mapping("measure_power", 0x90, 8, false, Signed(1000.0), &[], Always),
    mapping("meter_power", 0x91, 1, true, Energy, &[], Always),
    mapping(
        "light_hue",
        0x300,
        0,
        true,
        Unsigned(254.0),
        &[0, 6],
        Always,
    ),
    mapping(
        "light_saturation",
        0x300,
        1,
        true,
        Unsigned(254.0),
        &[3],
        Always,
    ),
    mapping("alarm_smoke", 0x5c, 1, true, Alarm, &[], Always),
    mapping("alarm_co", 0x5c, 2, true, Alarm, &[], Always),
    mapping("alarm_battery", 0x5c, 3, false, Alarm, &[], Always),
    mapping("measure_co2", 0x40d, 0, true, Concentration, &[], Always),
    mapping("measure_pm25", 0x42a, 0, true, Concentration, &[], Always),
    mapping("air_quality_state", 0x5b, 0, false, AirQuality, &[], Always),
];
fn boolean_capability(types: &[u32]) -> &'static str {
    for t in types {
        match t {
            0x107 => return "alarm_motion",
            0x43 | 0x44 => return "alarm_water",
            0x41 => return "alarm_generic",
            0x15 => return "alarm_contact",
            _ => (),
        }
    }
    "alarm_contact"
}
/// Herhaalde functies zoals onoff.2 gebruiken hetzelfde wirecontract.
pub fn base(capability: &str) -> &str {
    capability.split('.').next().unwrap_or(capability)
}
/// Bediening blijft mogelijk voor oude records zonder bewaarde clusterlijst.
pub fn for_capability(
    types: &[u32],
    servers: &[u32],
    capability: &str,
) -> Option<&'static Mapping> {
    MAPPINGS.iter().find(|m| {
        m.capability == base(capability)
            && (servers.is_empty() || servers.contains(&m.cluster))
            && m.applies(types, servers)
    })
}
/// Reports en initiële reads gebruiken precies dezelfde typefilters.
pub fn for_report(
    types: &[u32],
    servers: &[u32],
    cluster: u32,
    attribute: u32,
) -> Option<&'static Mapping> {
    MAPPINGS
        .iter()
        .find(|m| m.cluster == cluster && m.attribute == attribute && m.applies(types, servers))
}
/// Switch-events leveren een knop op, ook zonder meetattribuut.
pub fn mapped_cluster(cluster: u32) -> bool {
    cluster == 0x3b || MAPPINGS.iter().any(|m| m.cluster == cluster)
}
impl Mapping {
    /// Context voorkomt dubbele temperatuurmetingen en verkeerde Boolean State-sensortypen.
    pub fn applies(&self, types: &[u32], servers: &[u32]) -> bool {
        match self.filter {
            Always => true,
            NoTemperature => !servers.contains(&0x402),
            Boolean(cap) => boolean_capability(types) == cap,
        }
    }
    /// Null behoudt de capability zonder een verzonnen nulmeting; foute typen geven een fout.
    pub fn decode(&self, n: &Node<'_>) -> Result<Option<Json>> {
        if n.element.value == Value::Null {
            return Ok(None);
        }
        let v = n.element.value;
        let out = match (self.decoder, v) {
            (Bool, Value::Bool(b)) => Json::Bool(b),
            (Bool, Value::Uint(n)) => Json::Bool(n != 0),
            (Lock, Value::Uint(n)) => Json::Bool(n == 1),
            (Unsigned(divisor) | Signed(divisor), Value::Uint(n)) => number(n as f64 / divisor)?,
            (Signed(divisor), Value::Int(n)) => number(n as f64 / divisor)?,
            (Illuminance, Value::Uint(n)) if n <= 0xfffe => number(lux(n as u16))?,
            (Motion, Value::Uint(n)) => Json::Bool(n & 1 != 0),
            (Battery, Value::Uint(n)) => number((n as f64 / 2.0).min(100.0))?,
            (Energy, Value::Structure) => number(
                energy(
                    n.get(0)
                        .ok_or(Error::Invalid("missing cumulative energy"))?
                        .element
                        .value,
                )? / 1_000_000.0,
            )?,
            (Alarm, Value::Uint(n)) if n <= 2 => Json::Bool(n != 0),
            (Concentration, v) => {
                let value = nonnegative(v)?;
                // Boven 2^52 heeft de invoer al geen fractie meer; vermijd overflow en saturerende casts.
                let scaled = value * 100.0;
                number(if scaled < 4_503_599_627_370_496.0 {
                    ((scaled + 0.5) as u64) as f64 / 100.0
                } else {
                    value
                })?
            }
            (AirQuality, Value::Uint(n)) if n <= 6 => json::string(
                [
                    "unknown",
                    "good",
                    "fair",
                    "moderate",
                    "poor",
                    "very_poor",
                    "extremely_poor",
                ][n as usize],
            )?,
            _ => return Err(Error::Invalid("unsupported Matter capability value")),
        };
        Ok(Some(out))
    }
    /// Verpakt een concrete opdracht, met timed interaction voor een deurslot.
    pub fn command(&self, endpoint: u16, value: &Json) -> Result<Command> {
        let mut w = Writer::default();
        w.start(Tag::Anonymous, Value::Structure)?;
        let command = match self.capability {
            "onoff" => u32::from(
                value
                    .as_bool()
                    .ok_or(Error::Invalid("onoff needs a boolean"))?,
            ),
            "locked" => u32::from(
                !value
                    .as_bool()
                    .ok_or(Error::Invalid("locked needs a boolean"))?,
            ),
            "dim" => {
                w.uint_width(Tag::Context(0), level(value)?, 1)?;
                w.uint_width(Tag::Context(1), 0, 2)?;
                4
            }
            "light_hue" => {
                w.uint_width(Tag::Context(0), level(value)?, 1)?;
                w.uint_width(Tag::Context(1), 0, 1)?;
                transition(&mut w, 2)?;
                0
            }
            "light_saturation" => {
                w.uint_width(Tag::Context(0), level(value)?, 1)?;
                transition(&mut w, 1)?;
                3
            }
            _ => {
                return Err(Error::Invalid(
                    "Matter capability is read-only or unsupported",
                ));
            }
        };
        w.end()?;
        Ok(Command {
            path: im::CommandPath {
                endpoint,
                cluster: self.cluster,
                command,
            },
            fields: w.finish()?,
            timed: self.capability == "locked",
        })
    }
}
fn number(n: f64) -> Result<Json> {
    if n.is_finite() {
        Ok(Json::Number(json::Number::Float(n)))
    } else {
        Err(Error::Invalid("non-finite Matter measurement"))
    }
}
fn energy(v: Value<'_>) -> Result<f64> {
    if !matches!(v, Value::Int(_) | Value::Uint(_)) {
        return Err(Error::Invalid("energy is not integer"));
    }
    nonnegative(v)
}
fn nonnegative(v: Value<'_>) -> Result<f64> {
    let n = match v {
        Value::Int(n) => n as f64,
        Value::Uint(n) => n as f64,
        Value::Float(n) => n,
        _ => return Err(Error::Invalid("Matter measurement is not numeric")),
    };
    if !n.is_finite() || n < 0.0 {
        return Err(Error::Invalid("negative or non-finite Matter measurement"));
    }
    Ok(n)
}
/// Beperkte exp10 voor Matter's 16-bit lichtmeting: Taylor op [0, ln(2)), daarna exacte machten van twee.
fn lux(raw: u16) -> f64 {
    if raw == 0 {
        return 0.0;
    }
    let exponent = (f64::from(raw) - 1.0) / 10000.0 * core::f64::consts::LN_10;
    let power = (exponent / core::f64::consts::LN_2) as u32;
    let remainder = exponent - f64::from(power) * core::f64::consts::LN_2;
    let mut term = 1.0;
    let mut sum = 1.0;
    for n in 1..=16 {
        term *= remainder / f64::from(n);
        sum += term;
    }
    sum * f64::from(1u32 << power)
}
fn level(value: &Json) -> Result<u64> {
    let n = stulp_sdk::util::number(value).ok_or(Error::Invalid(
        "Matter level needs a number between 0 and 1",
    ))?;
    if !n.is_finite() || !(0.0..=1.0).contains(&n) {
        return Err(Error::Invalid(
            "Matter level needs a number between 0 and 1",
        ));
    }
    Ok((n * 254.0 + 0.5) as u64)
}
fn transition(w: &mut Writer, tag: u8) -> Result {
    w.uint_width(Tag::Context(tag), 0, 2)?;
    w.uint_width(Tag::Context(tag + 1), 0, 1)?;
    w.uint_width(Tag::Context(tag + 2), 0, 1)
}
/// Een owned command leent pas tijdens de IM-transactie zijn velden.
pub struct Command {
    /// Concreet bestemmingspad.
    pub path: im::CommandPath,
    /// Anonieme TLV-structure, ook voor opdrachten zonder velden.
    pub fields: Vec<u8>,
    /// Een deurslot vereist de timed handshake in hetzelfde exchange.
    pub timed: bool,
}
impl Command {
    /// Beide kleurcomponenten gaan tegelijk naar de lamp zodat geen tussenkleur verschijnt.
    pub fn hue_saturation(endpoint: u16, hue: &Json, saturation: &Json) -> Result<Self> {
        let mut w = Writer::default();
        w.start(Tag::Anonymous, Value::Structure)?;
        w.uint_width(Tag::Context(0), level(hue)?, 1)?;
        w.uint_width(Tag::Context(1), level(saturation)?, 1)?;
        transition(&mut w, 2)?;
        w.end()?;
        Ok(Self {
            path: im::CommandPath {
                endpoint,
                cluster: 0x300,
                command: 6,
            },
            fields: w.finish()?,
            timed: false,
        })
    }
    /// Leent de gecodeerde structure voor InvokeRequest.
    pub fn borrow(&self) -> im::Command<'_> {
        im::Command {
            path: self.path,
            fields: &self.fields,
            reference: None,
        }
    }
}
/// Device types blijven leidend, daarna pas de clusters als compatibiliteitsfallback.
pub fn class(types: &[u32], servers: &[u32]) -> &'static str {
    for t in types {
        match t {
            0x100 | 0x10d => return "light",
            0x10a | 0x10b => return "socket",
            0xa => return "lock",
            0x301 => return "thermostat",
            0x302 | 0x307 | 0x15 | 0x2c | 0x106 | 0x107 | 0x305 | 0x306 | 0x41 | 0x43 | 0x44
            | 0x76 => return "sensor",
            _ => (),
        }
    }
    if servers.contains(&0x101) {
        "lock"
    } else if servers.contains(&0x201) {
        "thermostat"
    } else if servers.contains(&6) {
        "light"
    } else {
        "sensor"
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn logarithmic_light_agrees_with_host_for_every_wire_value() {
        assert_eq!(lux(0), 0.0);
        for raw in 1..=0xfffe {
            let expected = 10.0_f64.powf((f64::from(raw) - 1.0) / 10000.0);
            assert!((lux(raw) / expected - 1.0).abs() < 5e-15, "{raw}");
        }
    }
    #[test]
    fn nullable_and_scaled_measurements_retain_type_rules() -> Result {
        fn decode(cap: &str, wire: Value<'_>) -> Result<Option<Json>> {
            let n = Node {
                element: crate::tlv::Element {
                    tag: Tag::Anonymous,
                    value: wire,
                },
                children: Vec::new(),
            };
            for_capability(&[], &[], cap)
                .ok_or(Error::Invalid("missing test mapping"))?
                .decode(&n)
        }
        assert_eq!(
            decode("measure_temperature", Value::Int(-250))?,
            Some(number(-2.5)?)
        );
        assert_eq!(
            decode("measure_co2", Value::Float(f64::from(3.1_f32)))?,
            Some(number(3.1)?)
        );
        assert_eq!(
            decode("measure_battery", Value::Uint(255))?,
            Some(number(100.0)?)
        );
        assert!(decode("measure_luminance", Value::Uint(0xffff)).is_err());
        assert!(decode("alarm_smoke", Value::Uint(3)).is_err());
        assert!(decode("measure_humidity", Value::Int(1)).is_err());
        assert!(decode("measure_co2", Value::Float(f64::NAN)).is_err());
        assert_eq!(decode("measure_temperature", Value::Null)?, None);
        assert_eq!(
            for_report(&[0x43], &[0x45], 0x45, 0).map(|m| m.capability),
            Some("alarm_water")
        );
        assert!(for_report(&[], &[0x402, 0x201], 0x201, 0).is_none());
        Ok(())
    }
    #[test]
    fn commands_preserve_rounding_timed_lock_and_combined_color() -> Result {
        let dim = for_capability(&[], &[8], "dim.2").ok_or(Error::Invalid("missing dim"))?;
        let command = dim.command(42, &number(0.5)?)?;
        assert_eq!(
            (command.path.endpoint, command.path.command, command.timed),
            (42, 4, false)
        );
        assert_eq!(Node::parse(&command.fields)?.uint(0)?, 127);
        assert!(dim.command(42, &number(1.01)?).is_err());
        let lock = for_capability(&[], &[], "locked")
            .ok_or(Error::Invalid("missing lock"))?
            .command(1, &Json::Bool(true))?;
        assert!(lock.timed);
        assert_eq!(lock.path.command, 0);
        let color = Command::hue_saturation(2, &number(1.0)?, &number(0.5)?)?;
        assert_eq!(color.path.command, 6);
        let fields = Node::parse(&color.fields)?;
        assert_eq!((fields.uint(0)?, fields.uint(1)?), (254, 127));
        Ok(())
    }
}
