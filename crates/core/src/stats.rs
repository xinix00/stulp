//! Vluchtige meetreeksen: drie vaste ringen, geen bestand of extra apparaatverzoek.
use crate::{
    Error, Result,
    json::{self, Number, Value},
};
use alloc::{string::String, vec::Vec};
const MAX_SERIES: usize = 2048;
const TIERS: [(u64, usize, &str); 3] = [
    (600, 144, "10m0s"),
    (7200, 84, "2h0m0s"),
    (18000, 144, "5h0m0s"),
];
#[derive(Clone, Copy, PartialEq)]
enum Kind {
    Gauge,
    Counter,
    Fraction,
}
impl Kind {
    fn of(cap: &str) -> Option<Self> {
        let base = cap.split('.').next().unwrap_or(cap);
        if base.starts_with("meter_") {
            Some(Self::Counter)
        } else if base.starts_with("measure_")
            || matches!(
                base,
                "dim" | "volume_set" | "light_hue" | "light_saturation" | "target_temperature"
            )
        {
            Some(Self::Gauge)
        } else if base.starts_with("alarm_") || matches!(base, "onoff" | "locked") {
            Some(Self::Fraction)
        } else {
            None
        }
    }
    fn name(self) -> &'static str {
        match self {
            Self::Gauge => "gauge",
            Self::Counter => "counter",
            Self::Fraction => "fraction",
        }
    }
}
#[derive(Clone, Copy, Default)]
struct Slot {
    period: u32,
    count: u16,
    sum: f32,
    min: f32,
    max: f32,
}
struct Ring {
    slots: Vec<Slot>,
    head: usize,
    every: u64,
}
impl Ring {
    fn new(every: u64, count: usize) -> Result<Self> {
        let mut slots = Vec::new();
        slots.try_reserve_exact(count).map_err(|_| Error::Memory)?;
        slots.resize(count, Slot::default());
        Ok(Self {
            slots,
            head: 0,
            every,
        })
    }
    fn slot(&mut self, at: u64) -> Option<&mut Slot> {
        let period = u32::try_from(at / self.every).ok()?;
        let h = self.slots.get(self.head)?;
        if h.count > 0 && h.period > period {
            return None;
        }
        if h.period != period {
            self.head = (self.head + 1) % self.slots.len();
            *self.slots.get_mut(self.head)? = Slot {
                period,
                ..Default::default()
            };
        }
        self.slots.get_mut(self.head)
    }
    fn spread(&mut self, value: f64, from_ms: u64, until_ms: u64) {
        let every = self.every * 1000;
        // Een lange klokstap kost maximaal één ring; oudere vakken zouden toch verdwijnen.
        let mut cursor = from_ms / every * every;
        let newest = until_ms.saturating_sub(1) / every * every;
        cursor = cursor.max(newest.saturating_sub((self.slots.len() as u64 - 1) * every));
        while cursor < until_ms {
            let end = cursor.saturating_add(every).min(until_ms);
            let begin = cursor.max(from_ms);
            if end > begin {
                let part = (end - begin) as f32 / every as f32;
                let count = (f64::from(part) * 100.0 + 0.5) as u16;
                if let Some(slot) = self.slot(cursor / 1000) {
                    let value = value as f32;
                    slot.sum += value * f32::from(count);
                    if slot.count == 0 {
                        slot.min = value;
                        slot.max = value;
                    } else {
                        slot.min = slot.min.min(value);
                        slot.max = slot.max.max(value);
                    }
                    slot.count = slot.count.saturating_add(count);
                }
            }
            let Some(next) = cursor.checked_add(every) else {
                break;
            };
            cursor = next;
        }
    }
}
struct Series {
    device: String,
    name: String,
    cap: String,
    kind: Kind,
    rings: [Ring; 3],
    last: Option<(f64, u64)>,
}
impl Series {
    fn new(device: &str, name: &str, cap: &str, kind: Kind) -> Result<Self> {
        Ok(Self {
            device: json::copy(device)?,
            name: json::copy(name)?,
            cap: json::copy(cap)?,
            kind,
            rings: [
                Ring::new(600, 144)?,
                Ring::new(7200, 84)?,
                Ring::new(18000, 144)?,
            ],
            last: None,
        })
    }
    fn add(&mut self, value: f64, at: u64) {
        if self.kind == Kind::Fraction {
            self.close(at);
        } else {
            for ring in &mut self.rings {
                if let Some(slot) = ring.slot(at / 1000) {
                    let n = value as f32;
                    if self.kind == Kind::Counter && slot.count > 0 && n < slot.max {
                        *slot = Slot {
                            period: slot.period,
                            min: n,
                            max: n,
                            sum: n,
                            count: 1,
                        };
                        continue;
                    }
                    if slot.count == 0 {
                        slot.min = n;
                        slot.max = n;
                    } else {
                        slot.min = slot.min.min(n);
                        slot.max = slot.max.max(n);
                    }
                    slot.sum += n;
                    slot.count = slot.count.saturating_add(1);
                }
            }
        }
        self.last = Some((value, at));
    }
    fn close(&mut self, at: u64) {
        if self.kind == Kind::Fraction
            && let Some((value, from)) = self.last
            && at > from
        {
            for ring in &mut self.rings {
                ring.spread(value, from, at);
            }
            self.last = Some((value, at));
        }
    }
    fn bytes(&self) -> usize {
        self.rings
            .iter()
            .map(|r| r.slots.len() * core::mem::size_of::<Slot>())
            .sum()
    }
}
/// Alle reeksen delen de configuratie-eigenaar; uitschakelen geeft het geheugen terug.
#[derive(Default)]
pub struct Statistics {
    series: Vec<Series>,
    closed: u64,
}
impl Statistics {
    /// Observeert de bestaande apparaatpublicatie; getallen blijven canoniek.
    pub fn observe(&mut self, device: &str, name: &str, state: &Value, at: u64) -> Result {
        let Some(fields) = state.as_object() else {
            return Ok(());
        };
        for (cap, v) in fields.iter() {
            let Some(kind) = Kind::of(cap) else {
                continue;
            };
            let value = match v {
                Value::Bool(b) => f64::from(u8::from(*b)),
                Value::Number(n) => n.as_f64(),
                _ => continue,
            };
            if !value.is_finite() || !(value as f32).is_finite() {
                continue;
            }
            let index = if let Some(i) = self
                .series
                .iter()
                .position(|s| s.device == device && s.cap == cap)
            {
                i
            } else {
                let i = self.series.len();
                json::push(
                    &mut self.series,
                    Series::new(device, name, cap, kind)?,
                    MAX_SERIES,
                )?;
                i
            };
            let s = self.series.get_mut(index).ok_or(Error::Full)?;
            if s.name != name {
                s.name = json::copy(name)?;
            }
            s.add(value, at);
        }
        Ok(())
    }
    /// Sluit stille schakelaars iedere vijf minuten af zonder ze opnieuw uit te lezen.
    pub fn tick(&mut self, at: u64) {
        if self.closed == 0 {
            self.closed = at;
            return;
        }
        if at.saturating_sub(self.closed) >= 300_000 {
            for s in &mut self.series {
                s.close(at);
            }
            self.closed = at;
        }
    }
    /// Werkelijk gereserveerde meetvakken, exclusief ids en eigenaarmetadata.
    pub fn bytes(&self) -> usize {
        self.series.iter().map(Series::bytes).sum()
    }
    /// De bestaande indexvorm voor Manage.
    pub fn list(&self) -> Result<Value> {
        let mut series = Vec::new();
        for s in &self.series {
            json::push(
                &mut series,
                json::fields(&[
                    ("deviceId", json::string(&s.device)?),
                    ("deviceName", json::string(&s.name)?),
                    ("capability", json::string(&s.cap)?),
                    ("kind", json::string(s.kind.name())?),
                    ("bytes", Value::uint(s.bytes() as u64)),
                ])?,
                MAX_SERIES,
            )?;
        }
        json::fields(&[
            ("series", Value::Array(series)),
            ("bytes", Value::uint(self.bytes() as u64)),
        ])
    }
    /// Levert alleen gevulde vakken van dag, week of maand, oud naar nieuw.
    pub fn window(&self, device: &str, cap: &str, window: &str) -> Result<Value> {
        let s = self
            .series
            .iter()
            .find(|s| s.device == device && s.cap == cap)
            .ok_or(Error::Missing(
                "nothing has been recorded for this device and capability",
            ))?;
        let tier = match window {
            "week" | "1" => 1,
            "month" | "2" => 2,
            _ => 0,
        };
        let ring = s.rings.get(tier).ok_or(Error::Full)?;
        let mut ordered = Vec::new();
        for slot in ring.slots.iter().filter(|s| s.count != 0) {
            json::push(&mut ordered, slot, 144)?;
        }
        ordered.sort_unstable_by_key(|s| s.period);
        let mut slots = Vec::new();
        for slot in ordered {
            let nanos = u64::from(slot.period)
                .checked_mul(ring.every)
                .and_then(|v| v.checked_mul(1_000_000_000))
                .ok_or(Error::Full)?;
            let mut entry = json::fields(&[
                ("at", json::string(&json::timestamp(nanos)?)?),
                ("count", Value::uint(u64::from(slot.count))),
            ])?;
            let number = |v: f64| Value::Number(Number::Float(v));
            if s.kind == Kind::Counter {
                json::set(&mut entry, "used", number(f64::from(slot.max - slot.min)))?;
            } else {
                json::set(
                    &mut entry,
                    if s.kind == Kind::Fraction {
                        "on"
                    } else {
                        "average"
                    },
                    number(f64::from(slot.sum) / f64::from(slot.count)),
                )?;
                if s.kind == Kind::Gauge {
                    json::set(&mut entry, "min", number(f64::from(slot.min)))?;
                    json::set(&mut entry, "max", number(f64::from(slot.max)))?;
                }
            }
            json::push(&mut slots, entry, 144)?;
        }
        json::fields(&[
            ("kind", json::string(s.kind.name())?),
            ("window", json::string(TIERS[tier].2)?),
            ("slots", Value::Array(slots)),
        ])
    }
}
