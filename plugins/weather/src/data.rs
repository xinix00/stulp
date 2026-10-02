//! Open-Meteo-waarden blijven canoniek; drempels kijken naar een echte overgang.
use alloc::string::String;
use stulp_core::json::{self, Value};
use stulp_sdk::{Error, Result};
/// Eén begrensd antwoord, met de originele eenheden en tijdzoneverschuiving.
pub struct Weather {
    pub(crate) root: Value,
}
impl Weather {
    /// Een antwoord zonder current is geen bruikbaar weerbericht.
    pub fn decode(bytes: &[u8]) -> Result<Self> {
        let root = json::parse(bytes).map_err(stulp_core::Error::from)?;
        if json::get(&root, "current")
            .and_then(Value::as_object)
            .is_none()
        {
            return Err(Error::Invalid(
                "Open-Meteo stuurde geen bruikbaar weerbericht",
            ));
        }
        if json::array(
            json::get(&root, "minutely_15").unwrap_or(&Value::Null),
            "time",
        )
        .len()
            > 512
        {
            return Err(Error::Invalid("too many forecast quarters"));
        }
        Ok(Self { root })
    }
    /// Huidige meting, met de Go-nulwaarde voor ontbrekende optionele velden.
    pub fn current(&self, key: &str) -> f64 {
        number(json::get(&self.root, "current").and_then(|v| json::get(v, key))).unwrap_or(0.0)
    }
    /// Dagwaarden blijven op datumvolgorde, morgen is index één.
    pub fn daily(&self, key: &str, index: usize) -> f64 {
        number(json::get(&self.root, "daily").and_then(|v| json::array(v, key).get(index)))
            .unwrap_or(0.0)
    }
    /// Het minimum van morgen beschrijft de nacht die nog komt.
    pub fn tonight(&self) -> f64 {
        let days = json::get(&self.root, "daily")
            .map(|v| json::array(v, "temperature_2m_min"))
            .unwrap_or(&[]);
        number(days.get(1).or_else(|| days.first())).unwrap_or(0.0)
    }
    /// Neerslag per kwartier tot en met de opgegeven grens.
    pub fn rain_within(&self, minutes: u64) -> f64 {
        let current = json::get(&self.root, "current").unwrap_or(&Value::Null);
        let at = local_time(json::text(current, "time"));
        let limit = at.saturating_add(minutes.saturating_mul(60));
        let quarters = json::get(&self.root, "minutely_15").unwrap_or(&Value::Null);
        let mut total = 0.0;
        for (i, time) in json::array(quarters, "time").iter().enumerate() {
            if local_time(time.as_str().unwrap_or("")) > limit {
                break;
            }
            total += number(json::array(quarters, "precipitation").get(i)).unwrap_or(0.0);
        }
        total
    }
    /// De hoogste CAPE of geschaalde lightning-potential van de komende kwartieren.
    pub fn thunder(&self) -> f64 {
        let quarters = json::get(&self.root, "minutely_15").unwrap_or(&Value::Null);
        let mut highest = 0.0_f64;
        for (i, _) in json::array(quarters, "time").iter().enumerate() {
            highest = highest.max(number(json::array(quarters, "cape").get(i)).unwrap_or(0.0));
            highest = highest.max(
                number(json::array(quarters, "lightning_potential").get(i)).unwrap_or(0.0) * 100.0,
            );
        }
        highest
    }
    /// Een tuin heeft geen negatief tekort.
    pub fn irrigation(&self) -> f64 {
        (self.daily("et0_fao_evapotranspiration", 0) - self.daily("precipitation_sum", 0)).max(0.0)
    }
    /// Neerslag omvat regen, hagel en sneeuw.
    pub fn raining(&self) -> bool {
        self.current("precipitation") > 0.0
    }
    /// De actuele WMO-code.
    pub fn code(&self) -> i64 {
        self.current("weather_code") as i64
    }
    /// Browserwaarden worden één keer op één decimaal afgerond.
    pub fn values(&self) -> Result<Value> {
        let mut out = json::object();
        for (cap, key) in [
            ("measure_temperature", "temperature_2m"),
            ("measure_temperature.feels", "apparent_temperature"),
            ("measure_humidity", "relative_humidity_2m"),
            ("measure_pressure", "pressure_msl"),
            ("measure_rain", "precipitation"),
            ("measure_wind_strength", "wind_speed_10m"),
            ("measure_gust_strength", "wind_gusts_10m"),
            ("cloud_cover", "cloud_cover"),
            ("measure_ultraviolet", "uv_index"),
            ("measure_temperature.dewpoint", "dew_point_2m"),
            ("measure_temperature.soil", "soil_temperature_0cm"),
        ] {
            json::set(&mut out, cap, float(round(self.current(key))))?;
        }
        for (key, n) in [
            (
                "measure_wind_angle",
                round(self.current("wind_direction_10m") * 0.1) * 10.0,
            ),
            ("visibility", round(self.current("visibility") / 1000.0)),
            (
                "rain_chance",
                round(self.daily("precipitation_probability_max", 0)),
            ),
            ("irrigation_need", round(self.irrigation())),
        ] {
            json::set(&mut out, key, float(n))?;
        }
        json::set(&mut out, "weather_state", json::string(state(self.code()))?)?;
        json::set(
            &mut out,
            "weather_description",
            json::string(&describe(self.code())?)?,
        )?;
        Ok(out)
    }
    /// Alle Flow-tokens blijven in de door app.json aangegeven eenheid.
    pub fn tokens(&self) -> Result<Value> {
        let mut out = json::fields(&[
            ("description", json::string(&describe(self.code())?)?),
            (
                "wind",
                json::string(compass(self.current("wind_direction_10m")))?,
            ),
            ("code", Value::Number(json::Number::Int(self.code()))),
        ])?;
        for (key, n) in [
            ("temperature", self.current("temperature_2m")),
            ("wind_speed", self.current("wind_speed_10m")),
            ("gust_speed", self.current("wind_gusts_10m")),
            ("rain", self.current("precipitation")),
            ("uv", self.current("uv_index")),
            ("humidity", self.current("relative_humidity_2m")),
            ("rain_soon", self.rain_within(60)),
            ("max_today", self.daily("temperature_2m_max", 0)),
            ("min_tonight", self.tonight()),
        ] {
            json::set(&mut out, key, float(round(n)))?;
        }
        Ok(out)
    }
}
pub(crate) fn float(n: f64) -> Value {
    Value::Number(json::Number::Float(n))
}
/// Getalvelden en keuzelijsten mogen dezelfde grens vertegenwoordigen.
pub fn number(value: Option<&Value>) -> Option<f64> {
    let n = match value? {
        Value::Number(n) => n.as_f64(),
        Value::String(s) => s.trim().parse::<f64>().ok()?,
        _ => return None,
    };
    n.is_finite().then_some(n)
}
/// Afronden als Go math.Round, dus halve waarden van nul af.
pub fn round(n: f64) -> f64 {
    if n.abs() > 9e15 {
        return n;
    }
    let n = n * 10.0;
    ((n + if n >= 0.0 { 0.5 } else { -0.5 }) as i64) as f64 / 10.0
}
fn local_time(text: &str) -> u64 {
    // Alle tijden in één antwoord hebben dezelfde offset, die valt uit de vergelijking.
    let mut bytes = [0u8; 20];
    let normalized = match text.len() {
        10 => "T00:00:00Z",
        16 => ":00Z",
        19 => "Z",
        _ => return 0,
    };
    let Some(head) = bytes.get_mut(..text.len()) else {
        return 0;
    };
    head.copy_from_slice(text.as_bytes());
    let Some(tail) = bytes.get_mut(text.len()..) else {
        return 0;
    };
    tail.copy_from_slice(normalized.as_bytes());
    core::str::from_utf8(&bytes)
        .ok()
        .and_then(json::unix_seconds)
        .unwrap_or(0)
}
/// De zestien Nederlandse kompasstreken, rond en voorbij nul graden.
pub fn compass(degrees: f64) -> &'static str {
    const POINTS: [&str; 16] = [
        "N", "NNO", "NO", "ONO", "O", "OZO", "ZO", "ZZO", "Z", "ZZW", "ZW", "WZW", "W", "WNW",
        "NW", "NNW",
    ];
    if !degrees.is_finite() {
        return "";
    }
    let n = (degrees % 360.0) / 22.5;
    let index = ((n + if n >= 0.0 { 0.5 } else { -0.5 }) as i64).rem_euclid(16) as usize;
    POINTS.get(index).copied().unwrap_or("")
}
/// Dezelfde tien toestanden als de manifestkeuzelijst.
pub fn state(code: i64) -> &'static str {
    match code {
        0 => "clear",
        1 | 2 => "partly",
        3 => "cloudy",
        45 | 48 => "fog",
        51 | 53 | 55 => "drizzle",
        56 | 57 | 66 | 67 => "freezing",
        61 | 63 | 65 => "rain",
        71 | 73 | 75 | 77 | 85 | 86 => "snow",
        80..=82 => "showers",
        95 | 96 | 99 => "thunderstorm",
        _ => "unknown",
    }
}
/// Volledige WMO-beschrijving; onbekende codes behouden hun nummer.
pub fn describe(code: i64) -> Result<String> {
    let text = match code {
        0 => "Onbewolkt",
        1 => "Vrijwel onbewolkt",
        2 => "Half bewolkt",
        3 => "Zwaar bewolkt",
        45 => "Nevel",
        48 => "Mist met rijpaanslag",
        51 => "Lichte motregen",
        53 => "Motregen",
        55 => "Zware motregen",
        56 => "Lichte ijzel",
        57 => "Zware ijzel",
        61 => "Lichte regen",
        63 => "Regen",
        65 => "Zware regen",
        66 => "Lichte ijzelregen",
        67 => "Zware ijzelregen",
        71 => "Lichte sneeuwval",
        73 => "Sneeuwval",
        75 => "Zware sneeuwval",
        77 => "Sneeuwkorrels",
        80 => "Lichte buien",
        81 => "Buien",
        82 => "Zware buien",
        85 => "Lichte sneeuwbuien",
        86 => "Zware sneeuwbuien",
        95 => "Onweer",
        96 => "Onweer met lichte hagel",
        99 => "Onweer met zware hagel",
        _ => {
            use core::fmt::Write;
            let mut out = String::new();
            out.try_reserve(64).map_err(|_| stulp_core::Error::Memory)?;
            write!(&mut out, "Onbekend weertype (WMO {code})")
                .map_err(|_| Error::Invalid("description formatting failed"))?;
            return Ok(out);
        }
    };
    Ok(json::copy(text)?)
}
