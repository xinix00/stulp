//! De echte kWh-meter verankert de verdeling; gaten en resets worden niet als verbruik geboekt.
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Result,
    util::{field, float, number},
};
/// Tellers en het integratievenster van één warmtepomp.
#[derive(Default, Clone, Copy)]
pub struct Split {
    heating_wh: f64,
    hotwater_wh: f64,
    measured: Option<u64>,
    priority: Option<i64>,
    total: Option<f64>,
    heating: f64,
    hotwater: f64,
}
impl Split {
    /// Herstel alleen duurzame tellers, nooit een oud integratievenster.
    pub fn restore(store: &Value) -> Self {
        Self {
            heating: number(field(store, "heatingKwh")).unwrap_or(0.0),
            hotwater: number(field(store, "hotwaterKwh")).unwrap_or(0.0),
            total: number(field(store, "lastTotal")),
            ..Self::default()
        }
    }
    /// Verdeel één vermogensmeting; een gat van meer dan dertig minuten telt niet mee.
    pub fn power(&mut self, now: u64, watt: f64, priority: i64) -> (f64, f64) {
        let (h, w) = if priority == 3 {
            (0.0, watt)
        } else {
            (watt, 0.0)
        };
        if let Some(last) = self.measured {
            let gap = now.saturating_sub(last);
            if gap > 0 && gap <= 1_800_000 {
                self.heating_wh += h * gap as f64 / 3_600_000.0;
                self.hotwater_wh += w * gap as f64 / 3_600_000.0;
            }
        }
        self.measured = Some(now);
        self.priority = Some(priority);
        (h, w)
    }
    /// Een delta boven vijf kWh of onder nul is een nieuwe ijking.
    pub fn anchor(&mut self, total: f64) -> bool {
        let previous = self.total.replace(total);
        let window = self.heating_wh + self.hotwater_wh;
        let share = self.heating_wh;
        self.heating_wh = 0.0;
        self.hotwater_wh = 0.0;
        let Some(previous) = previous else {
            return false;
        };
        let delta = total - previous;
        if delta <= 0.0 || delta > 5.0 {
            return false;
        }
        if window > 0.0 {
            self.heating += delta * share / window;
            self.hotwater += delta * (window - share) / window;
        } else {
            match self.priority {
                Some(3) => self.hotwater += delta,
                Some(_) => self.heating += delta,
                None => return false,
            }
        }
        true
    }
    /// Snapshot voor opslag vóór het publiceren van afgeleide meterstanden.
    pub fn store(&self) -> Result<Value> {
        let mut v = json::fields(&[
            ("heatingKwh", float(self.heating)?),
            ("hotwaterKwh", float(self.hotwater)?),
        ])?;
        if let Some(total) = self.total {
            json::set(&mut v, "lastTotal", float(total)?)?;
        }
        Ok(v)
    }
    /// De twee tellers tellen precies op tot het verdeelde verbruik.
    pub fn meters(&self) -> (f64, f64) {
        (self.heating, self.hotwater)
    }
}
