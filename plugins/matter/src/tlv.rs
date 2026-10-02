//! Matter TLV behoudt tagsoorten, integerbreedtes en geleende gegevens; nesting stopt op 32.
use crate::append;
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
/// Maximale diepte, gelijk aan de oorspronkelijke decoder.
pub const MAX_DEPTH: usize = 32;
/// Alle acht wirevormen hebben vijf semantische tagsoorten.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tag {
    /// Geen tag.
    Anonymous,
    /// Contextnummer van één byte.
    Context(u8),
    /// Common-profile tag.
    Common(u32),
    /// Impliciet profiel, vastgesteld door het protocol.
    Implicit(u32),
    /// Vendor, profiel en tagnummer.
    Full(u16, u16, u32),
}
/// Eén geleend element; containers verschijnen vóór hun kinderen.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Value<'a> {
    /// Signed integer met tekenuitbreiding.
    Int(i64),
    /// Unsigned integer zonder precisieverlies.
    Uint(u64),
    /// Booleaanse wirewaarde.
    Bool(bool),
    /// IEEE floating point.
    Float(f64),
    /// Gevalideerde UTF-8.
    String(&'a str),
    /// Geleende octetten.
    Bytes(&'a [u8]),
    /// Expliciete nullwaarde.
    Null,
    /// Getagde velden.
    Structure,
    /// Array-elementen.
    Array,
    /// List-elementen.
    List,
    /// Einde van een container.
    End,
}
/// Tag en inhoud van één wire-element.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Element<'a> {
    /// Identiteit binnen de container.
    pub tag: Tag,
    /// Geleende inhoud.
    pub value: Value<'a>,
}
/// Begrensde schrijver; iedere bewerking meldt allocatie- en invoerfouten.
#[derive(Default)]
pub struct Writer {
    bytes: Vec<u8>,
    depth: usize,
}
fn width(n: u64) -> u8 {
    if n <= 255 {
        0
    } else if n <= 65535 {
        1
    } else if n <= u64::from(u32::MAX) {
        2
    } else {
        3
    }
}
impl Writer {
    fn control(&mut self, tag: Tag, kind: u8) -> Result {
        match tag {
            Tag::Anonymous => append(&mut self.bytes, &[kind]),
            Tag::Context(n) => append(&mut self.bytes, &[0x20 | kind, n]),
            Tag::Common(n) | Tag::Implicit(n) => {
                let flag = if matches!(tag, Tag::Common(_)) {
                    0x40
                } else {
                    0x80
                };
                let long = n > 65535;
                append(
                    &mut self.bytes,
                    &[flag | if long { 0x20 } else { 0 } | kind],
                )?;
                append(
                    &mut self.bytes,
                    &n.to_le_bytes()[..if long { 4 } else { 2 }],
                )
            }
            Tag::Full(v, p, n) => {
                let long = n > 65535;
                append(&mut self.bytes, &[if long { 0xe0 } else { 0xc0 } | kind])?;
                append(&mut self.bytes, &v.to_le_bytes())?;
                append(&mut self.bytes, &p.to_le_bytes())?;
                append(
                    &mut self.bytes,
                    &n.to_le_bytes()[..if long { 4 } else { 2 }],
                )
            }
        }
    }
    /// Schrijft een unsigned getal in de kleinste passende breedte.
    pub fn uint(&mut self, tag: Tag, n: u64) -> Result {
        self.uint_width(tag, n, 1 << width(n))
    }
    /// Behoudt de ASN.1-veldbreedte voor certificaten.
    pub fn uint_width(&mut self, tag: Tag, n: u64, bytes: usize) -> Result {
        let w = match bytes {
            1 => 0,
            2 => 1,
            4 => 2,
            8 => 3,
            _ => return Err(Error::Invalid("invalid TLV integer width")),
        };
        if bytes < 8 && n >= (1u64 << (bytes * 8)) {
            return Err(Error::Invalid("TLV integer does not fit width"));
        }
        self.control(tag, 4 + w)?;
        append(&mut self.bytes, &n.to_le_bytes()[..bytes])
    }
    /// Schrijft een signed getal in de kleinste passende breedte.
    pub fn int(&mut self, tag: Tag, n: i64) -> Result {
        let w = if i8::try_from(n).is_ok() {
            0
        } else if i16::try_from(n).is_ok() {
            1
        } else if i32::try_from(n).is_ok() {
            2
        } else {
            3
        };
        self.control(tag, w)?;
        append(&mut self.bytes, &n.to_le_bytes()[..1 << w])
    }
    /// Een boolean draagt zijn waarde in de controlbyte.
    pub fn boolean(&mut self, tag: Tag, n: bool) -> Result {
        self.control(tag, if n { 9 } else { 8 })
    }
    /// Een float van vier bytes.
    pub fn float32(&mut self, tag: Tag, n: f32) -> Result {
        self.control(tag, 10)?;
        append(&mut self.bytes, &n.to_le_bytes())
    }
    /// Een float van acht bytes.
    pub fn float64(&mut self, tag: Tag, n: f64) -> Result {
        self.control(tag, 11)?;
        append(&mut self.bytes, &n.to_le_bytes())
    }
    fn data(&mut self, tag: Tag, kind: u8, bytes: &[u8]) -> Result {
        let w = width(bytes.len() as u64);
        self.control(tag, kind + w)?;
        append(
            &mut self.bytes,
            &(bytes.len() as u64).to_le_bytes()[..1 << w],
        )?;
        append(&mut self.bytes, bytes)
    }
    /// UTF-8 met een lengteprefix.
    pub fn string(&mut self, tag: Tag, s: &str) -> Result {
        self.data(tag, 12, s.as_bytes())
    }
    /// Octetten met een lengteprefix.
    pub fn bytes(&mut self, tag: Tag, b: &[u8]) -> Result {
        self.data(tag, 16, b)
    }
    /// Een expliciete nullwaarde.
    pub fn null(&mut self, tag: Tag) -> Result {
        self.control(tag, 20)
    }
    /// Open een structure, array of list.
    pub fn start(&mut self, tag: Tag, kind: Value<'_>) -> Result {
        let code = match kind {
            Value::Structure => 21,
            Value::Array => 22,
            Value::List => 23,
            _ => return Err(Error::Invalid("not a TLV container")),
        };
        if self.depth >= MAX_DEPTH {
            return Err(Error::Invalid("TLV nesting exceeds 32"));
        }
        self.control(tag, code)?;
        self.depth += 1;
        Ok(())
    }
    /// Sluit precies één open container.
    pub fn end(&mut self) -> Result {
        if self.depth == 0 {
            return Err(Error::Invalid("TLV end without container"));
        }
        append(&mut self.bytes, &[24])?;
        self.depth -= 1;
        Ok(())
    }
    /// Geeft alleen een volledig afgesloten wirebericht vrij.
    pub fn finish(self) -> Result<Vec<u8>> {
        if self.depth != 0 {
            return Err(Error::Invalid("TLV has unterminated containers"));
        }
        Ok(self.bytes)
    }
}
/// Streaming reader zonder allocatie; lengteprefixen worden vóór toegang getoetst.
pub struct Reader<'a> {
    data: &'a [u8],
    at: usize,
    depth: usize,
}
impl<'a> Reader<'a> {
    /// Leent één volledig TLV-bericht.
    pub fn new(data: &'a [u8]) -> Result<Self> {
        if data.len() > crate::MAX {
            return Err(Error::Invalid("TLV input exceeds 65535 bytes"));
        }
        Ok(Self {
            data,
            at: 0,
            depth: 0,
        })
    }
    fn take(&mut self, n: usize) -> Result<&'a [u8]> {
        let end = self
            .at
            .checked_add(n)
            .ok_or(Error::Invalid("TLV length overflow"))?;
        let b = self
            .data
            .get(self.at..end)
            .ok_or(Error::Invalid("truncated TLV element"))?;
        self.at = end;
        Ok(b)
    }
    fn le(&mut self, n: usize) -> Result<u64> {
        let mut b = [0; 8];
        b.get_mut(..n)
            .ok_or(Error::Invalid("TLV width"))?
            .copy_from_slice(self.take(n)?);
        Ok(u64::from_le_bytes(b))
    }
    /// Volgende element; EOF is alleen geldig buiten alle containers.
    #[expect(clippy::should_implement_trait)]
    pub fn next(&mut self) -> Result<Option<Element<'a>>> {
        if self.at == self.data.len() {
            if self.depth != 0 {
                return Err(Error::Invalid("unterminated TLV container"));
            }
            return Ok(None);
        }
        let control = self.le(1)? as u8;
        let kind = control & 31;
        let tag = match control >> 5 {
            0 => Tag::Anonymous,
            1 => Tag::Context(self.le(1)? as u8),
            2 => Tag::Common(self.le(2)? as u32),
            3 => Tag::Common(self.le(4)? as u32),
            4 => Tag::Implicit(self.le(2)? as u32),
            5 => Tag::Implicit(self.le(4)? as u32),
            6 => Tag::Full(self.le(2)? as u16, self.le(2)? as u16, self.le(2)? as u32),
            _ => Tag::Full(self.le(2)? as u16, self.le(2)? as u16, self.le(4)? as u32),
        };
        let value = match kind {
            0..=3 => {
                let w = 1usize << kind;
                let v = self.le(w)?;
                Value::Int(match w {
                    1 => i64::from(v as i8),
                    2 => i64::from(v as i16),
                    4 => i64::from(v as i32),
                    _ => v as i64,
                })
            }
            4..=7 => Value::Uint(self.le(1 << (kind - 4))?),
            8 => Value::Bool(false),
            9 => Value::Bool(true),
            10 => Value::Float(f64::from(f32::from_bits(self.le(4)? as u32))),
            11 => Value::Float(f64::from_bits(self.le(8)?)),
            12..=19 => {
                let n = usize::try_from(self.le(1 << (kind & 3))?)
                    .map_err(|_| Error::Invalid("TLV length overflow"))?;
                let bytes = self.take(n)?;
                if kind < 16 {
                    Value::String(
                        core::str::from_utf8(bytes)
                            .map_err(|_| Error::Invalid("TLV string is not UTF-8"))?,
                    )
                } else {
                    Value::Bytes(bytes)
                }
            }
            20 => Value::Null,
            21..=23 => {
                if self.depth >= MAX_DEPTH {
                    return Err(Error::Invalid("TLV nesting exceeds 32"));
                }
                self.depth += 1;
                match kind {
                    21 => Value::Structure,
                    22 => Value::Array,
                    _ => Value::List,
                }
            }
            24 => {
                if self.depth == 0 || tag != Tag::Anonymous {
                    return Err(Error::Invalid("invalid TLV container end"));
                }
                self.depth -= 1;
                Value::End
            }
            _ => return Err(Error::Invalid("reserved TLV type")),
        };
        Ok(Some(Element { tag, value }))
    }
}
/// Geleende boom voor kleine IM- en certificaatberichten; diepte en aantal zijn begrensd.
pub struct Node<'a> {
    /// Dit element zelf.
    pub element: Element<'a>,
    /// Kinderen van een container, anders leeg.
    pub children: Vec<Node<'a>>,
}
impl<'a> Node<'a> {
    /// Precies één root; extra bytes of duplicaten in structures worden geweigerd.
    pub fn parse(bytes: &'a [u8]) -> Result<Self> {
        let mut r = Reader::new(bytes)?;
        let e = r.next()?.ok_or(Error::Invalid("empty TLV"))?;
        let mut count = 0;
        let n = Self::read(&mut r, e, &mut count)?;
        if r.next()?.is_some() {
            return Err(Error::Invalid("multiple TLV roots"));
        }
        Ok(n)
    }
    fn read(r: &mut Reader<'a>, element: Element<'a>, count: &mut usize) -> Result<Self> {
        *count += 1;
        if *count > 4096 {
            return Err(Error::Invalid("too many TLV elements"));
        }
        let mut n = Self {
            element,
            children: Vec::new(),
        };
        if matches!(element.value, Value::Structure | Value::Array | Value::List) {
            loop {
                let e = r.next()?.ok_or(Error::Invalid("unterminated TLV tree"))?;
                if e.value == Value::End {
                    break;
                }
                if element.value == Value::Structure
                    && n.children.iter().any(|n| n.element.tag == e.tag)
                {
                    return Err(Error::Invalid("duplicate TLV structure tag"));
                }
                let child = Self::read(r, e, count)?;
                stulp_core::json::push(&mut n.children, child, 4096)?;
            }
        }
        Ok(n)
    }
    /// Zoekt een contextveld zonder waarden van een ander profiel te verwarren.
    pub fn get(&self, tag: u8) -> Option<&Node<'a>> {
        self.children
            .iter()
            .find(|n| n.element.tag == Tag::Context(tag))
    }
    /// Vereist een unsigned contextveld.
    pub fn uint(&self, tag: u8) -> Result<u64> {
        match self.get(tag).map(|n| n.element.value) {
            Some(Value::Uint(n)) => Ok(n),
            _ => Err(Error::Invalid("missing unsigned TLV field")),
        }
    }
    /// Vereist een octetstring.
    pub fn bytes(&self, tag: u8) -> Result<&'a [u8]> {
        match self.get(tag).map(|n| n.element.value) {
            Some(Value::Bytes(n)) => Ok(n),
            _ => Err(Error::Invalid("missing TLV bytes field")),
        }
    }
}

