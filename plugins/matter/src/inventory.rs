//! De clusterinventaris bewaart expliciete apparaatmetadata en leidt dekking af uit het register.
use crate::{
    attributes, capabilities,
    im::AttributePath,
    interaction::Interaction,
    tlv::{Node, Value},
};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::json::{self, Value as Json};
use stulp_sdk::{Client, Error, Result, Transport};
/// Globale metadata-attributen, in dezelfde volgorde als de Go-controller.
pub const METADATA: [u32; 6] = [0xfff8, 0xfff9, 0xfffa, 0xfffb, 0xfffc, 0xfffd];
/// Hexweergave blijft compatibel met bestaande stores en de diagnostiek-UI.
pub fn hex(value: u64) -> Result<String> {
    let mut s = String::new();
    s.try_reserve(18).map_err(|_| stulp_core::Error::Memory)?;
    write!(&mut s, "0x{value:X}").map_err(|_| Error::Invalid("Matter hexadecimal formatting"))?;
    Ok(s)
}
/// JSON-lijst met stabiele hex-IDs.
pub fn hex_ids(values: &[u32]) -> Result<Json> {
    let mut out = Vec::new();
    for value in values {
        json::push(&mut out, json::string(&hex(u64::from(*value))?)?, 4096)?;
    }
    Ok(Json::Array(out))
}
/// Naam is diagnostiek; onbekende clusters blijven zichtbaar.
pub fn name(cluster: u32) -> &'static str {
    match cluster {
        0x0003 => "Identify",
        0x0004 => "Groups",
        0x0005 => "Scenes",
        0x0006 => "On/Off",
        0x0008 => "Level Control",
        0x001D => "Descriptor",
        0x001E => "Binding",
        0x001F => "Access Control",
        0x0028 => "Basic Information",
        0x002A => "OTA Software Update Requestor",
        0x002F => "Power Source",
        0x0030 => "General Commissioning",
        0x0031 => "Network Commissioning",
        0x0033 => "General Diagnostics",
        0x0035 => "Thread Network Diagnostics",
        0x0036 => "Wi-Fi Network Diagnostics",
        0x0039 => "Bridged Device Basic Information",
        0x003B => "Switch",
        0x003E => "Operational Credentials",
        0x0045 => "Boolean State",
        0x0046 => "ICD Management",
        0x005B => "Air Quality",
        0x005C => "Smoke CO Alarm",
        0x0080 => "Boolean State Configuration",
        0x0081 => "Valve Configuration and Control",
        0x0090 => "Electrical Power Measurement",
        0x0091 => "Electrical Energy Measurement",
        0x0101 => "Door Lock",
        0x0102 => "Window Covering",
        0x0200 => "Pump Configuration and Control",
        0x0201 => "Thermostat",
        0x0202 => "Fan Control",
        0x0300 => "Color Control",
        0x0400 => "Illuminance Measurement",
        0x0402 => "Temperature Measurement",
        0x0403 => "Pressure Measurement",
        0x0404 => "Flow Measurement",
        0x0405 => "Relative Humidity Measurement",
        0x0406 => "Occupancy Sensing",
        0x040C => "Carbon Monoxide Concentration Measurement",
        0x040D => "Carbon Dioxide Concentration Measurement",
        0x0413 => "Nitrogen Dioxide Concentration Measurement",
        0x0415 => "Ozone Concentration Measurement",
        0x042A => "PM2.5 Concentration Measurement",
        0x042B => "Formaldehyde Concentration Measurement",
        0x042C => "PM1 Concentration Measurement",
        0x042D => "PM10 Concentration Measurement",
        0x042E => "Total VOC Concentration Measurement",
        0x042F => "Radon Concentration Measurement",
        _ => "Unknown cluster",
    }
}
fn infrastructure(cluster: u32) -> bool {
    matches!(
        cluster,
        0x0003
            | 0x0004
            | 0x0005
            | 0x001D
            | 0x001E
            | 0x001F
            | 0x0028
            | 0x002A
            | 0x0030
            | 0x0031
            | 0x0033
            | 0x0035
            | 0x0036
            | 0x0039
            | 0x003E
            | 0x0046
    )
}
/// Een sensor met extra attributen heet partial, niet volledig ondersteund.
pub fn coverage(cluster: u32) -> &'static str {
    if infrastructure(cluster) {
        "infrastructure"
    } else if cluster == 0x80 {
        "configuration"
    } else if capabilities::mapped_cluster(cluster) {
        "supported"
    } else {
        "unmapped"
    }
}
/// Ruwe metadata blijft getypeerd; afwezige FeatureMap is niet hetzelfde als nul.
pub struct Cluster {
    /// Servercluster-ID.
    pub id: u32,
    /// Geadverteerde attributes, gesorteerd en uniek.
    pub attributes: Vec<u32>,
    /// Geaccepteerde commands.
    pub accepted: Vec<u32>,
    /// Gegenereerde responses.
    pub generated: Vec<u32>,
    /// Geadverteerde events, ook onbekende blijven doorgegeven.
    pub events: Vec<u32>,
    /// Feature-bitmap, indien leesbaar.
    pub features: Option<u32>,
    /// ClusterRevision, indien leesbaar.
    pub revision: Option<u16>,
    /// Metadatafouten voorkomen niet dat bekende functies worden gebruikt.
    pub errors: Vec<String>,
}
impl Cluster {
    fn new(id: u32) -> Self {
        Self {
            id,
            attributes: Vec::new(),
            accepted: Vec::new(),
            generated: Vec::new(),
            events: Vec::new(),
            features: None,
            revision: None,
            errors: Vec::new(),
        }
    }
    /// Onbekende lege AttributeList volgt de compatibiliteitsfallback van Go.
    pub fn has(&self, attribute: u32) -> Option<bool> {
        if self.attributes.is_empty() {
            None
        } else {
            Some(self.attributes.contains(&attribute))
        }
    }
    /// Een geheugen- of opslagfout mag nooit als optionele hardwarefunctie verdwijnen.
    pub fn error(&mut self, error: Error) -> Result {
        let text = match error {
            Error::Cancelled => return Err(Error::Cancelled),
            Error::Core(e) => return Err(Error::Core(e)),
            Error::Invalid(s) | Error::Transport(s) => json::copy(s)?,
            Error::Timeout => json::copy("operation timed out")?,
            Error::Remote(s) => s,
        };
        json::push(&mut self.errors, text, 64)?;
        Ok(())
    }
    fn apply(&mut self, attribute: u32, bytes: &[u8]) -> Result {
        let n = Node::parse(bytes)?;
        match attribute {
            0xfff8..=0xfffb => {
                let mut ids = attributes::ids(&n, false)?;
                ids.sort_unstable();
                ids.dedup();
                match attribute {
                    0xfff8 => self.generated = ids,
                    0xfff9 => self.accepted = ids,
                    0xfffa => self.events = ids,
                    _ => self.attributes = ids,
                }
            }
            0xfffc => {
                self.features = Some(match n.element.value {
                    Value::Uint(n) => {
                        u32::try_from(n).map_err(|_| Error::Invalid("FeatureMap exceeds uint32"))?
                    }
                    _ => return Err(Error::Invalid("FeatureMap is not unsigned")),
                });
            }
            0xfffd => {
                self.revision = Some(match n.element.value {
                    Value::Uint(n) => u16::try_from(n)
                        .map_err(|_| Error::Invalid("ClusterRevision exceeds uint16"))?,
                    _ => return Err(Error::Invalid("ClusterRevision is not unsigned")),
                });
            }
            _ => return Err(Error::Invalid("unexpected cluster metadata attribute")),
        }
        Ok(())
    }
    /// De UI krijgt dezelfde veldnamen, met dekking afgeleid van de huidige mappings.
    pub fn json(&self) -> Result<Json> {
        let mut out = json::fields(&[
            ("id", json::string(&hex(u64::from(self.id))?)?),
            ("name", json::string(name(self.id))?),
            ("coverage", json::string(coverage(self.id))?),
            ("attributes", hex_ids(&self.attributes)?),
            ("acceptedCommands", hex_ids(&self.accepted)?),
            ("generatedCommands", hex_ids(&self.generated)?),
            ("events", hex_ids(&self.events)?),
        ])?;
        if let Some(features) = self.features {
            json::set(
                &mut out,
                "featureMap",
                json::string(&hex(u64::from(features))?)?,
            )?;
        }
        if let Some(revision) = self.revision
            && revision != 0
        {
            json::set(&mut out, "revision", Json::uint(u64::from(revision)))?;
        }
        if !infrastructure(self.id) {
            self.classify(&mut out)?;
        }
        if !self.errors.is_empty() {
            let mut errors = Vec::new();
            for text in &self.errors {
                json::push(&mut errors, json::string(text)?, 64)?;
            }
            json::set(&mut out, "errors", Json::Array(errors))?;
        }
        Ok(out)
    }
    fn classify(&self, out: &mut Json) -> Result {
        let mut mapped = Vec::new();
        let mut unmapped = Vec::new();
        for attribute in &self.attributes {
            if *attribute >= 0xfff8 {
                continue;
            }
            let yes = (self.id == 0x80 && *attribute <= 1)
                || capabilities::MAPPINGS
                    .iter()
                    .any(|m| m.cluster == self.id && m.attribute == *attribute);
            json::push(
                if yes { &mut mapped } else { &mut unmapped },
                *attribute,
                4096,
            )?;
        }
        let mut partial = !unmapped.is_empty();
        list(out, "mappedAttributes", &mapped)?;
        list(out, "unmappedAttributes", &unmapped)?;
        mapped.clear();
        unmapped.clear();
        for command in &self.accepted {
            let yes = capabilities::MAPPINGS
                .iter()
                .any(|m| m.cluster == self.id && m.commands.contains(command));
            json::push(
                if yes { &mut mapped } else { &mut unmapped },
                *command,
                4096,
            )?;
        }
        partial |= !unmapped.is_empty();
        list(out, "mappedCommands", &mapped)?;
        list(out, "unmappedCommands", &unmapped)?;
        list(out, "mappedEvents", &self.events)?;
        if partial && coverage(self.id) == "supported" {
            json::set(out, "coverage", json::string("partial")?)?;
        }
        Ok(())
    }
}
fn list(out: &mut Json, key: &str, ids: &[u32]) -> Result {
    if !ids.is_empty() {
        json::set(out, key, hex_ids(ids)?)?;
    }
    Ok(())
}
/// Eén endpoint houdt zijn eigen clusterlijst, ook na samenvoegen tot één fysiek apparaat.
pub struct Inventory {
    /// Descriptor-endpoint.
    pub endpoint: u16,
    /// Type-IDs van het apparaat.
    pub types: Vec<u32>,
    /// Metadata per geadverteerde servercluster.
    pub clusters: Vec<Cluster>,
}
impl Inventory {
    /// Batched reads blijven met zes clusters per request onder het Thread/UDP-budget.
    pub async fn inspect<T: Transport>(
        im: &mut Interaction<'_>,
        c: &mut Client<T>,
        endpoint: u16,
        types: &[u32],
        servers: &[u32],
        deadline: u64,
    ) -> Result<Self> {
        let mut out = Self {
            endpoint,
            types: Vec::new(),
            clusters: Vec::new(),
        };
        for t in types {
            json::push(&mut out.types, *t, 4096)?;
        }
        for id in servers {
            if !out.clusters.iter().any(|e| e.id == *id) {
                json::push(&mut out.clusters, Cluster::new(*id), 256)?;
            }
        }
        for batch in out.clusters.chunks_mut(6) {
            let mut paths = Vec::new();
            for entry in batch.iter() {
                for a in METADATA {
                    json::push(&mut paths, AttributePath::new(endpoint, entry.id, a), 36)?;
                }
            }
            let reports = match im.read(c, &paths, true, deadline).await {
                Ok(reports) => reports,
                Err(e) => {
                    if matches!(e, Error::Cancelled) {
                        return Err(e);
                    }
                    if let Error::Core(e) = e {
                        return Err(Error::Core(e));
                    }
                    for entry in batch {
                        entry.error(Error::Invalid("cluster metadata read failed"))?;
                    }
                    continue;
                }
            };
            for entry in batch {
                for attribute in METADATA {
                    let value =
                        reports.attribute(AttributePath::new(endpoint, entry.id, attribute), true);
                    let result = match value {
                        Ok(Some(bytes)) => entry.apply(attribute, &bytes),
                        Ok(None) => Ok(()),
                        Err(e) => Err(e),
                    };
                    if let Err(e) = result {
                        entry.error(e)?;
                    }
                }
            }
        }
        Ok(out)
    }
    /// AttributeList is leidend, maar ontbrekende metadata ontkent geen ondersteuning.
    pub fn has(&self, cluster: u32, attribute: u32) -> Option<bool> {
        self.clusters
            .iter()
            .find(|c| c.id == cluster)
            .and_then(|c| c.has(attribute))
    }
    /// Compatibel diagnostiekobject voor ~matter.endpointInventory.
    pub fn json(&self) -> Result<Json> {
        let mut clusters = Vec::new();
        for c in &self.clusters {
            json::push(&mut clusters, c.json()?, 256)?;
        }
        Ok(json::fields(&[
            ("endpoint", Json::uint(u64::from(self.endpoint))),
            ("deviceTypes", hex_ids(&self.types)?),
            ("clusters", Json::Array(clusters)),
        ])?)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn metadata_is_authoritative_and_unmapped_parts_remain_visible() -> Result {
        let mut cluster = Cluster::new(6);
        cluster.attributes = alloc::vec![0, 42, 0xfffb];
        cluster.accepted = alloc::vec![0, 1, 9];
        cluster.events = alloc::vec![123];
        assert_eq!(cluster.has(0), Some(true));
        assert_eq!(cluster.has(1), Some(false));
        let json = cluster.json()?;
        assert_eq!(json::text(&json, "coverage"), "partial");
        assert_eq!(
            json::array(&json, "unmappedAttributes"),
            [json::string("0x2A")?]
        );
        assert_eq!(json::array(&json, "mappedEvents"), [json::string("0x7B")?]);
        assert_eq!(Cluster::new(6).has(0), None);
        assert_eq!(coverage(0x1d), "infrastructure");
        assert_eq!(coverage(0x80), "configuration");
        Ok(())
    }
}
