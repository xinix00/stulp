//! Compatible ZIP backups for controllers whose plugins run in external slots.
//! Local executable bundles must be restored by a platform with bundle storage.
mod inflate;
mod io;
mod zip;
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    document::Document,
    json::{self, Value},
};
/// Write a Go-compatible archive for an external-slot controller.
pub fn write_external(document: &str, created: &str) -> Result<Vec<u8>> {
    let doc = Document::decode(document.as_bytes())?;
    let mut apps = Vec::new();
    for app in doc.records("apps") {
        if !json::text(app, "root").is_empty() {
            return Err(Error::Invalid(
                "backup contains local app bundles; use a bundle-capable host",
            ));
        }
        json::push(
            &mut apps,
            json::fields(&[("id", json::string(json::text(app, "id"))?)])?,
            4096,
        )?;
    }
    let meta = json::to_string(&json::fields(&[
        ("format", Value::uint(1)),
        ("createdAt", json::string(created)?),
        ("apps", Value::Array(apps)),
    ])?)?;
    let mut zip = zip::Writer::new(Vec::new());
    zip.add("backup.json", 0o100600, meta.as_bytes())?;
    zip.add("stulp.json", 0o100600, document.as_bytes())?;
    zip.finish()
}
/// Validate paths, checksums and app identities before returning the replacement document.
pub fn read_external(archive: &[u8]) -> Result<String> {
    let mut source = io::Cursor::new(archive);
    let entries = zip::directory(&mut source)?;
    if entries.len() != 2
        || entries
            .iter()
            .any(|e| !matches!(e.name.as_str(), "backup.json" | "stulp.json"))
        || entries[0].name == entries[1].name
    {
        return Err(Error::Invalid(
            "expected external-slot backup without local app bundles",
        ));
    }
    let mut read = |name: &str, cap| {
        entries
            .iter()
            .find(|e| e.name == name)
            .ok_or(Error::Missing("backup entry"))?
            .bytes(&mut source, cap)
    };
    let meta = json::parse(&read("backup.json", json::MAX_DOCUMENT)?)?;
    if json::uint(&meta, "format") != 1 {
        return Err(Error::Invalid("unsupported backup format"));
    }
    let bytes = read("stulp.json", stulp_core::document::MAX_BYTES)?;
    let doc = Document::decode(&bytes)?;
    let apps = json::array(&meta, "apps");
    if apps.len() != doc.records("apps").len() {
        return Err(Error::Invalid("backup app count mismatch"));
    }
    let mut ids = Vec::new();
    for entry in apps {
        let id = json::text(entry, "id");
        if id.is_empty()
            || ids.contains(&id)
            || !json::text(entry, "path").is_empty()
            || !json::text(doc.record("apps", id)?, "root").is_empty()
        {
            return Err(Error::Invalid("invalid external backup app"));
        }
        json::push(&mut ids, id, 4096)?;
    }
    doc.encode()
}

/// Decode a bundled IANA timezone; a deployment may override the bundled snapshot.
pub fn timezone(name: &str) -> Result<crate::timezone::Timezone> {
    if name.is_empty() || matches!(name, "UTC" | "Etc/UTC") {
        return Ok(Default::default());
    }
    if name.starts_with('/')
        || name
            .split('/')
            .any(|p| p.is_empty() || matches!(p, "." | ".."))
    {
        return Err(Error::Invalid("invalid timezone name"));
    }
    let mut source = io::Cursor::new(include_bytes!("../../data/zoneinfo.zip"));
    let entries = zip::directory(&mut source)?;
    let entry = entries
        .iter()
        .find(|e| e.name == name)
        .ok_or(Error::Missing("timezone unavailable"))?;
    crate::timezone::Timezone::decode(&entry.bytes(&mut source, 1 << 20)?)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]
    use super::*;
    #[test]
    fn external_backup_roundtrip_rejects_corruption_and_local_bundles() {
        let input = r#"{"version":2,"apps":[{"id":"com.stulp.virtualdevices","enabled":true}],"system":{"timezone":"Europe/Amsterdam"}}"#;
        let bytes = write_external(input, "2026-10-02T12:00:00Z").unwrap();
        let decoded = read_external(&bytes).unwrap();
        assert_eq!(
            Document::decode(input.as_bytes())
                .unwrap()
                .encode()
                .unwrap(),
            decoded
        );
        let mut corrupt = bytes;
        let at = corrupt.windows(8).position(|w| w == b"\"format\"").unwrap();
        corrupt[at] = b'x';
        assert!(read_external(&corrupt).is_err());
        assert!(
            write_external(
                r#"{"version":2,"apps":[{"id":"local","root":"/data/local"}]}"#,
                "now"
            )
            .is_err()
        );
    }
    #[test]
    fn bundled_timezones_support_dst_without_host_files() {
        let zone = timezone("Europe/Amsterdam").unwrap();
        let winter = zone.local(1_767_268_800).unwrap();
        let summer = zone.local(1_782_907_200).unwrap();
        assert_ne!(winter.hour, summer.hour);
        assert!(timezone("../secret").is_err());
        assert!(timezone("Unknown/Zone").is_err());
    }
}
