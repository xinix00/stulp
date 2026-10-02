//! Metingen en dynamische omvormertegels volgen uitsluitend de registerantwoorden.
use super::{
    modbus::Modbus,
    register::{self, Poller, n, rounded},
};
use alloc::string::String;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, Transport, clone,
    util::{field, join},
};
pub(super) struct Meter {
    pub(super) id: String,
    pub(super) driver: String,
    pub(super) next: u64,
    unit: u8,
    generation: u64,
    info_done: bool,
    info: Poller,
    read: Poller,
    system: Poller,
    pub(super) power: f64,
    mppt: Option<u8>,
    phases: bool,
}
pub(super) fn unit(d: &Value) -> Result<u8> {
    field(field(d, "settings"), "modbus_unitId")
        .as_u64()
        .and_then(|n| u8::try_from(n).ok())
        .filter(|n| *n > 0)
        .ok_or(Error::Invalid(
            "Het Modbus unit-id moet een getal van 1 tot en met 255 zijn.",
        ))
}
impl Meter {
    pub(super) fn new(id: &str, driver: &str, unit: u8, next: u64) -> Result<Self> {
        let card = register::card(driver)?;
        Ok(Self {
            id: json::copy(id)?,
            driver: json::copy(driver)?,
            next,
            unit,
            generation: u64::MAX,
            info_done: false,
            info: Poller::new(card, "info")?,
            read: Poller::new(card, "reading")?,
            system: Poller::new(card, "system")?,
            power: 0.,
            mppt: None,
            phases: false,
        })
    }
    pub(super) async fn refresh<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        bus: &mut Modbus,
        address: &str,
        timeout: u64,
        charger_power: f64,
    ) -> Result {
        let current = unit(c.state().device(&self.id)?)?;
        if current != self.unit {
            *self = Self::new(&self.id, &self.driver, current, self.next)?;
        }
        if self.generation != bus.generation {
            self.info_done = false;
            self.generation = bus.generation;
        }
        if !self.info_done
            && let Ok(info) = self.info.read(c, bus, address, timeout, self.unit).await
        {
            self.apply_info(c, &info).await?;
            self.info_done = true;
        }
        let mut values = self.read.read(c, bus, address, timeout, self.unit).await?;
        register::merge(
            &mut values,
            self.system.read(c, bus, address, timeout, 247).await?,
        )?;
        let report = self.apply(&values, charger_power)?;
        c.values(&self.id, report).await?;
        c.available(&self.id, true).await
    }
    async fn apply_info<T: Transport>(&mut self, c: &mut Client<T>, info: &Value) -> Result {
        let mut store = json::object();
        let serial = json::text(info, "Serial");
        if !serial.is_empty() {
            json::set(&mut store, "serial", json::string(serial)?)?;
        }
        if self.driver == "battery"
            && let Some(capacity) = n(info, "Capacity")
        {
            json::set(&mut store, "capacity", rounded(capacity)?)?;
        }
        if self.driver == "inverter"
            && let (Some(mppt), Some(output)) = (n(info, "MPPTCount"), n(info, "OutputType"))
        {
            self.mppt = Some((mppt as u8).min(4));
            self.phases = matches!(output as i64, 1 | 2);
            let output = match output as i64 {
                0 => "L/N",
                1 => "L1/L2/L3",
                2 => "L1/L2/L3/N",
                3 => "L1/L2/N",
                _ => "onbekend",
            };
            json::set(&mut store, "outputType", json::string(output)?)?;
            json::set(&mut store, "mpptCount", rounded(mppt)?)?;
            for i in 1..=4 {
                self.capability(
                    c,
                    &join(&["measure_voltage.pv", &super::decimal(i)?])?,
                    i as f64 <= mppt,
                )
                .await?;
            }
            for phase in ["phaseB", "phaseC"] {
                for quantity in ["measure_voltage.", "measure_current."] {
                    self.capability(c, &join(&[quantity, phase])?, self.phases)
                        .await?;
                }
            }
        }
        if store.as_object().is_some_and(|v| !v.is_empty()) {
            c.store(&self.id, store).await?;
        }
        Ok(())
    }
    async fn capability<T: Transport>(
        &self,
        c: &mut Client<T>,
        name: &str,
        wanted: bool,
    ) -> Result {
        let has = json::array(c.state().device(&self.id)?, "capabilities")
            .iter()
            .any(|v| v.as_str() == Some(name));
        if has != wanted {
            c.call(
                if wanted {
                    "capability.add"
                } else {
                    "capability.remove"
                },
                &json::fields(&[
                    ("deviceId", json::string(&self.id)?),
                    ("capability", json::string(name)?),
                ])?,
            )
            .await?;
        }
        Ok(())
    }
    pub(super) fn apply(&mut self, v: &Value, charger_power: f64) -> Result<Value> {
        let mut out = json::object();
        let pairs: &[(&str, &str)] = match self.driver.as_str() {
            "plant" => &[
                ("measure_power.grid", "GridPower"),
                ("measure_power.battery", "BatteryPower"),
                ("measure_power.load", "GeneralLoadPower"),
                ("measure_battery", "BatterySoC"),
            ],
            "battery" => &[
                ("measure_battery", "SoC"),
                ("measure_power", "Power"),
                ("meter_power.charged", "TotalCharged"),
                ("meter_power.discharged", "TotalDischarged"),
                ("measure_temperature.minCell", "MinCellTemp"),
                ("measure_temperature.maxCell", "MaxCellTemp"),
                ("measure_temperature.pcs", "PCSTemp"),
            ],
            "inverter" => &[
                ("measure_power", "Power"),
                ("meter_power.daily", "DailyYield"),
                ("meter_power", "TotalYield"),
            ],
            "energy" => &[
                ("measure_power", "Power"),
                ("measure_power.L1", "PowerL1"),
                ("measure_power.L2", "PowerL2"),
                ("measure_power.L3", "PowerL3"),
                ("meter_power.imported", "TotalImport"),
                ("meter_power.exported", "TotalExport"),
            ],
            "evaccharger" => &[("meter_power.charged", "TotalCharged")],
            _ => return Err(Error::Invalid("unknown meter driver")),
        };
        for &(cap, key) in pairs {
            put(&mut out, v, cap, key)?;
        }
        match self.driver.as_str() {
            "plant" => {
                let own = n(v, "SolarPower");
                let third = n(v, "ThirdPartyInverterPower");
                if own.is_some() || third.is_some() {
                    json::set(
                        &mut out,
                        "measure_power.solar",
                        rounded(own.unwrap_or(0.) + third.unwrap_or(0.))?,
                    )?;
                }
                json::set(&mut out, "measure_power.evcharger", rounded(charger_power)?)?;
            }
            "battery" => {
                if !field(v, "Firmware").is_null() {
                    json::set(&mut out, "firmware", clone(field(v, "Firmware"))?)?;
                }
                if let (Some(status), Some(power)) = (n(v, "Status"), n(v, "Power")) {
                    let state = if status == 1. {
                        if power > 0. {
                            "charging"
                        } else {
                            "discharging"
                        }
                    } else {
                        "idle"
                    };
                    json::set(&mut out, "battery_charging_state", json::string(state)?)?;
                }
            }
            "inverter" => {
                if let Some(mppt) = self.mppt {
                    for i in 1..=mppt {
                        put(
                            &mut out,
                            v,
                            &join(&["measure_voltage.pv", &super::decimal(u64::from(i))?])?,
                            &join(&["PV", &super::decimal(u64::from(i))?, "Voltage"])?,
                        )?;
                    }
                    for phase in if self.phases {
                        &["A", "B", "C"][..]
                    } else {
                        &["A"][..]
                    } {
                        for (cap, reg) in [
                            ("measure_voltage.phase", "Voltage"),
                            ("measure_current.phase", "Current"),
                        ] {
                            put(
                                &mut out,
                                v,
                                &join(&[cap, phase])?,
                                &join(&["Phase", phase, reg])?,
                            )?;
                        }
                    }
                }
            }
            "evaccharger" => {
                if let Some(kw) = n(v, "Power") {
                    let watts = kw * 1000.;
                    self.power = watts;
                    json::set(&mut out, "measure_power", rounded(watts)?)?;
                    if let Some(status) = n(v, "Status") {
                        let state = match status as i64 {
                            2 | 3 => "plugged_in",
                            4 | 5 => {
                                if watts > 0. {
                                    "plugged_in_charging"
                                } else {
                                    "plugged_in"
                                }
                            }
                            _ => "plugged_out",
                        };
                        json::set(&mut out, "evcharger_charging", Value::Bool(watts > 0.))?;
                        json::set(&mut out, "evcharger_charging_state", json::string(state)?)?;
                    }
                }
            }
            _ => (),
        }
        if let Some(status) = n(v, "GridStatus") {
            let state = match status as i64 {
                0 => "on_grid",
                1 => "off_grid",
                2 => "off_grid_manual",
                _ => "unknown",
            };
            json::set(&mut out, "grid_status", json::string(state)?)?;
        }
        if let Some(control) = n(v, "PhaseControl") {
            json::set(
                &mut out,
                "phase_control",
                json::string(match control as i64 {
                    0 => "off",
                    1 => "on",
                    _ => "unknown",
                })?,
            )?;
        }
        Ok(out)
    }
}
fn put(out: &mut Value, v: &Value, cap: &str, key: &str) -> Result {
    if let Some(value) = n(v, key) {
        json::set(out, cap, rounded(value)?)?;
    }
    Ok(())
}
