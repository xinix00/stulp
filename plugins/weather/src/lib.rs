//! Weer per locatie, met tienminutenpolling en dezelfde Flow-overgangen als de Go-plugin.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, HttpRequest, Plugin, Result, Transport, clone};
pub mod data;
use data::{Weather, float, number};
struct Location {
    id: String,
    latitude: f64,
    longitude: f64,
    last: Option<Weather>,
    next: u64,
}
struct Pair {
    id: String,
    candidate: Option<Value>,
}
/// De plugin bezit zijn locaties en koppelsessies, geen gedeelde globale singleton.
#[derive(Default)]
pub struct WeatherPlugin {
    locations: Vec<Location>,
    pairs: Vec<Pair>,
}
const FORECAST: &str = "https://api.open-meteo.com/v1/forecast";
const GEOCODING: &str = "https://geocoding-api.open-meteo.com/v1/search";

/// De forecastquery vraagt canonieke wind, twee dagen en acht kwartieren.
pub fn forecast_url(latitude: f64, longitude: f64) -> Result<String> {
    use core::fmt::Write;
    if !(-90.0..=90.0).contains(&latitude) || !(-180.0..=180.0).contains(&longitude) {
        return Err(Error::Invalid("ongeldig coördinaat"));
    }
    let mut out = String::new();
    out.try_reserve(1200)
        .map_err(|_| stulp_core::Error::Memory)?;
    write!(&mut out,"{FORECAST}?latitude={latitude}&longitude={longitude}&current=temperature_2m,relative_humidity_2m,apparent_temperature,is_day,precipitation,rain,snowfall,weather_code,cloud_cover,pressure_msl,wind_speed_10m,wind_direction_10m,wind_gusts_10m,dew_point_2m,visibility,uv_index,soil_temperature_0cm&daily=temperature_2m_max,temperature_2m_min,precipitation_sum,precipitation_hours,precipitation_probability_max,wind_speed_10m_max,wind_gusts_10m_max,uv_index_max,et0_fao_evapotranspiration,sunrise,sunset&minutely_15=precipitation,cape,lightning_potential&forecast_minutely_15=8&wind_speed_unit=ms&timezone=auto&forecast_days=2").map_err(|_|Error::Invalid("forecast URL formatting failed"))?;
    Ok(out)
}
async fn get<T: Transport>(client: &mut Client<T>, url: &str) -> Result<Value> {
    let mut request = HttpRequest::get(url)?;
    json::push(
        &mut request.headers,
        (json::copy("Accept")?, json::copy("application/json")?),
        32,
    )?;
    let response = client.http(request).await?;
    let body = json::parse(&response.body).map_err(stulp_core::Error::from)?;
    if response.status >= 400 {
        let reason = json::text(&body, "reason");
        return Err(if reason.is_empty() {
            Error::Invalid("Open-Meteo weigerde de aanvraag")
        } else {
            Error::Remote(json::copy(reason)?)
        });
    }
    Ok(body)
}
async fn current<T: Transport>(
    client: &mut Client<T>,
    latitude: f64,
    longitude: f64,
) -> Result<Weather> {
    let root = get(client, &forecast_url(latitude, longitude)?).await?;
    Weather::decode(
        json::to_string(&root)
            .map_err(stulp_core::Error::from)?
            .as_bytes(),
    )
}
fn coordinates(value: &Value) -> Result<(f64, f64)> {
    let latitude = number(json::get(value, "latitude")).ok_or(Error::Invalid(
        "er is geen coördinaat om het weer van op te vragen",
    ))?;
    let longitude = number(json::get(value, "longitude")).ok_or(Error::Invalid(
        "er is geen coördinaat om het weer van op te vragen",
    ))?;
    if !(-90.0..=90.0).contains(&latitude) || !(-180.0..=180.0).contains(&longitude) {
        return Err(Error::Invalid("ongeldig coördinaat"));
    }
    Ok((latitude, longitude))
}
fn device_arg(args: &Value) -> &str {
    json::get(args, "device")
        .map(|v| v.as_str().unwrap_or_else(|| json::text(v, "$device")))
        .unwrap_or("")
}

