//! Native endpoints delen één apparaat; bridgekinderen behouden hun eigen identiteit en routes.
use crate::{capabilities, model, settings::decimal};
use alloc::vec::Vec;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Error, Result, clone,
    util::{field, join},
};
/// Bewaarde hex-ID-lijsten blijven bruikbaar na een JSON-rondreis.
pub fn ids(raw: &Value) -> Result<Vec<u32>> {
    let mut out = Vec::new();
    for value in raw.as_array().unwrap_or(&[]) {
        if let Some(s) = value.as_str() {
            let parsed = if let Some(s) = s.strip_prefix("0x").or_else(|| s.strip_prefix("0X")) {
                u32::from_str_radix(s, 16)
            } else {
                s.parse()
            };
            if let Ok(n) = parsed {
                json::push(&mut out, n, 4096)?;
            }
        }
    }
    Ok(out)
}
/// Hoofdendpoint van een bestaande record; corrupte waarden worden geen afgekapt endpoint.
pub fn primary(device: &Value) -> u16 {
    field(field(device, "store"), "matter.endpoint")
        .as_u64()
        .and_then(|n| u16::try_from(n).ok())
        .unwrap_or(0)
}
/// De concrete route blijft behouden wanneer capabilities na samenvoegen worden genummerd.
pub fn endpoint(device: &Value, capability: &str) -> u16 {
    field(
        field(field(device, "store"), "matter.capabilityEndpoints"),
        capability,
    )
    .as_u64()
    .and_then(|n| u16::try_from(n).ok())
    .unwrap_or_else(|| primary(device))
}
/// Alle endpoints, inclusief losse draadloze knoppen.
pub fn endpoints(device: &Value) -> Result<Vec<u16>> {
    let store = field(device, "store");
    let mut out = Vec::new();
    if let Some(n) = field(store, "matter.endpoint")
        .as_u64()
        .and_then(|n| u16::try_from(n).ok())
    {
        json::push(&mut out, n, 4096)?;
    }
    for v in json::array(store, "matter.endpoints") {
        if let Some(n) = v.as_u64().and_then(|n| u16::try_from(n).ok()) {
            json::push(&mut out, n, 4096)?;
        }
    }
    for cap in json::array(device, "capabilities") {
        if let Some(cap) = cap.as_str() {
            json::push(&mut out, endpoint(device, cap), 4096)?;
        }
    }
    out.sort_unstable();
    out.dedup();
    Ok(out)
}
/// Bridges groeperen meerdere producten; hun endpoints mogen nooit worden samengevoegd.
pub fn bridged(device: &Value) -> bool {
    let store = field(device, "store");
    json::boolean(store, "matter.bridged")
        || json::array(store, "matter.serverClusters")
            .iter()
            .filter_map(Value::as_str)
            .any(|s| s.eq_ignore_ascii_case("0x39"))
}
fn rank(device: &Value) -> u8 {
    if json::array(device, "capabilities")
        .iter()
        .filter_map(Value::as_str)
        .any(|c| matches!(capabilities::base(c), "onoff" | "dim" | "locked"))
    {
        return 3;
    }
    match json::text(device, "class") {
        "light" | "socket" | "lock" | "thermostat" => 2,
        "sensor" => 1,
        _ => 0,
    }
}
/// Gevonden capability voor een report, zonder de route van een gelijknamige tweede knop te verliezen.
pub fn capability<'a>(device: &'a Value, base: &str, at: u16) -> Option<&'a str> {
    json::array(device, "capabilities")
        .iter()
        .filter_map(Value::as_str)
        .find(|cap| capabilities::base(cap) == base && endpoint(device, cap) == at)
}
/// Persistente eventnummers zijn decimale tekst, nooit floats.
pub fn event_number(device: &Value) -> u64 {
    json::text(field(device, "store"), "matter.lastEventNumber")
        .parse()
        .unwrap_or(0)
}
fn object(value: &Value) -> Result<Value> {
    if value.as_object().is_some() {
        clone(value)
    } else {
        Ok(json::object())
    }
}
fn merge_object(left: &Value, right: &Value) -> Result<Value> {
    let mut out = object(left)?;
    if let Some(right) = right.as_object() {
        for (key, value) in right.iter() {
            json::set(&mut out, key, clone(value)?)?;
        }
    }
    Ok(out)
}
fn union(left: &Value, right: &Value) -> Result<Value> {
    let mut out = Vec::new();
    for value in left
        .as_array()
        .unwrap_or(&[])
        .iter()
        .chain(right.as_array().unwrap_or(&[]))
    {
        if value.as_str().is_some() && !out.iter().any(|prior| json::equal(prior, value)) {
            json::push(&mut out, clone(value)?, 4096)?;
        }
    }
    Ok(Value::Array(out))
}
fn merge_keyed(left: &Value, right: &Value, key: &str) -> Result<Value> {
    let mut out = Vec::new();
    for value in left
        .as_array()
        .unwrap_or(&[])
        .iter()
        .chain(right.as_array().unwrap_or(&[]))
    {
        if field(value, key) == &Value::Null {
            continue;
        }
        if let Some(at) = out
            .iter()
            .position(|prior| json::equal(field(prior, key), field(value, key)))
        {
            out[at] = clone(value)?;
        } else {
            json::push(&mut out, clone(value)?, 4096)?;
        }
    }
    if key == "endpoint" {
        out.sort_unstable_by_key(|v| json::uint(v, key));
    } else {
        out.sort_unstable_by(|a, b| json::text(a, key).cmp(json::text(b, key)));
    }
    Ok(Value::Array(out))
}
fn merge_metadata(dest: &mut Value, source: &Value) -> Result {
    let mut store = object(field(dest, "store"))?;
    let from = field(source, "store");
    for key in ["matter.deviceTypes", "matter.serverClusters"] {
        let value = union(field(&store, key), field(from, key))?;
        json::set(&mut store, key, value)?;
    }
    for (key, by) in [
        ("~matter.endpointInventory", "endpoint"),
        ("matter.settings", "id"),
    ] {
        let value = merge_keyed(field(&store, key), field(from, key), by)?;
        json::set(&mut store, key, value)?;
    }
    if event_number(source) > event_number(dest) {
        json::set(
            &mut store,
            "matter.lastEventNumber",
            json::string(&decimal(event_number(source))?)?,
        )?;
    }
    let settings = merge_object(field(dest, "settings"), field(source, "settings"))?;
    if json::text(dest, "groupId").is_empty() && !json::text(source, "groupId").is_empty() {
        json::set(dest, "groupId", clone(field(source, "groupId"))?)?;
    }
    json::set(
        dest,
        "available",
        Value::Bool(json::boolean(dest, "available") || json::boolean(source, "available")),
    )?;
    json::set(dest, "settings", settings)?;
    json::set(dest, "store", store)?;
    Ok(())
}
struct Occurrence<'a> {
    device: &'a str,
    cap: &'a str,
    base: &'a str,
    endpoint: u16,
    value: Option<&'a Value>,
    ordinal: usize,
}
/// Resultaat bevat ook de verwijzingen die de controller in flows en scènes moet vervangen.
pub struct Combined {
    /// Eén native apparaat per geselecteerde node, plus ongewijzigde bridgekinderen.
    pub devices: Vec<Value>,
    /// Oude device-ID naar deviceId en capabilityhernoemingen.
    pub replacements: Value,
}
/// Controller-side startup migration, before the plugin receives its snapshot.
/// Uses no device I/O or autonomous plugin deletion. Each node's references and
/// records commit atomically; bridge children and other apps are never selected.
pub fn reconcile<S: stulp_core::store::Storage>(
    store: &mut stulp_core::store::Store<S>,
    now: &str,
) -> Result {
    let mut nodes = Vec::new();
    for device in store.document().records("devices") {
        let node = json::text(field(device, "store"), "matter.nodeId");
        if json::text(device, "appId") == "com.stulp.matter"
            && json::text(device, "driverId") == "matter"
            && !bridged(device)
            && !node.is_empty()
            && !nodes.iter().any(|n| n == node)
        {
            json::push(&mut nodes, json::copy(node)?, 4096)?;
        }
    }
    for node in nodes {
        let mut devices = Vec::new();
        for device in store.document().records("devices") {
            if json::text(device, "appId") == "com.stulp.matter"
                && json::text(device, "driverId") == "matter"
                && json::text(field(device, "store"), "matter.nodeId") == node
                && !bridged(device)
            {
                json::push(&mut devices, store.device(json::text(device, "id"))?, 128)?;
            }
        }
        if devices.len() < 2 {
            continue;
        }
        // Upgrade each endpoint before unioning cluster metadata. Otherwise a
        // secondary lux sensor could inherit the primary lamp's endpoint route.
        // These changes remain in the same transaction as the consolidation.
        for device in &mut devices {
            upgrade(device)?;
        }
        let combined = combine(devices)?;
        for device in combined.devices {
            let id = json::text(&device, "id");
            if json::get(&combined.replacements, id)
                .is_some_and(|r| json::text(r, "deviceId") == id)
            {
                store.consolidate_devices(
                    "com.stulp.matter",
                    device,
                    &combined.replacements,
                    now,
                )?;
            }
        }
    }
    Ok(())
}
/// Behoudt de sterkste hoofdidentiteit en groepeert herhaalde capabilities op endpointvolgorde.
pub fn combine(devices: Vec<Value>) -> Result<Combined> {
    let mut selected = Vec::new();
    let mut node = "";
    for (index, device) in devices.iter().enumerate() {
        let candidate = json::text(field(device, "store"), "matter.nodeId");
        if candidate.is_empty() || bridged(device) {
            continue;
        }
        if node.is_empty() {
            node = candidate;
        }
        if node == candidate {
            json::push(&mut selected, index, 128)?;
        }
    }
    if selected.len() < 2 {
        return Ok(Combined {
            devices,
            replacements: json::object(),
        });
    }
    selected.sort_unstable_by_key(|i| {
        (
            core::cmp::Reverse(rank(&devices[*i])),
            primary(&devices[*i]),
            *i,
        )
    });
    let base_index = selected[0];
    let mut merged = clone(&devices[base_index])?;
    let mut occurrences = Vec::new();
    let mut all_endpoints = Vec::new();
    let mut replacements = json::object();
    for index in &selected {
        let device = &devices[*index];
        let id = json::text(device, "id");
        for ep in endpoints(device)? {
            json::push(&mut all_endpoints, ep, 4096)?;
        }
        for cap in json::array(device, "capabilities")
            .iter()
            .filter_map(Value::as_str)
        {
            let ordinal = occurrences.len();
            json::push(
                &mut occurrences,
                Occurrence {
                    device: id,
                    cap,
                    base: capabilities::base(cap),
                    endpoint: endpoint(device, cap),
                    value: json::get(field(device, "state"), cap),
                    ordinal,
                },
                4096,
            )?;
        }
        merge_metadata(&mut merged, device)?;
        if !id.is_empty() {
            json::set(
                &mut replacements,
                id,
                json::fields(&[
                    ("deviceId", json::string(json::text(&merged, "id"))?),
                    ("capabilities", json::object()),
                ])?,
            )?;
        }
    }
    occurrences.sort_unstable_by_key(|o| (o.base, o.endpoint, o.ordinal));
    let mut caps = Vec::new();
    let mut state = json::object();
    let mut routes = json::object();
    let mut at = 0;
    while at < occurrences.len() {
        let end = at
            + occurrences[at..]
                .iter()
                .take_while(|o| o.base == occurrences[at].base)
                .count();
        for (ordinal, o) in occurrences[at..end].iter().enumerate() {
            let cap = if end - at > 1 {
                join(&[o.base, ".", &decimal((ordinal + 1) as u64)?])?
            } else {
                json::copy(o.base)?
            };
            json::push(&mut caps, json::string(&cap)?, 4096)?;
            json::set(&mut routes, &cap, Value::uint(u64::from(o.endpoint)))?;
            if let Some(value) = o.value {
                json::set(&mut state, &cap, clone(value)?)?;
            }
            if !o.device.is_empty() {
                let mut replacement = clone(field(&replacements, o.device))?;
                let mut map = clone(field(&replacement, "capabilities"))?;
                json::set(&mut map, o.cap, json::string(&cap)?)?;
                json::set(&mut replacement, "capabilities", map)?;
                json::set(&mut replacements, o.device, replacement)?;
            }
        }
        at = end;
    }
    all_endpoints.sort_unstable();
    all_endpoints.dedup();
    let mut eps = Vec::new();
    for ep in all_endpoints {
        json::push(&mut eps, Value::uint(u64::from(ep)), 4096)?;
    }
    let eps = Value::Array(eps);
    let mut store = object(field(&merged, "store"))?;
    json::set(&mut store, "matter.endpoints", clone(&eps)?)?;
    json::set(&mut store, "matter.capabilityEndpoints", routes)?;
    let mut data = object(field(&merged, "data"))?;
    json::set(&mut data, "endpoints", eps)?;
    let suffix = join(&[" · ", &decimal(u64::from(primary(&merged)))?])?;
    let name = json::text(&merged, "name");
    let name = json::string(name.strip_suffix(&suffix).unwrap_or(name))?;
    for (key, value) in [
        ("name", name),
        ("capabilities", Value::Array(caps)),
        ("state", state),
        ("store", store),
        ("data", data),
    ] {
        json::set(&mut merged, key, value)?;
    }
    let mut out = Vec::new();
    let mut merged = Some(merged);
    for (index, device) in devices.into_iter().enumerate() {
        if index == base_index {
            json::push(
                &mut out,
                merged
                    .take()
                    .ok_or(Error::Invalid("combined device consumed twice"))?,
                128,
            )?;
        } else if !selected.contains(&index) {
            json::push(&mut out, device, 128)?;
        }
    }
    Ok(Combined {
        devices: out,
        replacements,
    })
}
/// Een hernieuwde descriptor vervangt alleen hardwarevelden, niet gebruikersnamen of koppelsleutels.
pub fn refresh(existing: &Value, prototype: &Value) -> Result<Value> {
    let mut out = clone(prototype)?;
    for key in ["id", "name", "groupId"] {
        if let Some(value) = json::get(existing, key) {
            json::set(&mut out, key, clone(value)?)?;
        }
    }
    for (container, keys) in [
        (
            "data",
            &[
                "id",
                "nodeId",
                "endpoint",
                "endpoints",
                "vendorId",
                "productId",
            ][..],
        ),
        (
            "store",
            &[
                "manufacturer",
                "matter.attestation",
                "matter.bridged",
                "matter.endpoint",
                "matter.endpoints",
                "matter.capabilityEndpoints",
                "matter.deviceTypes",
                "matter.serverClusters",
                "~matter.endpointInventory",
                "matter.settings",
                "matter.modelVersion",
            ][..],
        ),
    ] {
        let mut value = object(field(existing, container))?;
        for key in keys {
            if let Some(v) = json::get(field(prototype, container), key) {
                json::set(&mut value, key, clone(v)?)?;
            }
        }
        json::set(&mut out, container, value)?;
    }
    for key in ["settings", "state"] {
        json::set(
            &mut out,
            key,
            merge_object(field(existing, key), field(prototype, key))?,
        )?;
    }
    json::set(&mut out, "available", Value::Bool(true))?;
    json::set(&mut out, "message", json::string("")?)?;
    Ok(out)
}
/// Oude bewegingssensoren krijgen de al geadverteerde lichtmeter zonder opnieuw koppelen.
pub fn upgrade(device: &mut Value) -> Result<bool> {
    if !ids(field(field(device, "store"), "matter.serverClusters"))?.contains(&0x400)
        || json::array(device, "capabilities")
            .iter()
            .filter_map(Value::as_str)
            .any(|cap| capabilities::base(cap) == "measure_luminance")
    {
        return Ok(false);
    }
    let endpoint = json::array(device, "capabilities")
        .iter()
        .filter_map(Value::as_str)
        .find(|cap| capabilities::base(cap) == "alarm_motion")
        .map(|cap| endpoint(device, cap))
        .unwrap_or_else(|| primary(device));
    let mut routes = json::object();
    for cap in json::array(device, "capabilities")
        .iter()
        .filter_map(Value::as_str)
    {
        json::set(
            &mut routes,
            cap,
            Value::uint(u64::from(self::endpoint(device, cap))),
        )?;
    }
    json::set(
        &mut routes,
        "measure_luminance",
        Value::uint(u64::from(endpoint)),
    )?;
    let mut caps = Vec::new();
    for cap in json::array(device, "capabilities") {
        json::push(&mut caps, clone(cap)?, 4096)?;
    }
    json::push(&mut caps, json::string("measure_luminance")?, 4096)?;
    let mut store = object(field(device, "store"))?;
    json::set(&mut store, "matter.capabilityEndpoints", routes)?;
    json::set(device, "store", store)?;
    json::set(device, "capabilities", Value::Array(caps))?;
    Ok(true)
}
/// Naam bewaren gebeurt na combineren, zodat het kunstmatige endpointachtervoegsel verdwijnt.
pub fn finish(devices: Vec<Value>) -> Result<Vec<Value>> {
    let mut devices = combine(devices)?.devices;
    for device in &mut devices {
        model::preserve_name(device)?;
    }
    Ok(devices)
}
#[cfg(test)]
mod tests {
    use super::*;
    fn device(id: &str, ep: u16, cap: &str, class: &str) -> Result<Value> {
        Ok(json::fields(&[
            ("id", json::string(id)?),
            (
                "name",
                json::string(&join(&["Light · ", &decimal(u64::from(ep))?])?)?,
            ),
            ("class", json::string(class)?),
            (
                "capabilities",
                Value::Array(alloc::vec![json::string(cap)?]),
            ),
            ("state", json::fields(&[(cap, Value::Bool(false))])?),
            (
                "store",
                json::fields(&[
                    ("matter.nodeId", json::string("0000000000010000")?),
                    ("matter.endpoint", Value::uint(u64::from(ep))),
                ])?,
            ),
        ])?)
    }
    #[test]
    fn native_buttons_preserve_routes_and_references_but_bridge_children_stay_separate() -> Result {
        let light = device("light", 1, "onoff", "light")?;
        let a = device("a", 4, "button", "sensor")?;
        let b = device("b", 5, "button", "sensor")?;
        let mut bridge = device("bridge", 6, "measure_temperature", "sensor")?;
        let mut store = clone(field(&bridge, "store"))?;
        json::set(&mut store, "matter.bridged", Value::Bool(true))?;
        json::set(&mut bridge, "store", store)?;
        let result = combine(alloc::vec![b, bridge, a, light])?;
        assert_eq!(result.devices.len(), 2);
        let light = result
            .devices
            .iter()
            .find(|d| json::text(d, "id") == "light")
            .ok_or(Error::Invalid("missing combined light"))?;
        assert_eq!(json::text(light, "name"), "Light");
        assert_eq!(
            json::array(light, "capabilities"),
            [
                json::string("button.1")?,
                json::string("button.2")?,
                json::string("onoff")?
            ]
        );
        assert_eq!(
            (
                endpoint(light, "button.1"),
                endpoint(light, "button.2"),
                endpoint(light, "onoff")
            ),
            (4, 5, 1)
        );
        assert_eq!(endpoints(light)?, [1, 4, 5]);
        assert_eq!(
            json::text(
                field(field(&result.replacements, "b"), "capabilities"),
                "button"
            ),
            "button.2"
        );
        assert_eq!(
            json::text(field(&result.replacements, "b"), "deviceId"),
            "light"
        );
        Ok(())
    }
    #[test]
    fn refresh_keeps_user_identity_and_credentials_and_upgrade_is_idempotent() -> Result {
        let mut old = device("old", 3, "alarm_motion", "sensor")?;
        json::set(&mut old, "name", json::string("Hallway")?)?;
        json::set(&mut old, "groupId", json::string("room")?)?;
        let mut store = clone(field(&old, "store"))?;
        json::set(&mut store, "matter.noc", json::string("original")?)?;
        json::set(
            &mut store,
            "matter.serverClusters",
            Value::Array(alloc::vec![json::string("0x400")?, json::string("0x406")?]),
        )?;
        json::set(&mut old, "store", store)?;
        assert!(upgrade(&mut old)?);
        assert!(!upgrade(&mut old)?);
        assert_eq!(endpoint(&old, "measure_luminance"), 3);
        let prototype = device("prototype", 7, "onoff", "light")?;
        let new = refresh(&old, &prototype)?;
        assert_eq!(json::text(&new, "id"), "old");
        assert_eq!(json::text(&new, "name"), "Hallway");
        assert_eq!(json::text(&new, "groupId"), "room");
        assert_eq!(json::text(field(&new, "store"), "matter.noc"), "original");
        assert_eq!(primary(&new), 7);
        Ok(())
    }
}
