//! Eén Flow bezit zijn snapshot en wacht op callbacks zonder de controller te blokkeren.
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    flow::{self, Execution, MAX_NODES},
    json::{self, TryClone, Value},
    store::{Storage, Store},
};

/// Dezelfde uiterste looptijd als de Go-engine.
pub const TIMEOUT_MS: u64 = 45_000;
#[path = "flow_triggers.rs"]
mod triggers;

/// Werk dat alleen de eigenaar van transport, klok of opslag kan uitvoeren.
pub enum Effect {
    /// Eén opdracht naar de geïsoleerde app.
    Call {
        /// De eigenaar van de callback.
        app: String,
        /// De RPC-methode.
        method: &'static str,
        /// Argumenten in de oorspronkelijke, canonieke eenheden.
        params: Value,
    },
    /// Een melding vraagt de host om een nieuw id en een duurzame schrijfopdracht.
    Notification(String),
    /// Callback of timer loopt nog; ondertussen kunnen andere taken verder.
    Waiting,
    /// Alle bereikbare kaarten zijn afgehandeld, of er is een fout.
    Finished,
}

/// Begrensde uitvoering; wijzigingen in het canvas veranderen deze run niet achteraf.
pub struct Run {
    definition: Value,
    execution: Execution,
    ran_at: String,
    deadline: u64,
    wake: Option<u64>,
    waiting: Option<Value>,
    conditions: Vec<Value>,
    actions: Vec<Value>,
    blocked: bool,
    done: bool,
    error: String,
    context: Value,
    filters: Vec<(String, Value)>,
    starts: Vec<String>,
    starting: bool,
    filtering: Option<String>,
}

impl Run {
    /// De Test-knop omzeilt enabled en start bij alle triggers, zoals Go.
    pub fn manual(definition: &Value, now_ms: u64, ran_at: &str) -> Result<Self> {
        let mut starts = Vec::new();
        for node in json::array(definition, "nodes") {
            if flow::kind(node) == "trigger" {
                json::push(&mut starts, json::text(node, "id"), MAX_NODES)?;
            }
        }
        let execution = Execution::start_many(definition, &starts)?;
        let mut conditions = Vec::new();
        let mut actions = Vec::new();
        conditions
            .try_reserve_exact(MAX_NODES)
            .map_err(|_| Error::Memory)?;
        actions
            .try_reserve_exact(MAX_NODES)
            .map_err(|_| Error::Memory)?;
        Ok(Self {
            definition: definition.try_clone()?,
            execution,
            ran_at: json::copy(ran_at)?,
            deadline: now_ms.checked_add(TIMEOUT_MS).ok_or(Error::Full)?,
            wake: None,
            waiting: None,
            conditions,
            actions,
            blocked: false,
            done: false,
            error: String::new(),
            context: Value::Null,
            filters: Vec::new(),
            starts: Vec::new(),
            starting: false,
            filtering: None,
        })
    }

    /// Tijd- en stabiliteitstriggers kiezen concrete nodes; andere triggers starten niet mee.
    pub fn selected(
        definition: &Value,
        starts: &[&str],
        context: Value,
        now: u64,
        ran_at: &str,
    ) -> Result<Self> {
        let mut run = Self::manual(definition, now, ran_at)?;
        run.execution = Execution::start_many(definition, starts)?;
        run.context = context;
        Ok(run)
    }

    /// Het oorspronkelijke Flow-id blijft gelijk, ook als de gebruiker het canvas verwijdert.
    pub fn id(&self) -> &str {
        json::text(&self.definition, "id")
    }
    /// De naam van de Flow, voor logregels.
    pub fn name(&self) -> &str {
        json::text(&self.definition, "name")
    }
    /// Historie wordt geordend op starttijd, niet op de volgorde van callbacks.
    pub fn ran_at(&self) -> &str {
        &self.ran_at
    }
    /// De vastgelegde fout is leeg na een geslaagde uitvoering.
    pub fn error(&self) -> &str {
        &self.error
    }

    /// Draait hoogstens één kaart per beurt. Een delay gebruikt geen slapende thread.
    pub fn advance<S: Storage>(&mut self, store: &Store<S>, now: u64) -> Result<Effect> {
        if self.done {
            return Ok(Effect::Finished);
        }
        if now >= self.deadline {
            self.stop("flow execution timed out")?;
            return Ok(Effect::Finished);
        }
        if let Some(wake) = self.wake {
            if now >= wake {
                self.wake = None;
                self.complete(Value::Bool(true), now)?;
            }
            return Ok(Effect::Waiting);
        }
        if self.waiting.is_some() {
            return Ok(Effect::Waiting);
        }
        if let Some(effect) = self.filter_step(store)? {
            return Ok(effect);
        }
        let Some(node) = self.execution.next(&self.definition)? else {
            self.done = true;
            return Ok(Effect::Finished);
        };
        let step = json::get(node, "step")
            .ok_or(Error::Missing("flow step"))?
            .try_clone()?;
        self.waiting = Some(step.try_clone()?);
        match self.dispatch(store, &step, now) {
            Ok(effect) => Ok(effect),
            Err(error) => {
                self.fail(&error_text(&error)?)?;
                Ok(if self.done {
                    Effect::Finished
                } else {
                    Effect::Waiting
                })
            }
        }
    }

