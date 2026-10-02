//! De zeven drivers verschillen alleen in typeherkenning en de richting van de luifel.
use stulp_core::json::{self, Value};
use stulp_sdk::{Error, Result};
const TYPES: &[(&str, &[&str])] = &[
    (
        "io_vertical_exterior_blind",
        &["io:VerticalExteriorAwningIOComponent"],
    ),
    (
        "io_exterior_venetian_blind",
        &["ogp:Blind", "io:ExteriorVenetianBlindIOComponent"],
    ),
    (
        "io_roller_shutter",
        &[
            "ogp:Shutter",
            "io:RollerShutterGenericIOComponent",
            "io:RollerShutterWithLowSpeedManagementIOComponent",
            "io:Re3js3W69CrGF8kKXvvmYtT4zNGqicXRjvuAnmmbvPZXnt",
        ],
    ),
    (
        "io_velux_roller_shutter",
        &["io:RollerShutterVeluxIOComponent"],
    ),
    (
        "io_velux_interior_blind",
        &["io:VerticalInteriorBlindVeluxIOComponent"],
    ),
    ("io_velux_roof_window", &["io:WindowOpenerVeluxIOComponent"]),
    ("io_horizontal_awning", &["io:HorizontalAwningIOComponent"]),
];
pub(super) fn driver(name: &str) -> Option<&'static str> {
    TYPES
        .iter()
        .find(|(_, names)| names.contains(&name))
        .map(|(id, _)| *id)
}
pub(super) fn valid(id: &str) -> bool {
    TYPES.iter().any(|(name, _)| *name == id)
}
/// De horizontale luifel keert uitsluitend de richting om, nooit de sluitingsas.
pub fn command(driver: &str, state: &str) -> Result<&'static str> {
    if !valid(driver) {
        return Err(Error::Invalid("unknown covering driver"));
    }
    match (driver == "io_horizontal_awning", state) {
        (false, "up") | (true, "down") => Ok("open"),
        (false, "down") | (true, "up") => Ok("close"),
        _ => Err(Error::Invalid("unknown covering direction")),
    }
}
/// Een gesloten percentage wordt een geopende fractie.
pub fn position(closure: f64) -> f64 {
    if closure.is_nan() {
        0.0
    } else {
        (1.0 - closure / 100.0).clamp(0.0, 1.0)
    }
}
/// TaHoma verwacht gehele sluitingspercenten, afgerond naar het dichtstbijzijnde getal.
pub fn closure(position: f64) -> u64 {
    let p = if position.is_nan() {
        0.0
    } else {
        position.clamp(0.0, 1.0)
    };
    ((1.0 - p) * 100.0 + 0.5) as u64
}
pub(super) fn values(device: &Value, driver: &str) -> Result<Value> {
    let state = |name| {
        json::array(device, "states")
            .iter()
            .find(|v| json::text(v, "name") == name)
            .and_then(|v| json::get(v, "value"))
    };
    let closure = state("core:ClosureState")
        .and_then(crate::number)
        .filter(|v| v.is_finite());
    let text = state("core:OpenClosedState")
        .and_then(Value::as_str)
        .unwrap_or("");
    let mut result = json::object();
    if let Some(c) = closure {
        json::set(
            &mut result,
            "windowcoverings_set",
            Value::Number(json::Number::Float(position(c))),
        )?;
    }
    let direction = if closure.is_some_and(|c| c != 0.0 && c != 100.0) {
        "idle"
    } else {
        match (driver == "io_horizontal_awning", text) {
            (false, "open") | (true, "closed") => "up",
            (false, "closed") | (true, "open") => "down",
            _ => "",
        }
    };
    if !direction.is_empty() {
        json::set(
            &mut result,
            "windowcoverings_state",
            json::string(direction)?,
        )?;
    }
    Ok(result)
}
