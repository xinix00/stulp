//! Alleen een expliciet onbekende batchmethode valt terug op losse opdrachten, nooit een netwerkfout.
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
};
use stulp_protocol::{Frame, Kind};
pub(super) struct Group {
    pub(super) owner: u64,
    pub(super) deadline: u64,
    params: Value,
    sent: usize,
    singles: bool,
    errors: Value,
}
pub(super) enum Step {
    Original,
    Call(Value),
    Done(Value),
}
impl Group {
    pub(super) fn new(owner: u64, now: u64, params: &Value) -> Result<Self> {
        if json::array(params, "commands").len() > 256 {
            return Err(Error::Full);
        }
        for command in json::array(params, "commands") {
            if json::text(command, "capability").is_empty() || json::get(command, "value").is_none()
            {
                return Err(Error::Invalid("invalid grouped capability command"));
            }
        }
        Ok(Self {
            owner,
            deadline: now.saturating_add(30_000),
            params: params.try_clone()?,
            sent: 0,
            singles: false,
            errors: json::object(),
        })
    }
    pub(super) fn reply(&mut self, frame: &Frame) -> Result<Step> {
        let error = json::get(&frame.value, "e").unwrap_or(&Value::Null);
        let message = error
            .as_str()
            .unwrap_or_else(|| json::text(error, "message"));
        if !self.singles {
            if frame.kind != Kind::Error || !message.contains("unknown method") {
                return Ok(Step::Original);
            }
            self.singles = true;
        } else if frame.kind == Kind::Error {
            let previous = json::array(&self.params, "commands")
                .get(self.sent.saturating_sub(1))
                .ok_or(Error::Changed)?;
            json::set(
                &mut self.errors,
                json::text(previous, "capability"),
                json::string(if message.is_empty() {
                    "capability invocation failed"
                } else {
                    message
                })?,
            )?;
        }
        let Some(command) = json::array(&self.params, "commands").get(self.sent) else {
            return Ok(Step::Done(self.errors.try_clone()?));
        };
        let mut params = command.try_clone()?;
        json::set(
            &mut params,
            "deviceId",
            json::string(json::text(&self.params, "deviceId"))?,
        )?;
        json::set(
            &mut params,
            "options",
            json::get(&self.params, "options")
                .unwrap_or(&json::object())
                .try_clone()?,
        )?;
        self.sent += 1;
        Ok(Step::Call(params))
    }
}
