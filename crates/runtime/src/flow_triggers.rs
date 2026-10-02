//! Triggerselectie en tokens houden dezelfde canonieke waarden als appcallbacks.
use super::*;
fn field<'a>(v: &'a Value, key: &str) -> &'a Value {
    json::get(v, key).unwrap_or(&Value::Null)
}
fn lookup<'a>(name: &str, context: &'a Value) -> Option<&'a Value> {
    if let Some(key) = name.strip_prefix("state.") {
        return json::get(field(context, "state"), key);
    }
    let tokens = field(context, "tokens");
    if let Some(key) = name.strip_prefix("tokens.") {
        return json::get(tokens, key);
    }
    json::get(tokens, name).or_else(|| json::get(field(context, "state"), name))
}
pub(super) fn args<S: Storage>(store: &Store<S>, step: &Value, input: &Value) -> Result<Value> {
    let mut out = json::Object::new();
    if let Some(object) = field(step, "args").as_object() {
        for (name, value) in object.iter() {
            out.push(
                name,
                resolve(
                    store,
                    value,
                    input,
                    !stulp_core::display::wants_number(store, step, name),
                    0,
                )?,
            )?;
        }
    }
    Ok(Value::Object(out))
}
pub(super) fn resolve<S: Storage>(
    store: &Store<S>,
    value: &Value,
    context: &Value,
    as_text: bool,
    depth: usize,
) -> Result<Value> {
    if depth > 64 {
        return Err(Error::Full);
    }
    match value {
        Value::Object(o) => {
            let mut out = json::Object::new();
            for (k, v) in o.iter() {
                out.push(k, resolve(store, v, context, as_text, depth + 1)?)?;
            }
            Ok(Value::Object(out))
        }
        Value::Array(a) => {
            let mut out = Vec::new();
            for v in a {
                json::push(
                    &mut out,
                    resolve(store, v, context, as_text, depth + 1)?,
                    json::MAX_DOCUMENT,
                )?;
            }
            Ok(Value::Array(out))
        }
        Value::String(s) => {
            if let Some(name) = s.strip_prefix("{{").and_then(|s| s.strip_suffix("}}"))
                && !name.contains("{{")
                && let Some(value) = lookup(name.trim(), context)
            {
                if as_text
                    && let Some(text) =
                        stulp_core::display::token(store, context, name.trim(), value)?
                {
                    return Ok(Value::String(text));
                }
                return Ok(value.try_clone()?);
            }
            let mut out = json::copy(s)?;
            for _ in 0..100 {
                let Some(start) = out.find("{{") else { break };
                let Some(end) = out.get(start + 2..).and_then(|s| s.find("}}")) else {
                    break;
                };
                let name = out[start + 2..start + 2 + end].trim();
                let replacement = match lookup(name, context) {
                    Some(Value::String(s)) => json::copy(s)?,
                    Some(v) => match stulp_core::display::token(store, context, name, v)? {
                        Some(text) => text,
                        None => json::to_string(v)?,
                    },
                    None => String::new(),
                };
                let growth = replacement.len().saturating_sub(end + 4);
                if out.len().saturating_add(growth) > json::MAX_DOCUMENT {
                    return Err(Error::Full);
                }
                out.try_reserve(growth).map_err(|_| Error::Memory)?;
                out.replace_range(start..start + end + 4, &replacement);
            }
            Ok(Value::String(out))
        }
        _ => Ok(value.try_clone()?),
    }
}
fn builtin(step: &Value, input: &Value) -> Result<bool> {
    let args = field(step, "args");
    let state = field(input, "state");
    let id = json::text(step, "cardId");
    if let Some((_, action)) = id
        .strip_prefix("capability.")
        .and_then(|s| s.rsplit_once('.'))
    {
        match action {
            "became" => return Ok(json::equal(field(args, "value"), field(state, "value"))),
            "rose_above" | "fell_below" => {
                let threshold = number(field(args, "value"))
                    .ok_or(Error::Invalid("numeric trigger threshold is required"))?;
                let (Some(current), Some(previous)) = (
                    number(field(state, "value")),
                    number(field(state, "oldValue")),
                ) else {
                    return Ok(false);
                };
                return Ok(if action == "rose_above" {
                    previous <= threshold && current > threshold
                } else {
                    previous >= threshold && current < threshold
                });
            }
            _ => (),
        }
    }
    for key in ["capability", "event"] {
        let wanted = json::text(args, key);
        if !wanted.is_empty() && wanted != json::text(state, key) {
            return Ok(false);
        }
    }
    Ok(true)
}
impl Run {
    /// Een automatisch event start uitsluitend bij passende triggers van een ingeschakelde Flow.
    /// Appfilters draaien als echte callbacks vóór de eerste condition/action.
    pub fn triggered(
        definition: &Value,
        input: &Value,
        now: u64,
        ran_at: &str,
    ) -> Result<Option<Self>> {
        if !json::boolean(definition, "enabled") {
            return Ok(None);
        }
        let mut matched = Vec::new();
        let mut filters = Vec::new();
        for node in json::array(definition, "nodes") {
            let step = field(node, "step");
            if flow::kind(node) != "trigger"
                || json::text(step, "appId") != json::text(input, "appId")
                || json::text(step, "cardId") != json::text(input, "id")
                || json::text(step, "cardType") != json::text(input, "kind")
            {
                continue;
            }
            if let Some(args) = field(step, "args").as_object()
                && args.iter().any(|(_, v)| {
                    let id = json::text(v, "$device");
                    !id.is_empty() && id != json::text(field(input, "state"), "deviceId")
                })
            {
                continue;
            }
            if json::text(step, "appId") == "stulp" {
                if builtin(step, input)? {
                    json::push(&mut matched, json::copy(json::text(node, "id"))?, MAX_NODES)?;
                }
            } else {
                json::push(
                    &mut filters,
                    (json::copy(json::text(node, "id"))?, step.try_clone()?),
                    MAX_NODES,
                )?;
            }
        }
        if matched.is_empty() && filters.is_empty() {
            return Ok(None);
        }
        let mut run = Self::manual(definition, now, ran_at)?;
        run.execution = Execution::start_many(definition, &[])?;
        run.context = input.try_clone()?;
        run.starts = matched;
        run.filters = filters;
        run.starting = true;
        Ok(Some(run))
    }
    pub(super) fn filter_step<S: Storage>(&mut self, store: &Store<S>) -> Result<Option<Effect>> {
        if !self.starting {
            return Ok(None);
        }
        if !self.filters.is_empty() {
            let (id, step) = self.filters.remove(0);
            let args = args(store, &step, &self.context)?;
            let mut state = field(&self.context, "state").try_clone()?;
            if state.is_null() {
                state = json::object();
            }
            if let Some(overlay) =
                resolve(store, field(&step, "state"), &self.context, false, 0)?.as_object()
            {
                for (key, v) in overlay.iter() {
                    json::set(&mut state, key, v.try_clone()?)?;
                }
            }
            let effect = Effect::Call {
                app: json::copy(json::text(&step, "appId"))?,
                method: "flow.run",
                params: json::fields(&[
                    ("kind", json::string(json::text(&step, "cardType"))?),
                    ("id", json::string(json::text(&step, "cardId"))?),
                    ("args", args),
                    ("state", state),
                ])?,
            };
            self.filtering = Some(id);
            self.waiting = Some(step);
            return Ok(Some(effect));
        }
        let mut starts = Vec::new();
        for id in &self.starts {
            json::push(&mut starts, id.as_str(), MAX_NODES)?;
        }
        self.execution = Execution::start_many(&self.definition, &starts)?;
        self.starting = false;
        Ok(None)
    }
}