/// Een kaart vuurt bij de grensovergang; een herhaalde meting boven de grens is geen overgang.
pub fn trigger_filter(id: &str, args: &Value, state: &Value) -> Result<bool> {
    let n = |key| number(json::get(state, key)).unwrap_or(0.0);
    match id {
        "rain_started" | "rain_stopped" => Ok(true),
        "weather_changed" => {
            let wanted = json::text(args, "state");
            Ok(wanted.is_empty() || wanted == "any" || wanted == json::text(state, "state"))
        }
        "rain_expected" => {
            let within = number(json::get(args, "within")).unwrap_or(60.0) as i64;
            let (now, was) = match within {
                15 => ("in15", "was15"),
                30 => ("in30", "was30"),
                60 => ("in60", "was60"),
                120 => ("in120", "was120"),
                _ => return Ok(false),
            };
            Ok(n(now) > 0.0 && n(was) == 0.0)
        }
        _ => {
            let (key, down) = match id {
                "wind_changed" | "gust_changed" => ("speed", false),
                "temperature_rose" => ("celsius", false),
                "temperature_fell" => ("celsius", true),
                "uv_changed" => ("index", false),
                "visibility_changed" => ("distance", true),
                "thunder_near" => ("energy", false),
                _ => return Err(Error::Invalid("unknown weather trigger")),
            };
            let Some(limit) = number(json::get(args, key)) else {
                return Ok(false);
            };
            Ok(if down {
                n("now") <= limit && n("was") > limit
            } else {
                n("now") >= limit && n("was") < limit
            })
        }
    }
}
/// Voorwaarden beoordelen het verse weerbericht, inclusief de nacht die nog komt.
pub fn condition(id: &str, args: &Value, weather: &Weather) -> Result<bool> {
    let (argument, value) = match id {
        "is_raining" => return Ok(weather.raining()),
        "is_day" => return Ok(weather.current("is_day") == 1.0),
        "is_sunny" => {
            return Ok(weather.current("is_day") == 1.0
                && weather.current("cloud_cover")
                    < number(json::get(args, "cloud")).unwrap_or(30.0));
        }
        "frost_tonight" => {
            return Ok(weather.tonight() <= number(json::get(args, "celsius")).unwrap_or(0.0));
        }
        "weather_is" => {
            let wanted = json::text(args, "state");
            if wanted.is_empty() {
                return Err(Error::Invalid("kies eerst een weertype"));
            }
            return Ok(data::state(weather.code()) == wanted);
        }
        "wind_above" => ("speed", weather.current("wind_speed_10m")),
        "gust_above" => ("speed", weather.current("wind_gusts_10m")),
        "temperature_above" => ("celsius", weather.current("temperature_2m")),
        "uv_above" => ("index", weather.current("uv_index")),
        "rain_today" => ("chance", weather.daily("precipitation_probability_max", 0)),
        "warmer_today" => ("celsius", weather.daily("temperature_2m_max", 0)),
        "garden_dry" => ("millimetres", weather.irrigation()),
        _ => return Err(Error::Invalid("unknown weather condition")),
    };
    let limit =
        number(json::get(args, argument)).ok_or(Error::Invalid("vul eerst een grens in"))?;
    Ok(value >= limit)
}
impl WeatherPlugin {
    async fn api<T: Transport>(&self, client: &mut Client<T>, params: &Value) -> Result<Value> {
        let body = json::get(params, "body").unwrap_or(&Value::Null);
        match json::text(params, "handler") {
            "status" => {
                let mut found = Vec::new();
                for location in &self.locations {
                    let device = client.state().device(&location.id)?;
                    let mut entry = json::fields(&[
                        ("name", json::string(json::text(device, "name"))?),
                        ("latitude", float(location.latitude)),
                        ("longitude", float(location.longitude)),
                        ("answered", Value::Bool(location.last.is_some())),
                    ])?;
                    if let Some(weather) = &location.last {
                        json::set(
                            &mut entry,
                            "state",
                            json::string(data::state(weather.code()))?,
                        )?;
                        json::set(
                            &mut entry,
                            "description",
                            json::string(&data::describe(weather.code())?)?,
                        )?;
                        json::set(
                            &mut entry,
                            "temperature",
                            stulp_sdk::measure(
                                data::round(weather.current("temperature_2m")),
                                "°C",
                            )?,
                        )?;
                    }
                    json::push(&mut found, entry, 4096)?;
                }
                Ok(json::fields(&[("locations", Value::Array(found))])?)
            }
            "search" => {
                let name = json::text(body, "name");
                let mut found = Vec::new();
                if !name.trim().is_empty() {
                    let name = stulp_sdk::query(name)?;
                    let mut url = json::copy(GEOCODING)?;
                    let suffix = "&count=10&language=nl&format=json";
                    url.try_reserve(name.len() + suffix.len() + 6)
                        .map_err(|_| stulp_core::Error::Memory)?;
                    url.push_str("?name=");
                    url.push_str(&name);
                    url.push_str(suffix);
                    let answer = get(client, &url).await?;
                    for place in json::array(&answer, "results") {
                        let name = json::text(place, "name");
                        let region = json::text(place, "admin1");
                        let code = json::text(place, "country_code");
                        let mut where_ = json::copy(name)?;
                        where_
                            .try_reserve(region.len() + code.len() + 3)
                            .map_err(|_| stulp_core::Error::Memory)?;
                        if (!region.is_empty() && region != name) || !code.is_empty() {
                            where_.push_str(", ");
                        }
                        if !region.is_empty() && region != name {
                            where_.push_str(region);
                            if !code.is_empty() {
                                where_.push(' ');
                            }
                        }
                        where_.push_str(code);
                        json::push(
                            &mut found,
                            json::fields(&[
                                ("name", json::string(name)?),
                                ("where", json::string(&where_)?),
                                (
                                    "latitude",
                                    clone(json::get(place, "latitude").unwrap_or(&Value::Null))?,
                                ),
                                (
                                    "longitude",
                                    clone(json::get(place, "longitude").unwrap_or(&Value::Null))?,
                                ),
                                ("people", Value::uint(json::uint(place, "population"))),
                            ])?,
                            20,
                        )?;
                    }
                }
                Ok(json::fields(&[
                    ("found", Value::uint(found.len() as u64)),
                    ("places", Value::Array(found)),
                ])?)
            }
            "peek" => {
                let (lat, lon) = coordinates(body)?;
                let weather = current(client, lat, lon).await?;
                Ok(json::fields(&[
                    (
                        "description",
                        json::string(&data::describe(weather.code())?)?,
                    ),
                    (
                        "temperature",
                        stulp_sdk::measure(data::round(weather.current("temperature_2m")), "°C")?,
                    ),
                    (
                        "wind",
                        stulp_sdk::measure(data::round(weather.current("wind_speed_10m")), "m/s")?,
                    ),
                    (
                        "direction",
                        json::string(data::compass(weather.current("wind_direction_10m")))?,
                    ),
                    ("raining", Value::Bool(weather.raining())),
                ])?)
            }
            _ => Err(Error::Invalid("unknown weather API handler")),
        }
    }
    fn pair(&mut self, params: &Value) -> Result<Value> {
        let pair = self
            .pairs
            .iter_mut()
            .find(|p| p.id == json::text(params, "sessionId"))
            .ok_or(Error::Invalid("pair session does not exist"))?;
        match json::text(params, "event") {
            "add" => {
                let data = json::get(params, "data").unwrap_or(&Value::Null);
                let (latitude, longitude) = coordinates(data)?;
                let name = json::text(data, "name").trim();
                if name.is_empty() {
                    return Err(Error::Invalid("geef de plek een naam"));
                }
                pair.candidate = Some(json::fields(&[
                    ("name", json::string(name)?),
                    (
                        "data",
                        json::fields(&[
                            ("latitude", float(latitude)),
                            ("longitude", float(longitude)),
                        ])?,
                    ),
                    (
                        "store",
                        json::fields(&[("where", json::string(json::text(data, "where"))?)])?,
                    ),
                ])?);
                Ok(json::fields(&[("name", json::string(name)?)])?)
            }
            "list_devices" => {
                let mut items = Vec::new();
                if let Some(candidate) = &pair.candidate {
                    json::push(&mut items, clone(candidate)?, 1)?;
                }
                Ok(Value::Array(items))
            }
            _ => Err(Error::Invalid("unknown weather pair event")),
        }
    }
}
impl Plugin for WeatherPlugin {
    fn assets(&self) -> &'static [&'static str] {
        &["settings/index.html", "drivers/location/pair/search.html"]
    }
    fn manifest(&self) -> &'static [u8] {
        include_bytes!("../app.json")
    }
    async fn handle<T: Transport>(
        &mut self,
        client: &mut Client<T>,
        method: &str,
        params: &Value,
    ) -> Result<Value> {
        match method {
            "app.init" => Ok(Value::Null),
            "driver.init" => {
                if json::text(params, "driverId") != "location" {
                    return Err(Error::Invalid("unknown weather driver"));
                }
                Ok(Value::Null)
            }
            "device.init" => {
                if json::text(params, "driverId") != "location" {
                    return Err(Error::Invalid("unknown weather driver"));
                }
                let id = json::text(params, "deviceId");
                let device = client.state().device(id)?;
                let (latitude, longitude) =
                    coordinates(json::get(device, "data").unwrap_or(&Value::Null))?;
                self.locations.retain(|l| l.id != id);
                json::push(
                    &mut self.locations,
                    Location {
                        id: json::copy(id)?,
                        latitude,
                        longitude,
                        last: None,
                        next: client.now().saturating_add(2000),
                    },
                    4096,
                )?;
                client
                    .call(
                        "device.set",
                        &json::fields(&[
                            ("deviceId", json::string(id)?),
                            ("field", json::string("class")?),
                            ("value", json::string("weather")?),
                        ])?,
                    )
                    .await?;
                Ok(Value::Null)
            }
            "device.delete" => {
                self.locations
                    .retain(|l| l.id != json::text(params, "deviceId"));
                Ok(Value::Null)
            }
            "api.invoke" => self.api(client, params).await,
            "pair.start" => {
                if json::text(params, "driverId") != "location" {
                    return Err(Error::Invalid("unknown weather driver"));
                }
                let id = json::text(params, "sessionId");
                if id.is_empty() || self.pairs.iter().any(|p| p.id == id) {
                    return Err(Error::Invalid("invalid or duplicate pair session"));
                }
                json::push(
                    &mut self.pairs,
                    Pair {
                        id: json::copy(id)?,
                        candidate: None,
                    },
                    32,
                )?;
                json::parse(br#"["add","list_devices"]"#).map_err(|e| Error::Core(e.into()))
            }
            "pair.emit" => self.pair(params),
            "pair.close" => {
                self.pairs
                    .retain(|p| p.id != json::text(params, "sessionId"));
                Ok(Value::Null)
            }
            "pair.list" => {
                let mut items = Vec::new();
                for pair in &self.pairs {
                    if let Some(value) = &pair.candidate {
                        json::push(&mut items, clone(value)?, 32)?;
                    }
                }
                Ok(Value::Array(items))
            }
            "flow.run" => {
                let args = json::get(params, "args").unwrap_or(&Value::Null);
                let id = json::text(params, "id");
                match json::text(params, "kind") {
                    "trigger" | "device-trigger" => Ok(Value::Bool(trigger_filter(
                        id,
                        args,
                        json::get(params, "state").unwrap_or(&Value::Null),
                    )?)),
                    "condition" => {
                        let location = self
                            .locations
                            .iter()
                            .find(|l| l.id == device_arg(args))
                            .ok_or(Error::Invalid("die locatie hoort niet bij deze app"))?;
                        let weather =
                            current(client, location.latitude, location.longitude).await?;
                        Ok(Value::Bool(condition(id, args, &weather)?))
                    }
                    _ => Err(Error::Invalid("unknown weather card type")),
                }
            }
            "registrations" => stulp_sdk::registrations(
                &json::parse(self.manifest()).map_err(stulp_core::Error::from)?,
            ),
            "ui.asset" => match json::text(params, "path") {
                "settings/index.html" => stulp_sdk::asset(include_bytes!("../settings/index.html")),
                "drivers/location/pair/search.html" => {
                    stulp_sdk::asset(include_bytes!("../drivers/location/pair/search.html"))
                }
                _ => Ok(json::fields(&[("found", Value::Bool(false))])?),
            },
            _ => Err(Error::Invalid("unknown weather method")),
        }
    }
    async fn tick<T: Transport>(&mut self, client: &mut Client<T>) -> Result {
        // Eén locatie per beurt, zodat nieuwe controllercallbacks tussen locaties aan bod komen.
        let Some(location) = self.locations.iter_mut().find(|l| l.next <= client.now()) else {
            return Ok(());
        };
        location.next = client.now().saturating_add(600_000);
        match current(client, location.latitude, location.longitude).await {
            Ok(weather) => {
                client.available(&location.id, true).await?;
                client.values(&location.id, weather.values()?).await?;
                let previous = location.last.replace(weather);
                if let (Some(previous), Some(weather)) = (previous, location.last.as_ref()) {
                    transitions(client, &location.id, &previous, weather).await?;
                }
            }
            Err(error) => {
                client
                    .unavailable(&location.id, &stulp_sdk::message(&error)?)
                    .await?
            }
        }
        Ok(())
    }
}
async fn fire<T: Transport>(
    client: &mut Client<T>,
    device: &str,
    card: &str,
    mut state: Value,
    weather: &Weather,
) -> Result {
    json::set(&mut state, "deviceId", json::string(device)?)?;
    client
        .call(
            "flow.trigger",
            &json::fields(&[
                ("kind", json::string("device-trigger")?),
                ("id", json::string(card)?),
                ("tokens", weather.tokens()?),
                ("state", state),
            ])?,
        )
        .await?;
    Ok(())
}
async fn transitions<T: Transport>(
    client: &mut Client<T>,
    id: &str,
    was: &Weather,
    now: &Weather,
) -> Result {
    if now.raining() != was.raining() {
        fire(
            client,
            id,
            if now.raining() {
                "rain_started"
            } else {
                "rain_stopped"
            },
            json::object(),
            now,
        )
        .await?;
    }
    for (card, key, scale) in [
        ("wind_changed", "wind_speed_10m", 1.0),
        ("gust_changed", "wind_gusts_10m", 1.0),
        ("temperature_rose", "temperature_2m", 1.0),
        ("temperature_fell", "temperature_2m", 1.0),
        ("uv_changed", "uv_index", 1.0),
        ("visibility_changed", "visibility", 0.001),
    ] {
        let a = now.current(key) * scale;
        let b = was.current(key) * scale;
        if a != b {
            fire(
                client,
                id,
                card,
                json::fields(&[("now", float(a)), ("was", float(b))])?,
                now,
            )
            .await?;
        }
    }
    if now.thunder() != was.thunder() {
        fire(
            client,
            id,
            "thunder_near",
            json::fields(&[("now", float(now.thunder())), ("was", float(was.thunder()))])?,
            now,
        )
        .await?;
    }
    let mut state = json::object();
    for (n, a, b) in [
        (15, "in15", "was15"),
        (30, "in30", "was30"),
        (60, "in60", "was60"),
        (120, "in120", "was120"),
    ] {
        json::set(&mut state, a, float(now.rain_within(n)))?;
        json::set(&mut state, b, float(was.rain_within(n)))?;
    }
    fire(client, id, "rain_expected", state, now).await?;
    if data::state(now.code()) != data::state(was.code()) {
        fire(
            client,
            id,
            "weather_changed",
            json::fields(&[("state", json::string(data::state(now.code()))?)])?,
            now,
        )
        .await?;
    }
    Ok(())
}
