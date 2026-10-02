//! Native TZif discovery; decoding is shared with HopOS.
pub(crate) use stulp_controller::timezone::Timezone;
use stulp_core::{Error, Result};
pub(crate) fn load(name: &str) -> Result<Timezone> {
    if name.is_empty() || matches!(name, "UTC" | "Etc/UTC") {
        return Ok(Timezone::default());
    }
    if name == "Local" {
        return Timezone::decode(&read(std::path::Path::new("/etc/localtime"))?);
    }
    if name.starts_with('/')
        || name
            .split('/')
            .any(|p| p.is_empty() || matches!(p, "." | ".."))
    {
        return Err(Error::Invalid("invalid timezone name"));
    }
    for root in ["/usr/share/zoneinfo", "/var/db/timezone/zoneinfo"] {
        let path = std::path::Path::new(root).join(name);
        if path.is_file() {
            return Timezone::decode(&read(&path)?);
        }
    }
    Err(Error::Missing("timezone not installed"))
}

fn read(path: &std::path::Path) -> Result<Vec<u8>> {
    use std::io::Read;
    let file =
        std::fs::File::open(path).map_err(|_| Error::Missing("timezone file unavailable"))?;
    let mut bytes = Vec::new();
    bytes
        .try_reserve_exact(1_048_577)
        .map_err(|_| Error::Memory)?;
    file.take(1_048_577)
        .read_to_end(&mut bytes)
        .map_err(|_| Error::Invalid("timezone read failed"))?;
    if bytes.len() > 1_048_576 {
        return Err(Error::Full);
    }
    Ok(bytes)
}
