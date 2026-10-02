//! Stulps lengte-geprefixte appkanaal en wederzijdse attach-authenticatie.
#![no_std]
#![forbid(unsafe_code)]
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used, clippy::panic))]
extern crate alloc;

pub mod session;
pub mod token;
use alloc::vec::Vec;
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
};

/// De bestaande appprotocolversie.
pub const VERSION: u64 = 1;
/// Vóór allocatie gecontroleerde transportgrens uit Go.
pub const MAX_FRAME: usize = 8 << 20;
/// De begroeting kan een volledig appmanifest bevatten (Go-meting 14-08: 68 KiB).
pub const MAX_GREETING: usize = 256 << 10;

/// Een incrementele lezer bezit precies één bericht en kan elke fragmentatie aan.
pub struct Decoder {
    prefix: [u8; 4],
    prefix_len: usize,
    body: Vec<u8>,
    expected: Option<usize>,
    limit: usize,
    poisoned: bool,
}

impl Decoder {
    /// Een limiet per fase voorkomt dat een begroeting al 8 MiB claimt.
    pub const fn new(limit: usize) -> Self {
        Self {
            prefix: [0; 4],
            prefix_len: 0,
            body: Vec::new(),
            expected: None,
            limit,
            poisoned: false,
        }
    }

    /// Consumeert hoogstens één bericht; de aanroeper bewaart niet-geconsumeerde bytes.
    pub fn feed(&mut self, bytes: &[u8]) -> Result<usize> {
        if self.poisoned {
            return Err(Error::Invalid("protocol stream is poisoned"));
        }
        let mut used = 0;
        while self.prefix_len < 4 && used < bytes.len() {
            let b = bytes.get(used).ok_or(Error::Full)?;
            *self.prefix.get_mut(self.prefix_len).ok_or(Error::Full)? = *b;
            self.prefix_len += 1;
            used += 1;
        }
        if self.prefix_len < 4 {
            return Ok(used);
        }
        if self.expected.is_none() {
            let size = u32::from_be_bytes(self.prefix) as usize;
            if size == 0 || size > self.limit || size > MAX_FRAME {
                self.poisoned = true;
                return Err(Error::Invalid("invalid frame length"));
            }
            if self.body.try_reserve_exact(size).is_err() {
                self.poisoned = true;
                return Err(Error::Memory);
            }
            self.expected = Some(size);
        }
        let remaining = self.expected.unwrap_or(0).saturating_sub(self.body.len());
        let take = remaining.min(bytes.len().saturating_sub(used));
        self.body
            .extend_from_slice(bytes.get(used..used + take).ok_or(Error::Full)?);
        Ok(used + take)
    }

    /// Neemt de voltooide buffer over; er is geen tweede kopie van het frame.
    pub fn take(&mut self) -> Option<Vec<u8>> {
        if self.expected != Some(self.body.len()) {
            return None;
        }
        self.expected = None;
        self.prefix_len = 0;
        Some(core::mem::take(&mut self.body))
    }

    /// EOF midden in een lengte of body is een protocolfout, geen nette afsluiting.
    pub fn finish(&self) -> Result {
        if self.poisoned || self.prefix_len != 0 {
            Err(Error::Invalid("truncated protocol frame"))
        } else {
            Ok(())
        }
    }
}

/// De vier soorten uit het bestaande JSON-protocol.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// RPC-verzoek.
    Request,
    /// RPC-antwoord.
    Response,
    /// RPC-fout.
    Error,
    /// Eenzijdige gebeurtenis.
    Event,
}

/// Een frame behoudt de oorspronkelijke JSON-payload zonder typeverlies.
pub struct Frame {
    /// Framevariant.
    pub kind: Kind,
    /// Verzoekidentiteit, nul bij events.
    pub id: u64,
    /// Gevalideerde JSON-boom.
    pub value: Value,
}

impl Frame {
    /// Parseert en valideert de framevorm.
    pub fn decode(bytes: &[u8]) -> Result<Self> {
        let value = json::parse_bounded(bytes, MAX_FRAME)?;
        let kind = match json::text(&value, "t") {
            "req" => Kind::Request,
            "res" => Kind::Response,
            "err" => Kind::Error,
            "ev" => Kind::Event,
            _ => return Err(Error::Invalid("unknown frame type")),
        };
        let id = json::uint(&value, "id");
        if kind != Kind::Event && id == 0 {
            return Err(Error::Invalid("RPC frame id is required"));
        }
        if matches!(kind, Kind::Request | Kind::Event) && json::text(&value, "m").is_empty() {
            return Err(Error::Invalid("frame method is required"));
        }
        if kind == Kind::Error
            && json::get(&value, "e").is_none_or(|e| json::text(e, "message").is_empty())
        {
            return Err(Error::Invalid("error frame message is required"));
        }
        Ok(Self { kind, id, value })
    }

    /// Methode voor requests en events.
    pub fn method(&self) -> &str {
        json::text(&self.value, "m")
    }

    /// Bouwt een antwoord met dezelfde correlatie-id.
    pub fn response(id: u64, result: core::result::Result<Value, &str>) -> Result<Value> {
        let mut value = json::fields(&[("id", Value::uint(id))])?;
        match result {
            Ok(result) => {
                json::set(&mut value, "t", json::string("res")?)?;
                json::set(&mut value, "r", result)?;
            }
            Err(message) => {
                json::set(&mut value, "t", json::string("err")?)?;
                json::set(
                    &mut value,
                    "e",
                    json::fields(&[("message", json::string(message)?)])?,
                )?;
            }
        }
        Ok(value)
    }

    /// Bouwt een uitgaand verzoek of event.
    pub fn request(id: u64, method: &str, params: &Value) -> Result<Value> {
        json::fields(&[
            ("t", json::string(if id == 0 { "ev" } else { "req" })?),
            ("id", Value::uint(id)),
            ("m", json::string(method)?),
            ("p", params.try_clone()?),
        ])
    }
}

/// Encodeert inclusief de big-endian lengteprefix, met één transportbuffer.
pub fn encode(value: &Value) -> Result<Vec<u8>> {
    let body = json::to_string(value)?;
    if body.is_empty() || body.len() > MAX_FRAME {
        return Err(Error::Full);
    }
    let length = u32::try_from(body.len()).map_err(|_| Error::Full)?;
    let mut bytes = Vec::new();
    bytes
        .try_reserve_exact(body.len() + 4)
        .map_err(|_| Error::Memory)?;
    bytes.extend_from_slice(&length.to_be_bytes());
    bytes.extend_from_slice(body.as_bytes());
    Ok(bytes)
}
