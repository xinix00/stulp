//! Discovery vraagt de soortspecifieke probe op iedere expliciete unit.
use super::{modbus::Modbus, register};
use alloc::vec::Vec;
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, Result, Transport, util::join};
pub(super) fn units(text: &str) -> Result<Vec<u8>> {
    let text = if text.trim().is_empty() {
        "1-32,247"
    } else {
        text
    };
    let mut seen = [false; 256];
    let mut out = Vec::new();
    for part in text.split(',').map(str::trim).filter(|s| !s.is_empty()) {
        let (a, b) = part.split_once('-').unwrap_or((part, part));
        let a = a
            .trim()
            .parse::<u8>()
            .ok()
            .filter(|v| *v > 0)
            .ok_or(Error::Invalid("Unit-id moet 1..255 zijn."))?;
        let b = b
            .trim()
            .parse::<u8>()
            .ok()
            .filter(|v| *v >= a)
            .ok_or(Error::Invalid("Ongeldig bereik van unit-ids."))?;
        for unit in a..=b {
            if !seen[usize::from(unit)] {
                json::push(&mut out, unit, 255)?;
                seen[usize::from(unit)] = true;
            }
        }
    }
    if out.is_empty() {
        return Err(Error::Invalid("Er zijn geen unit-ids om af te tasten."));
    }
    out.sort_unstable();
    Ok(out)
}
pub(super) fn charger_plan(text: &str, exact: &str) -> Result<(Vec<u8>, Vec<u8>, bool)> {
    if !exact.trim().is_empty() {
        let unit = exact
            .trim()
            .parse::<u8>()
            .ok()
            .filter(|v| *v > 0 && *v <= 246)
            .ok_or(Error::Invalid("AC-laadpaal unit-id moet 1..246 zijn."))?;
        let mut list = Vec::new();
        json::push(&mut list, unit, 1)?;
        return Ok((list, Vec::new(), true));
    }
    let mut preferred = units(text)?;
    preferred.retain(|u| *u <= 246);
    let mut fallback = Vec::new();
    for u in 1..=246 {
        if !preferred.contains(&u) {
            json::push(&mut fallback, u, 246)?;
        }
    }
    Ok((preferred, fallback, false))
}
pub(super) async fn check<T: Transport>(
    c: &mut Client<T>,
    bus: &mut Modbus,
    address: &str,
    timeout: u64,
) -> Result {
    bus.read(c, address, timeout, 247, 30005, 2)
        .await
        .map_err(super::modbus::Failure::sdk)?;
    Ok(())
}
pub(super) async fn pair<T: Transport>(
    c: &mut Client<T>,
    bus: &mut Modbus,
    address: &str,
    driver: &str,
) -> Result<Value> {
    let text = c
        .state()
        .setting("units")
        .and_then(Value::as_str)
        .unwrap_or("");
    let exact = c
        .state()
        .setting("chargerUnit")
        .and_then(Value::as_str)
        .unwrap_or("");
    let (preferred, fallback, exact) = if driver == "evaccharger" {
        charger_plan(text, exact)?
    } else {
        (units(text)?, Vec::new(), false)
    };
    let timeout = if driver == "evaccharger" { 1100 } else { 500 };
    check(c, bus, address, timeout).await?;
    let probe = register::probe(driver)?;
    let mut found = Vec::new();
    let mut first_error = None;
    for (list, timeout) in [(&preferred, timeout), (&fallback, 100)] {
        for &unit in list {
            match bus
                .read(c, address, timeout, unit, probe.addr, probe.count)
                .await
            {
                Ok(_) => {
                    json::push(&mut found, unit, 255)?;
                    if driver == "evaccharger" {
                        break;
                    }
                }
                Err(super::modbus::Failure::Refused(_)) => (),
                Err(e) => {
                    if first_error.is_none() {
                        first_error = Some(e.sdk());
                    }
                }
            }
        }
        if driver == "evaccharger" && !found.is_empty() {
            break;
        }
    }
    if found.is_empty() {
        if let Some(e) = first_error
            && check(c, bus, address, timeout).await.is_err()
        {
            return Err(e);
        }
        if driver == "evaccharger" {
            return Err(Error::Invalid(if exact {
                "Geen AC-laadpaal op het ingestelde unit-id gevonden."
            } else {
                "Geen AC-laadpaal gevonden; vul het EVAC unit-id uit mySigen in."
            }));
        }
    }
    let label = match driver {
        "plant" => "Sigenergy-systeem",
        "inverter" => "Sigenergy-omvormer",
        "battery" => "Sigenergy-batterij",
        "energy" => "Sigenergy-netmeter",
        _ => "Sigenergy AC-laadpaal",
    };
    let mut candidates = Vec::new();
    for unit in found {
        let mut id = join(&["unit ", &super::decimal(u64::from(unit))?])?;
        if matches!(driver, "inverter" | "battery") {
            let serial = register::card(driver)?
                .iter()
                .find(|r| r.name == "Serial")
                .ok_or(Error::Invalid("Sigenergy serial register missing"))?;
            if let Ok(words) = bus
                .read(c, address, 500, unit, serial.addr, serial.count)
                .await
                && let Ok(value) = serial.decode(&words)
                && let Some(serial) = value.as_str().filter(|s| !s.is_empty())
            {
                id = json::copy(serial)?;
            }
        }
        json::push(
            &mut candidates,
            json::fields(&[
                ("name", json::string(&join(&[label, " ", &id])?)?),
                ("data", json::fields(&[("id", json::string(&id)?)])?),
                (
                    "settings",
                    json::fields(&[("modbus_unitId", Value::uint(u64::from(unit)))])?,
                ),
            ])?,
            255,
        )?;
    }
    Ok(Value::Array(candidates))
}
pub(super) async fn test<T: Transport>(
    c: &mut Client<T>,
    bus: &mut Modbus,
    body: &Value,
) -> Result<Value> {
    let host = json::text(body, "host").trim();
    let port = u16::try_from(json::uint(body, "port"))
        .ok()
        .filter(|p| *p > 0)
        .unwrap_or(502);
    let address = super::modbus::address(host, port)?;
    let text = json::text(body, "units");
    let mut list = units(text)?;
    let (a, _, exact) = charger_plan(text, json::text(body, "chargerUnit"))?;
    if exact && !list.contains(&a[0]) {
        json::push(&mut list, a[0], 255)?;
    }
    bus.reset();
    check(c, bus, &address, 2000).await?;
    let mut found = Vec::new();
    let mut error = None;
    for unit in &list {
        let mut offers = Vec::new();
        for (driver, label) in [
            ("plant", "systeem"),
            ("energy", "netmeter"),
            ("inverter", "omvormer"),
            ("battery", "batterij"),
            ("evaccharger", "AC-laadpaal"),
        ] {
            let probe = register::probe(driver)?;
            match bus
                .read(c, &address, 500, *unit, probe.addr, probe.count)
                .await
            {
                Ok(_) => json::push(&mut offers, json::string(label)?, 5)?,
                Err(super::modbus::Failure::Refused(_)) => (),
                Err(e) => {
                    if error.is_none() {
                        error = Some(e.sdk());
                    }
                }
            }
        }
        if !offers.is_empty() {
            json::push(
                &mut found,
                json::fields(&[
                    ("unit", Value::uint(u64::from(*unit))),
                    ("offers", Value::Array(offers)),
                ])?,
                255,
            )?;
        }
    }
    if found.is_empty() {
        return Err(error.unwrap_or(Error::Invalid(
            "Het adres antwoordt, maar biedt geen Sigenergy-apparaten aan.",
        )));
    }
    Ok(json::fields(&[
        ("found", Value::Array(found)),
        ("units", Value::uint(list.len() as u64)),
    ])?)
}
