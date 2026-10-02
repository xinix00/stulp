//! Begrensde RTSP/RTP- en cameracontainerlogica, zonder sockets of gedeelde staat.
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
mod camera;
mod codec;
pub use camera::{Camera, Output};
mod mux;
mod rtp;
mod sdp;
mod wire;
pub use codec::{Codec, Info};
pub use mux::Muxer;
pub use rtp::{Assembler, Packet, Unit};
pub use sdp::Media;
pub use wire::{Decoder, Message, Session};
/// Een toegangseenheid blijft begrensd, ook zonder marker of na een kapotte camera.
pub const MAX_FRAME: usize = 8 << 20;
pub(super) fn bytes(out: &mut Vec<u8>, input: &[u8]) -> Result {
    if input.len() > MAX_FRAME.saturating_sub(out.len()) {
        return Err(Error::Invalid("camera frame too large"));
    }
    out.try_reserve(input.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    out.extend_from_slice(input);
    Ok(())
}
pub(super) fn copy(input: &[u8]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    bytes(&mut out, input)?;
    Ok(out)
}
pub(super) fn bad<T>() -> Result<T> {
    Err(Error::Invalid("malformed camera bitstream"))
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic)]
mod tests {
    use super::*;
    use alloc::{string::String, vec};
    fn packet(sequence: u16, timestamp: u32, marker: bool, payload: &[u8]) -> Packet<'_> {
        Packet {
            payload_type: 96,
            source: 1,
            sequence,
            timestamp,
            marker,
            payload,
        }
    }
    #[test]
    fn fragments_markers_timestamps_loss_and_wrap() {
        let mut a = Assembler::new(Codec::H264, 96);
        assert!(
            a.push(packet(65534, 100, false, &[0x7c, 0x85, 1, 2]))
                .unwrap()
                .is_empty()
        );
        let done = a
            .push(packet(65535, 100, true, &[0x7c, 0x45, 3, 4]))
            .unwrap();
        assert_eq!(done[0].parts, vec![vec![0x65, 1, 2, 3, 4]]);
        assert_eq!(done[0].timestamp, 100);
        let done = a.push(packet(0, 3700, true, &[0x41, 9])).unwrap();
        assert_eq!(done[0].timestamp, 3700);
        assert!(
            a.push(packet(1, 7300, false, &[0x7c, 0x85, 1]))
                .unwrap()
                .is_empty()
        );
        assert!(
            a.push(packet(3, 7300, true, &[0x7c, 0x45, 3]))
                .unwrap()
                .is_empty()
        );
        assert!(
            a.push(packet(4, 10900, false, &[0x65, 8]))
                .unwrap()
                .is_empty()
        );
        let done = a.push(packet(5, 14500, true, &[0x41, 7])).unwrap();
        assert_eq!(done.len(), 2);
        assert_eq!(done[0].timestamp, 10900);
        assert_eq!(done[1].timestamp, 14500);
        assert!(
            a.push(packet(5, 14500, true, &[0x41, 7]))
                .unwrap()
                .is_empty()
        );
    }
    #[test]
    fn av1_aggregation_fragmentation_and_real_keyframe_detection() {
        let mut a = Assembler::new(Codec::Av1, 96);
        assert!(
            a.push(packet(1, 100, false, &[0x50, 0x30, 0]))
                .unwrap()
                .is_empty()
        );
        let done = a.push(packet(2, 100, true, &[0x90, 1, 2])).unwrap();
        assert_eq!(done[0].parts, vec![vec![0x30, 0, 1, 2]]);
        assert!(codec::keyframe(Codec::Av1, &done[0].parts));
        let done = a
            .push(packet(3, 200, true, &[0x20, 2, 0x08, 1, 0x30, 0x20]))
            .unwrap();
        assert_eq!(a.sequence_header(), &[0x08, 1]);
        assert!(!codec::keyframe(Codec::Av1, &done[0].parts));
        assert!(
            a.push(packet(4, 300, true, &[0x10, 0x10]))
                .unwrap()
                .is_empty()
        );
        assert!(a.push(packet(5, 400, true, &[0x90, 3])).unwrap().is_empty());
    }
    #[test]
    fn rtp_bounds_extensions_and_padding() {
        let bytes = [
            0xb1, 0xe0, 0, 1, 0, 0, 0, 9, 0, 0, 0, 1, 1, 2, 3, 4, 0, 0, 0, 1, 1, 2, 3, 4, 0x65, 9,
            0, 2,
        ];
        let p = Packet::parse(&bytes).unwrap();
        assert_eq!(p.payload, &[0x65, 9]);
        assert!(p.marker);
        for n in 0..25 {
            assert!(Packet::parse(&bytes[..n]).is_err());
        }
        let mut bad = bytes;
        bad[27] = 0;
        assert!(Packet::parse(&bad).is_err());
    }
    #[test]
    fn fragmented_rtsp_handshake_and_interleaved_keepalive_body() {
        let mut s = Session::new("rtsps://name:p%2Bss@127.0.0.1:7441/live").unwrap();
        let request = String::from_utf8(s.start().unwrap()).unwrap();
        assert!(!request.contains("name:"));
        assert!(request.contains("Authorization: Basic bmFtZTpwK3Nz"));
        let body = "v=0\nm=video 0 RTP/AVP 96 97\na=control:track0\na=rtpmap:96 unsupported/90000\na=rtpmap:97 AV1/90000\nm=audio 0 RTP/AVP 98\na=control:audio\n";
        let response = std::format!(
            "RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: {}\r\n\r\n{}",
            body.len(),
            body
        );
        let mut d = Decoder::default();
        for byte in &response.as_bytes()[..response.len() - 1] {
            d.feed(&[*byte]).unwrap();
            assert!(d.next_message().unwrap().is_none());
        }
        d.feed(&response.as_bytes()[response.len() - 1..]).unwrap();
        let setup = String::from_utf8(
            s.response(d.next_message().unwrap().unwrap())
                .unwrap()
                .unwrap(),
        )
        .unwrap();
        assert!(setup.starts_with("SETUP rtsps://127.0.0.1:7441/live/track0 "));
        assert_eq!(s.media().unwrap().payload, 97);
        d.feed(b"RTSP/1.0 200 OK\r\nCSeq: 2\r\nSession: abc;timeout=60\r\nTransport: RTP/AVP/TCP;interleaved=0-1\r\n\r\n").unwrap();
        let play = String::from_utf8(
            s.response(d.next_message().unwrap().unwrap())
                .unwrap()
                .unwrap(),
        )
        .unwrap();
        assert!(play.contains("Session: abc"));
        d.feed(b"RTSP/1.0 200 OK\r\nCSeq: 3\r\n\r\n").unwrap();
        s.response(d.next_message().unwrap().unwrap()).unwrap();
        assert!(s.ready());
        s.keepalive().unwrap();
        d.feed(b"RTSP/1.0 200 OK\r\nCSeq: 4\r\nContent-Length: 3\r\n\r\nabc$\x00\x00\x02hi")
            .unwrap();
        s.response(d.next_message().unwrap().unwrap()).unwrap();
        assert!(
            matches!(d.next_message().unwrap(),Some(Message::Interleaved {channel:0,bytes}) if bytes==b"hi")
        );
        d.feed(b"RTSP/1.0 200 OK\r\nCSeq: 5\r\nContent-Length: 0\r\nContent-Length: 8\r\n\r\n")
            .unwrap();
        assert!(d.next_message().is_err());
    }
}
