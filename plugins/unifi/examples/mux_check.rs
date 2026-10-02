//! Synthetisch camerabeeld uit Go/ffmpeg door de Rust-container schrijven.
use std::io::{BufRead, Write};
use stulp_core::json::{self, Value};
use stulp_protocol::token;
use stulp_unifi::rtsp::Muxer;
fn main() -> Result<(), Box<dyn std::error::Error>> {
    for line in std::io::stdin().lock().lines() {
        let line = line?;
        if line.len() > json::MAX_DOCUMENT {
            return Err("fixture too large".into());
        }
        let v = json::parse(line.as_bytes())?;
        let out = process(&v)?;
        writeln!(std::io::stdout().lock(), "{}", json::to_string(&out)?)?;
    }
    Ok(())
}

fn process(v: &Value) -> stulp_sdk::Result<Value> {
    let mut mux = if json::text(v, "codec") == "av1" {
        Muxer::av1(&token::decode(json::text(v, "sequence"))?)?
    } else {
        Muxer::h264(
            &token::decode(json::text(v, "sps"))?,
            &token::decode(json::text(v, "pps"))?,
        )?
    };
    let mut fragments = Vec::new();
    for frame in json::array(v, "frames") {
        let mut parts = Vec::new();
        for part in json::array(frame, "parts") {
            json::push(
                &mut parts,
                token::decode(
                    part.as_str()
                        .ok_or(stulp_sdk::Error::Invalid("part is not base64"))?,
                )?,
                128,
            )?;
        }
        let fragment = mux.fragment(
            &parts,
            u32::try_from(json::uint(frame, "timestamp"))
                .map_err(|_| stulp_sdk::Error::Invalid("timestamp out of range"))?,
        )?;
        json::push(
            &mut fragments,
            json::string(&token::base64(&fragment)?)?,
            512,
        )?;
    }
    let out = json::fields(&[
        ("header", json::string(&token::base64(mux.header())?)?),
        ("mime", json::string(mux.mime())?),
        ("fragments", Value::Array(fragments)),
    ])?;
    Ok(out)
}
