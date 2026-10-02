//! Descriptor- en clusterreads vormen hetzelfde apparaatmodel als de bestaande Go-controller.
use crate::{
    attributes, capabilities,
    im::AttributePath,
    interaction::Interaction,
    inventory::{self, Inventory},
    settings,
    tlv::{Node, Value},
};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::json::{self, Value as Json};
use stulp_sdk::{Client, Error, Result, Transport, clone, util::join};
/// Modelversie blijft compatibel met de herontdekking van bestaande Matter-apparaten.
pub const VERSION: u64 = 4;
/// Publieke identiteitsinformatie, zonder private fabricsleutels.
pub struct Identity<'a> {
    /// Operationeel node-ID.
    pub node: u64,
    /// Gevalideerd NOC van deze node.
    pub noc: &'a [u8],
    /// Door AddNOC toegewezen fabric-index.
    pub fabric: u8,
    /// Canoniek UDP-adres met eventueel IPv6-scope.
    pub address: &'a str,
    /// Vendor-ID uit de geverifieerde onboarding-identiteit.
    pub vendor: u16,
    /// Product-ID uit de geverifieerde onboarding-identiteit.
    pub product: u16,
}
struct Metadata {
    vendor_name: String,
    product_name: String,
    label: String,
    vendor: u16,
    product: u16,
}
impl Metadata {
    fn copy(&self) -> Result<Self> {
        Ok(Self {
            vendor_name: json::copy(&self.vendor_name)?,
            product_name: json::copy(&self.product_name)?,
            label: json::copy(&self.label)?,
            vendor: self.vendor,
            product: self.product,
        })
    }
    async fn overlay<T: Transport>(
        &mut self,
        im: &mut Interaction<'_>,
        c: &mut Client<T>,
        endpoint: u16,
        cluster: u32,
        deadline: u64,
    ) -> Result {
        for attribute in [1, 3, 5, 2, 4] {
            let Some(bytes) = attributes::read(
                im,
                c,
                AttributePath::new(endpoint, cluster, attribute),
                true,
                deadline,
            )
            .await?
            else {
                continue;
            };
            let n = Node::parse(&bytes)?;
            if n.element.value == Value::Null {
                continue;
            }
            match (attribute, n.element.value) {
                (1 | 3 | 5, Value::String(s)) => {
                    *match attribute {
                        1 => &mut self.vendor_name,
                        3 => &mut self.product_name,
                        _ => &mut self.label,
                    } = json::copy(s)?;
                }
                (2 | 4, Value::Uint(n)) => {
                    *if attribute == 2 {
                        &mut self.vendor
                    } else {
                        &mut self.product
                    } = u16::try_from(n)
                        .map_err(|_| Error::Invalid("basic information ID exceeds uint16"))?;
                }
                _ => return Err(Error::Invalid("basic information attribute has wrong type")),
            }
        }
        Ok(())
    }
    fn name(&self, bridged: bool) -> Result<String> {
        let name = if bridged && !self.label.trim().is_empty() {
            self.label.trim()
        } else if !self.product_name.trim().is_empty() {
            self.product_name.trim()
        } else {
            self.label.trim()
        };
        if !name.is_empty() {
            return Ok(json::copy(name)?);
        }
        let mut out = String::new();
        out.try_reserve(16).map_err(|_| stulp_core::Error::Memory)?;
        write!(&mut out, "Matter {:04X}:{:04X}", self.vendor, self.product)
            .map_err(|_| Error::Invalid("Matter name formatting"))?;
        Ok(out)
    }
    fn manufacturer(&self) -> Result<String> {
        if !self.vendor_name.trim().is_empty() {
            return Ok(json::copy(self.vendor_name.trim())?);
        }
        let mut out = String::new();
        out.try_reserve(16).map_err(|_| stulp_core::Error::Memory)?;
        write!(&mut out, "Matter 0x{:04X}", self.vendor)
            .map_err(|_| Error::Invalid("Matter vendor formatting"))?;
        Ok(out)
    }
}
/// Exact node-ID als hextekst, zonder JSON-float of verlies boven 2^53.
pub fn node_id(id: u64) -> Result<String> {
    let mut out = String::new();
    out.try_reserve(16).map_err(|_| stulp_core::Error::Memory)?;
    write!(&mut out, "{id:016X}").map_err(|_| Error::Invalid("Matter node formatting"))?;
    Ok(out)
}
async fn descriptor<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    endpoint: u16,
    attribute: u32,
    deadline: u64,
) -> Result<Vec<u32>> {
    let bytes = attributes::required(
        im,
        c,
        AttributePath::new(endpoint, 0x1d, attribute),
        deadline,
    )
    .await?;
    attributes::ids(&Node::parse(&bytes)?, attribute == 0)
}
/// Leest capabilities volgens de inventaris; nullmetingen maken de functie niet onzichtbaar.
pub async fn initial<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    inventory: &Inventory,
    servers: &[u32],
    deadline: u64,
) -> Result<(Json, Json)> {
    let mut caps = Vec::new();
    let mut state = json::object();
    for mapping in capabilities::MAPPINGS {
        if !servers.contains(&mapping.cluster) || !mapping.applies(&inventory.types, servers) {
            continue;
        }
        let present = inventory.has(mapping.cluster, mapping.attribute);
        if present == Some(false) {
            continue;
        }
        let Some(bytes) = attributes::read(
            im,
            c,
            AttributePath::new(inventory.endpoint, mapping.cluster, mapping.attribute),
            mapping.optional || present.is_some(),
            deadline,
        )
        .await?
        else {
            continue;
        };
        let value = mapping.decode(&Node::parse(&bytes)?)?;
        if !caps
            .iter()
            .any(|cap: &Json| cap.as_str() == Some(mapping.capability))
        {
            json::push(&mut caps, json::string(mapping.capability)?, 256)?;
        }
        if let Some(value) = value {
            json::set(&mut state, mapping.capability, value)?;
        }
    }
    if servers.contains(&0x3b) {
        json::push(&mut caps, json::string("button")?, 256)?;
        json::set(&mut state, "button", Json::Bool(false))?;
    }
    Ok((Json::Array(caps), state))
}
/// Inspecteert ieder endpoint; samenvoegen van native endpoints gebeurt in de apparaatlaag.
pub async fn inspect<T: Transport>(
    im: &mut Interaction<'_>,
    c: &mut Client<T>,
    identity: &Identity<'_>,
    deadline: u64,
) -> Result<Vec<Json>> {
    let mut metadata = Metadata {
        vendor_name: String::new(),
        product_name: String::new(),
        label: String::new(),
        vendor: identity.vendor,
        product: identity.product,
    };
    metadata.overlay(im, c, 0, 0x28, deadline).await?;
    let mut parts = descriptor(im, c, 0, 3, deadline).await?;
    parts.retain(|v| *v <= u32::from(u16::MAX));
    parts.sort_unstable();
    parts.dedup();
    if parts.is_empty() || parts.len() > 128 {
        return Err(Error::Invalid(
            "Matter descriptor needs 1..128 endpoint parts",
        ));
    }
    let mut devices = Vec::new();
    for endpoint in &parts {
        let endpoint = *endpoint as u16;
        let types = descriptor(im, c, endpoint, 0, deadline).await?;
        let servers = descriptor(im, c, endpoint, 1, deadline).await?;
        let bridged = servers.contains(&0x39);
        let mut meta = metadata.copy()?;
        if bridged {
            meta.overlay(im, c, endpoint, 0x39, deadline).await?;
        }
        let mut inventory = Inventory::inspect(im, c, endpoint, &types, &servers, deadline).await?;
        let (caps, state) = initial(im, c, &inventory, &servers, deadline).await?;
        let (settings, settings_metadata) =
            settings::inspect(im, c, &mut inventory, deadline).await?;
        let mut name = meta.name(bridged)?;
        if parts.len() > 1 && !bridged {
            name = join(&[&name, " · ", &settings::decimal(u64::from(endpoint))?])?;
        }
        let mut routes = json::object();
        for cap in caps.as_array().unwrap_or(&[]) {
            if let Some(cap) = cap.as_str() {
                json::set(&mut routes, cap, Json::uint(u64::from(endpoint)))?;
            }
        }
        let node = node_id(identity.node)?;
        let endpoints = Json::Array(one(Json::uint(u64::from(endpoint)))?);
        let data = json::fields(&[
            (
                "id",
                json::string(&join(&[
                    &node,
                    "-",
                    &settings::decimal(u64::from(endpoint))?,
                ])?)?,
            ),
            ("nodeId", json::string(&node)?),
            ("endpoint", Json::uint(u64::from(endpoint))),
            ("vendorId", Json::uint(u64::from(meta.vendor))),
            ("productId", Json::uint(u64::from(meta.product))),
        ])?;
        let store = json::fields(&[
            ("manufacturer", json::string(&meta.manufacturer()?)?),
            ("matter.attestation", json::string("dac-pai-verified")?),
            ("matter.nodeId", json::string(&node)?),
            ("matter.endpoint", Json::uint(u64::from(endpoint))),
            ("matter.endpoints", endpoints),
            ("matter.capabilityEndpoints", routes),
            ("matter.address", json::string(identity.address)?),
            (
                "matter.noc",
                clone(stulp_sdk::util::field(
                    &stulp_sdk::asset(identity.noc)?,
                    "data",
                ))?,
            ),
            ("matter.fabricIndex", Json::uint(u64::from(identity.fabric))),
            ("matter.deviceTypes", inventory::hex_ids(&types)?),
            ("matter.serverClusters", inventory::hex_ids(&servers)?),
            ("matter.bridged", Json::Bool(bridged)),
            (
                "~matter.endpointInventory",
                Json::Array(one(inventory.json()?)?),
            ),
            ("matter.settings", settings_metadata),
            ("matter.modelVersion", Json::uint(VERSION)),
        ])?;
        let device = json::fields(&[
            ("driverId", json::string("matter")?),
            ("name", json::string(&name)?),
            (
                "class",
                json::string(capabilities::class(&types, &servers))?,
            ),
            ("capabilities", caps),
            ("state", state),
            ("settings", settings),
            ("data", data),
            ("store", store),
        ])?;
        json::push(&mut devices, device, 128)?;
    }
    crate::devices::finish(devices)
}
fn one(value: Json) -> Result<Vec<Json>> {
    let mut list = Vec::new();
    json::push(&mut list, value, 1)?;
    Ok(list)
}
/// Bewaart de hardware-identiteit eenmaal; latere gebruikersnamen blijven ongemoeid.
pub fn preserve_name(device: &mut Json) -> Result {
    let mut store = clone(stulp_sdk::util::field(device, "store"))?;
    if store.as_object().is_none() {
        store = json::object();
    }
    if json::text(&store, "__stulp.hardwareName").is_empty() {
        json::set(
            &mut store,
            "__stulp.hardwareName",
            json::string(json::text(device, "name"))?,
        )?;
    }
    json::set(device, "store", store)?;
    Ok(())
}
