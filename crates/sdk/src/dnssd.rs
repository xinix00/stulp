//! Begrensde vertaling van Apple's dns-sd zone-uitvoer naar gewone DNS-records.
//! TXT blijft binair; dezelfde Matter-collector verwerkt UDP en de systeemfallback.
use crate::{Datagram, Error, Result, util::join};
use alloc::{string::String, vec::Vec};
use stulp_core::json;
const MAX_ZONE: usize = 1024 * 1024;
fn append(out: &mut Vec<u8>, bytes: &[u8]) -> Result {
    if bytes.len() > 9000usize.saturating_sub(out.len()) {
        return Err(Error::Invalid("DNS record exceeds packet limit"));
    }
    out.try_reserve(bytes.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    out.extend_from_slice(bytes);
    Ok(())
}
fn name(out: &mut Vec<u8>, text: &str) -> Result {
    let start = out.len();
    let mut label = Vec::new();
    let bytes = text.trim_end_matches('.').as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        let byte = bytes[i];
        i += 1;
        if byte == b'.' {
            if label.is_empty() {
                return Err(Error::Invalid("empty DNS label"));
            }
            append(out, &[label.len() as u8])?;
            append(out, &label)?;
            label.clear();
        } else {
            let decoded = if byte == b'\\' {
                let next = *bytes.get(i).ok_or(Error::Invalid("truncated DNS escape"))?;
                if let Some(digits) = bytes
                    .get(i..i + 3)
                    .filter(|b| b.iter().all(u8::is_ascii_digit))
                {
                    let value = u16::from(digits[0] - b'0') * 100
                        + u16::from(digits[1] - b'0') * 10
                        + u16::from(digits[2] - b'0');
                    i += 3;
                    u8::try_from(value).map_err(|_| Error::Invalid("invalid DNS escape"))?
                } else {
                    i += 1;
                    next
                }
            } else {
                byte
            };
            json::push(&mut label, decoded, 63)?;
        }
    }
    if label.is_empty() {
        return Err(Error::Invalid("empty DNS name"));
    }
    append(out, &[label.len() as u8])?;
    append(out, &label)?;
    append(out, &[0])?;
    if out.len() - start > 255 {
        return Err(Error::Invalid("DNS name exceeds 255 bytes"));
    }
    Ok(())
}
fn record(out: &mut Vec<u8>, owner: &str, kind: u16, data: &[u8]) -> Result {
    name(out, owner)?;
    append(out, &kind.to_be_bytes())?;
    append(out, &[0, 1, 0, 0, 0, 120])?;
    append(out, &(data.len() as u16).to_be_bytes())?;
    append(out, data)
}
/// Leest uitsluitend de ongecomprimeerde PTR-vragen die de SDK zelf maakt.
pub fn services(packet: &[u8]) -> Result<Vec<String>> {
    if packet.len() < 12 || packet.len() > 8192 || packet[2..4] != [0, 0] {
        return Err(Error::Invalid("invalid DNS query"));
    }
    let count = u16::from_be_bytes([packet[4], packet[5]]);
    if !(1..=12).contains(&count) {
        return Err(Error::Invalid("DNS service count"));
    }
    let mut cursor = 12;
    let mut out = Vec::new();
    for _ in 0..count {
        let mut service = String::new();
        loop {
            let length = usize::from(
                *packet
                    .get(cursor)
                    .ok_or(Error::Invalid("truncated DNS query"))?,
            );
            cursor += 1;
            if length == 0 {
                break;
            }
            if length > 63 {
                return Err(Error::Invalid("compressed DNS query unsupported"));
            }
            let label = packet
                .get(cursor..cursor + length)
                .ok_or(Error::Invalid("truncated DNS label"))?;
            let label =
                core::str::from_utf8(label).map_err(|_| Error::Invalid("non-text DNS service"))?;
            service = join(&[&service, label, "."])?;
            cursor += length;
        }
        if packet.get(cursor..cursor + 4) != Some(&[0, 12, 0, 1])
            || service.len() > 253
            || !service.ends_with(".local.")
            || !service.starts_with('_')
        {
            return Err(Error::Invalid("invalid DNS-SD service query"));
        }
        cursor += 4;
        json::push(&mut out, service, 12)?;
    }
    if cursor != packet.len() {
        return Err(Error::Invalid("trailing DNS query bytes"));
    }
    Ok(out)
}
fn txt(line: &[u8]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < line.len() {
        if line[i] != b'"' {
            i += 1;
            continue;
        }
        i += 1;
        let mut text = Vec::new();
        while i < line.len() && line[i] != b'"' {
            if line[i] == b'\\' {
                i += 1;
            }
            let Some(byte) = line.get(i) else {
                break;
            };
            json::push(&mut text, *byte, 255)?;
            i += 1;
        }
        if i >= line.len() {
            break;
        }
        append(&mut out, &[text.len() as u8])?;
        append(&mut out, &text)?;
        i += 1;
    }
    Ok(out)
}
/// SRV/TXT-regels worden begrensde DNS-antwoorden, met impliciete PTR zoals de Go-collector.
pub fn zone(service: &str, output: &[u8]) -> Result<Vec<Datagram>> {
    if output.len() > MAX_ZONE || !service.ends_with(".local.") {
        return Err(Error::Invalid("invalid DNS-SD zone"));
    }
    let short = service.trim_end_matches(".local.");
    let suffix = join(&[".", short])?;
    let mut packets = Vec::new();
    for line in output.split(|b| *b == b'\n') {
        let mut fields = line
            .split(u8::is_ascii_whitespace)
            .filter(|f| !f.is_empty());
        let Some(owner) = fields
            .next()
            .and_then(|b| core::str::from_utf8(b).ok())
            .filter(|s| s.ends_with(&suffix))
        else {
            continue;
        };
        let Some(kind) = fields.next() else {
            continue;
        };
        let instance = join(&[owner, ".local."])?;
        let mut data = Vec::new();
        let kind = match kind {
            b"SRV" => {
                let priority = fields.next().and_then(number);
                let weight = fields.next().and_then(number);
                let port = fields.next().and_then(number);
                let host = fields.next().and_then(|b| core::str::from_utf8(b).ok());
                let (Some(priority), Some(weight), Some(port), Some(host)) =
                    (priority, weight, port, host)
                else {
                    continue;
                };
                append(&mut data, &priority.to_be_bytes())?;
                append(&mut data, &weight.to_be_bytes())?;
                append(&mut data, &port.to_be_bytes())?;
                name(&mut data, host)?;
                33
            }
            b"TXT" => {
                data = txt(line)?;
                16
            }
            _ => continue,
        };
        let mut payload = Vec::new();
        append(&mut payload, &[0, 0, 0x84, 0, 0, 0, 0, 2, 0, 0, 0, 0])?;
        let mut pointer = Vec::new();
        name(&mut pointer, &instance)?;
        record(&mut payload, service, 12, &pointer)?;
        record(&mut payload, &instance, kind, &data)?;
        json::push(
            &mut packets,
            Datagram {
                source: json::copy("127.0.0.1:5353")?,
                interface: 0,
                payload,
            },
            256,
        )?;
    }
    Ok(packets)
}
fn number(bytes: &[u8]) -> Option<u16> {
    core::str::from_utf8(bytes).ok()?.parse().ok()
}
#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    #[test]
    fn txt_preserves_binary_bytes_spaces_escapes_and_ignores_truncation() {
        let value = txt(b"X TXT \"nn=MyHome12\" \"xp=\xb4\x44\x83\x6c\x32\x60\x4a\x7f\" \"DN=Aqara switch\" \"q=\\\"\" \"truncated").unwrap();
        assert!(
            value
                .windows(11)
                .any(|v| v == b"xp=\xb4\x44\x83\x6c\x32\x60\x4a\x7f")
        );
        assert!(value.windows(15).any(|v| v == b"DN=Aqara switch"));
        assert!(!value.windows(9).any(|v| v == b"truncated"));
    }
}
