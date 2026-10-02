//! IANA TZif wordt eenmaal geladen; geen globale TZ-mutatie of libc-slot rond kalenderwerk.
use alloc::vec::Vec;
use stulp_core::{Error, Result, json};
use stulp_runtime::calendar::{self, Civil};
#[derive(Clone, Copy)]
struct Zone {
    offset: i32,
    dst: bool,
}
struct Transition {
    at: i64,
    zone: usize,
}
#[derive(Default)]
/// Decoded IANA transitions and optional POSIX extension rules.
pub struct Timezone {
    zones: Vec<Zone>,
    transitions: Vec<Transition>,
    extend: Option<Extension>,
}
impl Timezone {
    /// Validate and decode a bounded TZif byte stream without platform I/O.
    pub fn decode(bytes: &[u8]) -> Result<Self> {
        let (version, counts) = header(bytes, 0)?;
        let (start, width, c) = if matches!(version, b'2' | b'3' | b'4') {
            let next = 44usize.checked_add(size(counts, 4)?).ok_or(Error::Full)?;
            let (_, c) = header(bytes, next)?;
            (next + 44, 8, c)
        } else if version == 0 {
            (44, 4, counts)
        } else {
            return Err(Error::Invalid("unsupported timezone format"));
        };
        let end = start.checked_add(size(c, width)?).ok_or(Error::Full)?;
        let block = bytes
            .get(start..end)
            .ok_or(Error::Invalid("truncated timezone"))?;
        let [_, _, _, times, types, chars] = c;
        if types == 0 || types > 256 || times > 100_000 {
            return Err(Error::Invalid("invalid timezone counts"));
        }
        let zone_start = times * (width + 1);
        let strings = block
            .get(zone_start + types * 6..zone_start + types * 6 + chars)
            .ok_or(Error::Invalid("timezone names missing"))?;
        let mut zones = Vec::new();
        for i in 0..types {
            let p = zone_start + i * 6;
            let offset = i32::from_be_bytes(
                block
                    .get(p..p + 4)
                    .ok_or(Error::Full)?
                    .try_into()
                    .map_err(|_| Error::Full)?,
            );
            let dst = *block.get(p + 4).ok_or(Error::Full)?;
            let name = usize::from(*block.get(p + 5).ok_or(Error::Full)?);
            if dst > 1 || strings.get(name..).is_none_or(|s| !s.contains(&0)) {
                return Err(Error::Invalid("invalid timezone type"));
            }
            json::push(
                &mut zones,
                Zone {
                    offset,
                    dst: dst != 0,
                },
                256,
            )?;
        }
        let mut transitions: Vec<Transition> = Vec::new();
        for i in 0..times {
            let b = block.get(i * width..(i + 1) * width).ok_or(Error::Full)?;
            let at = if width == 8 {
                i64::from_be_bytes(b.try_into().map_err(|_| Error::Full)?)
            } else {
                i64::from(i32::from_be_bytes(b.try_into().map_err(|_| Error::Full)?))
            };
            let zone = usize::from(*block.get(times * width + i).ok_or(Error::Full)?);
            if zone >= types || transitions.last().is_some_and(|t| t.at >= at) {
                return Err(Error::Invalid("invalid timezone transition"));
            }
            json::push(&mut transitions, Transition { at, zone }, 100_000)?;
        }
        let extend = bytes
            .get(end..)
            .and_then(|s| s.strip_prefix(b"\n"))
            .and_then(|s| s.strip_suffix(b"\n"))
            .filter(|s| !s.is_empty())
            .map(|s| {
                Extension::parse(
                    core::str::from_utf8(s)
                        .map_err(|_| Error::Invalid("invalid timezone extension"))?,
                )
            })
            .transpose()?;
        Ok(Self {
            zones,
            transitions,
            extend,
        })
    }
    /// Convert a Unix instant using the transition table or extension rule.
    pub fn local(&self, unix: i64) -> Result<Civil> {
        calendar::civil(unix, self.offset(unix)?)
    }
    fn offset(&self, unix: i64) -> Result<i32> {
        if self.zones.is_empty() {
            return Ok(0);
        }
        if self.transitions.last().is_none_or(|t| unix >= t.at)
            && let Some(ext) = &self.extend
        {
            return ext.offset(unix);
        }
        let i = self.transitions.partition_point(|t| t.at <= unix);
        let index = if i > 0 {
            self.transitions.get(i - 1).ok_or(Error::Full)?.zone
        } else {
            self.first()
        };
        Ok(self
            .zones
            .get(index)
            .ok_or(Error::Invalid("timezone type missing"))?
            .offset)
    }
    fn first(&self) -> usize {
        if !self.transitions.iter().any(|t| t.zone == 0) {
            return 0;
        }
        if let Some(t) = self.transitions.first()
            && self.zones.get(t.zone).is_some_and(|z| z.dst)
            && let Some(i) = (0..t.zone)
                .rev()
                .find(|&i| self.zones.get(i).is_some_and(|z| !z.dst))
        {
            return i;
        }
        self.zones.iter().position(|z| !z.dst).unwrap_or(0)
    }
}

