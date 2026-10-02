//! Clustercommando's voor commissioning; iedere stap gebruikt de bestaande beveiligde IM-sessie.
use crate::{
    attestation::Verified,
    im::{AttributePath, Command, CommandPath},
    interaction::Interaction,
    tlv::{Node, Tag, Value, Writer},
};
use alloc::{string::String, vec::Vec};
use stulp_core::json;
use stulp_sdk::{Client, Error, Result, Transport, util::join};
const GENERAL: u32 = 0x30;
const CREDENTIALS: u32 = 0x3e;
fn start() -> Result<Writer> {
    let mut w = Writer::default();
    w.start(Tag::Anonymous, Value::Structure)?;
    Ok(w)
}
fn finish(mut w: Writer) -> Result<Vec<u8>> {
    w.end()?;
    w.finish()
}
fn byte(n: &Node<'_>, tag: u8) -> Result<u8> {
    u8::try_from(n.uint(tag)?)
        .map_err(|_| Error::Invalid("commissioning response field exceeds one byte"))
}
fn detail(n: &Node<'_>, tag: u8) -> Result<String> {
    match n.get(tag).map(|n| n.element.value) {
        Some(Value::String(s)) => Ok(json::copy(s)?),
        _ => Ok(String::new()),
    }
}
fn rejected(status: u8, debug: &str) -> Result {
    if status == 0 {
        return Ok(());
    }
    let code =
        json::to_string(&json::Value::uint(u64::from(status))).map_err(stulp_core::Error::from)?;
    Err(Error::Remote(join(&[
        "Matter commissioning status ",
        &code,
        if debug.is_empty() { "" } else { ": " },
        debug,
    ])?))
}
fn result(bytes: &[u8], debug: u8) -> Result {
    let n = Node::parse(bytes)?;
    rejected(byte(&n, 0)?, &detail(&n, debug)?)
}
/// Voert de clusterstappen uit zonder het apparaatmodel of de fabric-opslag over te nemen.
pub struct Commissioning<'a> {
    /// PASE tijdens toevoegen, CASE voor afronden of verwijderen.
    pub im: Interaction<'a>,
    /// General Commissioning en Operational Credentials staan normaal op endpoint nul.
    pub endpoint: u16,
}
/// AddNOC-afwijzingen blijven getypeerd; FabricConflict mag geen blinde tweede AddNOC veroorzaken.
pub enum NocResult {
    /// Nieuw fabric-index, niet nul.
    Added(u8),
    /// Echte clusterafwijzing, inclusief optionele diagnostiek.
    Rejected {
        /// StatusFabricConflict is 0x09.
        status: u8,
        /// Tekst van het apparaat, geen certificaat- of sleutelbytes.
        debug: String,
    },
}
impl NocResult {
    /// Zet een echte afwijzing om naar de gedeelde SDK-foutvorm.
    pub fn index(self) -> Result<u8> {
        match self {
            Self::Added(i) => Ok(i),
            Self::Rejected { status, debug } => {
                rejected(status, &debug)?;
                Err(Error::Invalid("invalid NOC rejection status"))
            }
        }
    }
}
impl Commissioning<'_> {
    async fn invoke<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        cluster: u32,
        request: u32,
        response: u32,
        fields: &[u8],
        deadline: u64,
    ) -> Result<Vec<u8>> {
        let answer = self
            .im
            .invoke(
                c,
                &[Command {
                    path: CommandPath {
                        endpoint: self.endpoint,
                        cluster,
                        command: request,
                    },
                    fields,
                    reference: None,
                }],
                None,
                deadline,
            )
            .await?;
        let mut result = None;
        for chunk in answer.iter() {
            for item in chunk?.results {
                if result.is_some() {
                    return Err(Error::Invalid(
                        "commissioning command returned multiple results",
                    ));
                }
                item.status.result()?;
                if item.path
                    != (CommandPath {
                        endpoint: self.endpoint,
                        cluster,
                        command: response,
                    })
                {
                    return Err(Error::Invalid("commissioning response path differs"));
                }
                let mut w = Writer::default();
                if let Some(fields) = item.fields {
                    w.node(&fields, Tag::Anonymous)?;
                    result = Some(w.finish()?);
                } else {
                    result = Some(Vec::new());
                }
            }
        }
        result.ok_or(Error::Invalid("commissioning command returned no result"))
    }
    /// De fail-safe maakt wijzigingen tijdelijk tot CommissioningComplete.
    pub async fn arm<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        seconds: u16,
        breadcrumb: u64,
        deadline: u64,
    ) -> Result {
        let mut w = start()?;
        w.uint(Tag::Context(0), u64::from(seconds))?;
        w.uint(Tag::Context(1), breadcrumb)?;
        result(
            &self.invoke(c, GENERAL, 0, 1, &finish(w)?, deadline).await?,
            1,
        )
    }
    /// Leest de werkelijke mogelijkheden voordat de bestaande radio-instelling wordt behouden.
    pub async fn configure<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        country: &str,
        breadcrumb: u64,
        deadline: u64,
    ) -> Result {
        let country = country.trim();
        if country.len() != 2 || !country.bytes().all(|b| b.is_ascii_alphabetic()) {
            return Err(Error::Invalid("Matter country requires two ASCII letters"));
        }
        let letters = [
            country.as_bytes()[0].to_ascii_uppercase(),
            country.as_bytes()[1].to_ascii_uppercase(),
        ];
        let country =
            core::str::from_utf8(&letters).map_err(|_| Error::Invalid("Matter country"))?;
        let paths = [
            AttributePath::new(self.endpoint, GENERAL, 2),
            AttributePath::new(self.endpoint, GENERAL, 3),
        ];
        let reports = self.im.read(c, &paths, true, deadline).await?;
        let mut current = None;
        let mut capability = None;
        for chunk in reports.iter() {
            for a in chunk?.attributes {
                if let Some(status) = a.status {
                    status.result()?;
                    continue;
                }
                let Some(Value::Uint(n @ 0..=2)) = a.value.as_ref().map(|n| n.element.value) else {
                    continue;
                };
                if a.path == paths[0] {
                    current = Some(n);
                } else if a.path == paths[1] {
                    capability = Some(n);
                }
            }
        }
        let current = current.ok_or(Error::Invalid("Matter regulatory config missing"))?;
        let capability =
            capability.ok_or(Error::Invalid("Matter regulatory capability missing"))?;
        let location = if capability == 2 {
            if current <= 1 { current } else { 1 }
        } else {
            capability
        };
        let mut w = start()?;
        w.uint_width(Tag::Context(0), location, 1)?;
        w.string(Tag::Context(1), country)?;
        w.uint(Tag::Context(2), breadcrumb)?;
        result(
            &self.invoke(c, GENERAL, 2, 3, &finish(w)?, deadline).await?,
            1,
        )
    }
    async fn certificate<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        kind: u8,
        deadline: u64,
    ) -> Result<Vec<u8>> {
        let mut w = start()?;
        w.uint_width(Tag::Context(0), u64::from(kind), 1)?;
        let response = self
            .invoke(c, CREDENTIALS, 2, 3, &finish(w)?, deadline)
            .await?;
        let n = Node::parse(&response)?;
        let bytes = n.bytes(0)?;
        if bytes.is_empty() || bytes.len() > 4096 {
            return Err(Error::Invalid("attestation certificate length"));
        }
        crate::copy(bytes)
    }
    /// Houdt de door echte apparaten vereiste volgorde PAI, DAC, attestation aan.
    pub async fn attest<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        challenge: &[u8; 16],
        vendor: u16,
        product: u16,
        deadline: u64,
    ) -> Result<Verified> {
        let pai = self.certificate(c, 2, deadline).await?;
        let dac = self.certificate(c, 1, deadline).await?;
        let verified = Verified::chain(&pai, &dac, vendor, product)?;
        let nonce = c.random()?;
        let mut w = start()?;
        w.bytes(Tag::Context(0), &nonce)?;
        let response = self
            .invoke(c, CREDENTIALS, 0, 1, &finish(w)?, deadline)
            .await?;
        let n = Node::parse(&response)?;
        verified.verify(n.bytes(0)?, n.bytes(1)?, challenge, &nonce)?;
        Ok(verified)
    }
    /// De operationele sleutel wordt pas teruggegeven na DAC-binding, nonce en PKCS#10-verificatie.
    pub async fn csr<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        attestation: &Verified,
        challenge: &[u8; 16],
        deadline: u64,
    ) -> Result<[u8; 65]> {
        let nonce = c.random()?;
        let mut w = start()?;
        w.bytes(Tag::Context(0), &nonce)?;
        w.boolean(Tag::Context(1), false)?;
        let response = self
            .invoke(c, CREDENTIALS, 4, 5, &finish(w)?, deadline)
            .await?;
        let n = Node::parse(&response)?;
        attestation.csr(n.bytes(0)?, n.bytes(1)?, challenge, &nonce)
    }
    /// Status-only AddTrustedRoot antwoordt met hetzelfde commandpad.
    pub async fn add_root<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        root: &[u8],
        deadline: u64,
    ) -> Result {
        if root.is_empty() || root.len() > 400 {
            return Err(Error::Invalid("Matter root certificate length"));
        }
        let mut w = start()?;
        w.bytes(Tag::Context(0), root)?;
        self.invoke(c, CREDENTIALS, 11, 11, &finish(w)?, deadline)
            .await?;
        Ok(())
    }
    /// Eén AddNOC per fail-safe-poging; een afwijzing wordt nooit automatisch herhaald.
    pub async fn add_noc<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        noc: &[u8],
        admin: Administrator<'_>,
        deadline: u64,
    ) -> Result<NocResult> {
        if noc.is_empty() || noc.len() > 400 || admin.subject == 0 {
            return Err(Error::Invalid("Matter NOC inputs"));
        }
        let mut w = start()?;
        w.bytes(Tag::Context(0), noc)?;
        w.bytes(Tag::Context(2), admin.ipk)?;
        w.uint(Tag::Context(3), admin.subject)?;
        w.uint(Tag::Context(4), u64::from(admin.vendor))?;
        let fields = zeroize::Zeroizing::new(finish(w)?);
        let response = self.invoke(c, CREDENTIALS, 6, 8, &fields, deadline).await?;
        let n = Node::parse(&response)?;
        let status = byte(&n, 0)?;
        if status != 0 {
            return Ok(NocResult::Rejected {
                status,
                debug: detail(&n, 2)?,
            });
        }
        let index = byte(&n, 1)?;
        if index == 0 {
            return Err(Error::Invalid("NOCResponse returned fabric index zero"));
        }
        Ok(NocResult::Added(index))
    }
    /// Zoekt alleen onze fabric, ongefilterd over PASE; andere controllers blijven ongemoeid.
    pub async fn stale<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        fabric: u64,
        deadline: u64,
    ) -> Result<Option<u8>> {
        let path = AttributePath::new(self.endpoint, CREDENTIALS, 1);
        let reports = self.im.read(c, &[path], false, deadline).await?;
        for report in reports.iter() {
            for a in report?.attributes {
                if a.path != path {
                    continue;
                }
                if let Some(status) = a.status {
                    status.result()?;
                    continue;
                }
                let value = a.value.ok_or(Error::Invalid("fabric table missing"))?;
                if value.element.value != Value::Array {
                    return Err(Error::Invalid("fabric table is not an array"));
                }
                for entry in value.children {
                    if entry.element.value != Value::Structure {
                        return Err(Error::Invalid("fabric descriptor type"));
                    }
                    if entry.uint(3)? == fabric {
                        let index = byte(&entry, 254)?;
                        if index != 0 {
                            return Ok(Some(index));
                        }
                    }
                }
            }
        }
        Ok(None)
    }
    /// Verwijderen gebeurt op het apparaat voordat de eigen lokale pairing wordt vergeten.
    pub async fn remove<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        index: u8,
        deadline: u64,
    ) -> Result {
        if index == 0 {
            return Err(Error::Invalid("RemoveFabric requires nonzero index"));
        }
        let mut w = start()?;
        w.uint_width(Tag::Context(0), u64::from(index), 1)?;
        result(
            &self
                .invoke(c, CREDENTIALS, 10, 8, &finish(w)?, deadline)
                .await?,
            2,
        )
    }
    /// Sluit commissioning af over CASE, nadat inventaris en apparaatmodel zijn gelezen.
    pub async fn complete<T: Transport>(&mut self, c: &mut Client<T>, deadline: u64) -> Result {
        result(
            &self
                .invoke(c, GENERAL, 4, 5, &finish(start()?)?, deadline)
                .await?,
            1,
        )
    }
}
/// AddNOC krijgt de oorspronkelijke epoch-IPK, niet de daarvan afgeleide operationele IPK.
pub struct Administrator<'a> {
    /// Privé epoch-key van de eigen fabric.
    pub ipk: &'a [u8; 16],
    /// Eigen controller-node-ID.
    pub subject: u64,
    /// Stulp gebruikt het Matter-testvendor-ID 0xFFF1.
    pub vendor: u16,
}
