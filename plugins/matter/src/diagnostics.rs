//! Diagnostiek toont uitsluitend daadwerkelijk gelezen attributen binnen fysieke grenzen.
use crate::{
    im::AttributePath,
    interaction::Interaction,
    tlv::{Node, Value as Tlv},
};
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, Transport, clone,
    util::{field, join},
};
type Values = Vec<(u32, Vec<u8>)>;
async fn cluster<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    id: u32,
    deadline: u64,
) -> Result<Values> {
    let mut path = AttributePath::new(0, id, 0);
    path.attribute = None;
    let reports = im.read(c, &[path], true, deadline).await?;
    let mut ids = Vec::new();
    for chunk in reports.iter() {
        for a in chunk?.attributes {
            if a.path.endpoint == Some(0)
                && a.path.cluster == Some(id)
                && a.status.is_none()
                && let Some(attribute) = a.path.attribute
                && !ids.contains(&attribute)
            {
                json::push(&mut ids, attribute, 256)?;
            }
        }
    }
    let mut values = Vec::new();
    for attribute in ids {
        if let Some(bytes) = reports.attribute(AttributePath::new(0, id, attribute), true)? {
            json::push(&mut values, (attribute, bytes), 256)?;
        }
    }
    Ok(values)
}
fn nodes(values: &Values) -> Result<Vec<(u32, Node<'_>)>> {
    let mut out = Vec::new();
    for (id, bytes) in values {
        json::push(&mut out, (*id, Node::parse(bytes)?), 256)?;
    }
    Ok(out)
}
type Fields<'a> = [(u32, Node<'a>)];
fn get<'a, 'b>(values: &'b Fields<'a>, id: u32) -> Option<&'b Node<'a>> {
    values.iter().find(|(key, _)| *key == id).map(|(_, n)| n)
}
fn uint(out: &mut Value, values: &Fields<'_>, id: u32, key: &str, min: u64, max: u64) -> Result {
    if let Some(Tlv::Uint(n)) = get(values, id).map(|n| n.element.value)
        && (min..=max).contains(&n)
    {
        json::set(out, key, Value::uint(n))?;
    }
    Ok(())
}
fn text(out: &mut Value, values: &Fields<'_>, id: u32, key: &str) -> Result {
    if let Some(Tlv::String(s)) = get(values, id).map(|n| n.element.value)
        && !s.is_empty()
    {
        json::set(out, key, json::string(s)?)?;
    }
    Ok(())
}
fn rssi(out: &mut Value, values: &Fields<'_>, id: u32, key: &str) -> Result {
    if let Some(Tlv::Int(n)) = get(values, id).map(|n| n.element.value)
        && (-128..=20).contains(&n)
    {
        json::set(out, key, Value::int(n))?;
    }
    Ok(())
}
fn flag(out: &mut Value, values: &Fields<'_>, id: u32, key: &str) -> Result {
    if let Some(Tlv::Bool(b)) = get(values, id).map(|n| n.element.value) {
        json::set(out, key, Value::Bool(b))?;
    }
    Ok(())
}
fn octets(out: &mut Value, values: &Fields<'_>, id: u32, key: &str, integer: bool) -> Result {
    let value = match get(values, id).map(|n| n.element.value) {
        Some(Tlv::Bytes(bytes)) if !bytes.is_empty() => {
            use core::fmt::Write;
            let mut s = String::new();
            s.try_reserve(bytes.len() * 2)
                .map_err(|_| stulp_core::Error::Memory)?;
            for b in bytes {
                write!(s, "{b:02x}").map_err(|_| Error::Invalid("diagnostics hex"))?;
            }
            s
        }
        Some(Tlv::Uint(n)) if integer => crate::model::node_id(n)?,
        _ => return Ok(()),
    };
    json::set(out, key, json::string(&value)?)?;
    Ok(())
}
fn named(out: &mut Value, values: &Fields<'_>, id: u32, key: &str, names: &[&str]) -> Result {
    if let Some(Tlv::Uint(n)) = get(values, id).map(|n| n.element.value) {
        let value = if let Ok(i) = usize::try_from(n)
            && let Some(name) = names.get(i)
        {
            json::copy(name)?
        } else {
            join(&["code ", &crate::settings::decimal(n)?])?
        };
        json::set(out, key, json::string(&value)?)?;
    }
    Ok(())
}
fn basic(values: &Fields<'_>) -> Result<Value> {
    let mut out = json::object();
    for (id, name) in [
        (1, "vendorName"),
        (3, "productName"),
        (8, "hardwareVersion"),
        (10, "softwareVersion"),
        (15, "serialNumber"),
    ] {
        text(&mut out, values, id, name)?;
    }
    Ok(out)
}
fn general(values: &Fields<'_>) -> Result<Value> {
    let mut out = json::object();
    for (id, name, max) in [
        (1, "rebootCount", 1 << 32),
        (2, "upTimeSeconds", 100 * 365 * 24 * 3600),
        (3, "totalOperationalHours", 100 * 365 * 24),
    ] {
        uint(&mut out, values, id, name, 0, max)?;
    }
    named(
        &mut out,
        values,
        4,
        "bootReason",
        &[
            "onbekend",
            "spanning ingeschakeld",
            "brownout",
            "hardwarewatchdog",
            "softwarewatchdog",
            "software-update",
            "software gaf opdracht",
        ],
    )?;
    let mut faults = Vec::new();
    for (id, label) in [(5, "hardware"), (6, "radio"), (7, "netwerk")] {
        if let Some(n) = get(values, id)
            && !n.children.is_empty()
        {
            json::push(
                &mut faults,
                join(&[
                    &crate::settings::decimal(n.children.len() as u64)?,
                    " actieve ",
                    label,
                    "fout(en)",
                ])?,
                3,
            )?;
        }
    }
    faults.sort();
    let mut list = Vec::new();
    for text in faults {
        json::push(&mut list, json::string(&text)?, 3)?;
    }
    if !list.is_empty() {
        json::set(&mut out, "activeFaults", Value::Array(list))?;
    }
    Ok(out)
}
fn table(values: &Fields<'_>, id: u32, route: bool) -> Result<Value> {
    let mut out = Vec::new();
    if let Some(entries) = get(values, id)
        && entries.element.value == Tlv::Array
    {
        for entry in &entries.children {
            if entry.element.value != Tlv::Structure {
                continue;
            }
            let mut fields = Vec::new();
            // Herparseer een klein element zodat dezelfde typecontroles voor attributen en velden gelden.
            for child in &entry.children {
                if let crate::tlv::Tag::Context(id) = child.element.tag {
                    let mut w = crate::tlv::Writer::default();
                    w.node(child, crate::tlv::Tag::Anonymous)?;
                    json::push(&mut fields, (u32::from(id), w.finish()?), 32)?;
                }
            }
            let fields = nodes(&fields)?;
            let mut n = json::object();
            octets(&mut n, &fields, 0, "extAddress", true)?;
            let integers: &[(u32, &str, u64)] = if route {
                &[
                    (1, "rloc16", 65535),
                    (2, "routerId", 62),
                    (4, "pathCost", 16),
                    (5, "lqiIn", 255),
                    (6, "lqiOut", 255),
                    (7, "ageSeconds", 1 << 32),
                ]
            } else {
                &[
                    (1, "ageSeconds", 1 << 32),
                    (2, "rloc16", 65535),
                    (5, "lqi", 255),
                    (8, "frameErrorRate", 100),
                    (9, "messageErrorRate", 100),
                ]
            };
            for (id, name, max) in integers {
                uint(&mut n, &fields, *id, name, 0, *max)?;
            }
            if route {
                flag(&mut n, &fields, 8, "allocated")?;
                flag(&mut n, &fields, 9, "linkEstablished")?;
            } else {
                rssi(&mut n, &fields, 6, "averageRssi")?;
                rssi(&mut n, &fields, 7, "lastRssi")?;
                for (id, name) in [
                    (10, "rxOnWhenIdle"),
                    (11, "fullThreadDevice"),
                    (13, "isChild"),
                ] {
                    flag(&mut n, &fields, id, name)?;
                }
            }
            json::push(&mut out, n, 256)?;
        }
    }
    Ok(Value::Array(out))
}
fn thread(values: &Fields<'_>) -> Result<Value> {
    let mut out = json::object();
    uint(&mut out, values, 0, "channel", 11, 26)?;
    for (id, name, max) in [
        (3, "panId", 65535),
        (6, "overrunCount", 1 << 63),
        (9, "partitionId", 1 << 32),
        (13, "leaderRouterId", 62),
    ] {
        uint(&mut out, values, id, name, 0, max)?;
    }
    text(&mut out, values, 2, "networkName")?;
    octets(&mut out, values, 4, "extendedPanId", false)?;
    named(
        &mut out,
        values,
        1,
        "routingRole",
        &[
            "onbekend",
            "niet toegewezen",
            "losgekoppeld",
            "slapend eindapparaat",
            "eindapparaat",
            "router-kandidaat",
            "router",
            "leider",
        ],
    )?;
    for (id, name, route) in [(7, "neighbours", false), (8, "routes", true)] {
        let value = table(values, id, route)?;
        if value.as_array().is_some_and(|v| !v.is_empty()) {
            json::set(&mut out, name, value)?;
        }
    }
    Ok(out)
}
fn wifi(values: &Fields<'_>) -> Result<Value> {
    let mut out = json::object();
    octets(&mut out, values, 0, "bssid", false)?;
    uint(&mut out, values, 3, "channel", 1, 233)?;
    rssi(&mut out, values, 4, "rssi")?;
    for (id, name, max) in [
        (5, "beaconLostCount", 1 << 32),
        (6, "beaconRxCount", 1 << 32),
        (9, "packetUnicastRx", 1 << 32),
        (10, "packetUnicastTx", 1 << 32),
        (11, "currentMaxRate", 1 << 40),
        (12, "overrunCount", 1 << 63),
    ] {
        uint(&mut out, values, id, name, 0, max)?;
    }
    named(
        &mut out,
        values,
        1,
        "securityType",
        &["onbepaald", "geen", "WEP", "WPA", "WPA2", "WPA3"],
    )?;
    named(
        &mut out,
        values,
        2,
        "version",
        &[
            "802.11a", "802.11b", "802.11g", "802.11n", "802.11ac", "802.11ax", "802.11ah",
        ],
    )?;
    Ok(out)
}
/// Vraagt vier clusters één keer op; ontbrekende clusters worden niet als nulmetingen gepresenteerd.
pub async fn inspect<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    device: &Value,
    deadline: u64,
) -> Result<Value> {
    let mut out = json::fields(&[(
        "nodeId",
        clone(field(field(device, "store"), "matter.nodeId"))?,
    )])?;
    if let Some(inventory) = json::get(field(device, "store"), "~matter.endpointInventory") {
        json::set(&mut out, "inventory", clone(inventory)?)?;
    }
    let mut missing = Vec::new();
    let mut errors = Vec::new();
    let mut radio = false;
    for (id, name) in [
        (0x28, "Basisinformatie"),
        (0x33, "Algemene diagnostiek"),
        (0x35, "Thread"),
        (0x36, "Wi-Fi"),
    ] {
        match cluster(im, c, id, deadline).await {
            Ok(values) if !values.is_empty() => {
                let values = nodes(&values)?;
                let value = match id {
                    0x28 => basic(&values)?,
                    0x33 => general(&values)?,
                    0x35 => thread(&values)?,
                    _ => wifi(&values)?,
                };
                if id == 0x35 || id == 0x36 {
                    if !radio {
                        json::set(&mut out, if id == 0x35 { "thread" } else { "wifi" }, value)?;
                        radio = true;
                    }
                } else if let Some(fields) = value.as_object() {
                    for (key, value) in fields.iter() {
                        json::set(&mut out, key, clone(value)?)?;
                    }
                }
            }
            Ok(_) => {
                if id == 0x33 {
                    json::push(&mut missing, json::string(name)?, 4)?;
                }
            }
            Err(Error::Core(e)) => return Err(Error::Core(e)),
            Err(e) => {
                json::push(
                    &mut errors,
                    json::string(&join(&[name, ": ", &stulp_sdk::message(&e)?])?)?,
                    4,
                )?;
            }
        }
    }
    if !radio {
        json::push(
            &mut missing,
            json::string("Radiodiagnostiek (Thread of Wi-Fi)")?,
            4,
        )?;
    }
    if !missing.is_empty() {
        json::set(&mut out, "missing", Value::Array(missing))?;
    }
    if !errors.is_empty() {
        json::set(&mut out, "errors", Value::Array(errors))?;
    }
    Ok(out)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn impossible_radio_values_are_absent_and_unknown_enums_remain_visible() -> Result {
        let mut w = crate::tlv::Writer::default();
        w.int(crate::tlv::Tag::Anonymous, -129)?;
        let mut role = crate::tlv::Writer::default();
        role.uint(crate::tlv::Tag::Anonymous, 42)?;
        let bytes = alloc::vec![(4, w.finish()?), (1, role.finish()?)];
        let value = wifi(&nodes(&bytes)?)?;
        assert!(json::get(&value, "rssi").is_none());
        assert_eq!(json::text(&value, "securityType"), "code 42");
        assert!(json::get(&value, "channel").is_none());
        Ok(())
    }
}
