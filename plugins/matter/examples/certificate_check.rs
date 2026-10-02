//! Zelfstandige Go-verificatie van Rust-certificaten; vaste sleutels zijn uitsluitend testdata.
use p256::{SecretKey, elliptic_curve::sec1::ToEncodedPoint};
use stulp_matter::certificate::{Certificate, private_der};
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root_key = SecretKey::from_slice(&[1; 32]).map_err(|_| "test root key")?;
    let key = SecretKey::from_slice(&[2; 32]).map_err(|_| "test node key")?;
    let root = Certificate::root(&root_key, 0x2222, [3; 16], 800000000, 1500000000)?;
    let node = root.issue(
        &root_key,
        key.public_key().to_encoded_point(false).as_bytes(),
        (0x3333, 0x1111),
        [4; 16],
        (800000000, 1500000000),
    )?;
    println!(
        "{}\n{}\n{}\n{}\n{}",
        hex(&root.der()?),
        hex(&root.tlv()?),
        hex(&node.der()?),
        hex(&node.tlv()?),
        hex(&private_der(&key)?)
    );
    Ok(())
}