impl Writer {
    /// Schrijft een geleende boom met een nieuwe roottag, zonder de payload te klonen.
    pub fn node(&mut self, node: &Node<'_>, tag: Tag) -> Result {
        match node.element.value {
            Value::Int(n) => self.int(tag, n),
            Value::Uint(n) => self.uint(tag, n),
            Value::Bool(n) => self.boolean(tag, n),
            Value::Float(n) => self.float64(tag, n),
            Value::String(s) => self.string(tag, s),
            Value::Bytes(b) => self.bytes(tag, b),
            Value::Null => self.null(tag),
            Value::Structure | Value::Array | Value::List => {
                self.start(tag, node.element.value)?;
                for child in &node.children {
                    self.node(child, child.element.tag)?;
                }
                self.end()
            }
            Value::End => Err(Error::Invalid("cannot write standalone TLV end")),
        }
    }
}
impl<'a> Node<'a> {
    /// Neemt het eigendom van één contextveld over, zonder bytebuffers te klonen.
    pub fn take(&mut self, tag: u8) -> Option<Node<'a>> {
        let at = self
            .children
            .iter()
            .position(|n| n.element.tag == Tag::Context(tag))?;
        Some(self.children.remove(at))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn all_tag_forms_and_signed_widths_roundtrip() -> Result {
        let tags = [
            Tag::Anonymous,
            Tag::Context(255),
            Tag::Common(0xffff),
            Tag::Common(0x10000),
            Tag::Implicit(1),
            Tag::Implicit(0x20000),
            Tag::Full(0xfff1, 0, 1),
            Tag::Full(1, 2, 0x30000),
        ];
        let mut w = Writer::default();
        w.start(Tag::Anonymous, Value::List)?;
        for (i, t) in tags.iter().enumerate() {
            w.uint(*t, i as u64)?;
        }
        w.end()?;
        let bytes = w.finish()?;
        let mut r = Reader::new(&bytes)?;
        r.next()?;
        for (i, tag) in tags.iter().enumerate() {
            assert_eq!(
                r.next()?,
                Some(Element {
                    tag: *tag,
                    value: Value::Uint(i as u64)
                })
            );
        }
        assert!(matches!(
            r.next()?,
            Some(Element {
                value: Value::End,
                ..
            })
        ));
        assert!(r.next()?.is_none());
        for n in [
            i64::MIN,
            i64::MAX,
            -1,
            0,
            127,
            128,
            -128,
            -129,
            32767,
            32768,
            -32769,
            i64::from(i32::MAX) + 1,
        ] {
            let mut w = Writer::default();
            w.int(Tag::Anonymous, n)?;
            let b = w.finish()?;
            assert_eq!(
                Reader::new(&b)?.next()?,
                Some(Element {
                    tag: Tag::Anonymous,
                    value: Value::Int(n)
                })
            );
        }
        Ok(())
    }
    #[test]
    fn hostile_lengths_depth_and_duplicate_fields_fail() -> Result {
        for b in [
            &[0x10, 255][..],
            &[0x18][..],
            &[0x1f][..],
            &[0x0c, 1, 255][..],
            &[0x15, 0x38, 1][..],
        ] {
            let mut r = Reader::new(b)?;
            let mut failed = false;
            for _ in 0..4 {
                match r.next() {
                    Err(_) => {
                        failed = true;
                        break;
                    }
                    Ok(None) => break,
                    _ => (),
                }
            }
            assert!(failed);
        }
        let mut nested = [21; 33];
        let mut r = Reader::new(&nested)?;
        for _ in 0..32 {
            r.next()?;
        }
        assert!(r.next().is_err());
        nested.fill(0);
        assert!(Node::parse(&[21, 0x24, 1, 1, 0x24, 1, 2, 24]).is_err());
        assert!(Node::parse(&[21]).is_err());
        assert!(Node::parse(&[4, 1, 4, 2]).is_err());
        Ok(())
    }
}
