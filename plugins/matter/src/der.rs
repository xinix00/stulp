//! Kleine DER-codec voor de operationele Matter-certificaten, geen algemene PKI-parser.
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
pub(crate) const LIMIT: usize = 4096;
pub(crate) fn object(tag: u8, parts: &[&[u8]]) -> Result<Vec<u8>> {
    let len = parts
        .iter()
        .try_fold(0usize, |n, p| n.checked_add(p.len()))
        .filter(|n| *n <= LIMIT)
        .ok_or(Error::Invalid("DER size limit"))?;
    let mut out = Vec::new();
    out.try_reserve(len + 4)
        .map_err(|_| stulp_core::Error::Memory)?;
    out.push(tag);
    if len < 128 {
        out.push(len as u8);
    } else if len < 256 {
        out.extend_from_slice(&[0x81, len as u8]);
    } else {
        out.push(0x82);
        out.extend_from_slice(&(len as u16).to_be_bytes());
    }
    for part in parts {
        out.extend_from_slice(part);
    }
    Ok(out)
}
pub(crate) fn integer(bytes: &[u8]) -> Result<Vec<u8>> {
    let first = bytes.iter().position(|b| *b != 0);
    match first {
        None => object(2, &[&[0]]),
        Some(i) if bytes[i] & 128 != 0 => object(2, &[&[0], &bytes[i..]]),
        Some(i) => object(2, &[&bytes[i..]]),
    }
}
#[derive(Clone, Copy)]
pub(crate) struct Element<'a> {
    pub tag: u8,
    pub value: &'a [u8],
    pub wire: &'a [u8],
}
pub(crate) struct Reader<'a>(pub &'a [u8]);
impl<'a> Reader<'a> {
    pub(crate) fn next(&mut self) -> Result<Element<'a>> {
        if self.0.len() < 2 || self.0.len() > LIMIT {
            return Err(Error::Invalid("DER input length"));
        }
        let tag = self.0[0];
        if tag & 31 == 31 {
            return Err(Error::Invalid("DER high tag unsupported"));
        }
        let (head, len) = match self.0[1] {
            n @ 0..=127 => (2, usize::from(n)),
            0x81 if self.0.len() >= 3 && self.0[2] >= 128 => (3, usize::from(self.0[2])),
            0x82 if self.0.len() >= 4 && self.0[2] != 0 => {
                (4, usize::from(u16::from_be_bytes([self.0[2], self.0[3]])))
            }
            _ => return Err(Error::Invalid("DER indefinite or noncanonical length")),
        };
        let end = head + len;
        let wire = self.0.get(..end).ok_or(Error::Invalid("truncated DER"))?;
        self.0 = &self.0[end..];
        Ok(Element {
            tag,
            value: &wire[head..],
            wire,
        })
    }
    pub(crate) fn take(&mut self, tag: u8) -> Result<Element<'a>> {
        let e = self.next()?;
        if e.tag != tag {
            return Err(Error::Invalid("unexpected DER tag"));
        }
        Ok(e)
    }
    pub(crate) fn end(&self) -> Result {
        if !self.0.is_empty() {
            return Err(Error::Invalid("trailing DER fields"));
        }
        Ok(())
    }
}
pub(crate) fn only(bytes: &[u8], tag: u8) -> Result<Element<'_>> {
    let mut r = Reader(bytes);
    let e = r.take(tag)?;
    r.end()?;
    Ok(e)
}
pub(crate) fn unsigned(e: Element<'_>) -> Result<&[u8]> {
    if e.tag != 2
        || e.value.is_empty()
        || e.value[0] & 128 != 0
        || (e.value.len() > 1 && e.value[0] == 0 && e.value[1] & 128 == 0)
    {
        return Err(Error::Invalid("DER integer not canonical unsigned"));
    }
    Ok(if e.value.len() > 1 && e.value[0] == 0 {
        &e.value[1..]
    } else {
        e.value
    })
}
