//! Begrensde Flow-grafen en uitvoering zonder recursie of gedeelde staat.
use crate::{
    Error, Result,
    json::{self, Value},
};

/// Het canvascontract uit de Go-implementatie.
pub const MAX_NODES: usize = 128;
/// Maximale verbindingen per Flow.
pub const MAX_EDGES: usize = 256;

/// De genormaliseerde soort van een kaart.
pub fn kind(node: &Value) -> &str {
    match json::get(node, "step")
        .map(|s| json::text(s, "cardType"))
        .unwrap_or("")
    {
        "device-trigger" | "trigger" => "trigger",
        "condition" => "condition",
        "action" => "action",
        _ => "",
    }
}

fn node_index(nodes: &[Value], id: &str) -> Result<usize> {
    nodes
        .iter()
        .position(|n| json::text(n, "id") == id)
        .ok_or(Error::Missing("connection references a missing card"))
}

/// Valideert een DAG; een nog niet verbonden canvas mag worden opgeslagen.
pub fn validate(flow: &Value) -> Result {
    let name = json::text(flow, "name").trim();
    if name.is_empty() || name.len() > 160 {
        return Err(Error::Invalid("flow name is required, at most 160 bytes"));
    }
    let nodes = json::array(flow, "nodes");
    let edges = json::array(flow, "edges");
    if nodes.len() > MAX_NODES || edges.len() > MAX_EDGES {
        return Err(Error::Full);
    }
    for (i, node) in nodes.iter().enumerate() {
        validate_node(node)?;
        if nodes
            .iter()
            .take(i)
            .any(|n| json::text(n, "id") == json::text(node, "id"))
        {
            return Err(Error::Conflict("duplicate flow card id"));
        }
    }
    let mut incoming = [0_u16; MAX_NODES];
    for (i, edge) in edges.iter().enumerate() {
        let from = node_index(nodes, json::text(edge, "from"))?;
        let to = node_index(nodes, json::text(edge, "to"))?;
        if from == to {
            return Err(Error::Invalid("card cannot connect to itself"));
        }
        if nodes.get(to).map(kind) == Some("trigger") {
            return Err(Error::Invalid("trigger cannot have incoming connections"));
        }
        if edges.iter().take(i).any(|e| {
            json::text(e, "from") == json::text(edge, "from")
                && json::text(e, "to") == json::text(edge, "to")
        }) {
            return Err(Error::Conflict("duplicate flow connection"));
        }
        let degree = incoming.get_mut(to).ok_or(Error::Full)?;
        *degree += 1;
    }
    let mut seen = [false; MAX_NODES];
    for _ in 0..nodes.len() {
        let next =
            (0..nodes.len()).find(|&i| incoming.get(i) == Some(&0) && seen.get(i) == Some(&false));
        let index = next.ok_or(Error::Invalid("flow connections may not contain a cycle"))?;
        *seen.get_mut(index).ok_or(Error::Full)? = true;
        let id = nodes
            .get(index)
            .map(|n| json::text(n, "id"))
            .ok_or(Error::Full)?;
        for edge in edges.iter().filter(|e| json::text(e, "from") == id) {
            let to = node_index(nodes, json::text(edge, "to"))?;
            let degree = incoming.get_mut(to).ok_or(Error::Full)?;
            *degree = degree
                .checked_sub(1)
                .ok_or(Error::Invalid("invalid graph degree"))?;
        }
    }
    Ok(())
}

fn validate_node(node: &Value) -> Result {
    if json::text(node, "id").trim().is_empty() {
        return Err(Error::Invalid("flow card id is required"));
    }
    if kind(node).is_empty() {
        return Err(Error::Invalid("flow card type is invalid"));
    }
    let step = json::get(node, "step").ok_or(Error::Missing("flow step is required"))?;
    if json::text(step, "appId").is_empty() || json::text(step, "cardId").is_empty() {
        return Err(Error::Invalid("flow appId and cardId are required"));
    }
    for key in ["x", "y"] {
        if let Some(value) = json::get(node, key) {
            let Value::Number(number) = value else {
                return Err(Error::Invalid("invalid card position"));
            };
            let n = number.as_f64();
            if !n.is_finite() || n.abs() > 1_000_000.0 {
                return Err(Error::Invalid("invalid card position"));
            }
        }
    }
    Ok(())
}

