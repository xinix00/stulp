//! Gegroepeerde leesronden; geweigerde bereiken vallen blijvend terug op losse registers.
use super::modbus::{Failure, Modbus};
use alloc::vec::Vec;
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, Transport,
    util::{field, float},
};
#[derive(Clone, Copy)]
pub(super) enum Kind {
    U16,
    I16,
    U32,
    I32,
    U64,
    Text,
}
#[derive(Clone, Copy)]
pub(super) struct Reg {
    pub(super) name: &'static str,
    pub(super) class: &'static str,
    pub(super) addr: u16,
    pub(super) count: u16,
    pub(super) kind: Kind,
    pub(super) gain: f64,
}
impl Reg {
    pub(super) fn decode(&self, words: &[u16]) -> Result<Value> {
        if words.len() < usize::from(self.count) {
            return Err(Error::Invalid("short Sigenergy register"));
        }
        let raw = match self.kind {
            Kind::U16 => f64::from(words[0]),
            Kind::I16 => f64::from(words[0] as i16),
            Kind::U32 => f64::from(u32::from(words[0]) << 16 | u32::from(words[1])),
            Kind::I32 => f64::from((u32::from(words[0]) << 16 | u32::from(words[1])) as i32),
            Kind::U64 => {
                ((u64::from(words[0]) << 48)
                    | (u64::from(words[1]) << 32)
                    | (u64::from(words[2]) << 16)
                    | u64::from(words[3])) as f64
            }
            Kind::Text => {
                let mut bytes = Vec::new();
                bytes
                    .try_reserve_exact(usize::from(self.count) * 2)
                    .map_err(|_| stulp_core::Error::Memory)?;
                for word in &words[..usize::from(self.count)] {
                    for byte in word.to_be_bytes() {
                        if byte != 0 {
                            bytes.push(byte);
                        }
                    }
                }
                let text = core::str::from_utf8(&bytes)
                    .map_err(|_| Error::Invalid("invalid Sigenergy text register"))?;
                return Ok(json::string(text.trim())?);
            }
        };
        float(raw / self.gain)
    }
}
struct Run {
    regs: Vec<&'static Reg>,
    start: u16,
    count: u16,
    solo: bool,
}
pub(super) struct Poller {
    runs: Vec<Run>,
}
impl Poller {
    pub(super) fn new(card: &'static [Reg], class: &str) -> Result<Self> {
        let mut sorted = Vec::new();
        for reg in card.iter().filter(|r| r.class == class) {
            json::push(&mut sorted, reg, 64)?;
        }
        sorted.sort_unstable_by_key(|r| r.addr);
        let mut runs: Vec<Run> = Vec::new();
        for reg in sorted {
            if let Some(last) = runs.last_mut() {
                let end = last.start + last.count;
                let next = reg.addr + reg.count;
                if (last.start >= 40000) == (reg.addr >= 40000)
                    && reg.addr >= end
                    && reg.addr - end <= 8
                    && next - last.start <= 120
                {
                    json::push(&mut last.regs, reg, 64)?;
                    last.count = next - last.start;
                    continue;
                }
            }
            let mut regs = Vec::new();
            json::push(&mut regs, reg, 64)?;
            json::push(
                &mut runs,
                Run {
                    regs,
                    start: reg.addr,
                    count: reg.count,
                    solo: false,
                },
                64,
            )?;
        }
        Ok(Self { runs })
    }
    pub(super) async fn read<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        bus: &mut Modbus,
        address: &str,
        timeout: u64,
        unit: u8,
    ) -> Result<Value> {
        let mut values = json::object();
        for run in &mut self.runs {
            if !run.solo && run.regs.len() > 1 {
                match bus
                    .read(c, address, timeout, unit, run.start, run.count)
                    .await
                {
                    Ok(words) => {
                        for reg in &run.regs {
                            let offset = usize::from(reg.addr - run.start);
                            match reg.decode(&words[offset..offset + usize::from(reg.count)]) {
                                Ok(value) => json::set(&mut values, reg.name, value)?,
                                Err(Error::Invalid("invalid Sigenergy text register")) => (),
                                Err(e) => return Err(e),
                            }
                        }
                        continue;
                    }
                    Err(Failure::Refused(_)) => run.solo = true,
                    Err(e) => return Err(e.sdk()),
                }
            }
            for reg in &run.regs {
                match bus
                    .read(c, address, timeout, unit, reg.addr, reg.count)
                    .await
                {
                    Ok(words) => match reg.decode(&words) {
                        Ok(v) => json::set(&mut values, reg.name, v)?,
                        Err(Error::Invalid("invalid Sigenergy text register")) => (),
                        Err(e) => return Err(e),
                    },
                    Err(Failure::Refused(_)) => (),
                    Err(e) => return Err(e.sdk()),
                }
            }
        }
        Ok(values)
    }
}
pub(super) fn card(driver: &str) -> Result<&'static [Reg]> {
    use super::map::*;
    match driver {
        "plant" => Ok(PLANT),
        "inverter" => Ok(INVERTER),
        "battery" => Ok(BATTERY),
        "energy" => Ok(ENERGY),
        "evaccharger" => Ok(EVACCHARGER),
        _ => Err(Error::Invalid("unknown Sigenergy driver")),
    }
}
pub(super) fn probe(driver: &str) -> Result<&'static Reg> {
    let name = match driver {
        "plant" => "GridPower",
        "energy" => "Power",
        "inverter" => "MPPTCount",
        "battery" => "SoC",
        "evaccharger" => "Status",
        _ => return Err(Error::Invalid("unknown Sigenergy driver")),
    };
    card(driver)?
        .iter()
        .find(|r| r.name == name)
        .ok_or(Error::Invalid("Sigenergy probe missing"))
}
pub(super) fn merge(into: &mut Value, from: Value) -> Result {
    if let Some(fields) = from.as_object() {
        for (k, v) in fields.iter() {
            json::set(into, k, stulp_sdk::clone(v)?)?;
        }
    }
    Ok(())
}
pub(super) fn n(value: &Value, key: &str) -> Option<f64> {
    stulp_sdk::util::number(field(value, key))
}
pub(super) fn rounded(n: f64) -> Result<Value> {
    let scaled = n * 1000.;
    float(if scaled >= i64::MAX as f64 || scaled <= i64::MIN as f64 {
        n
    } else {
        ((scaled + if scaled < 0. { -0.5 } else { 0.5 }) as i64) as f64 / 1000.
    })
}
#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]
    use super::super::{map::*, tests::wire::Wire};
    use super::*;
    use stulp_sdk::Plugin;
    #[test]
    fn original_scales_word_order_and_group_ranges() {
        for (card, name, words, want) in [
            (PLANT, "GridPower", &[0xffff, 0xf63c][..], -2500.),
            (PLANT, "BatterySoC", &[523][..], 52.3),
            (BATTERY, "MinCellTemp", &[0xfff1][..], -1.5),
            (INVERTER, "PhaseAVoltage", &[0, 0x5ab6][..], 232.22),
            (BATTERY, "TotalCharged", &[0, 0, 1, 0x86a0][..], 1000.),
            (EVACCHARGER, "Power", &[0, 0x1b58][..], 7.),
        ] {
            let reg = card.iter().find(|r| r.name == name).unwrap();
            assert_eq!(
                stulp_sdk::util::number(&reg.decode(words).unwrap()),
                Some(want)
            );
        }
        let runs = Poller::new(PLANT, "reading").unwrap().runs;
        let ranges: Vec<_> = runs.iter().map(|r| (r.start, r.count)).collect();
        assert_eq!(ranges, [(30005, 10), (30035, 4), (30194, 2), (30282, 2)]);
        let reg = INVERTER.iter().find(|r| r.name == "Serial").unwrap();
        assert_eq!(
            reg.decode(&[0x5349, 0x4745, 0x3132, 0x3300, 0, 0, 0, 0, 0, 0])
                .unwrap()
                .as_str(),
            Some("SIGE123")
        );
    }
    #[test]
    fn refused_range_switches_to_solos_and_missing_values_stay_missing() {
        let manifest = super::super::Sigenergy::default().manifest();
        let mut w = Wire::client("com.stulp.sigenergy", manifest, "[]", "{}").into_transport();
        // EVAC: combined 32000..32004 is refused, then the power register alone is absent.
        w.tcp_bodies.push_back(alloc::vec![2, 0x83, 2]);
        for _ in 0..2 {
            w.tcp_bodies.push_back(alloc::vec![2, 3, 2, 0, 4]);
            w.tcp_bodies
                .push_back(alloc::vec![2, 3, 4, 0, 0, 0x27, 0x10]);
            w.tcp_bodies.push_back(alloc::vec![2, 0x83, 2]);
        }
        let mut c = w.reconnect("com.stulp.sigenergy", manifest);
        let mut bus = Modbus::default();
        let mut p = Poller::new(EVACCHARGER, "reading").unwrap();
        for _ in 0..2 {
            let v = hostnet::block_on(p.read(&mut c, &mut bus, "192.0.2.1:502", 500, 2)).unwrap();
            assert_eq!(n(&v, "Status"), Some(4.));
            assert_eq!(n(&v, "TotalCharged"), Some(100.));
            assert!(field(&v, "Power").is_null());
        }
        let w = c.into_transport();
        assert_eq!(w.tcp.len(), 7);
        assert!(w.tcp.iter().all(|r| r.frame[7] == 3));
        assert_eq!(&w.tcp[0].frame[8..], &[0x7d, 0, 0, 5]);
        assert!(w.tcp.iter().all(|r| r.generation == 0));
    }
}
