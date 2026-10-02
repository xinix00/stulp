//! Duurzame fabric-identiteit; nooit een onleesbare bestaande identiteit vervangen.
use crate::{
    case,
    certificate::{Certificate, Name, private_der, private_key},
};
use core::time::Duration;
use p256::{SecretKey, elliptic_curve::sec1::ToEncodedPoint};
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, Result, Transport, clone, util::field};
use zeroize::{Zeroize, Zeroizing};
fn uint(n: u64) -> Value {
    Value::Number(json::Number::Uint(n))
}
const LAST_NODE: u64 = 0xffff_ffef_ffff_ffff;
fn bytes(value: &Value, key: &str) -> Result<Zeroizing<alloc::vec::Vec<u8>>> {
    let text = json::get(value, key)
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty() && s.len() <= 5500)
        .ok_or(Error::Invalid("stored Matter key or certificate missing"))?;
    Ok(Zeroizing::new(stulp_protocol::token::decode(text)?))
}
fn integer(v: &Value, key: &str) -> Result<u64> {
    match json::get(v, key) {
        Some(Value::Number(json::Number::Uint(n))) => Ok(*n),
        Some(Value::Number(json::Number::Int(n))) if *n >= 0 => Ok(*n as u64),
        _ => Err(Error::Invalid("Matter identity needs an exact integer")),
    }
}
fn encoded(bytes: &[u8]) -> Result<Value> {
    clone(field(&stulp_sdk::asset(bytes)?, "data"))
}
fn timestamp(now: u64) -> Result<(u32, u32)> {
    let date = ::der::DateTime::from_unix_duration(Duration::from_secs(now))
        .map_err(|_| Error::Invalid("invalid Matter wall clock"))?;
    let year = date
        .year()
        .checked_add(20)
        .ok_or(Error::Invalid("Matter certificate date overflow"))?;
    let end = ::der::DateTime::new(
        year,
        date.month(),
        date.day(),
        date.hour(),
        date.minutes(),
        date.seconds(),
    )
    .or_else(|e| {
        if date.month() == 2 && date.day() == 29 {
            ::der::DateTime::new(year, 3, 1, date.hour(), date.minutes(), date.seconds())
        } else {
            Err(e)
        }
    })
    .map_err(|_| Error::Invalid("Matter certificate expiry range"))?;
    let chip = |unix: u64| {
        u32::try_from(
            unix.checked_sub(946684800)
                .ok_or(Error::Invalid("wall clock predates Matter epoch"))?,
        )
        .map_err(|_| Error::Invalid("Matter epoch exceeded"))
    };
    Ok((
        chip(
            now.checked_sub(300)
                .ok_or(Error::Invalid("wall clock missing"))?,
        )?,
        chip(end.unix_duration().as_secs())?,
    ))
}
fn random_key<T: Transport>(c: &mut Client<T>) -> Result<SecretKey> {
    for _ in 0..16 {
        let random = Zeroizing::new(c.random()?);
        if let Ok(key) = SecretKey::from_slice(&*random) {
            return Ok(key);
        }
    }
    Err(Error::Invalid("CSPRNG did not provide a P-256 scalar"))
}
fn random_id<T: Transport>(c: &mut Client<T>) -> Result<u64> {
    for _ in 0..16 {
        let random = Zeroizing::new(c.random()?);
        let mut bytes = [0; 8];
        bytes.copy_from_slice(&random[..8]);
        let id = u64::from_le_bytes(bytes) & (i64::MAX as u64);
        if id != 0 {
            return Ok(id);
        }
    }
    Err(Error::Invalid("CSPRNG did not provide a Matter ID"))
}
fn serial<T: Transport>(c: &mut Client<T>) -> Result<[u8; 16]> {
    let mut result = [0; 16];
    result.copy_from_slice(&c.random()?[..16]);
    Ok(result)
}
/// Opgeslagen root en controller-identiteit. Alleen gereed na bevestigde persistente opslag.
pub struct Fabric {
    record: Value,
    root_key: SecretKey,
    root: Certificate,
    controller: Certificate,
    case: case::Fabric,
    id: u64,
    next: u64,
}
impl Fabric {
    /// Laadt uitsluitend onze eigen private app-state, of maakt en bewaart één nieuwe fabric.
    pub async fn load<T: Transport>(c: &mut Client<T>) -> Result<Self> {
        let state = field(c.state().root(), "appState");
        if !state.is_null() && state.as_object().is_none() {
            return Err(Error::Invalid("stored Matter state is unreadable"));
        }
        if let Some(record) = json::get(state, "fabric").filter(|v| !v.is_null()) {
            return Self::parse(record);
        }
        if json::get(c.state().root(), "devices")
            .and_then(Value::as_object)
            .is_some_and(|v| !v.is_empty())
        {
            return Err(Error::Invalid(
                "Matter devices exist but fabric identity is missing",
            ));
        }
        let id = random_id(c)?;
        let root_id = random_id(c)?;
        let node = random_id(c)?;
        let root_key = random_key(c)?;
        let controller_key = random_key(c)?;
        let validity = timestamp(c.wall_time()?)?;
        let root = Certificate::root(&root_key, root_id, serial(c)?, validity.0, validity.1)?;
        let controller = root.issue(
            &root_key,
            controller_key
                .public_key()
                .to_encoded_point(false)
                .as_bytes(),
            (node, id),
            serial(c)?,
            validity,
        )?;
        let random = Zeroizing::new(c.random()?);
        let mut ipk = Zeroizing::new([0; 16]);
        ipk.copy_from_slice(&random[..16]);
        let record = json::fields(&[
            ("fabricId", uint(id)),
            ("rootId", uint(root_id)),
            ("controllerNodeId", uint(node)),
            ("nextNodeId", uint(0x10000u64)),
            ("ipk", encoded(&*ipk)?),
            ("rootKeyDer", encoded(&private_der(&root_key)?)?),
            ("rootCertDer", encoded(&root.der()?)?),
            ("controllerKeyDer", encoded(&private_der(&controller_key)?)?),
            ("controllerCertDer", encoded(&controller.der()?)?),
        ])?;
        let pending = Self::parse(&record)?;
        save(c, record).await?;
        Ok(pending)
    }
    fn parse(record: &Value) -> Result<Self> {
        let id = integer(record, "fabricId")?;
        let root_id = integer(record, "rootId")?;
        let node = integer(record, "controllerNodeId")?;
        let next = integer(record, "nextNodeId")?;
        if id == 0 || root_id == 0 || node == 0 || node > LAST_NODE || next == 0 || next > LAST_NODE
        {
            return Err(Error::Invalid(
                "stored Matter identity or allocator invalid",
            ));
        }
        let root_key = private_key(&bytes(record, "rootKeyDer")?)?;
        let controller_key = private_key(&bytes(record, "controllerKeyDer")?)?;
        let root = Certificate::from_der(&bytes(record, "rootCertDer")?)?;
        let controller = Certificate::from_der(&bytes(record, "controllerCertDer")?)?;
        root.verify(&root)?;
        controller.verify(&root)?;
        if root.subject() != Name::Root(root_id)
            || controller.subject() != Name::Node(node, id)
            || root_key.public_key().to_encoded_point(false).as_bytes() != root.public()
            || controller_key
                .public_key()
                .to_encoded_point(false)
                .as_bytes()
                != controller.public()
        {
            return Err(Error::Invalid(
                "stored Matter certificates, keys and IDs differ",
            ));
        }
        let ipk = bytes(record, "ipk")?;
        let ipk: [u8; 16] = ipk
            .as_slice()
            .try_into()
            .map_err(|_| Error::Invalid("stored Matter IPK length"))?;
        let mut private = controller_key.to_bytes();
        let case = case::Fabric::new(
            id,
            node,
            root.public(),
            ipk,
            private.into(),
            &controller.tlv()?,
        );
        private.zeroize();
        Ok(Self {
            record: clone(record)?,
            root_key,
            root,
            controller,
            case: case?,
            id,
            next,
        })
    }
    /// Operationele verbindingen lenen de gevalideerde identiteit.
    pub fn case(&self) -> &case::Fabric {
        &self.case
    }
    /// Fabric-ID voor inventaris en commissioning.
    pub fn id(&self) -> u64 {
        self.id
    }
    /// Compact rootcertificaat voor AddTrustedRootCertificate.
    pub fn root(&self) -> Result<alloc::vec::Vec<u8>> {
        self.root.tlv()
    }
    /// Compact controllercertificaat voor CASE.
    pub fn controller(&self) -> Result<alloc::vec::Vec<u8>> {
        self.controller.tlv()
    }
    /// Geeft een NOC uit voor de openbare sleutel uit een geverifieerd CSR.
    pub fn issue(
        &self,
        public: &[u8],
        node: u64,
        serial: [u8; 16],
        now: u64,
    ) -> Result<alloc::vec::Vec<u8>> {
        if node == 0 || node > LAST_NODE || node == self.case.node() {
            return Err(Error::Invalid("invalid new Matter node ID"));
        }
        self.root
            .issue(
                &self.root_key,
                public,
                (node, self.id),
                serial,
                timestamp(now)?,
            )?
            .tlv()
    }
    /// Een node-ID wordt pas uitgegeven nadat de verhoogde teller duurzaam bevestigd is.
    pub async fn allocate<T: Transport>(&mut self, c: &mut Client<T>) -> Result<u64> {
        let mut allocated = self.next;
        if allocated == self.case.node() {
            allocated = allocated
                .checked_add(1)
                .ok_or(Error::Invalid("Matter node IDs exhausted"))?;
        }
        let next = allocated
            .checked_add(1)
            .filter(|n| *n <= LAST_NODE)
            .ok_or(Error::Invalid("Matter node IDs exhausted"))?;
        let mut candidate = clone(&self.record)?;
        json::set(&mut candidate, "nextNodeId", uint(next))?;
        save(c, clone(&candidate)?).await?;
        self.record = candidate;
        self.next = next;
        Ok(allocated)
    }
}
async fn save<T: Transport>(c: &mut Client<T>, record: Value) -> Result {
    let state = field(c.state().root(), "appState");
    let mut candidate = if state.is_null() {
        json::object()
    } else {
        clone(state)?
    };
    json::set(&mut candidate, "fabric", record)?;
    c.app_state(candidate).await
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic)]
#[path = "../../../tests/wire.rs"]
mod wire;
#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic)]
mod tests {
    use super::*;

