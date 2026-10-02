//! Stabiliteit is een timer buiten een Flow-run; tussentijdse wijzigingen resetten hem.
use crate::flows::Run;
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result, flow,
    json::{self, Number, TryClone, Value},
    store::{Storage, Store},
};
const MAX_WATCHES: usize = 4096;
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
struct Watch {
    flow: String,
    node: String,
    spec: Value,
    deadline: u64,
    revision: u64,
    fired: bool,
    seen: bool,
}
/// Eén eigenaar onderhoudt deadlines voor de actuele, ingeschakelde Flow-definities.
#[derive(Default)]
pub struct Stability {
    watches: Vec<Watch>,
}
impl Stability {
    /// Verzoent timers zonder een ongewijzigde deadline door andere Flow-edits te verschuiven.
    pub fn reconcile<S: Storage>(&mut self, store: &Store<S>, now: u64) -> Result {
        for w in &mut self.watches {
            w.seen = false;
        }
        for definition in store
            .document()
            .records("flows")
            .iter()
            .filter(|d| json::boolean(d, "enabled"))
        {
            for node in json::array(definition, "nodes") {
                let Some(spec) = spec(node)? else {
                    continue;
                };
                let device = json::text(&spec, "deviceId");
                let cap = json::text(&spec, "capability");
                let Ok(d) = store.device(device) else {
                    continue;
                };
                let current = field(field(&d, "state"), cap);
                if current.is_null() || !json::equal(current, field(&spec, "value")) {
                    continue;
                }
                let fid = json::text(definition, "id");
                let nid = json::text(node, "id");
                let revision = store.capability_revision(device, cap);
                let delay = json::uint(&spec, "delay");
                if let Some(w) = self
                    .watches
                    .iter_mut()
                    .find(|w| w.flow == fid && w.node == nid)
                {
                    if !json::equal(&w.spec, &spec) || w.revision != revision {
                        w.spec = spec;
                        w.deadline = now.saturating_add(delay);
                        w.fired = false;
                        w.revision = revision;
                    }
                    w.seen = true;
                } else {
                    json::push(
                        &mut self.watches,
                        Watch {
                            flow: json::copy(fid)?,
                            node: json::copy(nid)?,
                            spec,
                            deadline: now.saturating_add(delay),
                            revision,
                            fired: false,
                            seen: true,
                        },
                        MAX_WATCHES,
                    )?;
                }
            }
        }
        self.watches.retain(|w| w.seen);
        Ok(())
    }
    /// Levert hoogstens één nieuwe uitvoering; een bezette host vraagt later opnieuw.
    pub fn due<S: Storage>(
        &mut self,
        store: &Store<S>,
        now: u64,
        ran_at: &str,
    ) -> Result<Option<Run>> {
        let Some(w) = self
            .watches
            .iter_mut()
            .find(|w| !w.fired && w.deadline <= now)
        else {
            return Ok(None);
        };
        let d = store.device(json::text(&w.spec, "deviceId"))?;
        let state = json::fields(&[
            ("device", json::string(json::text(&d, "name"))?),
            ("deviceId", field(&w.spec, "deviceId").try_clone()?),
            ("capability", field(&w.spec, "capability").try_clone()?),
            ("value", field(&w.spec, "value").try_clone()?),
            ("seconds", field(&w.spec, "seconds").try_clone()?),
        ])?;
        let context = json::fields(&[("tokens", state.try_clone()?), ("state", state)])?;
        let run = Run::selected(
            store.document().record("flows", &w.flow)?,
            &[&w.node],
            context,
            now,
            ran_at,
        )?;
        w.fired = true;
        Ok(Some(run))
    }
}
fn spec(node: &Value) -> Result<Option<Value>> {
    let step = field(node, "step");
    if flow::kind(node) != "trigger" || json::text(step, "appId") != "stulp" {
        return Ok(None);
    }
    let args = field(step, "args");
    let id = json::text(step, "cardId");
    let (cap, target) = if id == "device_capability_stays" {
        (
            json::text(args, "capability"),
            field(args, "value").try_clone()?,
        )
    } else if let Some((cap, suffix)) = id
        .strip_prefix("capability.")
        .and_then(|s| s.rsplit_once('.'))
    {
        if !matches!(suffix, "on_for" | "off_for") {
            return Ok(None);
        }
        (cap, Value::Bool(suffix == "on_for"))
    } else {
        return Ok(None);
    };
    let device = field(args, "device");
    let device = device
        .as_str()
        .unwrap_or_else(|| json::text(device, "$device"));
    let Value::Number(seconds) = field(args, "seconds") else {
        return Ok(None);
    };
    let seconds = seconds.as_f64();
    if device.is_empty()
        || cap.is_empty()
        || !seconds.is_finite()
        || seconds <= 0.0
        || seconds > 86400.0
    {
        return Ok(None);
    }
    let delay = (seconds * 1000.0) as u64;
    if delay == 0 {
        return Err(Error::Invalid(
            "stability interval is below clock resolution",
        ));
    }
    Ok(Some(json::fields(&[
        ("deviceId", json::string(device)?),
        ("capability", json::string(cap)?),
        ("value", target),
        ("seconds", Value::Number(Number::Float(seconds))),
        ("delay", Value::uint(delay)),
    ])?))
}
