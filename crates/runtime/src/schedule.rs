//! Minuuttriggers kiezen hun eigen startnodes en onthouden de UTC-offset bij DST.
use crate::{calendar::Civil, flows::Run};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::{
    Error, Result,
    flow::{self, MAX_NODES},
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
/// De adapter levert tijdzoneregels en de astronomische berekening voor een lokale datum.
pub trait Clock {
    /// UTC naar lokale kalender met historische offset.
    fn local(&self, unix: i64) -> Result<Civil>;
    /// UTC-seconden van een zonne-event; poolnacht of pooldag geeft None.
    fn solar(&self, date: Civil, latitude: f64, longitude: f64, rise: bool) -> Result<Option<i64>>;
}
struct Last {
    flow: String,
    node: String,
    minute: Civil,
}
/// Ongewijzigde tijdkaarten lopen eenmaal per lokale minuut en offset.
#[derive(Default)]
pub struct Schedule {
    last: Vec<Last>,
}
impl Schedule {
    /// Eén uitvoering per beurt; alleen echt gestarte nodes worden als afgehandeld gemarkeerd.
    pub fn due<S: Storage>(
        &mut self,
        store: &Store<S>,
        clock: &impl Clock,
        unix: i64,
        now: u64,
        ran_at: &str,
    ) -> Result<Option<Run>> {
        self.last.retain(|entry| {
            store
                .document()
                .record("flows", &entry.flow)
                .is_ok_and(|f| {
                    json::boolean(f, "enabled")
                        && json::array(f, "nodes")
                            .iter()
                            .any(|n| json::text(n, "id") == entry.node && scheduled(n))
                })
        });
        let local = clock.local(unix)?;
        for definition in store
            .document()
            .records("flows")
            .iter()
            .filter(|d| json::boolean(d, "enabled"))
        {
            let fid = json::text(definition, "id");
            let mut starts = Vec::new();
            for node in json::array(definition, "nodes")
                .iter()
                .filter(|n| scheduled(n))
            {
                let id = json::text(node, "id");
                if self
                    .last
                    .iter()
                    .any(|l| l.flow == fid && l.node == id && l.minute == local)
                {
                    continue;
                }
                if matches(clock, node, local)? {
                    json::push(&mut starts, id, MAX_NODES)?;
                }
            }
            if starts.is_empty() {
                continue;
            }
            let mut time = String::new();
            time.try_reserve_exact(5).map_err(|_| Error::Memory)?;
            write!(time, "{:02}:{:02}", local.hour, local.minute).map_err(|_| Error::Full)?;
            let mut date = String::new();
            date.try_reserve_exact(10).map_err(|_| Error::Memory)?;
            write!(
                date,
                "{:04}-{:02}-{:02}",
                local.year, local.month, local.day
            )
            .map_err(|_| Error::Full)?;
            let values = json::fields(&[
                ("time", json::string(&time)?),
                ("date", json::string(&date)?),
            ])?;
            let context = json::fields(&[("tokens", values.try_clone()?), ("state", values)])?;
            let run = Run::selected(definition, &starts, context, now, ran_at)?;
            let mut entries = Vec::new();
            for id in &starts {
                json::push(
                    &mut entries,
                    Last {
                        flow: json::copy(fid)?,
                        node: json::copy(id)?,
                        minute: local,
                    },
                    MAX_NODES,
                )?;
            }
            let new = entries
                .iter()
                .filter(|e| {
                    !self
                        .last
                        .iter()
                        .any(|l| l.flow == e.flow && l.node == e.node)
                })
                .count();
            if self.last.len().saturating_add(new) > 4096 {
                return Err(Error::Full);
            }
            self.last.try_reserve(new).map_err(|_| Error::Memory)?;
            for e in entries {
                if let Some(old) = self
                    .last
                    .iter_mut()
                    .find(|l| l.flow == e.flow && l.node == e.node)
                {
                    *old = e;
                } else {
                    self.last.push(e);
                }
            }
            return Ok(Some(run));
        }
        Ok(None)
    }
}
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
fn scheduled(node: &Value) -> bool {
    let step = field(node, "step");
    flow::kind(node) == "trigger"
        && json::text(step, "appId") == "stulp"
        && matches!(json::text(step, "cardId"), "time_at" | "sunrise" | "sunset")
}
fn number(v: &Value) -> Option<f64> {
    match v {
        Value::Number(n) => Some(n.as_f64()),
        _ => None,
    }
}
fn matches(clock: &impl Clock, node: &Value, now: Civil) -> Result<bool> {
    let step = field(node, "step");
    let args = field(step, "args");
    let id = json::text(step, "cardId");
    if id == "time_at" {
        let time = json::text(args, "time");
        let Some((hour, minute)) = time.split_once(':') else {
            return Ok(false);
        };
        return Ok(hour.bytes().all(|b| b.is_ascii_digit())
            && minute.bytes().all(|b| b.is_ascii_digit())
            && hour.len() <= 2
            && !hour.is_empty()
            && minute.len() == 2
            && hour.parse::<u8>().ok() == Some(now.hour)
            && minute.parse::<u8>().ok() == Some(now.minute));
    }
    let (Some(latitude), Some(longitude)) = (
        number(field(args, "latitude")),
        number(field(args, "longitude")),
    ) else {
        return Ok(false);
    };
    if !(-90.0..=90.0).contains(&latitude) || !(-180.0..=180.0).contains(&longitude) {
        return Ok(false);
    }
    let offset = number(field(args, "offset")).unwrap_or(0.0);
    if !offset.is_finite() || offset.abs() > 1e7 {
        return Ok(false);
    }
    let Some(target) = clock.solar(now, latitude, longitude, id == "sunrise")? else {
        return Ok(false);
    };
    let target = clock.local(
        target
            .checked_add((offset * 60.0) as i64)
            .ok_or(Error::Full)?,
    )?;
    Ok(target.year == now.year
        && target.month == now.month
        && target.day == now.day
        && target.hour == now.hour
        && target.minute == now.minute)
}