    fn dispatch<S: Storage>(&mut self, store: &Store<S>, step: &Value, now: u64) -> Result<Effect> {
        let app = json::text(step, "appId");
        let kind = json::text(step, "cardType");
        let id = json::text(step, "cardId");
        // Handmatige tests hebben geen triggertokens; ontbrekende tokens worden lege tekst.
        let args = triggers::args(store, step, &self.context)?;
        if app != "stulp" {
            return Ok(Effect::Call {
                app: json::copy(app)?,
                method: "flow.run",
                params: json::fields(&[
                    ("kind", json::string(kind)?),
                    ("id", json::string(id)?),
                    ("args", args),
                    (
                        "state",
                        triggers::resolve(
                            store,
                            json::get(step, "state").unwrap_or(&json::object()),
                            &self.context,
                            false,
                            0,
                        )?,
                    ),
                ])?,
            });
        }
        if kind == "condition" {
            self.complete(Value::Bool(condition(store, id, &args)?), now)?;
            return Ok(Effect::Waiting);
        }
        match id {
            "delay" => {
                let seconds = number(json::get(&args, "seconds").unwrap_or(&Value::Null))
                    .ok_or(Error::Invalid("delay must be between 0 and 30 seconds"))?;
                if !(0.0..=30.0).contains(&seconds) {
                    return Err(Error::Invalid("delay must be between 0 and 30 seconds"));
                }
                self.wake = Some(now.saturating_add((seconds * 1000.0) as u64));
                Ok(Effect::Waiting)
            }
            "notification" => Ok(Effect::Notification(json::copy(json::text(
                &args, "excerpt",
            ))?)),
            _ => action(store, id, &args),
        }
    }

    /// Een condition moet een boolean leveren; een fout stopt de volledige run.
    pub fn complete(&mut self, value: Value, now: u64) -> Result {
        if self.done {
            return Ok(());
        }
        if now >= self.deadline {
            return self.stop("flow execution timed out");
        }
        if let Some(id) = self.filtering.take() {
            let Some(passed) = value.as_bool() else {
                return self.stop("flow trigger filter did not return a boolean");
            };
            if passed {
                json::push(&mut self.starts, id, MAX_NODES)?;
            }
            self.waiting = None;
            return Ok(());
        }
        let step = self
            .waiting
            .as_ref()
            .ok_or(Error::Conflict("no flow callback pending"))?;
        let condition = json::text(step, "cardType") == "condition";
        let raw = if condition {
            match value.as_bool() {
                Some(value) => value,
                None => return self.stop("flow condition did not return a boolean"),
            }
        } else {
            true
        };
        let passed = raw != json::boolean(step, "inverted");
        let mut result = step_result(step)?;
        json::set(&mut result, "result", value)?;
        if condition {
            json::set(&mut result, "passed", Value::Bool(passed))?;
        }
        self.execution.complete(&self.definition, raw)?;
        self.waiting = None;
        self.wake = None;
        if condition {
            self.blocked |= !passed;
            self.conditions.push(result);
        } else {
            self.actions.push(result);
        }
        Ok(())
    }

    /// De wachtende kaart mislukte. Een actie bewaart haar fout en stopt alleen haar
    /// eigen tak: acties op andere takken draaien nog, zoals Homey dat doet. Een
    /// mislukte trigger-filter of condition stopt de volledige run.
    pub fn fail(&mut self, message: &str) -> Result {
        if self.done {
            return Ok(());
        }
        let filtering = self.filtering.is_some();
        let Some(step) = self
            .waiting
            .take_if(|step| !filtering && json::text(step, "cardType") == "action")
        else {
            return self.stop(message);
        };
        let mut result = step_result(&step)?;
        json::set(&mut result, "error", json::string(message)?)?;
        self.execution.skip()?;
        self.wake = None;
        self.actions.push(result);
        if self.error.is_empty() {
            self.error = json::copy(message)?;
        }
        Ok(())
    }

    /// Beëindigt de run, met de wachtende kaart in het resultaat; latere callbacks doen niets.
    pub fn stop(&mut self, message: &str) -> Result {
        if self.done {
            return Ok(());
        }
        let error = json::copy(message)?;
        if let Some(step) = &self.waiting {
            let result = step_result(step)?;
            if json::text(step, "cardType") == "condition" {
                self.conditions.push(result);
            } else {
                self.actions.push(result);
            }
        }
        self.error = error;
        self.waiting = None;
        self.wake = None;
        self.done = true;
        Ok(())
    }

