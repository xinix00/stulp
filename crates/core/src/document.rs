//! Het versie-2-configuratiedocument, zonder runtime-capabilitywaarden.
use crate::{
    Error, Result,
    json::{self, TryClone, Value},
};
use alloc::vec::Vec;

/// Huidige duurzame vorm, gelijk aan de Go-controller.
pub const VERSION: u64 = 2;
/// De bestaande Go-archiefgrens voor het volledige document, afzonderlijk van API-bodies.
pub const MAX_BYTES: usize = 64 << 20;
/// Harde grenzen voordat een mutatie werk of opslag kan opeisen.
pub const MAX_RECORDS: usize = 4096;
/// De notificatiegeschiedenis blijft een kleine ring.
pub const MAX_NOTIFICATIONS: usize = 200;

/// Een document bewaart onbekende velden om uitbreidingen niet stil te verliezen.
pub struct Document {
    root: Value,
}

impl Document {
    /// Leest een bestaand document of initialiseert lege opslag.
    pub fn decode(bytes: &[u8]) -> Result<Self> {
        if bytes.len() > MAX_BYTES {
            return Err(Error::Full);
        }
        let mut root = if bytes.iter().all(u8::is_ascii_whitespace) {
            json::object()
        } else {
            json::parse_bounded(bytes, MAX_BYTES)?
        };
        if root.as_object().is_none() {
            return Err(Error::Invalid("document must be an object"));
        }
        if let Some(v) = json::get(&root, "version") {
            let version = v
                .as_u64()
                .ok_or(Error::Invalid("invalid document version"))?;
            if version > VERSION {
                return Err(Error::Invalid("document was written by a newer Stulp"));
            }
        }
        json::set(&mut root, "version", Value::uint(VERSION))?;
        for key in [
            "apps",
            "devices",
            "deviceGroups",
            "flows",
            "scenes",
            "notifications",
        ] {
            match json::get(&root, key) {
                None | Some(Value::Null) => json::set(&mut root, key, Value::Array(Vec::new()))?,
                Some(Value::Array(items)) if items.len() <= MAX_RECORDS => validate_records(items)?,
                _ => return Err(Error::Invalid("invalid document collection")),
            }
        }
        for key in ["appSettings", "appState", "system"] {
            match json::get(&root, key) {
                None | Some(Value::Null) => json::set(&mut root, key, json::object())?,
                Some(Value::Object(_)) => (),
                _ => return Err(Error::Invalid("invalid document settings")),
            }
        }
        let mut out = Self { root };
        out.clean_devices()?;
        Ok(out)
    }

    /// Snapshot dat niet via een openbare API mag worden geëxporteerd: bevat appgeheimen.
    pub fn root(&self) -> &Value {
        &self.root
    }

    /// Een faalbare kandidaat; de eigenaar publiceert hem pas na opslag.
    pub fn candidate(&self) -> Result<Self> {
        Ok(Self {
            root: self.root.try_clone()?,
        })
    }

    /// Leest een collectie als slice, zonder kopieën.
    pub fn records(&self, collection: &str) -> &[Value] {
        json::array(&self.root, collection)
    }

    /// Zoekt één configuratierecord.
    pub fn record(&self, collection: &str, id: &str) -> Result<&Value> {
        self.records(collection)
            .iter()
            .find(|r| json::text(r, "id") == id)
            .ok_or(Error::Missing("record does not exist"))
    }

    /// Vervangt een gevalideerde collectie.
    pub(crate) fn replace(&mut self, collection: &str, records: Vec<Value>) -> Result {
        if records.len() > MAX_RECORDS {
            return Err(Error::Full);
        }
        json::set(&mut self.root, collection, Value::Array(records))
    }

    /// Vervangt één vrij JSON-object, uitsluitend vanuit de eigenaar.
    pub(crate) fn set(&mut self, key: &str, value: Value) -> Result {
        json::set(&mut self.root, key, value)
    }

    /// Schrijft compact JSON met één newline, zoals Go.
    pub fn encode(&self) -> Result<alloc::string::String> {
        let mut out = json::to_string(&self.root)?;
        if out.len() >= MAX_BYTES {
            return Err(Error::Full);
        }
        out.try_reserve(1).map_err(|_| Error::Memory)?;
        out.push('\n');
        Ok(out)
    }

    /// Availability en capabilitywaarden zijn uitsluitend observaties van deze runtime.
    fn clean_devices(&mut self) -> Result {
        let mut devices = Vec::new();
        for record in self.records("devices") {
            let mut record = record.try_clone()?;
            for key in ["state", "available", "unavailableMessage"] {
                json::remove(&mut record, key)?;
            }
            if let Some(store) = json::get(&record, "store").and_then(Value::as_object) {
                let mut kept = json::Object::new();
                for (key, value) in store.iter().filter(|(key, _)| !key.starts_with('~')) {
                    kept.push(key, value.try_clone()?)?;
                }
                json::set(&mut record, "store", Value::Object(kept))?;
            }
            json::push(&mut devices, record, MAX_RECORDS)?;
        }
        self.replace("devices", devices)
    }
}

fn validate_records(records: &[Value]) -> Result {
    for (index, record) in records.iter().enumerate() {
        let id = json::text(record, "id");
        if id.is_empty() {
            return Err(Error::Invalid("record id is required"));
        }
        if records
            .iter()
            .take(index)
            .any(|r| json::text(r, "id") == id)
        {
            return Err(Error::Conflict("duplicate record id"));
        }
    }
    Ok(())
}
