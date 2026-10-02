//! Het bestaande STULPAB-formaat voor opslag die geen atomische rename heeft.
use crate::{Error, Result, document::MAX_BYTES};
use alloc::vec::Vec;

const HEADER: usize = 24;
const MAGIC: &[u8; 8] = b"STULPAB\x01";

/// Welk duurzaam bestand een generatie bevat.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Slot {
    /// Eerste versieplaats.
    A,
    /// Tweede versieplaats.
    B,
    /// Het oude ongesuffigeerde JSON-document.
    Legacy,
}

/// Een gevalideerd record leent zijn bytes zonder kopie.
pub struct Record<'a> {
    /// Monotone generatie, nul is nooit geldig.
    pub generation: u64,
    /// Het volledige configuratiedocument.
    pub payload: &'a [u8],
}

fn crc(mut crc: u32, bytes: &[u8]) -> u32 {
    for byte in bytes {
        crc ^= u32::from(*byte);
        for _ in 0..8 {
            crc = (crc >> 1) ^ if crc & 1 != 0 { 0xedb8_8320 } else { 0 };
        }
    }
    crc
}

/// Go crc32.IEEE over header zonder checksum en daarna de payload.
pub fn checksum(prefix: &[u8], payload: &[u8]) -> u32 {
    !crc(crc(u32::MAX, prefix), payload)
}

/// Exact dezelfde little-endian header als internal/store/files_ab.go.
pub fn encode(generation: u64, payload: &[u8]) -> Result<Vec<u8>> {
    if generation == 0 {
        return Err(Error::Invalid("document slot generation must be positive"));
    }
    if payload.len() > MAX_BYTES {
        return Err(Error::Full);
    }
    let length = u32::try_from(payload.len()).map_err(|_| Error::Full)?;
    let mut bytes = Vec::new();
    bytes
        .try_reserve_exact(HEADER + payload.len())
        .map_err(|_| Error::Memory)?;
    bytes.extend_from_slice(MAGIC);
    bytes.extend_from_slice(&generation.to_le_bytes());
    bytes.extend_from_slice(&length.to_le_bytes());
    let sum = checksum(&bytes, payload);
    bytes.extend_from_slice(&sum.to_le_bytes());
    bytes.extend_from_slice(payload);
    Ok(bytes)
}

/// Een onderbroken write wordt een ongeldige slot, nooit een halve configuratie.
pub fn decode(bytes: &[u8]) -> Option<Record<'_>> {
    if bytes.get(..8)? != MAGIC {
        return None;
    }
    let generation = u64::from_le_bytes(bytes.get(8..16)?.try_into().ok()?);
    let length = u32::from_le_bytes(bytes.get(16..20)?.try_into().ok()?) as usize;
    let expected = u32::from_le_bytes(bytes.get(20..24)?.try_into().ok()?);
    let payload = bytes.get(HEADER..)?;
    if generation == 0
        || length != payload.len()
        || length > MAX_BYTES
        || checksum(bytes.get(..20)?, payload) != expected
    {
        return None;
    }
    Some(Record {
        generation,
        payload,
    })
}

/// Selecteert de nieuwste volledige generatie; een conflict wordt nooit geraden.
pub fn select<'a>(
    a: Option<&'a [u8]>,
    b: Option<&'a [u8]>,
    legacy: Option<&'a [u8]>,
) -> Result<(Slot, Record<'a>)> {
    let left = a.and_then(decode);
    let right = b.and_then(decode);
    match (left, right) {
        (Some(a), Some(b)) if a.generation == b.generation => {
            if a.payload != b.payload {
                return Err(Error::Conflict(
                    "document slots have conflicting generations",
                ));
            }
            Ok((Slot::A, a))
        }
        (Some(a), Some(b)) => {
            if a.generation > b.generation {
                Ok((Slot::A, a))
            } else {
                Ok((Slot::B, b))
            }
        }
        (Some(a), None) => Ok((Slot::A, a)),
        (None, Some(b)) => Ok((Slot::B, b)),
        (None, None) => match legacy {
            Some(payload) => Ok((
                Slot::Legacy,
                Record {
                    generation: 0,
                    payload,
                },
            )),
            None if a.is_some() || b.is_some() => Err(Error::Invalid(
                "no valid document slot and no legacy document",
            )),
            None => Ok((
                Slot::Legacy,
                Record {
                    generation: 0,
                    payload: b"",
                },
            )),
        },
    }
}

/// De adapter implementeert alleen lezen en schrijven, niet de generatiekeuze.
pub trait Backend {
    /// Optioneel documentpad voor adapters met een canonieke opslaglocatie.
    fn path(&self) -> Option<&str> {
        None
    }
    /// Ontbreken is None; een transportfout is een fout.
    fn read(&mut self, slot: Slot) -> Result<Option<Vec<u8>>>;
    /// Een write kan een onzekere uitkomst hebben, bijvoorbeeld na een verloren RPC-antwoord.
    fn write(&mut self, slot: Slot, bytes: &[u8]) -> Result;
}

/// Opent één keer en schrijft daarna uitsluitend de inactieve slot.
pub struct Files<B> {
    backend: B,
    active: Slot,
    generation: u64,
}
impl<B: Backend> Files<B> {
    /// Leest alle noodzakelijke bronnen vóór de eigenaar begint te muteren.
    pub fn open(mut backend: B) -> Result<(Self, Vec<u8>)> {
        let a = backend.read(Slot::A)?;
        let b = backend.read(Slot::B)?;
        let legacy =
            if a.as_deref().and_then(decode).is_none() && b.as_deref().and_then(decode).is_none() {
                backend.read(Slot::Legacy)?
            } else {
                None
            };
        let (active, record) = select(a.as_deref(), b.as_deref(), legacy.as_deref())?;
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(record.payload.len())
            .map_err(|_| Error::Memory)?;
        bytes.extend_from_slice(record.payload);
        Ok((
            Self {
                backend,
                active,
                generation: record.generation,
            },
            bytes,
        ))
    }
}
impl<B: Backend> crate::store::Storage for Files<B> {
    fn path(&self) -> Option<&str> {
        self.backend.path()
    }
    fn save(&mut self, bytes: &[u8]) -> Result {
        let generation = self.generation.checked_add(1).ok_or(Error::Full)?;
        let target = if self.active == Slot::A {
            Slot::B
        } else {
            Slot::A
        };
        let encoded = encode(generation, bytes)?;
        if let Err(error) = self.backend.write(target, &encoded) {
            // Een verloren antwoord mag nieuwere bytes op schijf niet combineren
            // met oudere RAM-staat: lees exact deze generatie eerst terug.
            let written = self.backend.read(target)?;
            if !written
                .as_deref()
                .and_then(decode)
                .is_some_and(|r| r.generation == generation && r.payload == bytes)
            {
                return Err(error);
            }
        }
        self.generation = generation;
        self.active = target;
        Ok(())
    }
}
