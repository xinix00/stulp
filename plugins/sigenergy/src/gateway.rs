//! Gateway-opdrachten hebben een eenmalige voorbereiding en een begrensde readback.
use super::cloud::{self, Cloud};
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, Transport, clone,
    util::{field, join},
};
enum Command {
    Prepared {
        off: bool,
        expires: u64,
        generation: u64,
    },
    Readback {
        off: bool,
        deadline: u64,
        definite: bool,
        error: String,
    },
}
pub(super) struct Gateway {
    pub(super) id: String,
    pub(super) station: u64,
    pub(super) next: u64,
    command: Option<Command>,
}
impl Gateway {
    pub(super) fn new(id: &str, station: u64, next: u64) -> Result<Self> {
        Ok(Self {
            id: json::copy(id)?,
            station,
            next,
            command: None,
        })
    }
    pub(super) fn cancel(&mut self) {
        self.command = None;
        self.next = 0;
    }
    async fn apply<T: Transport>(&self, c: &mut Client<T>, v: &Value) -> Result {
        c.values(&self.id, cloud::values(v)?).await?;
        c.available(&self.id, true).await
    }
    pub(super) async fn prepare<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        cloud: &mut Cloud,
        off: bool,
    ) -> Result {
        if self.command.is_some() {
            return Err(Error::Invalid(
                "Stulp verwerkt nog een eerdere Gateway-schakeling.",
            ));
        }
        let v = cloud.preflight(c, self.station, off).await?;
        if cloud::reached(&v, off) {
            return self.apply(c, &v).await;
        }
        self.command = Some(Command::Prepared {
            off,
            expires: c.now().saturating_add(120_000),
            generation: cloud.generation,
        });
        self.next = c.now();
        Ok(())
    }
    pub(super) async fn poll<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        cloud: &mut Cloud,
    ) -> Result {
        self.next = c.now().saturating_add(30_000);
        match self.command.take() {
            Some(Command::Prepared {
                off,
                expires,
                generation,
            }) => {
                if c.now() >= expires || generation != cloud.generation {
                    return Err(Error::Invalid(
                        "De Gateway-voorbereiding is verlopen of vervangen.",
                    ));
                }
                // Herlezen vlak vóór schrijven. Een oude toestand kan de contactor nooit bedienen.
                let v = cloud.preflight(c, self.station, off).await?;
                if cloud::reached(&v, off) {
                    return self.apply(c, &v).await;
                }
                let body = json::fields(&[
                    ("onGridState", Value::uint(u64::from(off))),
                    ("stationId", Value::uint(self.station)),
                ])?;
                // De voorbereiding is al verbruikt. Ook een transportfout kan deze POST niet herhalen.
                let result = cloud
                    .request(
                        c,
                        "POST",
                        "/device/gateway/ongrid-state/update",
                        &body,
                        true,
                    )
                    .await;
                let (definite, error) = match result {
                    Ok(_) => (false, String::new()),
                    Err(e) => (matches!(e, Error::Remote(_)), stulp_sdk::message(&e)?),
                };
                self.command = Some(Command::Readback {
                    off,
                    deadline: c.now().saturating_add(30_000),
                    definite,
                    error,
                });
                self.next = c.now().saturating_add(1000);
                Ok(())
            }
            Some(Command::Readback {
                off,
                deadline,
                definite,
                error,
            }) => {
                if c.now() >= deadline {
                    return Err(Error::Invalid(
                        "Gateway bereikte de gevraagde stand niet binnen de wachttijd; de opdracht is niet herhaald.",
                    ));
                }
                let v = cloud.gateway(c, self.station).await?;
                self.apply(c, &v).await?;
                if cloud::reached(&v, off) {
                    return Ok(());
                }
                if definite {
                    return Err(Error::Remote(error));
                }
                if (3..=5).contains(&cloud::manual(&v)) {
                    return Err(Error::Invalid(
                        "Gateway stopte de overgang met een foutstatus.",
                    ));
                }
                if !off && cloud::grid(&v) == 1 {
                    return Err(Error::Invalid(
                        "Gateway staat automatisch off-grid; wacht tot het net terug is.",
                    ));
                }
                self.command = Some(Command::Readback {
                    off,
                    deadline,
                    definite,
                    error,
                });
                self.next = c.now().saturating_add(1000);
                Ok(())
            }
            None => {
                let v = cloud.gateway(c, self.station).await?;
                self.apply(c, &v).await
            }
        }
    }
}
pub(super) async fn stations<T: Transport>(c: &mut Client<T>, cloud: &mut Cloud) -> Result<Value> {
    cloud
        .request(c, "GET", "/device/owner/station/list", &Value::Null, true)
        .await
}
pub(super) async fn describe<T: Transport>(
    c: &mut Client<T>,
    cloud: &mut Cloud,
    stations: &Value,
    pair: bool,
) -> Result<Value> {
    let mut list = Vec::new();
    let mut first_error = None;
    for station in json::array(stations, "stationList") {
        let id = cloud::station_id(field(station, "stationId"))?;
        let name = json::text(station, "stationShowName").trim();
        let result = cloud.gateway(c, id).await;
        if pair {
            match result {
                Ok(status) if cloud::known(&status) => {
                    json::push(
                        &mut list,
                        json::fields(&[
                            (
                                "name",
                                json::string(&join(&[
                                    if name.is_empty() { "Sigenergy" } else { name },
                                    " Gateway",
                                ])?)?,
                            ),
                            (
                                "data",
                                json::fields(&[(
                                    "stationId",
                                    json::string(&super::decimal(id)?)?,
                                )])?,
                            ),
                            (
                                "store",
                                json::fields(&[
                                    ("stationName", json::string(name)?),
                                    ("manufacturer", json::string("Sigenergy")?),
                                ])?,
                            ),
                        ])?,
                        4096,
                    )?;
                }
                Ok(_) => (),
                Err(e) => {
                    if first_error.is_none() {
                        first_error = Some(e);
                    }
                }
            }
        } else {
            let mut summary = json::fields(&[
                ("id", Value::uint(id)),
                ("name", json::string(name)?),
                ("status", clone(field(station, "status"))?),
                ("activation", clone(field(station, "activationStatus"))?),
            ])?;
            match result {
                Ok(v) => {
                    json::set(&mut summary, "gateway", Value::Bool(cloud::known(&v)))?;
                    json::set(
                        &mut summary,
                        "gatewayControllable",
                        Value::Bool(
                            cloud::known(&v)
                                && cloud::flag(field(&v, "showButton")).unwrap_or(false),
                        ),
                    )?;
                    json::set(
                        &mut summary,
                        "offGrid",
                        Value::Bool(cloud::reached(&v, true)),
                    )?;
                    json::set(
                        &mut summary,
                        "gridStatus",
                        clone(field(&v, "onOffGridStatus"))?,
                    )?;
                }
                Err(e) => json::set(
                    &mut summary,
                    "gatewayError",
                    json::string(&stulp_sdk::message(&e)?)?,
                )?,
            }
            json::push(&mut list, summary, 4096)?;
        }
    }
    if pair {
        if list.is_empty() {
            return Err(first_error.unwrap_or(Error::Invalid(
                "mySigen gaf voor geen station een herkenbare Gateway-netstand terug.",
            )));
        }
        Ok(Value::Array(list))
    } else {
        Ok(json::fields(&[
            ("linked", Value::Bool(true)),
            ("stations", Value::Array(list)),
        ])?)
    }
}
