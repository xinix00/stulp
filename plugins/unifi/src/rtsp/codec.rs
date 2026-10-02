use super::{bad, bytes, copy};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_sdk::{Error, Result};
/// De codecs die de originele UniFi-plugin aan browsers levert.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Codec {
    /// H.264 zonder B-frameherordening.
    H264,
    /// AV1 met in-band sequence header.
    Av1,
}
/// Codecconfiguratie voor de MP4-sample entry.
pub struct Info {
    /// Codec.
    pub codec: Codec,
    /// Beeldbreedte.
    pub width: u16,
    /// Beeldhoogte.
    pub height: u16,
    /// Codecprofiel.
    pub profile: u8,
    /// H.264 constraint flags, of AV1 tier.
    pub flags: u8,
    /// Codecniveau.
    pub level: u8,
}
struct Bits<'a> {
    data: &'a [u8],
    at: usize,
}
impl<'a> Bits<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, at: 0 }
    }
    fn read(&mut self, n: usize) -> Result<u32> {
        if n > 32 || n > self.data.len().saturating_mul(8).saturating_sub(self.at) {
            return bad();
        }
        let mut value = 0;
        for _ in 0..n {
            value = (value << 1) | u32::from((self.data[self.at / 8] >> (7 - self.at % 8)) & 1);
            self.at += 1;
        }
        Ok(value)
    }
    fn ue(&mut self) -> Result<u32> {
        let mut n = 0;
        while self.read(1)? == 0 {
            n += 1;
            if n >= 32 {
                return bad();
            }
        }
        Ok(((1u32 << n) - 1) + self.read(n)?)
    }
    fn se(&mut self) -> Result<i64> {
        let v = i64::from(self.ue()?);
        Ok(if v % 2 == 0 { -v / 2 } else { (v + 1) / 2 })
    }
}
fn dimension(v: u32) -> Result<u16> {
    u16::try_from(v)
        .ok()
        .filter(|v| *v > 0)
        .ok_or(Error::Invalid("camera dimensions out of range"))
}
impl Info {
    /// Leest H.264 SPS inclusief high-profile scaling lists en uitsnede.
    pub fn h264(sps: &[u8]) -> Result<Self> {
        if sps.len() < 4 || sps[0] & 31 != 7 || sps.len() > 65535 {
            return bad();
        }
        let mut rbsp = Vec::new();
        let mut zeros = 0;
        for b in &sps[4..] {
            if zeros == 2 && *b == 3 {
                zeros = 0;
                continue;
            }
            bytes(&mut rbsp, &[*b])?;
            zeros = if *b == 0 { zeros + 1 } else { 0 };
        }
        let mut r = Bits::new(&rbsp);
        r.ue()?;
        let mut chroma = 1;
        let mut separate = 0;
        if matches!(
            sps[1],
            100 | 110 | 122 | 244 | 44 | 83 | 86 | 118 | 128 | 138 | 139 | 134 | 135
        ) {
            chroma = r.ue()?;
            if chroma > 3 {
                return bad();
            }
            if chroma == 3 {
                separate = r.read(1)?;
            }
            if r.ue()? > 6 || r.ue()? > 6 {
                return bad();
            }
            r.read(1)?;
            if r.read(1)? != 0 {
                for i in 0..if chroma == 3 { 12 } else { 8 } {
                    if r.read(1)? != 0 {
                        let mut last = 8i64;
                        let mut next = 8i64;
                        for _ in 0..if i < 6 { 16 } else { 64 } {
                            if next != 0 {
                                next = (last + r.se()?).rem_euclid(256);
                            }
                            if next != 0 {
                                last = next;
                            }
                        }
                    }
                }
            }
        }
        if r.ue()? > 12 {
            return bad();
        }
        match r.ue()? {
            0 => {
                if r.ue()? > 12 {
                    return bad();
                }
            }
            1 => {
                r.read(1)?;
                r.se()?;
                r.se()?;
                let n = r.ue()?;
                if n > 255 {
                    return bad();
                }
                for _ in 0..n {
                    r.se()?;
                }
            }
            2 => (),
            _ => return bad(),
        }
        r.ue()?;
        r.read(1)?;
        let w = r
            .ue()?
            .checked_add(1)
            .and_then(|v| v.checked_mul(16))
            .ok_or(Error::Invalid("SPS width overflow"))?;
        let h = r
            .ue()?
            .checked_add(1)
            .ok_or(Error::Invalid("SPS height overflow"))?;
        let frame = r.read(1)?;
        if frame == 0 {
            r.read(1)?;
        }
        r.read(1)?;
        let mut h = h
            .checked_mul(16 * (2 - frame))
            .ok_or(Error::Invalid("SPS height overflow"))?;
        let mut w = w;
        if r.read(1)? != 0 {
            let left = r.ue()?;
            let right = r.ue()?;
            let top = r.ue()?;
            let bottom = r.ue()?;
            let c = if separate != 0 { 0 } else { chroma };
            let x = if c == 1 || c == 2 { 2 } else { 1 };
            let y = if c == 1 { 2 } else { 1 } * (2 - frame);
            w = w
                .checked_sub(
                    left.checked_add(right)
                        .and_then(|v| v.checked_mul(x))
                        .ok_or(Error::Invalid("SPS crop overflow"))?,
                )
                .ok_or(Error::Invalid("invalid SPS crop"))?;
            h = h
                .checked_sub(
                    top.checked_add(bottom)
                        .and_then(|v| v.checked_mul(y))
                        .ok_or(Error::Invalid("SPS crop overflow"))?,
                )
                .ok_or(Error::Invalid("invalid SPS crop"))?;
        }
        Ok(Self {
            codec: Codec::H264,
            width: dimension(w)?,
            height: dimension(h)?,
            profile: sps[1],
            flags: sps[2],
            level: sps[3],
        })
    }
    /// Leest AV1 operating points en beeldmaat, zoals het oorspronkelijke camerapad.
    pub fn av1(sequence: &[u8]) -> Result<Self> {
        if sequence.first().is_none_or(|b| (b >> 3) & 15 != 1) {
            return bad();
        }
        let mut r = Bits::new(obu_payload(sequence)?);
        let profile = r.read(3)? as u8;
        if profile > 2 {
            return bad();
        }
        r.read(1)?;
        let reduced = r.read(1)? != 0;
        let mut level = 0;
        let mut tier = 0;
        if reduced {
            level = r.read(5)? as u8;
        } else {
            let mut model = false;
            let mut delay = 0;
            if r.read(1)? != 0 {
                r.read(32)?;
                r.read(32)?;
                if r.read(1)? != 0 {
                    r.ue()?;
                }
                model = r.read(1)? != 0;
                if model {
                    delay = r.read(5)? as usize + 1;
                    r.read(32)?;
                    r.read(5)?;
                    r.read(5)?;
                }
            }
            let initial = r.read(1)? != 0;
            let points = r.read(5)? + 1;
            for point in 0..points {
                r.read(12)?;
                let l = r.read(5)? as u8;
                let t = if l > 7 { r.read(1)? as u8 } else { 0 };
                if point == 0 {
                    level = l;
                    tier = t;
                }
                if model && r.read(1)? != 0 {
                    r.read(delay)?;
                    r.read(delay)?;
                    r.read(1)?;
                }
                if initial && r.read(1)? != 0 {
                    r.read(4)?;
                }
            }
        }
        let wb = r.read(4)? as usize + 1;
        let hb = r.read(4)? as usize + 1;
        Ok(Self {
            codec: Codec::Av1,
            width: dimension(r.read(wb)? + 1)?,
            height: dimension(r.read(hb)? + 1)?,
            profile,
            flags: tier,
            level,
        })
    }
    /// MIME inclusief codec voor MediaSource.
    pub fn mime(&self) -> Result<String> {
        let mut out = String::new();
        out.try_reserve(64).map_err(|_| stulp_core::Error::Memory)?;
        match self.codec {
            Codec::H264 => write!(
                &mut out,
                "video/mp4; codecs=\"avc1.{:02X}{:02X}{:02X}\"",
                self.profile, self.flags, self.level
            ),
            Codec::Av1 => write!(
                &mut out,
                "video/mp4; codecs=\"av01.{}.{:02}{}.08\"",
                self.profile,
                self.level,
                if self.flags == 0 { "M" } else { "H" }
            ),
        }
        .map_err(|_| Error::Invalid("codec formatting failed"))?;
        Ok(out)
    }
}
pub(super) fn leb(input: &[u8]) -> Result<(usize, usize)> {
    let mut n = 0u64;
    for (i, b) in input.iter().take(8).enumerate() {
        n |= u64::from(b & 127) << (i * 7);
        if b & 128 == 0 {
            return Ok((
                usize::try_from(n).map_err(|_| Error::Invalid("OBU length overflow"))?,
                i + 1,
            ));
        }
    }
    bad()
}
fn put_leb(out: &mut Vec<u8>, mut n: usize) -> Result {
    loop {
        let b = (n & 127) as u8;
        n >>= 7;
        bytes(out, &[b | if n > 0 { 128 } else { 0 }])?;
        if n == 0 {
            return Ok(());
        }
    }
}
pub(super) fn obu_payload(obu: &[u8]) -> Result<&[u8]> {
    let h = *obu.first().ok_or(Error::Invalid("empty OBU"))?;
    if h & 0x81 != 0 {
        return bad();
    }
    let mut at = 1 + usize::from(h & 4 != 0);
    let rest = obu.get(at..).ok_or(Error::Invalid("truncated OBU"))?;
    if h & 2 != 0 {
        let (n, len) = leb(rest)?;
        at += len;
        if n != obu.len() - at {
            return bad();
        }
    }
    obu.get(at..)
        .filter(|v| !v.is_empty())
        .ok_or(Error::Invalid("empty OBU payload"))
}
pub(super) fn av1_sample(unit: &[Vec<u8>]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    for obu in unit {
        let payload = obu_payload(obu)?;
        bytes(&mut out, &[obu[0] | 2])?;
        if obu[0] & 4 != 0 {
            bytes(&mut out, &[obu[1]])?;
        }
        put_leb(&mut out, payload.len())?;
        bytes(&mut out, payload)?;
    }
    Ok(out)
}
pub(super) fn keyframe(codec: Codec, unit: &[Vec<u8>]) -> bool {
    match codec {
        Codec::H264 => unit.iter().any(|n| n.first().is_some_and(|b| b & 31 == 5)),
        Codec::Av1 => unit
            .iter()
            .find(|n| n.first().is_some_and(|b| matches!((b >> 3) & 15, 3 | 6)))
            .and_then(|n| obu_payload(n).ok())
            .is_some_and(|p| p[0] & 0xe0 == 0),
    }
}
pub(super) fn config(info: &Info, first: &[u8], second: &[u8]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    match info.codec {
        Codec::H264 => {
            if first.len() > 65535
                || second.is_empty()
                || second.len() > 65535
                || second[0] & 31 != 8
            {
                return bad();
            }
            bytes(
                &mut out,
                &[1, info.profile, info.flags, info.level, 255, 225],
            )?;
            bytes(&mut out, &(first.len() as u16).to_be_bytes())?;
            bytes(&mut out, first)?;
            bytes(&mut out, &[1])?;
            bytes(&mut out, &(second.len() as u16).to_be_bytes())?;
            bytes(&mut out, second)?;
        }
        Codec::Av1 => {
            bytes(
                &mut out,
                &[
                    0x81,
                    (info.profile << 5) | (info.level & 31),
                    info.flags << 7,
                    0,
                ],
            )?;
            bytes(&mut out, &av1_sample(&[copy(first)?])?)?;
        }
    }
    Ok(out)
}
