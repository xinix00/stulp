//! Onafhankelijke Go-peer via stdin/stdout; uitsluitend synthetische testgeheimen.
use std::io::{BufRead, Write};
use stulp_matter::{
    message::{Header, Message, Protocol},
    spake::{Prover, Scalars, context},
};
fn hex(b: &[u8]) -> String {
    b.iter().map(|b| format!("{b:02x}")).collect()
}
fn decode(s: &str) -> Result<Vec<u8>, Box<dyn std::error::Error>> {
    if s.len() > 4096 || !s.len().is_multiple_of(2) {
        return Err("bad hex length".into());
    }
    s.as_bytes()
        .chunks_exact(2)
        .map(|b| Ok(u8::from_str_radix(std::str::from_utf8(b)?, 16)?))
        .collect()
}
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let p = Prover::new(
        context(b"synthetic request", b"synthetic response"),
        Scalars::derive(20202021, b"SPAKE2P Key Salt!", 1000)?,
        [1; 32],
    )?;
    println!("{}", hex(p.share()));
    std::io::stdout().flush()?;
    let mut input = String::new();
    std::io::stdin().lock().read_line(&mut input)?;
    let (share, confirmation) = input
        .trim()
        .split_once(' ')
        .ok_or("missing peer confirmation")?;
    let (a, keys) = p.finish(&decode(share)?, &decode(confirmation)?)?;
    let m = Message {
        header: Header {
            session: 17,
            counter: 345,
            ..Header::default()
        },
        protocol: Protocol {
            initiator: true,
            reliable: true,
            exchange: 456,
            opcode: 5,
            protocol: 1,
            ..Protocol::default()
        },
        payload: b"independent encrypted payload".to_vec(),
    };
    println!(
        "{} {} {} {} {}",
        hex(&a),
        hex(&keys.i2r),
        hex(&keys.r2i),
        hex(&keys.challenge),
        hex(&m.seal(&keys.i2r, 0x1122334455667788)?)
    );
    Ok(())
}
