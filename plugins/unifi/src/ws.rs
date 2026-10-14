//! Begrensde RFC 6455-clientcodec; sockets en herverbinden horen bij de eigenaar.
use alloc::{string::String, vec::Vec};
use stulp_core::json;
use stulp_sdk::{Error, Result, util::join};
const MAX: usize = 1 << 20;
/// Standaard base64 met padding: de Sec-WebSocket-Key en -Accept, en Basic-auth.
pub(super) fn base64(bytes: &[u8]) -> Result<String> {
    Ok(leanbase64::STANDARD
        .encode(bytes)
        .map_err(|_| stulp_core::Error::Memory)?)
}
// RFC 6455 gebruikt SHA-1 uitsluitend voor de upgradecontrole, niet als authenticatie.
fn accept(key: &str) -> Result<String> {
    if key.len() != 24 {
        return Err(Error::Invalid("invalid websocket nonce"));
    }
    let mut sha1 = leancrypto::sha1::Sha1::new();
    sha1.update(key.as_bytes());
    sha1.update(b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11");
    base64(&sha1.finish())
}
fn append(dst: &mut Vec<u8>, src: &[u8], max: usize) -> Result {
    if src.len() > max.saturating_sub(dst.len()) {
        return Err(Error::Invalid("websocket exceeds receive limit"));
    }
    dst.try_reserve(src.len())
        .map_err(|_| Error::Invalid("websocket allocation failed"))?;
    dst.extend_from_slice(src);
    Ok(())
}
pub(super) fn request(
    host: &str,
    path: &str,
    api_key: &str,
    nonce: &[u8],
) -> Result<(String, Vec<u8>)> {
    if [host, path, api_key]
        .iter()
        .any(|s| s.bytes().any(|b| b < 32 || b == 127))
        || !path.starts_with('/')
    {
        return Err(Error::Invalid("invalid websocket header"));
    }
    let key = base64(nonce)?;
    let expected = accept(&key)?;
    let request = join(&[
        "GET ",
        path,
        " HTTP/1.1\r\nHost: ",
        host,
        "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: ",
        &key,
        "\r\nX-API-KEY: ",
        api_key,
        "\r\n\r\n",
    ])?;
    Ok((expected, request.into_bytes()))
}
pub(super) enum Message {
    Open,
    Text(Vec<u8>),
    Ping(Vec<u8>),
    Pong,
    Close(Vec<u8>),
}
pub(super) struct Decoder {
    input: Vec<u8>,
    fragments: Vec<u8>,
    opcode: u8,
    expected: Option<String>,
}
impl Decoder {
    pub(super) fn new(expected: String) -> Self {
        Self {
            input: Vec::new(),
            fragments: Vec::new(),
            opcode: 0,
            expected: Some(expected),
        }
    }
    pub(super) fn feed(&mut self, bytes: &[u8]) -> Result {
        append(&mut self.input, bytes, MAX + 16398)
    }
    pub(super) fn next(&mut self) -> Result<Option<Message>> {
        if let Some(expected) = &self.expected {
            let Some(end) = self.input.windows(4).position(|b| b == b"\r\n\r\n") else {
                if self.input.len() > 16384 {
                    return Err(Error::Invalid("websocket upgrade header too large"));
                }
                return Ok(None);
            };
            if end > 16380 {
                return Err(Error::Invalid("websocket upgrade header too large"));
            }
            let text = core::str::from_utf8(&self.input[..end])
                .map_err(|_| Error::Invalid("invalid websocket header"))?;
            let mut lines = text.split("\r\n");
            let status = lines.next().unwrap_or("");
            if status.split_whitespace().take(2).ne(["HTTP/1.1", "101"]) {
                return Err(Error::Invalid("websocket upgrade rejected"));
            }
            let mut upgrade = false;
            let mut connection = false;
            let mut accepted = 0;
            for line in lines {
                let (name, value) = line
                    .split_once(':')
                    .ok_or(Error::Invalid("invalid websocket header"))?;
                let value = value.trim();
                if name.eq_ignore_ascii_case("upgrade") {
                    upgrade = value.eq_ignore_ascii_case("websocket");
                }
                if name.eq_ignore_ascii_case("connection") {
                    connection = value
                        .split(',')
                        .any(|v| v.trim().eq_ignore_ascii_case("upgrade"));
                }
                if name.eq_ignore_ascii_case("sec-websocket-accept") {
                    if value != expected {
                        return Err(Error::Invalid("websocket accept mismatch"));
                    }
                    accepted += 1;
                }
                if name.eq_ignore_ascii_case("sec-websocket-extensions")
                    || name.eq_ignore_ascii_case("sec-websocket-protocol")
                {
                    return Err(Error::Invalid("unsolicited websocket extension"));
                }
            }
            if !upgrade || !connection || accepted != 1 {
                return Err(Error::Invalid("incomplete websocket upgrade"));
            }
            self.input.drain(..end + 4);
            self.expected = None;
            return Ok(Some(Message::Open));
        }
        loop {
            if self.input.len() < 2 {
                return Ok(None);
            }
            let a = self.input[0];
            let b = self.input[1];
            let fin = a & 0x80 != 0;
            let op = a & 15;
            if a & 0x70 != 0 || b & 0x80 != 0 || ![0, 1, 2, 8, 9, 10].contains(&op) {
                return Err(Error::Invalid("invalid websocket frame flags"));
            }
            let mut size = usize::from(b & 127);
            let mut head = 2;
            if size == 126 {
                if self.input.len() < 4 {
                    return Ok(None);
                }
                size = usize::from(u16::from_be_bytes([self.input[2], self.input[3]]));
                head = 4;
                if size < 126 {
                    return Err(Error::Invalid("noncanonical websocket length"));
                }
            } else if size == 127 {
                if self.input.len() < 10 {
                    return Ok(None);
                }
                size = usize::try_from(u64::from_be_bytes(
                    self.input[2..10]
                        .try_into()
                        .map_err(|_| Error::Invalid("websocket length"))?,
                ))
                .map_err(|_| Error::Invalid("websocket length overflow"))?;
                head = 10;
                if size <= 65535 {
                    return Err(Error::Invalid("noncanonical websocket length"));
                }
            }
            if size > MAX || (op >= 8 && (!fin || size > 125)) {
                return Err(Error::Invalid("websocket frame exceeds limit"));
            }
            if self.input.len() < head + size {
                return Ok(None);
            }
            let body = &self.input[head..head + size];
            let mut out = Vec::new();
            if op >= 8 {
                append(&mut out, body, 125)?;
            } else {
                if (op == 0 && self.opcode == 0) || (op != 0 && self.opcode != 0) {
                    return Err(Error::Invalid("invalid websocket continuation"));
                }
                if op != 0 {
                    self.opcode = op;
                }
                append(&mut self.fragments, body, MAX)?;
                if fin {
                    if self.opcode == 1 {
                        core::str::from_utf8(&self.fragments)
                            .map_err(|_| Error::Invalid("invalid websocket text"))?;
                    }
                    self.opcode = 0;
                    out = core::mem::take(&mut self.fragments);
                }
            }
            self.input.drain(..head + size);
            match op {
                8 => {
                    if out.len() == 1 {
                        return Err(Error::Invalid("invalid websocket close"));
                    }
                    if out.len() >= 2 {
                        core::str::from_utf8(&out[2..])
                            .map_err(|_| Error::Invalid("invalid websocket close reason"))?;
                    }
                    return Ok(Some(Message::Close(out)));
                }
                9 => return Ok(Some(Message::Ping(out))),
                10 => return Ok(Some(Message::Pong)),
                _ if fin => return Ok(Some(Message::Text(out))),
                _ => (),
            }
        }
    }
}
pub(super) fn control(op: u8, body: &[u8], mask: [u8; 4]) -> Result<Vec<u8>> {
    if ![8, 9, 10].contains(&op) || body.len() > 125 {
        return Err(Error::Invalid("invalid websocket control"));
    }
    let mut out = Vec::new();
    append(&mut out, &[0x80 | op, 0x80 | body.len() as u8], 131)?;
    append(&mut out, &mask, 131)?;
    for (i, b) in body.iter().enumerate() {
        json::push(&mut out, b ^ mask[i % 4], 131)?;
    }
    Ok(out)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn upgrade_fragmented_message_and_interleaved_ping() -> Result {
        assert_eq!(
            accept("dGhlIHNhbXBsZSBub25jZQ==")?,
            "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
        );
        let mut d = Decoder::new(accept("dGhlIHNhbXBsZSBub25jZQ==")?);
        let h=b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n";
        for byte in &h[..h.len() - 1] {
            d.feed(&[*byte])?;
            assert!(d.next()?.is_none());
        }
        d.feed(&[
            10, 1, 2, b'h', b'e', 0x89, 1, b'?', 0x80, 3, b'l', b'l', b'o',
        ])?;
        assert!(matches!(d.next()?, Some(Message::Open)));
        assert!(matches!(d.next()?,Some(Message::Ping(b)) if b==b"?"));
        assert!(matches!(d.next()?,Some(Message::Text(b)) if b==b"hello"));
        assert!(d.next()?.is_none());
        assert_eq!(
            control(10, b"?", [1, 2, 3, 4])?,
            [0x8a, 0x81, 1, 2, 3, 4, b'?' ^ 1]
        );
        Ok(())
    }
    #[test]
    fn rejects_masking_overflow_continuations_and_failed_upgrade() -> Result {
        for bytes in [
            &[0x81, 0x80][..],
            &[0x80, 0][..],
            &[0x09, 0][..],
            &[0x81, 127, 0, 0, 0, 0, 0, 0x20, 0, 0][..],
            &[0x81, 126, 0, 1][..],
        ] {
            let mut d = Decoder::new(String::new());
            d.expected = None;
            d.feed(bytes)?;
            assert!(d.next().is_err());
        }
        let mut d = Decoder::new(json::copy("expected")?);
        d.feed(b"HTTP/1.1 101 Upgrade\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n")?;
        assert!(d.next().is_err());
        Ok(())
    }
}
