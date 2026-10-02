//! Kleine XML-boom voor UPnP: 1 MiB, 8192 elementen, maximaal 64 lagen.
use crate::{Error, Result};
use alloc::{string::String, vec::Vec};
use stulp_core::json;
/// Eén element. Namen bewaren hun prefix; selecteren gebeurt op lokale naam.
pub struct Node {
    name: String,
    text: String,
    parent: Option<usize>,
}
/// Eén document bezit alle elementen; geen recursieve allocaties of Drop.
pub struct Document {
    nodes: Vec<Node>,
}
impl Document {
    /// UTF-8 XML; DTD's en onbekende entiteiten worden geweigerd.
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.len() > 1 << 20 {
            return Err(Error::Invalid("XML exceeds 1 MiB"));
        }
        let mut rest = core::str::from_utf8(bytes)
            .map_err(|_| Error::Invalid("XML is not UTF-8"))?
            .trim_start_matches('\u{feff}');
        let mut nodes: Vec<Node> = Vec::new();
        let mut stack: Vec<usize> = Vec::new();
        let mut roots = 0;
        while !rest.is_empty() {
            if let Some(tail) = rest.strip_prefix("<!--") {
                let (comment, tail) = tail
                    .split_once("-->")
                    .ok_or(Error::Invalid("unclosed XML comment"))?;
                if comment.contains("--") {
                    return Err(Error::Invalid("invalid XML comment"));
                }
                rest = tail;
            } else if let Some(tail) = rest.strip_prefix("<?") {
                rest = tail
                    .split_once("?>")
                    .ok_or(Error::Invalid("unclosed XML instruction"))?
                    .1;
            } else if let Some(tail) = rest.strip_prefix("<![CDATA[") {
                let (data, tail) = tail
                    .split_once("]]>")
                    .ok_or(Error::Invalid("unclosed XML CDATA"))?;
                append(&mut nodes, stack.last().copied(), data, false)?;
                rest = tail;
            } else if rest.starts_with("<!") {
                return Err(Error::Invalid("XML declarations are unsupported"));
            } else if let Some(tail) = rest.strip_prefix("</") {
                let (name, tail) = tail
                    .split_once('>')
                    .ok_or(Error::Invalid("unclosed XML end tag"))?;
                let i = stack
                    .pop()
                    .ok_or(Error::Invalid("unexpected XML end tag"))?;
                if nodes[i].name != name.trim_end() {
                    return Err(Error::Invalid("mismatched XML end tag"));
                }
                rest = tail;
            } else if let Some(tail) = rest.strip_prefix('<') {
                let end = tag_end(tail)?;
                let tag = &tail[..end];
                let empty = tag.ends_with('/');
                let tag = if empty { &tag[..tag.len() - 1] } else { tag };
                let name_end = tag.find(char::is_whitespace).unwrap_or(tag.len());
                let name = &tag[..name_end];
                if !valid_name(name) {
                    return Err(Error::Invalid("invalid XML name"));
                }
                attributes(&tag[name_end..])?;
                let parent = stack.last().copied();
                if parent.is_none() {
                    roots += 1;
                    if roots > 1 {
                        return Err(Error::Invalid("multiple XML roots"));
                    }
                }
                let i = nodes.len();
                json::push(
                    &mut nodes,
                    Node {
                        name: json::copy(name)?,
                        text: String::new(),
                        parent,
                    },
                    8192,
                )?;
                if !empty {
                    json::push(&mut stack, i, 64)?;
                }
                rest = &tail[end + 1..];
            } else {
                let end = rest.find('<').unwrap_or(rest.len());
                let data = &rest[..end];
                if data.contains("]]>") {
                    return Err(Error::Invalid("CDATA terminator in XML text"));
                }
                append(&mut nodes, stack.last().copied(), data, true)?;
                rest = &rest[end..];
            }
        }
        if roots != 1 || !stack.is_empty() {
            return Err(Error::Invalid("incomplete XML document"));
        }
        Ok(Self { nodes })
    }
    /// Eerste element met deze lokale naam.
    pub fn find(&self, name: &str) -> Option<usize> {
        self.nodes.iter().position(|n| local(&n.name) == name)
    }
    /// Directe kinderen met deze lokale naam.
    pub fn children<'a>(
        &'a self,
        parent: usize,
        name: &'a str,
    ) -> impl Iterator<Item = usize> + 'a {
        self.nodes
            .iter()
            .enumerate()
            .filter(move |(_, n)| n.parent == Some(parent) && local(&n.name) == name)
            .map(|(i, _)| i)
    }
    /// Direct kind, niet een gelijknamig veld uit een genest apparaat.
    pub fn child(&self, parent: usize, name: &str) -> Option<usize> {
        self.children(parent, name).next()
    }
    /// Tekst van het element, met XML-entiteiten al één keer opgelost.
    pub fn text(&self, index: usize) -> &str {
        self.nodes.get(index).map_or("", |n| n.text.trim())
    }
    /// Tekst van een direct kind; afwezig is leeg.
    pub fn field(&self, parent: usize, name: &str) -> &str {
        self.child(parent, name).map_or("", |i| self.text(i))
    }
}
fn local(name: &str) -> &str {
    name.rsplit(':').next().unwrap_or(name)
}
fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name
            .chars()
            .next()
            .is_some_and(|c| c.is_alphabetic() || c == '_' || c == ':')
        && name
            .chars()
            .all(|c| c.is_alphanumeric() || matches!(c, '_' | ':' | '-' | '.'))
}
fn tag_end(s: &str) -> Result<usize> {
    let mut quote = None;
    for (i, c) in s.char_indices() {
        if let Some(q) = quote {
            if c == q {
                quote = None;
            }
        } else if matches!(c, '\'' | '"') {
            quote = Some(c);
        } else if c == '>' {
            return Ok(i);
        } else if c == '<' {
            break;
        }
    }
    Err(Error::Invalid("unclosed XML start tag"))
}
fn attributes(mut s: &str) -> Result {
    let mut names = Vec::new();
    loop {
        if s.is_empty() {
            return Ok(());
        }
        if !s.starts_with(char::is_whitespace) {
            return Err(Error::Invalid("XML attributes need whitespace"));
        }
        s = s.trim_start();
        if s.is_empty() {
            return Ok(());
        }
        let end = s
            .find(|c: char| c == '=' || c.is_whitespace())
            .ok_or(Error::Invalid("XML attribute needs a value"))?;
        let name = &s[..end];
        if !valid_name(name) || names.contains(&name) {
            return Err(Error::Invalid("invalid or duplicate XML attribute"));
        }
        json::push(&mut names, name, 128)?;
        s = s[end..]
            .trim_start()
            .strip_prefix('=')
            .ok_or(Error::Invalid("XML attribute needs equals"))?
            .trim_start();
        let quote = s
            .chars()
            .next()
            .filter(|c| matches!(c, '\'' | '"'))
            .ok_or(Error::Invalid("XML attribute needs quotes"))?;
        let (value, tail) = s[1..]
            .split_once(quote)
            .ok_or(Error::Invalid("unclosed XML attribute"))?;
        if value.contains('<') {
            return Err(Error::Invalid("invalid XML attribute"));
        }
        decode(value)?;
        s = tail;
    }
}
fn valid_char(c: char) -> bool {
    matches!(c, '\t' | '\n' | '\r')
        || ('\u{20}'..='\u{d7ff}').contains(&c)
        || ('\u{e000}'..='\u{fffd}').contains(&c)
        || c >= '\u{10000}'
}
fn append(nodes: &mut [Node], index: Option<usize>, data: &str, escaped: bool) -> Result {
    let Some(i) = index else {
        return if data.trim().is_empty() {
            Ok(())
        } else {
            Err(Error::Invalid("text outside XML root"))
        };
    };
    if !data.chars().all(valid_char) {
        return Err(Error::Invalid("invalid XML character"));
    }
    let text = if escaped {
        decode(data)?
    } else {
        json::copy(data)?
    };
    nodes[i]
        .text
        .try_reserve(text.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    nodes[i].text.push_str(&text);
    Ok(())
}
/// Decodeert standaard- en numerieke entiteiten; geen DTD of dubbele decodering.
pub fn decode(mut s: &str) -> Result<String> {
    let mut out = String::new();
    out.try_reserve(s.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    while let Some(i) = s.find('&') {
        out.push_str(&s[..i]);
        let (name, tail) = s[i + 1..]
            .split_once(';')
            .ok_or(Error::Invalid("unclosed XML entity"))?;
        let c = match name {
            "amp" => '&',
            "lt" => '<',
            "gt" => '>',
            "quot" => '"',
            "apos" => '\'',
            _ => {
                let number = if let Some(n) = name.strip_prefix("#x") {
                    u32::from_str_radix(n, 16).ok()
                } else if let Some(n) = name.strip_prefix('#') {
                    n.parse::<u32>().ok()
                } else {
                    None
                };
                number
                    .and_then(char::from_u32)
                    .filter(|c| valid_char(*c))
                    .ok_or(Error::Invalid("unknown or invalid XML entity"))?
            }
        };
        out.push(c);
        s = tail;
    }
    out.push_str(s);
    Ok(out)
}
