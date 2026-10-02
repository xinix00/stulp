//! De gebruikte Modbus/TCP-functies uit de Go-client; afwijkende frames sluiten de stroom.
use alloc::{string::String, vec::Vec};
use stulp_core::json;
use stulp_sdk::{Client, Error, Result, TcpRequest, Transport};
#[derive(Debug)]
pub(super) enum Failure {
    Refused(u8),
    Other(Error),
}
impl From<Error> for Failure {
    fn from(e: Error) -> Self {
        Self::Other(e)
    }
}
impl From<stulp_core::Error> for Failure {
    fn from(e: stulp_core::Error) -> Self {
        Self::Other(e.into())
    }
}
impl Failure {
    pub(super) fn sdk(self) -> Error {
        match self {
            Self::Other(e) => e,
            Self::Refused(code) => Error::Invalid(match code {
                1 => "Modbus: onbekende functie",
                2 => "Modbus: register ontbreekt",
                3 => "Modbus: ongeldige waarde",
                4 => "Modbus: apparaatstoring",
                5 => "Modbus: opdracht nog bezig",
                6 => "Modbus: apparaat bezet",
                8 => "Modbus: geheugenfout",
                10 => "Modbus: gateway-pad ontbreekt; controleer het unit-id",
                11 => "Modbus: achterliggend apparaat antwoordt niet",
                _ => "Modbus: onbekende uitzondering",
            }),
        }
    }
}
#[derive(Default)]
pub(super) struct Modbus {
    pub(super) generation: u64,
    transaction: u16,
}
impl Modbus {
    pub(super) fn reset(&mut self) {
        self.generation = self.generation.wrapping_add(1);
    }
    async fn exchange<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        address: &str,
        timeout: u64,
        unit: u8,
        function: u8,
        payload: &[u8],
    ) -> core::result::Result<Vec<u8>, Failure> {
        if payload.len() > 252 {
            return Err(Error::Invalid("Modbus PDU exceeds 253 bytes").into());
        }
        self.transaction = self.transaction.wrapping_add(1);
        let mut frame = Vec::new();
        frame
            .try_reserve_exact(8 + payload.len())
            .map_err(|_| stulp_core::Error::Memory)?;
        frame.extend_from_slice(&self.transaction.to_be_bytes());
        frame.extend_from_slice(&[0, 0]);
        frame.extend_from_slice(&(2 + payload.len() as u16).to_be_bytes());
        frame.extend_from_slice(&[unit, function]);
        frame.extend_from_slice(payload);
        let result = c
            .tcp(TcpRequest {
                address: json::copy(address)?,
                generation: self.generation,
                frame,
                prefix: 6,
                length_at: 4,
                minimum: 2,
                maximum: 254,
                timeout_ms: timeout,
            })
            .await;
        let answer = match result {
            Ok(v) => v,
            Err(e) => {
                self.reset();
                return Err(e.into());
            }
        };
        let checked = check(&answer, self.transaction, unit, function);
        if let Err(Failure::Other(_)) = &checked {
            self.reset();
        }
        let bytes = checked?;
        let mut out = Vec::new();
        out.try_reserve_exact(bytes.len())
            .map_err(|_| stulp_core::Error::Memory)?;
        out.extend_from_slice(bytes);
        Ok(out)
    }
    pub(super) async fn read<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        address: &str,
        timeout: u64,
        unit: u8,
        start: u16,
        count: u16,
    ) -> core::result::Result<Vec<u16>, Failure> {
        if count == 0 || count > 125 {
            return Err(Error::Invalid("Modbus read requires 1..125 registers").into());
        }
        let mut payload = [0; 4];
        payload[..2].copy_from_slice(&start.to_be_bytes());
        payload[2..].copy_from_slice(&count.to_be_bytes());
        let answer = self
            .exchange(c, address, timeout, unit, 3, &payload)
            .await?;
        if answer.len() != 1 + usize::from(count) * 2
            || answer.first().copied().map(usize::from) != Some(usize::from(count) * 2)
        {
            self.reset();
            return Err(Error::Invalid("Modbus register byte count mismatch").into());
        }
        let mut words = Vec::new();
        for bytes in answer[1..].chunks_exact(2) {
            json::push(&mut words, u16::from_be_bytes([bytes[0], bytes[1]]), 125)?;
        }
        Ok(words)
    }
    pub(super) async fn write_single<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        address: &str,
        timeout: u64,
        unit: u8,
        start: u16,
        value: u16,
    ) -> Result {
        let mut payload = [0; 4];
        payload[..2].copy_from_slice(&start.to_be_bytes());
        payload[2..].copy_from_slice(&value.to_be_bytes());
        let answer = self
            .exchange(c, address, timeout, unit, 6, &payload)
            .await
            .map_err(Failure::sdk)?;
        if answer != payload {
            self.reset();
            return Err(Error::Invalid("Modbus write confirmation mismatch"));
        }
        Ok(())
    }
}
fn check(answer: &[u8], tx: u16, unit: u8, function: u8) -> core::result::Result<&[u8], Failure> {
    if answer.len() < 8
        || answer[..2] != tx.to_be_bytes()
        || answer[2..4] != [0, 0]
        || answer[6] != unit
        || usize::from(u16::from_be_bytes([answer[4], answer[5]])) + 6 != answer.len()
    {
        return Err(Error::Invalid("Modbus MBAP header mismatch").into());
    }
    if answer[7] == function | 0x80 {
        return if answer.len() == 9 {
            Err(Failure::Refused(answer[8]))
        } else {
            Err(Error::Invalid("malformed Modbus exception").into())
        };
    }
    if answer[7] != function {
        return Err(Error::Invalid("Modbus function mismatch").into());
    }
    Ok(&answer[8..])
}
pub(super) fn address(host: &str, port: u16) -> Result<String> {
    if host.is_empty()
        || host
            .bytes()
            .any(|b| b <= 32 || b == 127 || b"/?#@\\".contains(&b))
    {
        return Err(Error::Invalid(
            "Vul het adres van het Sigenergy-systeem in.",
        ));
    }
    let port = super::decimal(u64::from(port))?;
    if host.contains(':') && !host.starts_with('[') {
        stulp_sdk::util::join(&["[", host, "]:", &port])
    } else {
        stulp_sdk::util::join(&[host, ":", &port])
    }
}
