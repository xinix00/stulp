use super::{Codec, Info};
use alloc::{string::String, vec::Vec};
use stulp_core::json;
use stulp_sdk::{Error, Result};
/// Eén geselecteerd videospoor; geluid krijgt geen toegang tot de beeldassembler.
pub struct Media {
    /// De gekozen codec.
    pub codec: Codec,
    /// Relatief of absoluut RTSP-controladres.
    pub control: String,
    /// RTP payload type.
    pub payload: u8,
    /// H.264 SPS, leeg bij AV1.
    pub sps: Vec<u8>,
    /// H.264 PPS, leeg bij AV1.
    pub pps: Vec<u8>,
}
impl Media {
    /// Selecteert het eerste ondersteunde videospoor, zonder latere audiosporen te mengen.
    pub fn parse(body: &str) -> Result<Self> {
        if body.len() > 65535 {
            return Err(Error::Invalid("SDP too large"));
        }
        for section in body.split("m=").skip(1) {
            let mut lines = section.lines();
            let head = lines.next().unwrap_or("");
            let mut fields = head.split_whitespace();
            if fields.next() != Some("video") {
                continue;
            }
            fields.next();
            let transport = fields.next().unwrap_or("");
            if !matches!(transport, "RTP/AVP" | "RTP/AVP/TCP") {
                continue;
            }
            for id in fields {
                let payload = id
                    .parse::<u8>()
                    .ok()
                    .filter(|n| *n <= 127)
                    .ok_or(Error::Invalid("invalid SDP payload type"))?;
                let mut control = "";
                let mut codec = None;
                let mut sets = None;
                for line in section.lines().skip(1).map(str::trim) {
                    if let Some(value) = line.strip_prefix("a=control:") {
                        control = value.trim();
                    }
                    if let Some(value) = line.strip_prefix("a=rtpmap:")
                        && let Some((pt, value)) = value.split_once(' ')
                        && pt.parse::<u8>().ok() == Some(payload)
                    {
                        codec = match value.trim() {
                            "H264/90000" => Some(Codec::H264),
                            "AV1/90000" => Some(Codec::Av1),
                            _ => None,
                        };
                    }
                    if let Some(value) = line.strip_prefix("a=fmtp:")
                        && let Some((pt, value)) = value.split_once(' ')
                        && pt.parse::<u8>().ok() == Some(payload)
                    {
                        for item in value.split(';').map(str::trim) {
                            if let Some(value) = item.strip_prefix("sprop-parameter-sets=") {
                                sets = value.split_once(',');
                            }
                        }
                    }
                }
                let Some(codec) = codec else {
                    continue;
                };
                if control.bytes().any(|b| b <= 32 || b == 127) {
                    return Err(Error::Invalid("invalid SDP control address"));
                }
                let (sps, pps) = if codec == Codec::H264 {
                    let (sps, pps) =
                        sets.ok_or(Error::Invalid("H.264 camera has no parameter sets"))?;
                    let sps = stulp_protocol::token::decode(sps.trim())?;
                    let pps = stulp_protocol::token::decode(pps.trim())?;
                    Info::h264(&sps)?;
                    if pps.first().is_none_or(|b| b & 31 != 8) {
                        return Err(Error::Invalid("invalid H.264 PPS"));
                    }
                    (sps, pps)
                } else {
                    (Vec::new(), Vec::new())
                };
                return Ok(Self {
                    codec,
                    control: json::copy(control)?,
                    payload,
                    sps,
                    pps,
                });
            }
        }
        Err(Error::Invalid(
            "camera offers no supported H.264 or AV1 video track",
        ))
    }
}
