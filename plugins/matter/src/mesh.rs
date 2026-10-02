//! De radiokaart groeit per antwoord; de UI ontvangt beoordeelde, ontdubbelde verbindingen.
use crate::discovery::Node;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Error, Result, clone,
    util::{field, join},
};
pub(crate) struct Map {
    pub(crate) nodes: Vec<Value>,
    routers: Vec<Value>,
    neighbours: Vec<Value>,
    warnings: Vec<Value>,
    pub(crate) deadline: u64,
}
fn upper(s: &str) -> Result<String> {
    let mut s = json::copy(s)?;
    s.make_ascii_uppercase();
    Ok(s)
}
impl Map {
    pub(crate) fn new(
        topology: &Value,
        discovered: &[Node],
        fabric: u64,
        deadline: u64,
        warning: Option<&Error>,
    ) -> Result<Self> {
        let mut nodes = Vec::new();
        let mut routers = Vec::new();
        let mut neighbours = Vec::new();
        let mut warnings = Vec::new();
        if let Some(e) = warning {
            json::push(&mut warnings, json::string(&stulp_sdk::message(e)?)?, 64)?;
        }
        for status in json::array(topology, "nodes") {
            let id = json::text(status, "nodeId");
            let first = json::array(status, "devices")
                .first()
                .unwrap_or(&Value::Null);
            let mut node = json::fields(&[
                ("nodeId", json::string(id)?),
                ("name", clone(field(first, "name"))?),
                ("deviceId", clone(field(first, "id"))?),
                ("address", clone(field(status, "address"))?),
                ("sessionOpen", clone(field(status, "sessionOpen"))?),
                ("subscribed", clone(field(status, "subscribed"))?),
                (
                    "endpoints",
                    Value::uint(json::array(status, "devices").len() as u64),
                ),
                ("neighbours", Value::uint(0)),
                ("pending", Value::Bool(true)),
            ])?;
            for n in discovered {
                if n.operational() != u64::from_str_radix(id, 16).ok().map(|id| (fabric, id)) {
                    continue;
                }
                let host = upper(n.host.trim_end_matches('.'))?;
                let host = host.strip_suffix(".LOCAL").unwrap_or(&host);
                if host.len() == 16 && host.bytes().all(|b| b.is_ascii_hexdigit()) {
                    json::set(&mut node, "extAddress", json::string(host)?)?;
                }
            }
            json::push(&mut nodes, node, 64)?;
            json::push(&mut neighbours, Value::Array(Vec::new()), 64)?;
        }
        for n in discovered.iter().filter(|n| n.service == "_meshcop._udp") {
            let mut router = crate::app::scan_entry(n)?;
            json::set(
                &mut router,
                "id",
                json::string(&join(&["router:", &n.instance])?)?,
            )?;
            json::set(&mut router, "name", json::string(&n.instance)?)?;
            let pan = upper(json::text(&router, "extendedPanId"))?;
            json::set(&mut router, "extendedPanId", json::string(&pan)?)?;
            json::push(&mut routers, router, 128)?;
        }
        routers.sort_unstable_by(|a, b| json::text(a, "name").cmp(json::text(b, "name")));
        Ok(Self {
            nodes,
            routers,
            neighbours,
            warnings,
            deadline,
        })
    }
    pub(crate) fn next(&self) -> Option<(usize, &str)> {
        self.nodes
            .iter()
            .enumerate()
            .find(|(_, n)| json::boolean(n, "pending"))
            .map(|(i, n)| (i, json::text(n, "deviceId")))
    }
    pub(crate) fn apply(&mut self, index: usize, result: Result<Value>) -> Result {
        let node = self
            .nodes
            .get_mut(index)
            .ok_or(Error::Invalid("mesh node index"))?;
        json::set(node, "pending", Value::Bool(false))?;
        match result {
            Err(e) => json::set(node, "error", json::string(&stulp_sdk::message(&e)?)?)?,
            Ok(d) => {
                if let Some(thread) = json::get(&d, "thread") {
                    json::set(node, "radio", json::string("thread")?)?;
                    for name in ["networkName", "routingRole"] {
                        if let Some(value) = json::get(thread, name) {
                            json::set(node, name, clone(value)?)?;
                        }
                    }
                    json::set(
                        node,
                        "neighbours",
                        Value::uint(json::array(thread, "neighbours").len() as u64),
                    )?;
                    self.neighbours[index] = json::get(thread, "neighbours")
                        .map(clone)
                        .transpose()?
                        .unwrap_or(Value::Array(Vec::new()));
                } else if let Some(wifi) = json::get(&d, "wifi") {
                    json::set(node, "radio", json::string("wifi")?)?;
                    if let Some(rssi) = json::get(wifi, "rssi") {
                        json::set(node, "rssi", clone(rssi)?)?;
                    }
                }
            }
        }
        Ok(())
    }
    pub(crate) fn snapshot(&self) -> Result<Value> {
        let mut links = Vec::new();
        let mut unidentified = 0u64;
        for (index, node) in self.nodes.iter().enumerate() {
            for neighbour in self.neighbours[index].as_array().unwrap_or(&[]) {
                let far = upper(json::text(neighbour, "extAddress"))?;
                let target = self
                    .nodes
                    .iter()
                    .find(|n| !far.is_empty() && json::text(n, "extAddress") == far)
                    .map(|n| json::text(n, "nodeId"))
                    .unwrap_or("");
                if target.is_empty() {
                    unidentified += 1;
                }
                let mut link = json::fields(&[
                    ("from", clone(field(node, "nodeId"))?),
                    ("to", json::string(target)?),
                    ("toExtAddress", json::string(&far)?),
                    ("kind", json::string("radio")?),
                    ("isChild", Value::Bool(json::boolean(neighbour, "isChild"))),
                    ("mutual", Value::Bool(false)),
                ])?;
                for (source, target) in [
                    ("lqi", "lqi"),
                    ("averageRssi", "rssi"),
                    ("frameErrorRate", "frameErrorRate"),
                ] {
                    if let Some(value) = json::get(neighbour, source) {
                        json::set(&mut link, target, clone(value)?)?;
                    }
                }
                draw(&mut links, link)?;
            }
            let network = json::text(node, "networkName");
            for router in &self.routers {
                if !network.is_empty()
                    && network.eq_ignore_ascii_case(json::text(router, "networkName"))
                {
                    draw(
                        &mut links,
                        json::fields(&[
                            ("from", clone(field(node, "nodeId"))?),
                            ("to", clone(field(router, "id"))?),
                            ("kind", json::string("border")?),
                            ("isChild", Value::Bool(false)),
                            ("mutual", Value::Bool(true)),
                        ])?,
                    )?;
                }
            }
        }
        let mut nodes = Vec::new();
        for n in &self.nodes {
            json::push(&mut nodes, clone(n)?, 64)?;
        }
        let mut routers = Vec::new();
        for r in &self.routers {
            json::push(&mut routers, clone(r)?, 128)?;
        }
        let mut warnings = Vec::new();
        for w in &self.warnings {
            json::push(&mut warnings, clone(w)?, 64)?;
        }
        Ok(json::fields(&[
            ("nodes", Value::Array(nodes)),
            ("routers", Value::Array(routers)),
            ("links", Value::Array(links)),
            ("unidentified", Value::uint(unidentified)),
            ("warnings", Value::Array(warnings)),
        ])?)
    }
}
fn grade(link: &mut Value) -> Result {
    let border = json::text(link, "kind") == "border";
    let lqi = json::get(link, "lqi").and_then(Value::as_u64);
    let grade = if border {
        "border"
    } else {
        match lqi {
            None => "unknown",
            Some(n) if n >= 150 => "strong",
            Some(n) if n >= 80 => "fair",
            _ => "weak",
        }
    };
    let weight = if border {
        1.4
    } else {
        1.5 + lqi.unwrap_or(0) as f64 / 255.0 * 3.0
    };
    json::set(link, "grade", json::string(grade)?)?;
    json::set(link, "weight", stulp_sdk::util::float(weight)?)?;
    Ok(())
}
fn draw(links: &mut Vec<Value>, mut link: Value) -> Result {
    let from = json::text(&link, "from");
    let to = json::text(&link, "to");
    let (a, b) = if to.is_empty() {
        (from, json::text(&link, "toExtAddress"))
    } else if from < to {
        (from, to)
    } else {
        (to, from)
    };
    let id = join(&[json::text(&link, "kind"), "|", a, "|", b])?;
    if let Some(existing) = links.iter_mut().find(|l| json::text(l, "id") == id) {
        if json::text(existing, "from") != from {
            json::set(existing, "mutual", Value::Bool(true))?;
        }
        for key in ["lqi", "rssi"] {
            if json::get(existing, key).is_none()
                && let Some(value) = json::get(&link, key)
            {
                json::set(existing, key, clone(value)?)?;
            }
        }
        grade(existing)?;
    } else {
        json::set(&mut link, "id", json::string(&id)?)?;
        grade(&mut link)?;
        json::push(links, link, 4096)?;
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn mutual_links_are_single_and_measurement_beats_missing() -> Result {
        let mut links = Vec::new();
        draw(
            &mut links,
            json::fields(&[
                ("from", json::string("a")?),
                ("to", json::string("b")?),
                ("kind", json::string("radio")?),
            ])?,
        )?;
        draw(
            &mut links,
            json::fields(&[
                ("from", json::string("b")?),
                ("to", json::string("a")?),
                ("kind", json::string("radio")?),
                ("lqi", Value::uint(180)),
            ])?,
        )?;
        assert_eq!(links.len(), 1);
        assert!(json::boolean(&links[0], "mutual"));
        assert_eq!(json::text(&links[0], "grade"), "strong");
        assert_eq!(json::text(&links[0], "id"), "radio|a|b");
        Ok(())
    }
}