fn header(b: &[u8], start: usize) -> Result<(u8, [usize; 6])> {
    let h = b
        .get(start..start.checked_add(44).ok_or(Error::Full)?)
        .ok_or(Error::Invalid("truncated timezone header"))?;
    if h.get(..4) != Some(b"TZif") {
        return Err(Error::Invalid("invalid timezone header"));
    }
    let mut counts = [0; 6];
    for (i, v) in counts.iter_mut().enumerate() {
        *v = u32::from_be_bytes(
            h.get(20 + i * 4..24 + i * 4)
                .ok_or(Error::Full)?
                .try_into()
                .map_err(|_| Error::Full)?,
        ) as usize;
    }
    Ok((*h.get(4).ok_or(Error::Full)?, counts))
}
fn size(c: [usize; 6], width: usize) -> Result<usize> {
    let [ut, std, leap, time, types, chars] = c;
    time.checked_mul(width + 1)
        .and_then(|s| s.checked_add(types.checked_mul(6)?))
        .and_then(|s| s.checked_add(chars))
        .and_then(|s| s.checked_add(leap.checked_mul(width + 4)?))
        .and_then(|s| s.checked_add(std))
        .and_then(|s| s.checked_add(ut))
        .ok_or(Error::Full)
}
struct Extension {
    std: i32,
    dst: i32,
    rules: Option<(Rule, Rule)>,
}
struct Rule {
    kind: RuleKind,
    seconds: i32,
}
enum RuleKind {
    Julian(i32),
    Day(i32),
    Month { month: u8, week: i32, day: i32 },
}
impl Extension {
    fn parse(s: &str) -> Result<Self> {
        let mut p = Parser(s);
        p.name()?;
        let std = -p.offset()?;
        if p.0.is_empty() || p.0.starts_with(',') {
            return Ok(Self {
                std,
                dst: std,
                rules: None,
            });
        }
        p.name()?;
        let dst = if p.0.is_empty() || p.0.starts_with(',') {
            std + 3600
        } else {
            -p.offset()?
        };
        if p.0.is_empty() {
            p.0 = ",M3.2.0,M11.1.0";
        }
        if !p.eat(',') {
            p.require(';')?;
        }
        let start = p.rule()?;
        p.require(',')?;
        let end = p.rule()?;
        if !p.0.is_empty() {
            return Err(Error::Invalid("invalid timezone extension suffix"));
        }
        Ok(Self {
            std,
            dst,
            rules: Some((start, end)),
        })
    }
    fn offset(&self, unix: i64) -> Result<i32> {
        let Some((start, end)) = &self.rules else {
            return Ok(self.std);
        };
        let year = calendar::civil(unix, 0)?.year;
        let start = start.at(year, self.std)?;
        let end = end.at(year, self.dst)?;
        let daylight = if start < end {
            unix >= start && unix < end
        } else {
            unix >= start || unix < end
        };
        Ok(if daylight { self.dst } else { self.std })
    }
}
impl Rule {
    fn at(&self, year: i32, offset: i32) -> Result<i64> {
        let midnight = calendar::midnight(year, 1, 1)?;
        let days = match self.kind {
            RuleKind::Julian(day) => {
                i64::from(day - 1 + i32::from(calendar::leap(year) && day >= 60))
            }
            RuleKind::Day(day) => i64::from(day),
            RuleKind::Month { month, week, day } => {
                let start = calendar::midnight(year, month, 1)?;
                let weekday = (start / 86400 + 4).rem_euclid(7);
                let mut d = (i64::from(day) - weekday).rem_euclid(7) + 7 * i64::from(week - 1);
                if d >= i64::from(calendar::month_days(year, month)) {
                    d -= 7;
                }
                (start - midnight) / 86400 + d
            }
        };
        Ok(midnight + days * 86400 + i64::from(self.seconds) - i64::from(offset))
    }
}
struct Parser<'a>(&'a str);
impl Parser<'_> {
    fn eat(&mut self, c: char) -> bool {
        if let Some(tail) = self.0.strip_prefix(c) {
            self.0 = tail;
            true
        } else {
            false
        }
    }
    fn require(&mut self, c: char) -> Result {
        if self.eat(c) {
            Ok(())
        } else {
            Err(Error::Invalid("invalid timezone rule"))
        }
    }
    fn number(&mut self, min: i32, max: i32) -> Result<i32> {
        let n = self.0.bytes().take_while(u8::is_ascii_digit).count();
        let (number, rest) = self.0.split_at(n);
        let number = number
            .parse::<i32>()
            .map_err(|_| Error::Invalid("invalid timezone number"))?;
        if !(min..=max).contains(&number) {
            return Err(Error::Invalid("timezone number out of range"));
        }
        self.0 = rest;
        Ok(number)
    }
    fn name(&mut self) -> Result {
        if self.eat('<') {
            let end = self
                .0
                .find('>')
                .ok_or(Error::Invalid("invalid timezone name"))?;
            if end == 0 {
                return Err(Error::Invalid("empty timezone name"));
            }
            self.0 = &self.0[end + 1..];
            return Ok(());
        }
        let n = self
            .0
            .bytes()
            .take_while(|b| !b.is_ascii_digit() && !matches!(b, b',' | b'+' | b'-'))
            .count();
        if n < 3 {
            return Err(Error::Invalid("invalid timezone name"));
        }
        self.0 = &self.0[n..];
        Ok(())
    }
    fn offset(&mut self) -> Result<i32> {
        let sign = if self.eat('-') {
            -1
        } else {
            self.eat('+');
            1
        };
        let mut n = self.number(0, 168)? * 3600;
        if self.eat(':') {
            n += self.number(0, 59)? * 60;
            if self.eat(':') {
                n += self.number(0, 59)?;
            }
        }
        Ok(sign * n)
    }
    fn rule(&mut self) -> Result<Rule> {
        let kind = if self.eat('J') {
            RuleKind::Julian(self.number(1, 365)?)
        } else if self.eat('M') {
            let month = self.number(1, 12)? as u8;
            self.require('.')?;
            let week = self.number(1, 5)?;
            self.require('.')?;
            let day = self.number(0, 6)?;
            RuleKind::Month { month, week, day }
        } else {
            RuleKind::Day(self.number(0, 365)?)
        };
        let seconds = if self.eat('/') { self.offset()? } else { 7200 };
        Ok(Rule { kind, seconds })
    }
}