/// Geeft aan of een trigger een actie kan bereiken, met dezelfde uitleg als Go.
pub fn runnable(flow: &Value) -> (bool, &'static str) {
    let nodes = json::array(flow, "nodes");
    if nodes.len() > MAX_NODES {
        return (false, "too many cards");
    }
    if !nodes.iter().any(|n| kind(n) == "trigger") {
        return (false, "add an ALS/trigger card");
    }
    if !nodes.iter().any(|n| kind(n) == "action") {
        return (false, "add a DAN/action card");
    }
    let mut reachable = [false; MAX_NODES];
    for (index, node) in nodes.iter().enumerate() {
        if let Some(r) = reachable.get_mut(index) {
            *r = kind(node) == "trigger";
        }
    }
    for _ in 0..nodes.len() {
        for edge in json::array(flow, "edges") {
            if let (Ok(from), Ok(to)) = (
                node_index(nodes, json::text(edge, "from")),
                node_index(nodes, json::text(edge, "to")),
            ) && reachable.get(from) == Some(&true)
                && let Some(r) = reachable.get_mut(to)
            {
                *r = true;
            }
        }
    }
    if nodes
        .iter()
        .enumerate()
        .any(|(i, n)| kind(n) == "action" && reachable.get(i) == Some(&true))
    {
        return (true, "");
    }
    (false, "connect the ALS/trigger card to a DAN/action card")
}

/// Een uitvoeringsplan bezit uitsluitend indices; callbackresultaten sturen vervolgwerk.
pub struct Execution {
    pending: [usize; MAX_NODES],
    head: usize,
    tail: usize,
    visited: [bool; MAX_NODES],
    waiting: Option<usize>,
}

impl Execution {
    /// Start bij één reeds gematchte trigger; de aanroeper bezit de onveranderlijke Flow.
    pub fn start(flow: &Value, trigger_id: &str) -> Result<Self> {
        Self::start_many(flow, &[trigger_id])
    }

    /// Meerdere gematchte triggers delen één visited-set en behouden de edgevolgorde.
    pub fn start_many(flow: &Value, trigger_ids: &[&str]) -> Result<Self> {
        validate(flow)?;
        let nodes = json::array(flow, "nodes");
        let mut out = Self {
            pending: [0; MAX_NODES],
            head: 0,
            tail: 0,
            visited: [false; MAX_NODES],
            waiting: None,
        };
        for id in trigger_ids {
            let index = node_index(nodes, id)?;
            if nodes.get(index).map(kind) != Some("trigger") {
                return Err(Error::Invalid("flow must start at a trigger"));
            }
            if out.visited.get(index) == Some(&true) {
                continue;
            }
            out.waiting = Some(index);
            out.complete(flow, true)?;
        }
        Ok(out)
    }

    /// Levert één kaart; eerst complete aanroepen voordat de volgende kaart mag starten.
    pub fn next<'a>(&mut self, flow: &'a Value) -> Result<Option<&'a Value>> {
        if self.waiting.is_some() {
            return Err(Error::Conflict("flow callback is still pending"));
        }
        let nodes = json::array(flow, "nodes");
        if self.head == self.tail {
            return Ok(None);
        }
        let index = *self.pending.get(self.head).ok_or(Error::Full)?;
        self.head += 1;
        self.waiting = Some(index);
        Ok(Some(nodes.get(index).ok_or(Error::Changed)?))
    }

    /// Een false condition stopt alleen zijn eigen tak; een gedeelde opvolger draait eenmaal.
    pub fn complete(&mut self, flow: &Value, passed: bool) -> Result {
        let index = self
            .waiting
            .take()
            .ok_or(Error::Conflict("no flow callback pending"))?;
        *self.visited.get_mut(index).ok_or(Error::Full)? = true;
        let nodes = json::array(flow, "nodes");
        let node = nodes.get(index).ok_or(Error::Changed)?;
        let inverted = json::get(node, "step").is_some_and(|s| json::boolean(s, "inverted"));
        if kind(node) == "condition" && passed == inverted {
            return Ok(());
        }
        for edge in json::array(flow, "edges")
            .iter()
            .filter(|e| json::text(e, "from") == json::text(node, "id"))
        {
            let to = node_index(nodes, json::text(edge, "to"))?;
            if self.visited.get(to) == Some(&false) {
                *self.pending.get_mut(self.tail).ok_or(Error::Full)? = to;
                self.tail += 1;
                *self.visited.get_mut(to).ok_or(Error::Full)? = true;
            }
        }
        Ok(())
    }
}
