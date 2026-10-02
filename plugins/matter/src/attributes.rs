//! Attribuutreads behouden unsupported, null en lijstfragmenten als verschillende uitkomsten.
use crate::{
    im::{Attribute, AttributePath},
    interaction::{Interaction, Reports},
    tlv::{Node, Tag, Value, Writer},
};
use alloc::vec::Vec;
use stulp_core::json;
use stulp_sdk::{Client, Error, Result, Transport};
/// Accumulator leent wiredata totdat één volledige attribuutwaarde is samengesteld.
#[derive(Default)]
pub struct Accumulator<'a> {
    value: Option<Node<'a>>,
    version: Option<u32>,
    unsupported: bool,
}
impl<'a> Accumulator<'a> {
    /// De aanroeper routeert uitsluitend reports voor hetzelfde concrete attribuut hierheen.
    pub fn apply(&mut self, report: Attribute<'a>) -> Result {
        if let Some(status) = report.status {
            if status.global == 0x86 && self.value.is_none() {
                self.unsupported = true;
                return Ok(());
            }
            status.result()?;
            return Err(Error::Invalid("attribute status contains no value"));
        }
        if self.unsupported {
            return Err(Error::Invalid("attribute is both unsupported and present"));
        }
        let node = report
            .value
            .ok_or(Error::Invalid("attribute has no data"))?;
        if report.path.list_index.is_none() {
            self.version = report.version;
            self.value = Some(node);
            return Ok(());
        }
        if self.version != report.version {
            return Err(Error::Invalid("attribute version changed within list"));
        }
        let list = self
            .value
            .as_mut()
            .ok_or(Error::Invalid("list fragment lacks initial array"))?;
        if list.element.value != Value::Array {
            return Err(Error::Invalid("list fragment targets a scalar"));
        }
        match report.path.list_index {
            Some(None) => json::push(&mut list.children, node, 4096)?,
            Some(Some(index)) => {
                let index = usize::from(index);
                if index >= list.children.len() {
                    return Err(Error::Invalid("list index outside attribute"));
                }
                if node.element.value == Value::Null {
                    list.children.remove(index);
                } else {
                    list.children[index] = node;
                }
            }
            None => (),
        }
        Ok(())
    }
    /// Encodeert de complete waarde; null blijft expliciet aanwezig.
    pub fn finish(self, optional: bool) -> Result<Option<Vec<u8>>> {
        if self.unsupported && optional {
            return Ok(None);
        }
        if self.unsupported {
            return Err(Error::Invalid("required Matter attribute unsupported"));
        }
        let mut node = self
            .value
            .ok_or(Error::Invalid("Matter attribute missing from response"))?;
        if node.element.value == Value::Array {
            for child in &mut node.children {
                child.element.tag = Tag::Anonymous;
            }
        }
        let mut w = Writer::default();
        w.node(&node, Tag::Anonymous)?;
        Ok(Some(w.finish()?))
    }
}
impl Reports {
    /// Negeert geen lijstindices en kiest nooit een antwoord voor een ander endpoint.
    pub fn attribute(&self, path: AttributePath, optional: bool) -> Result<Option<Vec<u8>>> {
        let mut collected = Accumulator::default();
        for report in self.iter() {
            for item in report?.attributes {
                if item.path.endpoint == path.endpoint
                    && item.path.cluster == path.cluster
                    && item.path.attribute == path.attribute
                {
                    collected.apply(item)?;
                }
            }
        }
        collected.finish(optional)
    }
}
/// Eén read met gecontroleerd antwoordpad en nullable optionaliteit.
pub async fn read<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    path: AttributePath,
    optional: bool,
    deadline: u64,
) -> Result<Option<Vec<u8>>> {
    im.read(c, &[path], true, deadline)
        .await?
        .attribute(path, optional)
}
/// Een verplichte read maakt ontbrekend nooit tot een lege TLV-buffer.
pub async fn required<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    path: AttributePath,
    deadline: u64,
) -> Result<Vec<u8>> {
    read(im, c, path, false, deadline)
        .await?
        .ok_or(Error::Invalid("required attribute absent"))
}
/// Numerieke descriptorlijsten blijven begrensd en verliezen geen geldige 32-bit IDs.
pub fn ids(node: &Node<'_>, device_types: bool) -> Result<Vec<u32>> {
    if node.element.value != Value::Array {
        return Err(Error::Invalid("Matter descriptor is not an array"));
    }
    let mut out = Vec::new();
    for item in &node.children {
        let value = if device_types {
            item.get(0).map(|n| n.element.value)
        } else {
            Some(item.element.value)
        };
        if let Some(Value::Uint(n)) = value
            && let Ok(n) = u32::try_from(n)
        {
            json::push(&mut out, n, 4096)?;
        }
    }
    Ok(out)
}
#[cfg(test)]
mod tests {
    use super::*;
    fn report<'a>(node: Node<'a>, index: Option<Option<u16>>, version: u32) -> Attribute<'a> {
        let mut path = AttributePath::new(1, 0x1d, 1);
        path.list_index = index;
        Attribute {
            path,
            version: Some(version),
            value: Some(node),
            status: None,
        }
    }
    #[test]
    fn fragmented_lists_keep_every_item_and_reject_missing_or_changed_base() -> Result {
        let mut a = Accumulator::default();
        a.apply(report(Node::parse(&[0x16, 4, 6, 0x18])?, None, 9))?;
        a.apply(report(Node::parse(&[4, 8])?, Some(None), 9))?;
        let bytes = a.finish(false)?.ok_or(Error::Invalid("absent test data"))?;
        assert_eq!(ids(&Node::parse(&bytes)?, false)?, [6, 8]);
        assert!(
            Accumulator::default()
                .apply(report(Node::parse(&[4, 8])?, Some(None), 9))
                .is_err()
        );
        let mut a = Accumulator::default();
        a.apply(report(Node::parse(&[0x16, 0x18])?, None, 9))?;
        assert!(
            a.apply(report(Node::parse(&[4, 8])?, Some(None), 10))
                .is_err()
        );
        Ok(())
    }
}
