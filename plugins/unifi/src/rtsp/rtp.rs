use super::{Codec, MAX_FRAME, bad, bytes, codec, copy};
use alloc::vec::Vec;
use stulp_core::json;
use stulp_sdk::{Error, Result};
/// Gevalideerde RTP-payload; padding, CSRC en extension worden niet als beeld gelezen.
pub struct Packet<'a> {
    /// RTP payload type uit SDP.
    pub payload_type: u8,
    /// Geordend pakketnummer met 16-bit wrap.
    pub sequence: u16,
    /// Identiteit van de camerastroom.
    pub source: u32,
    /// 90 kHz timestamp.
    pub timestamp: u32,
    /// Einde van een toegangseenheid.
    pub marker: bool,
    /// Codecbytes zonder RTP-kop.
    pub payload: &'a [u8],
}
impl<'a> Packet<'a> {
    /// Accepteert uitsluitend complete RTP-v2-pakketten.
    pub fn parse(b: &'a [u8]) -> Result<Self> {
        if b.len() < 12 || b[0] >> 6 != 2 {
            return bad();
        }
        let mut at = 12 + usize::from(b[0] & 15) * 4;
        if at > b.len() {
            return bad();
        }
        if b[0] & 16 != 0 {
            let ext = b
                .get(at..at + 4)
                .ok_or(Error::Invalid("truncated RTP extension"))?;
            at += 4 + usize::from(u16::from_be_bytes([ext[2], ext[3]])) * 4;
        }
        let end = if b[0] & 32 != 0 {
            let n = usize::from(*b.last().ok_or(Error::Invalid("missing RTP padding"))?);
            if n == 0 || n > b.len().saturating_sub(at) {
                return bad();
            }
            b.len() - n
        } else {
            b.len()
        };
        let payload = b
            .get(at..end)
            .filter(|v| !v.is_empty())
            .ok_or(Error::Invalid("empty RTP payload"))?;
        Ok(Self {
            payload_type: b[1] & 127,
            sequence: u16::from_be_bytes([b[2], b[3]]),
            source: u32::from_be_bytes([b[8], b[9], b[10], b[11]]),
            timestamp: u32::from_be_bytes([b[4], b[5], b[6], b[7]]),
            marker: b[1] & 128 != 0,
            payload,
        })
    }
}
/// Complete codec-eenheid, zonder nog ontbrekende fragments.
pub struct Unit {
    /// NALs (H.264) of OBUs (AV1).
    pub parts: Vec<Vec<u8>>,
    /// RTP-timestamp van deze eenheid.
    pub timestamp: u32,
}
/// Eén RTP-stroom, met begrensde fragmentopslag en expliciete verliesdetectie.
pub struct Assembler {
    codec: Codec,
    payload: u8,
    source: Option<u32>,
    last: Option<u16>,
    timestamp: Option<u32>,
    parts: Vec<Vec<u8>>,
    pending: Vec<u8>,
    used: usize,
    damaged: bool,
    sequence: Vec<u8>,
}
impl Assembler {
    /// Payload type hoort bij het gekozen videospoor.
    pub fn new(codec: Codec, payload: u8) -> Self {
        Self {
            codec,
            payload,
            source: None,
            last: None,
            timestamp: None,
            parts: Vec::new(),
            pending: Vec::new(),
            used: 0,
            damaged: false,
            sequence: Vec::new(),
        }
    }
    /// De laatst volledig ontvangen AV1 sequence header.
    pub fn sequence_header(&self) -> &[u8] {
        &self.sequence
    }
    fn clear(&mut self) {
        self.parts.clear();
        self.pending.clear();
        self.used = 0;
    }
    fn finish(&mut self, out: &mut Vec<Unit>) -> Result {
        if !self.damaged && self.pending.is_empty() && !self.parts.is_empty() {
            json::push(
                out,
                Unit {
                    parts: core::mem::take(&mut self.parts),
                    timestamp: self
                        .timestamp
                        .ok_or(Error::Invalid("frame timestamp missing"))?,
                },
                2,
            )?;
        }
        self.clear();
        Ok(())
    }
    /// Levert maximaal twee eenheden: een markerloos vorig beeld en het huidige beeld.
    pub fn push(&mut self, p: Packet<'_>) -> Result<Vec<Unit>> {
        let mut out = Vec::new();
        if p.payload_type != self.payload {
            return Ok(out);
        }
        if self.source != Some(p.source) {
            self.clear();
            self.source = Some(p.source);
            self.last = None;
            self.timestamp = None;
            self.sequence.clear();
        }
        let delta = self.last.map(|last| p.sequence.wrapping_sub(last));
        if delta.is_some_and(|d| d == 0 || d >= 32768) {
            return Ok(out);
        }
        if self.timestamp != Some(p.timestamp) {
            if delta.is_some_and(|d| d != 1) {
                self.damaged = true;
            }
            self.finish(&mut out)?;
            self.timestamp = Some(p.timestamp);
            self.damaged = false;
        } else if delta.is_some_and(|d| d != 1) {
            self.damaged = true;
            self.clear();
        }
        self.last = Some(p.sequence);
        if !self.damaged {
            let result = match self.codec {
                Codec::H264 => self.h264(p.payload),
                Codec::Av1 => self.av1(p.payload),
            };
            if let Err(e) = result {
                self.damaged = true;
                self.clear();
                return Err(e);
            }
        }
        if p.marker {
            self.finish(&mut out)?;
        }
        Ok(out)
    }
    fn count(&mut self, n: usize) -> Result {
        if n > MAX_FRAME.saturating_sub(self.used) {
            return Err(Error::Invalid("camera access unit exceeds 8 MiB"));
        }
        self.used += n;
        Ok(())
    }
    fn part(&mut self, n: &[u8]) -> Result {
        self.count(n.len())?;
        json::push(&mut self.parts, copy(n)?, 128)?;
        Ok(())
    }
    fn h264(&mut self, p: &[u8]) -> Result {
        let h = *p.first().ok_or(Error::Invalid("empty H.264 payload"))?;
        if h & 128 != 0 {
            return bad();
        }
        match h & 31 {
            1..=23 => {
                if !self.pending.is_empty() {
                    return bad();
                }
                self.part(p)
            }
            24 => {
                if !self.pending.is_empty() {
                    return bad();
                }
                let mut rest = &p[1..];
                while !rest.is_empty() {
                    if rest.len() < 2 {
                        return bad();
                    }
                    let n = usize::from(u16::from_be_bytes([rest[0], rest[1]]));
                    rest = &rest[2..];
                    if n == 0 || n > rest.len() || !(1..=23).contains(&(rest[0] & 31)) {
                        return bad();
                    }
                    self.part(&rest[..n])?;
                    rest = &rest[n..];
                }
                Ok(())
            }
            28 => {
                if p.len() < 3
                    || p[1] & 32 != 0
                    || p[1] & 0xc0 == 0xc0
                    || !(1..=23).contains(&(p[1] & 31))
                {
                    return bad();
                }
                let start = p[1] & 128 != 0;
                let end = p[1] & 64 != 0;
                if start {
                    if !self.pending.is_empty() {
                        return bad();
                    }
                    self.count(1)?;
                    bytes(&mut self.pending, &[(h & 0xe0) | (p[1] & 31)])?;
                }
                if self.pending.is_empty() {
                    self.damaged = true;
                    return Ok(());
                }
                if self.pending[0] != (h & 0xe0) | (p[1] & 31) {
                    return bad();
                }
                self.count(p.len() - 2)?;
                bytes(&mut self.pending, &p[2..])?;
                if end {
                    json::push(&mut self.parts, core::mem::take(&mut self.pending), 128)?;
                }
                Ok(())
            }
            _ => bad(),
        }
    }
    fn flush_obu(&mut self) -> Result {
        if self.pending.is_empty() {
            return bad();
        }
        let kind = (self.pending[0] >> 3) & 15;
        if kind != 2 {
            codec::obu_payload(&self.pending)?;
        }
        if kind == 1 {
            self.sequence = copy(&self.pending)?;
        }
        if kind == 2 {
            self.pending.clear();
            return Ok(());
        }
        json::push(&mut self.parts, core::mem::take(&mut self.pending), 128)?;
        Ok(())
    }
    fn av1(&mut self, p: &[u8]) -> Result {
        let h = *p.first().ok_or(Error::Invalid("empty AV1 payload"))?;
        if h & 7 != 0 || h & 0x88 == 0x88 {
            return bad();
        }
        let continues = h & 128 != 0;
        let more = h & 64 != 0;
        let count = usize::from((h >> 4) & 3);
        if continues == self.pending.is_empty() {
            self.damaged = true;
            return Ok(());
        }
        let mut rest = &p[1..];
        let mut i = 0;
        while !rest.is_empty() {
            let n = if count > 0 && i == count - 1 {
                rest.len()
            } else {
                let (n, used) = codec::leb(rest)?;
                rest = &rest[used..];
                n
            };
            if n == 0 || n > rest.len() || (count > 0 && i >= count) {
                return bad();
            }
            self.count(n)?;
            bytes(&mut self.pending, &rest[..n])?;
            rest = &rest[n..];
            i += 1;
            if !rest.is_empty() || !more {
                self.flush_obu()?;
            }
        }
        if i == 0 || (count > 0 && i != count) {
            return bad();
        }
        Ok(())
    }
}
