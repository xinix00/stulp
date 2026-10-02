use super::{Media, copy};
use alloc::{string::String, vec::Vec};
use core::fmt::Write;
use stulp_core::json;
use stulp_sdk::{Error, Result, util::join};
/// Een RTSP-antwoord of één ingesloten RTP/RTCP-pakket.
pub enum Message {
    /// Response hoort bij precies één CSeq.
    Response {
        /// Numerieke status.
        status: u16,
        /// Bijbehorende request.
        sequence: u32,
        /// Eventueel toegewezen sessie-id.
        session: String,
        /// SDP of andere responsebody.
        body: Vec<u8>,
    },
    /// Kanaal nul draagt video, kanaal één RTCP.
    Interleaved {
        /// Onderhandeld kanaal.
        channel: u8,
        /// Volledige RTP/RTCP-payload.
        bytes: Vec<u8>,
    },
}
/// Incrementeel, dus TCP-fragmentatie en responses tussen RTP-pakketten blijven geldig.
#[derive(Default)]
pub struct Decoder {
    buffer: Vec<u8>,
}
impl Decoder {
    /// Bytes worden vóór allocatie begrensd.
    pub fn feed(&mut self, input: &[u8]) -> Result {
        if input.len() > (128usize << 10).saturating_sub(self.buffer.len()) {
            return Err(Error::Invalid("RTSP buffer full"));
        }
        self.buffer
            .try_reserve(input.len())
            .map_err(|_| stulp_core::Error::Memory)?;
        self.buffer.extend_from_slice(input);
        Ok(())
    }
    /// Eén compleet bericht; onvolledige invoer blijft in de eigenaar.
    pub fn next_message(&mut self) -> Result<Option<Message>> {
        if self.buffer.first() == Some(&b'$') {
            if self.buffer.len() < 4 {
                return Ok(None);
            }
            let n = usize::from(u16::from_be_bytes([self.buffer[2], self.buffer[3]]));
            if self.buffer.len() < n + 4 {
                return Ok(None);
            }
            let msg = Message::Interleaved {
                channel: self.buffer[1],
                bytes: copy(&self.buffer[4..n + 4])?,
            };
            self.buffer.drain(..n + 4);
            return Ok(Some(msg));
        }
        let Some(end) = self
            .buffer
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|n| n + 4)
        else {
            if self.buffer.len() > 16384 {
                return Err(Error::Invalid("RTSP headers too large"));
            }
            return Ok(None);
        };
        if end > 16384 {
            return Err(Error::Invalid("RTSP headers too large"));
        }
        let text = core::str::from_utf8(&self.buffer[..end])
            .map_err(|_| Error::Invalid("invalid RTSP headers"))?;
        let mut lines = text.split("\r\n");
        let mut status = lines.next().unwrap_or("").split_whitespace();
        if status.next() != Some("RTSP/1.0") {
            return Err(Error::Invalid("invalid RTSP status line"));
        }
        let status = status
            .next()
            .and_then(|v| v.parse::<u16>().ok())
            .filter(|n| (100..600).contains(n))
            .ok_or(Error::Invalid("invalid RTSP status"))?;
        let mut length = None;
        let mut sequence = None;
        let mut session = "";
        for line in lines.filter(|line| !line.is_empty()) {
            let (key, value) = line
                .split_once(':')
                .ok_or(Error::Invalid("invalid RTSP header"))?;
            let value = value.trim();
            if key.eq_ignore_ascii_case("Content-Length") {
                if length.is_some() {
                    return Err(Error::Invalid("duplicate RTSP length"));
                }
                length = Some(
                    value
                        .parse::<usize>()
                        .ok()
                        .filter(|n| *n <= 65535)
                        .ok_or(Error::Invalid("invalid RTSP length"))?,
                );
            } else if key.eq_ignore_ascii_case("CSeq") {
                if sequence.is_some() {
                    return Err(Error::Invalid("duplicate RTSP sequence"));
                }
                sequence = Some(
                    value
                        .parse::<u32>()
                        .map_err(|_| Error::Invalid("invalid RTSP sequence"))?,
                );
            } else if key.eq_ignore_ascii_case("Session") {
                if !session.is_empty() {
                    return Err(Error::Invalid("duplicate RTSP session"));
                }
                session = value.split(';').next().unwrap_or("").trim();
                if session.len() > 256 || session.bytes().any(|b| b <= 32 || b == 127) {
                    return Err(Error::Invalid("invalid RTSP session"));
                }
            } else if key.eq_ignore_ascii_case("Transport") && !value.contains("interleaved=0-1") {
                return Err(Error::Invalid(
                    "camera did not select RTP over TCP channels 0-1",
                ));
            }
        }
        let n = end + length.unwrap_or(0);
        if self.buffer.len() < n {
            return Ok(None);
        }
        let message = Message::Response {
            status,
            sequence: sequence.ok_or(Error::Invalid("missing RTSP CSeq"))?,
            session: json::copy(session)?,
            body: copy(&self.buffer[end..n])?,
        };
        self.buffer.drain(..n);
        Ok(Some(message))
    }
}
/// RTSP-handshake en keepalive; de caller bezit de TCP/TLS-verbinding en deadlines.
pub struct Session {
    /// Host zonder vierkante haken.
    pub host: String,
    /// 554 voor RTSP, 322 voor RTSPS tenzij opgegeven.
    pub port: u16,
    /// Device-certificate TLS voor de lokale UniFi-console.
    pub tls: bool,
    target: String,
    auth: String,
    session: String,
    sequence: u32,
    expected: Option<u32>,
    step: u8,
    media: Option<Media>,
}
impl Session {
    /// Parseert een streamadres zonder credentials in de request-target te laten staan.
    pub fn new(address: &str) -> Result<Self> {
        if address.len() > 4096 || address.bytes().any(|b| b <= 32 || b == 127) {
            return Err(Error::Invalid("invalid RTSP address"));
        }
        let (tls, rest) = if let Some(v) = address.strip_prefix("rtsps://") {
            (true, v)
        } else if let Some(v) = address.strip_prefix("rtsp://") {
            (false, v)
        } else {
            return Err(Error::Invalid("unsupported RTSP scheme"));
        };
        let at = rest.find(['/', '?', '#']).unwrap_or(rest.len());
        let (authority, path) = rest.split_at(at);
        if path.contains('#') {
            return Err(Error::Invalid("RTSP URL fragments are not supported"));
        }
        let mut auth = String::new();
        let authority = if let Some((user, host)) = authority.rsplit_once('@') {
            let mut encoded = String::new();
            encoded
                .try_reserve(user.len().saturating_mul(3))
                .map_err(|_| stulp_core::Error::Memory)?;
            for c in user.chars() {
                if c == '+' {
                    encoded.push_str("%2B");
                } else {
                    encoded.push(c);
                }
            }
            let decoded = stulp_sdk::util::unquery(&encoded)?;
            auth = join(&["Basic ", &crate::ws::base64(decoded.as_bytes())?])?;
            host
        } else {
            authority
        };
        let (host, port) = if let Some(host) = authority.strip_prefix('[') {
            let (host, tail) = host
                .split_once(']')
                .ok_or(Error::Invalid("invalid RTSP IPv6 host"))?;
            (
                host,
                tail.strip_prefix(':')
                    .unwrap_or(if tail.is_empty() { "" } else { "invalid" }),
            )
        } else {
            authority.split_once(':').unwrap_or((authority, ""))
        };
        if host.is_empty() || host.contains(['[', ']', '@']) {
            return Err(Error::Invalid("invalid RTSP host"));
        }
        let port = if port.is_empty() {
            if tls { 322 } else { 554 }
        } else {
            port.parse::<u16>()
                .ok()
                .filter(|p| *p != 0)
                .ok_or(Error::Invalid("invalid RTSP port"))?
        };
        Ok(Self {
            host: json::copy(host)?,
            port,
            tls,
            target: join(&[if tls { "rtsps://" } else { "rtsp://" }, authority, path])?,
            auth,
            session: String::new(),
            sequence: 0,
            expected: None,
            step: 0,
            media: None,
        })
    }
    /// Is PLAY bevestigd?
    pub fn ready(&self) -> bool {
        self.step == 3
    }
    /// Het onderhandelde videospoor na DESCRIBE.
    pub fn media(&self) -> Option<&Media> {
        self.media.as_ref()
    }
    fn request(&mut self, method: &str, target: &str, headers: &str) -> Result<Vec<u8>> {
        if self.expected.is_some() {
            return Err(Error::Invalid("RTSP response still pending"));
        }
        let sequence = self
            .sequence
            .checked_add(1)
            .ok_or(Error::Invalid("RTSP sequence exhausted"))?;
        let mut out = String::new();
        out.try_reserve(target.len() + self.auth.len() + self.session.len() + headers.len() + 128)
            .map_err(|_| stulp_core::Error::Memory)?;
        write!(
            &mut out,
            "{method} {target} RTSP/1.0\r\nCSeq: {sequence}\r\nUser-Agent: Stulp\r\n"
        )
        .map_err(|_| Error::Invalid("RTSP formatting failed"))?;
        for (key, value) in [("Session", &self.session), ("Authorization", &self.auth)] {
            if !value.is_empty() {
                write!(&mut out, "{key}: {value}\r\n")
                    .map_err(|_| Error::Invalid("RTSP formatting failed"))?;
            }
        }
        out.push_str(headers);
        out.push_str("\r\n");
        self.sequence = sequence;
        self.expected = Some(sequence);
        Ok(out.into_bytes())
    }
    /// De eerste bytes na het openen van de socket.
    pub fn start(&mut self) -> Result<Vec<u8>> {
        let target = json::copy(&self.target)?;
        self.request("DESCRIBE", &target, "Accept: application/sdp\r\n")
    }
    /// Verwerkt uitsluitend de verwachte response en levert de volgende handshakestap.
    pub fn response(&mut self, message: Message) -> Result<Option<Vec<u8>>> {
        let Message::Response {
            status,
            sequence,
            session,
            body,
        } = message
        else {
            return Err(Error::Invalid("RTP is not an RTSP response"));
        };
        if self.expected != Some(sequence) {
            return Err(Error::Invalid("RTSP sequence mismatch"));
        }
        self.expected = None;
        if status != 200 {
            return Err(Error::Invalid("camera rejected RTSP request"));
        }
        if !session.is_empty() {
            if !self.session.is_empty() && session != self.session {
                return Err(Error::Invalid("camera changed RTSP session"));
            }
            self.session = session;
        }
        match self.step {
            0 => {
                let media = Media::parse(
                    core::str::from_utf8(&body)
                        .map_err(|_| Error::Invalid("invalid SDP encoding"))?,
                )?;
                let target = if media.control.starts_with("rtsp://")
                    || media.control.starts_with("rtsps://")
                {
                    json::copy(&media.control)?
                } else if media.control.is_empty() || media.control == "*" {
                    json::copy(&self.target)?
                } else {
                    join(&[
                        self.target.trim_end_matches('/'),
                        "/",
                        media.control.trim_start_matches('/'),
                    ])?
                };
                self.media = Some(media);
                self.step = 1;
                self.request(
                    "SETUP",
                    &target,
                    "Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n",
                )
                .map(Some)
            }
            1 => {
                if self.session.is_empty() {
                    return Err(Error::Invalid("SETUP returned no session"));
                }
                self.step = 2;
                let target = json::copy(&self.target)?;
                self.request("PLAY", &target, "").map(Some)
            }
            2 => {
                self.step = 3;
                Ok(None)
            }
            3 => Ok(None),
            _ => Err(Error::Invalid("invalid RTSP state")),
        }
    }
    /// Eén OPTIONS per 30 seconden; de caller bewaakt ook antwoord- en leesdeadlines.
    pub fn keepalive(&mut self) -> Result<Vec<u8>> {
        if !self.ready() {
            return Err(Error::Invalid("camera is not playing"));
        }
        let target = json::copy(&self.target)?;
        self.request("OPTIONS", &target, "")
    }
}