    const MANIFEST: &[u8] = include_bytes!("../app.json");
    #[test]
    fn failed_storage_never_exposes_an_identity_or_advances_allocator() {
        let c = wire::Wire::client("com.stulp.matter", MANIFEST, "[]", r#"{"retained":42}"#);
        let mut w = c.into_transport();
        w.fail_state = true;
        let mut c = w.reconnect("com.stulp.matter", MANIFEST);
        assert!(hostnet::block_on(Fabric::load(&mut c)).is_err());
        assert!(
            field(c.state().root(), "appState")
                .as_object()
                .unwrap()
                .get("fabric")
                .is_none()
        );
        let mut w = c.into_transport();
        w.fail_state = false;
        let mut c = w.reconnect("com.stulp.matter", MANIFEST);
        let mut f = hostnet::block_on(Fabric::load(&mut c)).unwrap();
        let compressed = f.case().compressed_id().unwrap();
        let own = f.controller().unwrap();
        let root = f.root().unwrap();
        assert_eq!(
            json::uint(field(c.state().root(), "appState"), "retained"),
            42
        );
        assert_eq!(hostnet::block_on(f.allocate(&mut c)).unwrap(), 0x10000);
        let mut w = c.into_transport();
        w.fail_state = true;
        let mut c = w.reconnect("com.stulp.matter", MANIFEST);
        assert!(hostnet::block_on(f.allocate(&mut c)).is_err());
        assert_eq!(f.next, 0x10001);
        let mut w = c.into_transport();
        w.fail_state = false;
        let mut c = w.reconnect("com.stulp.matter", MANIFEST);
        let mut reloaded = hostnet::block_on(Fabric::load(&mut c)).unwrap();
        assert_eq!(reloaded.case().compressed_id().unwrap(), compressed);
        assert_eq!(reloaded.controller().unwrap(), own);
        assert_eq!(reloaded.root().unwrap(), root);
        assert_eq!(
            hostnet::block_on(reloaded.allocate(&mut c)).unwrap(),
            0x10001
        );
    }
    #[test]
    fn corrupt_existing_state_is_not_replaced() {
        for state in [r#"{"fabric":{}}"#, r#"[]"#, r#"{"fabric":"broken"}"#] {
            let mut c = wire::Wire::client("com.stulp.matter", MANIFEST, "[]", state);
            assert!(hostnet::block_on(Fabric::load(&mut c)).is_err());
            assert!(
                !c.into_transport()
                    .sent
                    .iter()
                    .any(|v| json::text(v, "m") == "state.set")
            );
        }
    }
}
