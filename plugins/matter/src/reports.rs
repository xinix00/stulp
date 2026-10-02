//! Live reports vormen één update per apparaat; eventmarkers moeten vóór flowtriggers duurzaam staan.
use crate::{
    capabilities, devices, im,
    interaction::Reports,
    settings::{self, decimal},
    tlv::{Node, Tag, Value as Tlv},
};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Error, Result, clone,
    util::{field, join},
};
/// Device types van dit endpoint, met fallback voor oude gecombineerde records.
pub fn types(device: &Value, endpoint: u16) -> Result<Vec<u32>> {
    let store = field(device, "store");
    for inventory in json::array(store, "~matter.endpointInventory") {
        if json::uint(inventory, "endpoint") == u64::from(endpoint) {
            return devices::ids(field(inventory, "deviceTypes"));
        }
    }
    devices::ids(field(store, "matter.deviceTypes"))
}
/// Concrete attribuutpaden en urgente eventpaden; herhaalde functies houden hun endpoint.
pub fn subscription(devices: &[Value]) -> Result<im::Subscription> {
    let mut attributes = Vec::new();
    let mut events = Vec::new();
    for device in devices {
        for endpoint in devices::endpoints(device)? {
            if !events
                .iter()
                .any(|e: &im::EventPath| e.endpoint == Some(endpoint))
            {
                json::push(
                    &mut events,
                    im::EventPath {
                        endpoint: Some(endpoint),
                        urgent: Some(true),
                        ..Default::default()
                    },
                    256,
                )?;
            }
        }
        let mut servers = devices::ids(field(field(device, "store"), "matter.serverClusters"))?;
        if servers.is_empty() && json::text(device, "class") == "thermostat" {
            json::push(&mut servers, 0x201, 256)?;
        }
        for cap in json::array(device, "capabilities")
            .iter()
            .filter_map(Value::as_str)
        {
            let endpoint = devices::endpoint(device, cap);
            if let Some(m) = capabilities::for_capability(&types(device, endpoint)?, &servers, cap)
            {
                add_path(
                    &mut attributes,
                    im::AttributePath::new(endpoint, m.cluster, m.attribute),
                )?;
            }
        }
        for value in settings::metadata(device).as_array().unwrap_or(&[]) {
            if let Ok(setting) = settings::Setting::parse(value) {
                add_path(
                    &mut attributes,
                    im::AttributePath::new(setting.endpoint, 0x80, 0),
                )?;
            }
        }
    }
    if attributes.len() + events.len() > 256 {
        return Err(Error::Invalid("Matter subscription exceeds 256 paths"));
    }
    Ok(im::Subscription {
        attributes,
        events,
        minimum: 0,
        maximum: 300,
        keep: false,
        fabric_filtered: true,
    })
}
fn add_path(paths: &mut Vec<im::AttributePath>, path: im::AttributePath) -> Result {
    if !paths.contains(&path) {
        json::push(paths, path, 256)?;
    }
    Ok(())
}
/// Afgeleid flowbericht, pas te versturen na het succesvol opslaan van Update.device.
pub struct Flow {
    /// Triggerkaart, bijvoorbeeld matter_event of capability.button.2.on.
    pub card: String,
    /// Waarden voor flowtokens.
    pub tokens: Value,
    /// Filtercontext van de trigger.
    pub state: Value,
}
/// Samenhangend resultaat bevat zowel staat als de te dedupliceren flow-events.
pub struct Update {
    /// Volledige record; caller bewaart de eventmarker vóór het publiceren van events.
    pub device: Value,
    /// Events voor uitsluitend dit apparaat, in ontvangstvolgorde.
    pub events: Vec<Flow>,
}
fn object(value: &Value) -> Result<Value> {
    if value.as_object().is_some() {
        clone(value)
    } else {
        Ok(json::object())
    }
}
fn put(device: &mut Value, container: &str, key: &str, value: Value) -> Result {
    let mut out = object(field(device, container))?;
    json::set(&mut out, key, value)?;
    json::set(device, container, out)?;
    Ok(())
}
fn available(device: &mut Value) -> Result {
    json::set(device, "available", Value::Bool(true))?;
    json::set(device, "message", json::string("")?)?;
    Ok(())
}
fn changed(device: &Value, container: &str, key: &str, value: &Value) -> bool {
    !json::equal(field(field(device, container), key), value)
        || !json::boolean(device, "available")
        || !json::text(device, "message").is_empty()
}
fn apply_attribute(
    device: &mut Value,
    report: im::Attribute<'_>,
    endpoints: &[u16],
    servers: &[u32],
) -> Result<bool> {
    let (Some(endpoint), Some(cluster), Some(attribute), Some(value)) = (
        report.path.endpoint,
        report.path.cluster,
        report.path.attribute,
        report.value,
    ) else {
        return Ok(false);
    };
    if report.status.is_some() || !endpoints.contains(&endpoint) || report.path.list_index.is_some()
    {
        return Ok(false);
    }
    if cluster == 0x80
        && attribute == 0
        && let Tlv::Uint(n) = value.element.value
    {
        for setting in settings::metadata(device).as_array().unwrap_or(&[]) {
            let setting = match settings::Setting::parse(setting) {
                Ok(s) => s,
                Err(Error::Core(e)) => return Err(Error::Core(e)),
                Err(_) => continue,
            };
            if setting.endpoint == endpoint && n < u64::from(setting.levels) {
                let value = Value::uint(n);
                let result = changed(device, "settings", &setting.id, &value);
                if result {
                    put(device, "settings", &setting.id, value)?;
                    available(device)?;
                }
                return Ok(result);
            }
        }
    }
    let Some(mapping) =
        capabilities::for_report(&types(device, endpoint)?, servers, cluster, attribute)
    else {
        return Ok(false);
    };
    let Some(capability) = devices::capability(device, mapping.capability, endpoint) else {
        return Ok(false);
    };
    let value = match mapping.decode(&value) {
        Ok(Some(v)) => v,
        Err(Error::Core(e)) => return Err(Error::Core(e)),
        _ => return Ok(false),
    };
    if !changed(device, "state", capability, &value) {
        return Ok(false);
    }
    let capability = json::copy(capability)?;
    put(device, "state", &capability, value)?;
    available(device)?;
    Ok(true)
}
fn hex4(value: u32) -> Result<String> {
    let mut s = String::new();
    s.try_reserve(10).map_err(|_| stulp_core::Error::Memory)?;
    write!(&mut s, "0x{value:04X}").map_err(|_| Error::Invalid("Matter event formatting"))?;
    Ok(s)
}
fn event_name(cluster: u32, event: u32) -> Result<String> {
    let names: &[&str] = match cluster {
        0x3b => &[
            "switch_latched",
            "initial_press",
            "long_press",
            "short_release",
            "long_release",
            "multi_press_ongoing",
            "multi_press_complete",
        ],
        0x101 => &[
            "door_lock_alarm",
            "door_state_change",
            "lock_operation",
            "lock_operation_error",
            "lock_user_change",
        ],
        _ => &[],
    };
    if let Some(name) = names.get(event as usize) {
        return Ok(json::copy(name)?);
    }
    join(&["matter_", &hex4(cluster)?, "_", &hex4(event)?])
}
/// Generieke eventpayload bewaart gehele 64-bit waarden; onbekende structuurtags blijven bereikbaar.
pub fn value(node: &Node<'_>) -> Result<Value> {
    Ok(match node.element.value {
        Tlv::Bool(v) => Value::Bool(v),
        Tlv::Int(v) => Value::int(v),
        Tlv::Uint(v) => Value::uint(v),
        Tlv::Float(v) => stulp_sdk::util::float(v)?,
        Tlv::String(v) => json::string(v)?,
        Tlv::Bytes(bytes) => {
            let mut s = String::new();
            s.try_reserve(bytes.len() * 2)
                .map_err(|_| stulp_core::Error::Memory)?;
            for b in bytes {
                write!(&mut s, "{b:02x}")
                    .map_err(|_| Error::Invalid("Matter event bytes formatting"))?;
            }
            json::string(&s)?
        }
        Tlv::Array | Tlv::List => {
            let mut array = Vec::new();
            for n in &node.children {
                json::push(&mut array, value(n)?, 4096)?;
            }
            Value::Array(array)
        }
        Tlv::Structure => {
            let mut out = json::object();
            for (index, n) in node.children.iter().enumerate() {
                let key = decimal(match n.element.tag {
                    Tag::Context(n) => u64::from(n),
                    _ => index as u64,
                })?;
                json::set(&mut out, &key, value(n)?)?;
            }
            out
        }
        _ => Value::Null,
    })
}
fn switch(event: u32) -> Option<bool> {
    match event {
        1 | 2 | 5 => Some(true),
        3 | 4 | 6 => Some(false),
        _ => None,
    }
}
fn apply_event(
    device: &mut Value,
    event: im::Event<'_>,
    endpoints: &[u16],
    pending: &mut Vec<Flow>,
) -> Result<bool> {
    let (Some(endpoint), Some(cluster), Some(id), Some(data)) = (
        event.path.endpoint,
        event.path.cluster,
        event.path.event,
        event.value,
    ) else {
        return Ok(false);
    };
    if event.status.is_some() || !endpoints.contains(&endpoint) {
        return Ok(false);
    }
    if let Ok(previous) =
        json::text(field(device, "store"), "matter.lastEventNumber").parse::<u64>()
        && event.number <= previous
    {
        return Ok(false);
    }
    put(
        device,
        "store",
        "matter.lastEventNumber",
        json::string(&decimal(event.number)?)?,
    )?;
    let pressed = if cluster == 0x3b { switch(id) } else { None };
    let mut event_cap = None;
    if let Some(pressed) = pressed
        && let Some(capability) = devices::capability(device, "button", endpoint)
    {
        let capability = json::copy(capability)?;
        let previous = clone(field(field(device, "state"), &capability))?;
        put(device, "state", &capability, Value::Bool(pressed))?;
        if previous.as_bool() == Some(pressed) && matches!(id, 1 | 3 | 4 | 6) {
            let state = json::fields(&[
                ("deviceId", json::string(json::text(device, "id"))?),
                ("capability", json::string(&capability)?),
                ("value", Value::Bool(pressed)),
                ("oldValue", previous),
            ])?;
            let mut tokens = clone(&state)?;
            json::set(
                &mut tokens,
                "device",
                json::string(json::text(device, "name"))?,
            )?;
            json::push(
                pending,
                Flow {
                    card: join(&[
                        "capability.",
                        &capability,
                        if pressed { ".on" } else { ".off" },
                    ])?,
                    tokens,
                    state,
                },
                4096,
            )?;
        }
        event_cap = Some(capability);
    }
    available(device)?;
    let mut state = json::fields(&[
        ("deviceId", json::string(json::text(device, "id"))?),
        ("event", json::string(&event_name(cluster, id)?)?),
        ("cluster", json::string(&hex4(cluster)?)?),
        ("eventId", json::string(&hex4(id)?)?),
        ("endpoint", Value::uint(u64::from(endpoint))),
    ])?;
    if let Some(cap) = event_cap {
        json::set(&mut state, "capability", json::string(&cap)?)?;
    }
    if let Some(pressed) = pressed {
        json::set(&mut state, "pressed", Value::Bool(pressed))?;
    }
    let mut tokens = clone(&state)?;
    for (key, value) in [
        ("device", json::string(json::text(device, "name"))?),
        ("eventNumber", Value::uint(event.number)),
        ("priority", Value::uint(u64::from(event.priority))),
        ("data", value(&data)?),
    ] {
        json::set(&mut tokens, key, value)?;
    }
    json::push(
        pending,
        Flow {
            card: json::copy("matter_event")?,
            tokens,
            state,
        },
        4096,
    )?;
    Ok(true)
}
/// De aangeleverde record verandert niet bij een fout, ook niet halverwege een eventbatch.
pub fn apply(device: &Value, reports: &Reports) -> Result<Option<Update>> {
    let endpoints = devices::endpoints(device)?;
    let servers = devices::ids(field(field(device, "store"), "matter.serverClusters"))?;
    let mut device = clone(device)?;
    let mut changed = false;
    let mut events = Vec::new();
    for report in reports.iter() {
        for attribute in report?.attributes {
            changed |= apply_attribute(&mut device, attribute, &endpoints, &servers)?;
        }
    }
    for report in reports.iter() {
        for event in report?.events {
            changed |= apply_event(&mut device, event, &endpoints, &mut events)?;
        }
    }
    Ok(if changed {
        Some(Update { device, events })
    } else {
        None
    })
}
/// Rapportagewatchdog volgt dezelfde negotiated interval plus MRP-speling als Go.
pub fn watchdog(maximum: u16) -> u64 {
    (u64::from(maximum) * 1500 + 5000).max(10000)
}
#[cfg(test)]
mod tests {
    use super::*;
    fn button() -> Result<Value> {
        Ok(json::fields(&[
            ("id", json::string("switch")?),
            ("name", json::string("Hall")?),
            (
                "capabilities",
                Value::Array(alloc::vec![
                    json::string("button.2")?,
                    json::string("onoff")?
                ]),
            ),
            ("state", json::fields(&[("button.2", Value::Bool(true))])?),
            (
                "store",
                json::fields(&[
                    ("matter.nodeId", json::string("0000000000010000")?),
                    ("matter.endpoint", Value::uint(1)),
                    (
                        "matter.capabilityEndpoints",
                        json::fields(&[("button.2", Value::uint(5)), ("onoff", Value::uint(1))])?,
                    ),
                    (
                        "matter.serverClusters",
                        Value::Array(alloc::vec![json::string("0x6")?, json::string("0x3B")?]),
                    ),
                ])?,
            ),
        ])?)
    }
    fn events(numbers: &[(u64, u32)]) -> Result<Reports> {
        let mut events = Vec::new();
        for (number, event) in numbers {
            json::push(
                &mut events,
                im::Event {
                    path: im::EventPath {
                        endpoint: Some(5),
                        cluster: Some(0x3b),
                        event: Some(*event),
                        ..Default::default()
                    },
                    number: *number,
                    priority: 1,
                    timestamp: Some((4, 42)),
                    value: Some(Node::parse(&[0x15, 0x24, 0, 1, 0x18])?),
                    status: None,
                },
                32,
            )?;
        }
        let wire = im::Report {
            subscription: Some(123),
            attributes: Vec::new(),
            events,
            suppress: false,
            more: false,
        }
        .encode()?;
        let mut reports = Reports::default();
        reports.push(wire)?;
        Ok(reports)
    }
    #[test]
    fn repeated_press_has_exactly_one_extra_capability_trigger_and_duplicate_is_ignored() -> Result
    {
        let original = button()?;
        let report = events(&[
            (0x20000000000001, 1),
            (0x20000000000001, 1),
            (0x20000000000002, 2),
        ])?;
        let update = apply(&original, &report)?.ok_or(Error::Invalid("missing update"))?;
        assert_eq!(update.events.len(), 3);
        assert_eq!(update.events[0].card, "capability.button.2.on");
        assert_eq!(update.events[1].card, "matter_event");
        assert_eq!(
            json::text(&update.events[1].tokens, "event"),
            "initial_press"
        );
        assert_eq!(
            json::uint(&update.events[1].tokens, "eventNumber"),
            0x20000000000001
        );
        assert_eq!(
            json::text(field(&update.device, "store"), "matter.lastEventNumber"),
            "9007199254740994"
        );
        assert!(json::get(field(&original, "store"), "matter.lastEventNumber").is_none());
        assert!(apply(&update.device, &report)?.is_none());
        let paths = subscription(&[original])?;
        assert_eq!(paths.attributes, [im::AttributePath::new(1, 6, 0)]);
        assert_eq!(paths.events.len(), 2);
        assert!(paths.events.iter().all(|e| e.urgent == Some(true)));
        assert!(paths.events.iter().any(|e| e.endpoint == Some(5)));
        Ok(())
    }
    #[test]
    fn zero_is_a_valid_first_event_and_release_does_not_need_a_press() -> Result {
        let update =
            apply(&button()?, &events(&[(0, 3)])?)?.ok_or(Error::Invalid("missing first event"))?;
        assert_eq!(update.events.len(), 1);
        assert_eq!(
            field(field(&update.device, "state"), "button.2"),
            &Value::Bool(false)
        );
        assert!(apply(&update.device, &events(&[(0, 3)])?)?.is_none());
        assert_eq!(watchdog(300), 455000);
        assert_eq!(watchdog(1), 10000);
        Ok(())
    }
}
