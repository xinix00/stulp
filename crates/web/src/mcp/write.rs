//! MCP-edits delen validatie, eenheden en atomair opslaan met het bestaande Flow-canvas.
use super::*;
use crate::Environment;
fn values(v: &Value, key: &str) -> Result<Vec<Value>> {
    let mut out = Vec::new();
    for v in json::array(v, key) {
        json::push(&mut out, v.try_clone()?, 512)?;
    }
    Ok(out)
}
fn ids(list: &mut [Value], env: &mut impl Environment) -> Result {
    for v in list {
        if text(v, "id").is_empty() {
            json::set(v, "id", json::string(&env.id()?)?)?;
        }
    }
    Ok(())
}
pub(super) fn edit<S: Storage>(
    store: &mut Store<S>,
    name: &str,
    args: &Value,
    cards: &Value,
    env: &mut impl Environment,
) -> Result<Value> {
    let create = name == "flows_create";
    let id = if create {
        env.id()?
    } else {
        json::copy(text(args, "flowId"))?
    };
    if name == "flows_delete" {
        store.delete("flows", &id)?;
        return json::fields(&[
            ("deleted", Value::Bool(true)),
            ("flowId", json::string(&id)?),
        ]);
    }
    let before = if create {
        None
    } else {
        Some(store.document().record("flows", &id)?.try_clone()?)
    };
    let mut f = if let Some(before) = &before {
        crate::flow_units::convert(store, before, false, None)?
    } else {
        json::fields(&[("id", json::string(&id)?), ("enabled", Value::Bool(true))])?
    };
    let mut nodes = values(&f, "nodes")?;
    let mut edges = values(&f, "edges")?;
    let mut delta = json::object();
    match name {
        "flows_create" => {
            json::set(&mut f, "name", field(args, "name").try_clone()?)?;
            if let Some(v) = json::get(args, "enabled") {
                json::set(&mut f, "enabled", v.try_clone()?)?;
            }
            nodes = values(args, "nodes")?;
            edges = values(args, "edges")?;
            for n in &mut nodes {
                arguments::normalize(store, n, cards, true, false, false)?;
            }
        }
        "flows_update" => {
            let mut changed = false;
            for k in ["name", "enabled"] {
                if let Some(v) = json::get(args, k) {
                    json::set(&mut f, k, v.try_clone()?)?;
                    changed = true;
                }
            }
            if !changed {
                return Err(Error::Invalid("provide name and/or enabled"));
            }
        }
        "flows_add_cards" => {
            let mut added = values(args, "nodes")?;
            for n in &mut added {
                arguments::normalize(store, n, cards, true, false, false)?;
            }
            ids(&mut added, env)?;
            let mut names = Vec::new();
            for n in added {
                json::push(&mut names, field(&n, "id").try_clone()?, 128)?;
                json::push(&mut nodes, n, 128)?;
            }
            for e in values(args, "edges")? {
                json::push(&mut edges, e, 256)?;
            }
            json::set(&mut delta, "nodeIds", Value::Array(names))?;
        }
        "flows_configure_card" => {
            let id = text(args, "nodeId");
            let n = nodes
                .iter_mut()
                .find(|n| json::text(n, "id") == id)
                .ok_or(Error::Missing("Flow node does not exist"))?;
            let mut step = field(n, "step").try_clone()?;
            let mut changed = false;
            if let Some(patch) = json::get(args, "args").and_then(Value::as_object) {
                let mut a = field(&step, "args").try_clone()?;
                if a.is_null() {
                    a = json::object();
                }
                for (k, v) in patch.iter() {
                    if v.is_null() {
                        json::remove(&mut a, k)?;
                    } else {
                        json::set(&mut a, k, v.try_clone()?)?;
                    }
                }
                json::set(&mut step, "args", a)?;
                changed = true;
            }
            if let Some(v) = json::get(args, "inverted") {
                json::set(&mut step, "inverted", v.try_clone()?)?;
                changed = true;
            }
            for k in ["x", "y"] {
                if let Some(v) = json::get(args, k) {
                    json::set(n, k, v.try_clone()?)?;
                    changed = true;
                }
            }
            if !changed {
                return Err(Error::Invalid("provide args, inverted, x and/or y"));
            }
            json::set(n, "step", step)?;
            arguments::normalize(store, n, cards, false, true, false)?;
            json::set(&mut delta, "nodeId", json::string(id)?)?;
        }
        "flows_connect_cards" => {
            let edge = text(args, "edgeId");
            let edge = if edge.is_empty() {
                env.id()?
            } else {
                json::copy(edge)?
            };
            json::push(
                &mut edges,
                json::fields(&[
                    ("id", json::string(&edge)?),
                    ("from", json::string(text(args, "fromNodeId"))?),
                    ("to", json::string(text(args, "toNodeId"))?),
                ])?,
                256,
            )?;
            json::set(&mut delta, "edgeId", json::string(&edge)?)?;
        }
        "flows_disconnect_cards" => {
            let id = text(args, "edgeId");
            let from = text(args, "fromNodeId");
            let to = text(args, "toNodeId");
            if id.is_empty() && (from.is_empty() || to.is_empty()) {
                return Err(Error::Invalid("provide edgeId or fromNodeId and toNodeId"));
            }
            let count = edges.len();
            edges.retain(|e| {
                if id.is_empty() {
                    json::text(e, "from") != from || json::text(e, "to") != to
                } else {
                    json::text(e, "id") != id
                }
            });
            let removed = count - edges.len();
            if removed == 0 {
                return Err(Error::Missing("connection does not exist"));
            }
            json::set(
                &mut delta,
                "removedConnections",
                Value::uint(removed as u64),
            )?;
        }
        "flows_remove_card" => {
            let id = text(args, "nodeId");
            let count = nodes.len();
            nodes.retain(|n| json::text(n, "id") != id);
            if nodes.len() == count {
                return Err(Error::Missing("Flow node does not exist"));
            }
            edges.retain(|e| json::text(e, "from") != id && json::text(e, "to") != id);
            json::set(&mut delta, "removedNodeId", json::string(id)?)?;
        }
        _ => return Err(Error::Invalid("unknown Flow edit")),
    }
    ids(&mut nodes, env)?;
    ids(&mut edges, env)?;
    json::set(&mut f, "nodes", Value::Array(nodes))?;
    json::set(&mut f, "edges", Value::Array(edges))?;
    let f = crate::flow_units::convert(store, &f, true, before.as_ref())?;
    store.put(
        "flows",
        f,
        create,
        before.as_ref().map(|v| json::uint(v, "revision")),
        &env.now()?,
    )?;
    json::set(
        &mut delta,
        "flow",
        read::flow_summary(store.document().record("flows", &id)?)?,
    )?;
    Ok(delta)
}