    /// Het bestaande browsercontract, met canonieke callbackresultaten.
    pub fn result(&self) -> Result<Value> {
        if !self.done {
            return Err(Error::Conflict("flow is still running"));
        }
        let mut result = json::fields(&[
            ("flowId", json::string(self.id())?),
            ("success", Value::Bool(self.error.is_empty())),
            (
                "stopped",
                Value::Bool(self.error.is_empty() && self.blocked && self.actions.is_empty()),
            ),
            ("ranAt", json::string(&self.ran_at)?),
            ("conditions", Value::Array(self.conditions.try_clone()?)),
            ("actions", Value::Array(self.actions.try_clone()?)),
        ])?;
        if !self.error.is_empty() {
            json::set(&mut result, "error", json::string(&self.error)?)?;
        }
        Ok(result)
    }
}

fn step_result(step: &Value) -> Result<Value> {
    json::fields(&[
        ("appId", json::string(json::text(step, "appId"))?),
        ("cardId", json::string(json::text(step, "cardId"))?),
        ("cardType", json::string(json::text(step, "cardType"))?),
    ])
}

fn selected_device(args: &Value) -> &str {
    args.as_object()
        .and_then(|o| {
            o.iter().find_map(|(_, value)| {
                let id = json::text(value, "$device");
                (!id.is_empty()).then_some(id)
            })
        })
        .unwrap_or("")
}

fn derived(id: &str) -> Option<(&str, &str)> {
    id.strip_prefix("capability.")?.rsplit_once('.')
}

fn number(value: &Value) -> Option<f64> {
    if let Value::Number(n) = value {
        let v = n.as_f64();
        v.is_finite().then_some(v)
    } else {
        None
    }
}

fn condition<S: Storage>(store: &Store<S>, id: &str, args: &Value) -> Result<bool> {
    let (capability, comparison) = if id == "device_capability_equals" {
        (json::text(args, "capability"), "is")
    } else {
        derived(id).ok_or(Error::Invalid("unknown built-in condition"))?
    };
    let device = store.device(selected_device(args))?;
    let current = json::get(&device, "state")
        .and_then(|s| json::get(s, capability))
        .unwrap_or(&Value::Null);
    let value = json::get(args, "value").unwrap_or(&Value::Null);
    match comparison {
        "is" => Ok(json::equal(current, value)),
        "is_on" => Ok(current.as_bool() == Some(true)),
        "is_off" => Ok(current.as_bool() == Some(false)),
        "above" | "below" => {
            let a = number(current).ok_or(Error::Invalid("capability must be numeric"))?;
            let b = number(value).ok_or(Error::Invalid("threshold must be numeric"))?;
            Ok(if comparison == "above" { a > b } else { a < b })
        }
        _ => Err(Error::Invalid("unknown built-in condition")),
    }
}

fn action<S: Storage>(store: &Store<S>, id: &str, args: &Value) -> Result<Effect> {
    let (capability, operation) = if id == "set_device_capability" {
        (json::text(args, "capability"), "set")
    } else {
        derived(id).ok_or(Error::Invalid("unknown built-in action"))?
    };
    let device_id = selected_device(args);
    let device = store.device(device_id)?;
    if !json::array(&device, "capabilities")
        .iter()
        .any(|v| v.as_str() == Some(capability))
    {
        return Err(Error::Missing("device capability does not exist"));
    }
    let value = match operation {
        "run" | "turn_on" => Value::Bool(true),
        "turn_off" => Value::Bool(false),
        "toggle" => Value::Bool(
            !json::get(&device, "state")
                .and_then(|s| json::get(s, capability))
                .and_then(Value::as_bool)
                .ok_or(Error::Invalid("capability has no boolean state to toggle"))?,
        ),
        "set" => json::get(args, "value")
            .unwrap_or(&Value::Null)
            .try_clone()?,
        _ => return Err(Error::Invalid("unknown built-in action")),
    };
    Ok(Effect::Call {
        app: json::copy(json::text(&device, "appId"))?,
        method: "capability.invoke",
        params: json::fields(&[
            ("deviceId", json::string(device_id)?),
            ("capability", json::string(capability)?),
            ("value", value),
            ("options", json::object()),
        ])?,
    })
}

/// Fouten naar begrensde tekst omzetten zonder infallible format-allocatie.
pub fn error_text(error: &Error) -> Result<String> {
    use core::fmt::Write;
    let mut text = String::new();
    text.try_reserve(512).map_err(|_| Error::Memory)?;
    write!(&mut text, "{error}").map_err(|_| Error::Full)?;
    Ok(text)
}
