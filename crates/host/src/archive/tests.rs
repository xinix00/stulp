//! Archiefvalidatie moet falen vóór publicatie; opslagfouten moeten bundels terugzetten.
#![allow(clippy::unwrap_used, clippy::panic)]
use super::*;
use std::io::Cursor;
struct Temp(PathBuf);
impl Temp {
    fn new() -> Self {
        let path = std::env::temp_dir().join(format!(
            "stulp-archive-test-{}",
            crate::Environment.id().unwrap()
        ));
        fs::create_dir(&path).unwrap();
        Self(fs::canonicalize(path).unwrap())
    }
}
impl Drop for Temp {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn archive(extra: &[(&str, u32, &[u8])]) -> Vec<u8> {
    let mut zip = zip::Writer::new(Vec::new());
    zip.add(
        "backup.json",
        0o100600,
        br#"{"format":1,"apps":[]}"#.as_slice(),
    )
    .unwrap();
    zip.add("stulp.json", 0o100600, br#"{"version":2}"#.as_slice())
        .unwrap();
    for (name, mode, data) in extra {
        zip.add(name, *mode, *data).unwrap();
    }
    zip.finish().unwrap()
}
#[test]
fn rejects_traversal_duplicate_special_unknown_and_unowned_entries() {
    let t = Temp::new();
    let path = t.0.join("test.json");
    for name in [
        "../escape",
        "/absolute",
        "apps/../escape",
        "apps\\escape",
        "unknown",
        "stulp.json",
        "apps/000/unowned",
    ] {
        let bytes = archive(&[(name, 0o100600, b"test")]);
        assert!(
            Prepared::read(&mut Cursor::new(bytes), &path).is_err(),
            "{name}"
        );
        assert!(!path.exists());
    }
    assert!(
        Prepared::read(
            &mut Cursor::new(archive(&[("apps/link", 0o120777, b"target")])),
            &path
        )
        .is_err()
    );
    assert!(!t.0.join("escape").exists());
}
#[test]
fn corrupt_data_and_directory_never_replace_current_document() {
    let t = Temp::new();
    let path = t.0.join("test.json");
    fs::write(&path, b"old state").unwrap();
    let good = archive(&[]);
    let index = good.windows(6).position(|b| b == b"format").unwrap();
    let mut bad = good.clone();
    bad[index] = b'F';
    for bytes in [bad, good[..good.len() - 1].to_vec()] {
        assert!(Prepared::read(&mut Cursor::new(bytes), &path).is_err());
        assert_eq!(fs::read(&path).unwrap(), b"old state");
    }
}
struct FailedSave(String);
impl Storage for FailedSave {
    fn path(&self) -> Option<&str> {
        Some(&self.0)
    }
    fn save(&mut self, _: &[u8]) -> stulp_core::Result {
        Err(stulp_core::Error::Storage)
    }
}
#[test]
fn failed_document_publication_restores_old_bundles_and_keeps_runtime_state() {
    let t = Temp::new();
    let path = t.0.join("test.json");
    let apps = t.0.join("test.json.apps");
    fs::create_dir(&apps).unwrap();
    fs::write(apps.join("old"), b"previous bundle").unwrap();
    let mut zip = zip::Writer::new(Vec::new());
    zip.add(
        "backup.json",
        0o100600,
        br#"{"format":1,"apps":[{"id":"app","path":"apps/000"}]}"#.as_slice(),
    )
    .unwrap();
    zip.add(
        "stulp.json",
        0o100600,
        br#"{"version":2,"apps":[{"id":"app","root":"/old","enabled":false}]}"#.as_slice(),
    )
    .unwrap();
    zip.add(
        "apps/000/app.json",
        0o100600,
        br#"{"id":"app","sdk":3,"version":"1","name":{"en":"App"}}"#.as_slice(),
    )
    .unwrap();
    let prepared = Prepared::read(&mut Cursor::new(zip.finish().unwrap()), &path).unwrap();
    let before=br#"{"version":2,"devices":[{"id":"d","appId":"previous","name":"Keep","capabilities":["onoff"]}]}"#;
    fs::write(&path, before).unwrap();
    let mut store = Store::open(before, FailedSave(path.to_str().unwrap().into())).unwrap();
    store
        .observe(
            "previous",
            "d",
            json::parse(br#"{"onoff":true}"#).unwrap(),
            true,
            "",
        )
        .unwrap();
    let sequence = store.sequence();
    let encoded = store.document().encode().unwrap();
    assert!(prepared.apply(&mut store).is_err());
    assert_eq!(fs::read(apps.join("old")).unwrap(), b"previous bundle");
    assert!(!apps.join("000").exists());
    assert_eq!(fs::read(path).unwrap(), before);
    assert_eq!(store.sequence(), sequence);
    assert_eq!(store.document().encode().unwrap(), encoded);
    assert!(json::boolean(&store.device("d").unwrap(), "available"));
}
