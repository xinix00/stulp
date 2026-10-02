//! Apparaatinstellingen volgen AttributeList en FeatureMap; publiceren gebeurt pas na een echte WriteResponse.
use crate::{
    attributes,
    im::{AttributePath, Write},
    interaction::Interaction,
    inventory::Inventory,
    tlv::{Node, Tag, Value, Writer},
};
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value as Json};
use stulp_sdk::{
    Client, Error, Result, Transport,
    util::{field, join, number},
};
/// Schrijfbare gevoeligheid van Boolean State Configuration.
pub struct Setting {
    /// Stabiele sleutel, inclusief endpoint.
    pub id: String,
    /// Endpoint van de echte cluster.
    pub endpoint: u16,
    /// Aantal geldige niveaus, exclusieve bovengrens.
    pub levels: u8,
}
impl Setting {
    /// Metadata voor de bestaande generieke instellingen-UI.
    pub fn json(&self) -> Result<Json> {
        Ok(json::fields(&[
            ("id", json::string(&self.id)?),
            ("kind", json::string("sensitivity")?),
            ("endpoint", Json::uint(u64::from(self.endpoint))),
            ("cluster", json::string("0x80")?),
            ("attribute", json::string("0x0")?),
            ("levels", Json::uint(u64::from(self.levels))),
        ])?)
    }
    /// Oude records worden streng gelezen voordat een netwerkopdracht mogelijk wordt.
    pub fn parse(value: &Json) -> Result<Self> {
        let endpoint = u16::try_from(json::uint(value, "endpoint"))
            .map_err(|_| Error::Invalid("setting endpoint exceeds uint16"))?;
        let levels = u8::try_from(json::uint(value, "levels"))
            .map_err(|_| Error::Invalid("setting levels exceeds uint8"))?;
        let expected = join(&["matter.sensitivity.", &decimal(u64::from(endpoint))?])?;
        if json::text(value, "kind") != "sensitivity"
            || json::text(value, "id") != expected
            || levels < 2
        {
            return Err(Error::Invalid("unsupported Matter setting metadata"));
        }
        Ok(Self {
            id: expected,
            endpoint,
            levels,
        })
    }
    /// Zowel HTML-selecttekst als JSON-getallen moeten een exact geheel niveau bevatten.
    pub fn value(&self, value: &Json) -> Result<u8> {
        let n = if let Some(text) = value.as_str() {
            text.trim()
                .parse::<u8>()
                .map_err(|_| Error::Invalid("invalid sensitivity level"))?
        } else {
            let n = number(value).ok_or(Error::Invalid("invalid sensitivity level"))?;
            if !(0.0..=255.0).contains(&n) || f64::from(n as u8) != n {
                return Err(Error::Invalid("sensitivity level must be a uint8"));
            }
            n as u8
        };
        if n >= self.levels {
            return Err(Error::Invalid("sensitivity level exceeds advertised range"));
        }
        Ok(n)
    }
    /// Controleert aantal, pad en status; een ontvangen antwoord is op zichzelf geen bevestiging.
    pub async fn write<T: Transport>(
        &self,
        im: &mut Interaction<'_>,
        c: &mut Client<T>,
        value: u8,
        deadline: u64,
    ) -> Result {
        if value >= self.levels {
            return Err(Error::Invalid("sensitivity level exceeds advertised range"));
        }
        let path = AttributePath::new(self.endpoint, 0x80, 0);
        let mut w = Writer::default();
        w.uint_width(Tag::Anonymous, u64::from(value), 1)?;
        let bytes = w.finish()?;
        let results = im
            .write(
                c,
                &[Write {
                    path,
                    version: None,
                    value: &bytes,
                }],
                deadline,
            )
            .await?;
        if results.len() != 1 || results[0].0 != path {
            return Err(Error::Invalid("sensitivity write response path differs"));
        }
        results[0].1.result()
    }
}
pub(crate) fn decimal(value: u64) -> Result<String> {
    json::to_string(&Json::uint(value))
        .map_err(stulp_core::Error::from)
        .map_err(Error::from)
}
/// Valideert de hele patch vóór de eerste Write, zodat invoerfouten geen halve wijziging veroorzaken.
pub fn plan(metadata: &Json, patch: &Json) -> Result<Vec<(Setting, u8)>> {
    let settings = metadata
        .as_array()
        .ok_or(Error::Invalid("missing Matter setting metadata"))?;
    let mut out = Vec::new();
    for (id, value) in patch
        .as_object()
        .ok_or(Error::Invalid("settings patch is not an object"))?
        .iter()
    {
        let record = settings
            .iter()
            .find(|s| json::text(s, "id") == id)
            .ok_or(Error::Invalid("Matter setting is not supported"))?;
        let setting = Setting::parse(record)?;
        let value = setting.value(value)?;
        json::push(&mut out, (setting, value), 256)?;
    }
    out.sort_unstable_by(|a, b| a.0.id.cmp(&b.0.id));
    Ok(out)
}
/// Leest alleen aantoonbaar ondersteunde gevoeligheid; hardwarefouten blijven in de inventaris zichtbaar.
pub async fn inspect<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    inventory: &mut Inventory,
    deadline: u64,
) -> Result<(Json, Json)> {
    let mut settings = json::object();
    let mut metadata = Vec::new();
    let Some(cluster) = inventory.clusters.iter_mut().find(|v| v.id == 0x80) else {
        return Ok((settings, Json::Array(metadata)));
    };
    if cluster.has(0) != Some(true)
        || cluster.has(1) != Some(true)
        || cluster.features.is_some_and(|v| v & 8 == 0)
    {
        return Ok((settings, Json::Array(metadata)));
    }
    let result = async {
        let current = attributes::required(
            im,
            c,
            AttributePath::new(inventory.endpoint, 0x80, 0),
            deadline,
        )
        .await?;
        let levels = attributes::required(
            im,
            c,
            AttributePath::new(inventory.endpoint, 0x80, 1),
            deadline,
        )
        .await?;
        let (Value::Uint(current), Value::Uint(levels)) = (
            Node::parse(&current)?.element.value,
            Node::parse(&levels)?.element.value,
        ) else {
            return Err(Error::Invalid("sensitivity attributes are not unsigned"));
        };
        if !(2..=255).contains(&levels) || current >= levels {
            return Err(Error::Invalid("sensitivity attributes have invalid values"));
        }
        Ok((
            Setting {
                id: join(&[
                    "matter.sensitivity.",
                    &decimal(u64::from(inventory.endpoint))?,
                ])?,
                endpoint: inventory.endpoint,
                levels: levels as u8,
            },
            current,
        ))
    }
    .await;
    match result {
        Ok((setting, value)) => {
            json::set(&mut settings, &setting.id, Json::uint(value))?;
            json::push(&mut metadata, setting.json()?, 256)?;
        }
        Err(e) => cluster.error(e)?,
    }
    Ok((settings, Json::Array(metadata)))
}
/// Lees de metadata zonder onbekende storevelden te kopiëren.
pub fn metadata(device: &Json) -> &Json {
    field(field(device, "store"), "matter.settings")
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn invalid_patch_fails_before_writes_and_string_levels_are_exact() -> Result {
        let setting = Setting {
            id: json::copy("matter.sensitivity.7")?,
            endpoint: 7,
            levels: 3,
        };
        assert_eq!(setting.value(&json::string(" 2 ")?)?, 2);
        for bad in [
            Json::int(-1),
            Json::uint(3),
            stulp_sdk::util::float(1.5)?,
            json::string("1.5")?,
        ] {
            assert!(setting.value(&bad).is_err());
        }
        let metadata = Json::Array(alloc::vec![setting.json()?]);
        assert!(
            plan(
                &metadata,
                &json::fields(&[
                    ("matter.sensitivity.7", Json::uint(1)),
                    ("unknown", Json::uint(2))
                ])?
            )
            .is_err()
        );
        Ok(())
    }
}
