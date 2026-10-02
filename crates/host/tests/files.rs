//! Een tweede controller mag geen verouderde kopie over de eerste heen schrijven.
#![allow(clippy::unwrap_used)]
use stulp_core::store::Storage;
use stulp_host::files::Files;
#[test]
fn exclusive_document_owner_survives_atomic_replacement_and_releases_on_drop() {
    let path = std::env::temp_dir().join(format!("stulp-lock-test-{}", std::process::id()));
    std::fs::create_dir(&path).unwrap();
    let doc = path.join("test.json");
    let mut first = Files::new(&doc).unwrap();
    assert!(Files::new(&doc).is_err());
    first.save(b"{\"version\":2}").unwrap();
    assert!(Files::new(&doc).is_err());
    assert_eq!(first.read().unwrap(), b"{\"version\":2}");
    drop(first);
    let second = Files::new(&doc).unwrap();
    drop(second);
    std::fs::remove_dir_all(path).unwrap();
}
