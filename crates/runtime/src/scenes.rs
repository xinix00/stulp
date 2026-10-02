//! Eén scene bezit haar herstelplan; de transportadapter voert apparaatgroepen uit.
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
const MAX_STATES: usize = 256;
struct Plan {
    state: Value,
    result: Option<Value>,
    sent: bool,
}
/// Activatie bewaart de oude waarden vóór de eerste apparaatopdracht.
pub struct Scene {
    definition: Value,
    plans: Vec<Plan>,
    first: bool,
    on: bool,
    momentary: bool,
}
/// Numerieke tolerantie voor discrete dimstappen en afgeronde meetwaarden, gelijk aan Go.
pub fn same(a: &Value, b: &Value) -> bool {
    stulp_core::store::scene_values_equal(a, b)
}

fn field<'a>(v: &'a Value, key: &str) -> &'a Value {
    json::get(v, key).unwrap_or(&Value::Null)
}
fn key(a: &Value, b: &Value) -> bool {
    json::text(a, "deviceId") == json::text(b, "deviceId")
        && json::text(a, "capabilityId") == json::text(b, "capabilityId")
}
fn result(state: &Value, error: Option<&str>, unchanged: bool) -> Result<Value> {
    let mut r = state.try_clone()?;
    json::set(&mut r, "success", Value::Bool(error.is_none()))?;
    if let Some(e) = error {
        json::set(&mut r, "error", json::string(e)?)?;
    }
    if unchanged {
        json::set(&mut r, "unchanged", Value::Bool(true))?;
    }
    Ok(r)
}
impl Scene {
    /// Maakt een compleet plan; een opslagfout kan nog geen apparaat hebben gewijzigd.
    pub fn start<S: Storage>(store: &mut Store<S>, id: &str, on: bool) -> Result<Self> {
        let mut definition = store.document().record("scenes", id.trim())?.try_clone()?;
        let momentary = json::text(&definition, "kind") == "button";
        if momentary && !on {
            return Err(Error::Invalid("button scene cannot be turned off"));
        }
        let active = json::boolean(&definition, "active");
        let states = if on {
            json::array(&definition, "states")
        } else if active {
            json::array(&definition, "previous")
        } else {
            &[]
        };
        let mut current = Vec::new();
        let mut previous = Vec::new();
        for wanted in states {
            let value = store
                .device(json::text(wanted, "deviceId"))
                .ok()
                .and_then(|d| {
                    json::get(field(&d, "state"), json::text(wanted, "capabilityId"))
                        .filter(|v| !v.is_null())
                        .map(TryClone::try_clone)
                })
                .transpose()?;
            if on
                && !momentary
                && let Some(v) = &value
            {
                let mut prior = wanted.try_clone()?;
                json::set(&mut prior, "value", v.try_clone()?)?;
                json::push(&mut previous, prior, MAX_STATES)?;
            }
            json::push(&mut current, value, MAX_STATES)?;
        }
        let first = on && !momentary && !active;
        if first {
            json::set(&mut definition, "previous", Value::Array(previous))?;
        }
        let mut plans = Vec::new();
        for (index, wanted) in (if on {
            json::array(&definition, "states")
        } else if active {
            json::array(&definition, "previous")
        } else {
            &[]
        })
        .iter()
        .enumerate()
        {
            let unchanged = current
                .get(index)
                .and_then(Option::as_ref)
                .is_some_and(|v| same(v, field(wanted, "value")));
            let held = json::array(&definition, "previous")
                .iter()
                .any(|p| key(p, wanted));
            let outcome = if unchanged {
                Some(result(wanted, None, true)?)
            } else if on && !momentary && !held {
                Some(result(
                    wanted,
                    Some("current or stored restore value is unavailable"),
                    false,
                )?)
            } else {
                None
            };
            json::push(
                &mut plans,
                Plan {
                    state: wanted.try_clone()?,
                    result: outcome,
                    sent: false,
                },
                MAX_STATES,
            )?;
        }
        if first {
            let mut baseline = Vec::new();
            for p in json::array(&definition, "previous") {
                json::push(&mut baseline, p.try_clone()?, MAX_STATES)?;
            }
            store.begin_scene(id.trim(), baseline)?;
        }
        Ok(Self {
            definition,
            plans,
            first,
            on,
            momentary,
        })
    }
    /// Identiteit blijft behouden zolang de uitvoering duurt.
    pub fn id(&self) -> &str {
        json::text(&self.definition, "id")
    }
    /// Hoogstens één groep per apparaat; groepen mogen tegelijk op het appkanaal wachten.
    pub fn next_group(&mut self) -> Result<Option<(String, Value)>> {
        let Some(plan) = self.plans.iter().find(|p| p.result.is_none() && !p.sent) else {
            return Ok(None);
        };
        let id = json::copy(json::text(&plan.state, "deviceId"))?;
        let mut indices = Vec::new();
        for (i, p) in self.plans.iter().enumerate() {
            if p.result.is_none() && json::text(&p.state, "deviceId") == id {
                json::push(&mut indices, i, MAX_STATES)?;
            }
        }
        indices.sort_unstable_by_key(|i| {
            let p = &self.plans[*i].state;
            let priority = if json::text(p, "capabilityId").split('.').next() == Some("onoff") {
                match field(p, "value").as_bool() {
                    Some(true) => 0,
                    Some(false) => 2,
                    _ => 1,
                }
            } else {
                1
            };
            (priority, *i)
        });
        let mut commands = Vec::new();
        for i in &indices {
            let p = &self.plans[*i].state;
            json::push(
                &mut commands,
                json::fields(&[
                    ("capability", json::string(json::text(p, "capabilityId"))?),
                    ("value", field(p, "value").try_clone()?),
                ])?,
                MAX_STATES,
            )?;
        }
        let params = json::fields(&[
            ("deviceId", json::string(&id)?),
            ("commands", Value::Array(commands)),
            ("options", json::object()),
        ])?;
        for i in indices {
            self.plans[i].sent = true;
        }
        Ok(Some((id, params)))
    }
    /// Fouten per capability blijven afzonderlijk beschikbaar voor een latere herstelpoging.
    pub fn complete(&mut self, device: &str, errors: &Value, failure: Option<&str>) -> Result {
        for p in &mut self.plans {
            if p.result.is_some() || !p.sent || json::text(&p.state, "deviceId") != device {
                continue;
            }
            let error = failure.or_else(|| {
                json::get(errors, json::text(&p.state, "capabilityId"))
                    .and_then(Value::as_str)
                    .filter(|s| !s.is_empty())
            });
            p.result = Some(result(&p.state, error, false)?);
        }
        Ok(())
    }
    /// Een deadline maakt onafgeronde opdrachten zichtbaar als fout.
    pub fn cancel(&mut self) -> Result {
        for p in &mut self.plans {
            if p.result.is_none() {
                p.result = Some(result(&p.state, Some("scene execution timed out"), false)?);
            }
        }
        Ok(())
    }
    /// Alleen volledig beantwoorde plannen kunnen duurzaam afgerond worden.
    pub fn done(&self) -> bool {
        self.plans.iter().all(|p| p.result.is_some())
    }
    /// Publiceert de werkelijk overgebleven herstelstaat en het volledige resultaat.
    pub fn finish<S: Storage>(&self, store: &mut Store<S>) -> Result<Value> {
        if !self.done() {
            return Err(Error::Conflict("scene still has pending commands"));
        }
        let mut states = Vec::new();
        let mut succeeded = 0;
        for p in &self.plans {
            let r = p
                .result
                .as_ref()
                .ok_or(Error::Missing("scene outcome missing"))?;
            if json::boolean(r, "success") {
                succeeded += 1;
            }
            json::push(&mut states, r.try_clone()?, MAX_STATES)?;
        }
        if !self.momentary
            && (self.first || (!self.on && json::boolean(&self.definition, "active")))
        {
            let mut retained = Vec::new();
            for before in json::array(&self.definition, "previous") {
                let success = self
                    .plans
                    .iter()
                    .find(|p| key(&p.state, before))
                    .and_then(|p| p.result.as_ref())
                    .is_some_and(|r| json::boolean(r, "success"));
                if success == self.on {
                    json::push(&mut retained, before.try_clone()?, MAX_STATES)?;
                }
            }
            store.scene_remaining(self.id(), retained)?;
        }
        let active = json::boolean(store.document().record("scenes", self.id())?, "active");
        let success = succeeded == self.plans.len() && (self.momentary || active == self.on);
        let mut out = json::fields(&[
            ("sceneId", json::string(self.id())?),
            (
                "sceneName",
                json::string(json::text(&self.definition, "name"))?,
            ),
            ("requestedOn", Value::Bool(self.on)),
            ("active", Value::Bool(active)),
            ("success", Value::Bool(success)),
            ("attempted", Value::uint(self.plans.len() as u64)),
            ("succeeded", Value::uint(succeeded as u64)),
            ("failed", Value::uint((self.plans.len() - succeeded) as u64)),
            ("states", Value::Array(states)),
        ])?;
        if self.momentary {
            json::set(&mut out, "momentary", Value::Bool(true))?;
        }
        Ok(out)
    }
}
