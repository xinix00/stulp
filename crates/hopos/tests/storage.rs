//! De echte SDK-stack parkeert boven een async foutinjectie-backend; geen kernel of echte bestanden.
#![allow(clippy::unwrap_used)]
use applib::stacktask::Task;
use std::{
    collections::BTreeMap,
    future::{Future, poll_fn},
    pin::pin,
    task::Poll,
};
use stulp_core::{
    Error, Result, json, slots,
    store::{Storage, Store},
};
use stulp_hopos::storage::{Files, Io, Park};
#[derive(Default)]
struct Disk {
    bytes: BTreeMap<String, Vec<u8>>,
    durable: BTreeMap<String, Vec<u8>>,
    syncs: usize,
    fail_sync: usize,
    reads: usize,
}
async fn later() {
    let mut yielded = false;
    poll_fn(|cx| {
        if yielded {
            Poll::Ready(())
        } else {
            yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await;
}
impl Io for &mut Disk {
    async fn size(&mut self, p: &str) -> Result<Option<u64>> {
        later().await;
        Ok(self.bytes.get(p).map(|v| v.len() as u64))
    }
    async fn read(&mut self, p: &str, off: u64, dst: &mut [u8]) -> Result<usize> {
        later().await;
        self.reads += 1;
        let src = self
            .bytes
            .get(p)
            .ok_or(Error::Storage)?
            .get(off as usize..)
            .ok_or(Error::Storage)?;
        let n = src.len().min(dst.len()).min(997);
        dst[..n].copy_from_slice(&src[..n]);
        Ok(n)
    }
    async fn write(&mut self, p: &str, off: u64, b: &[u8]) -> Result {
        later().await;
        let v = self.bytes.get_mut(p).ok_or(Error::Storage)?;
        if v.len() != off as usize {
            return Err(Error::Storage);
        }
        v.extend_from_slice(b);
        Ok(())
    }
    async fn truncate(&mut self, p: &str) -> Result {
        later().await;
        self.bytes.insert(p.into(), Vec::new());
        Ok(())
    }
    async fn sync(&mut self, p: &str) -> Result {
        later().await;
        self.syncs += 1;
        if self.syncs == self.fail_sync {
            return Err(Error::Storage);
        }
        self.durable
            .insert(p.into(), self.bytes.get(p).ok_or(Error::Storage)?.clone());
        Ok(())
    }
}
#[test]
fn failed_barrier_does_not_publish_or_turn_readback_into_success() {
    let mut disk = Disk {
        fail_sync: 2,
        ..Disk::default()
    };
    // SAFETY: Alleen deze begrensde test bezit de stack; JSON-diepte is twee,
    // frames zijn klein en alle annuleringsfouten verlaten normaal de closure.
    let task = unsafe {
        Task::new(1 << 20, |s| -> Result<(String, u64, u64, bool)> {
            let (files, bytes) =
                slots::Files::open(Files::new(&mut disk, Park(s), "/data/stulp.json")?)?;
            let mut store = Store::open(&bytes, files)?;
            store.system(json::fields(&[("fixture", json::string("committed")?)])?)?;
            let before = store.sequence();
            let failed = store
                .system(json::fields(&[("fixture", json::string("unconfirmed")?)])?)
                .is_err();
            let label = json::text(
                json::get(store.document().root(), "system").ok_or(Error::Changed)?,
                "fixture",
            );
            Ok((json::copy(label)?, before, store.sequence(), failed))
        })
    }
    .unwrap();
    let mut polls = 0;
    let result = hostnet::block_on(async {
        let mut task = pin!(task);
        poll_fn(|cx| {
            polls += 1;
            task.as_mut().poll(cx)
        })
        .await
    })
    .unwrap();
    assert_eq!(result.0, "committed");
    assert_eq!(result.1, result.2);
    assert!(result.3);
    assert!(polls > 6, "I/O must return to the executor");
    assert_eq!(disk.durable.len(), 1);
    assert_eq!(disk.bytes.len(), 2);
    let synced = slots::decode(disk.durable.get("/data/stulp.json.a").unwrap()).unwrap();
    assert_eq!(synced.generation, 1);
    assert_eq!(disk.reads, 0, "uncertain barrier must poison readback");
}
#[test]
fn persisted_slots_reopen_and_paths_stay_canonical() {
    for bad in [
        "relative",
        "/",
        "/data/../stulp.json",
        "/data//x",
        "/data/./x",
        "/data/x\\y",
        "/data/\0",
    ] {
        assert!(!stulp_hopos::storage::valid_path(bad));
    }
    let mut disk = Disk::default();
    let bytes = br#"{"version":2,"system":{"fixture":"persistent"}}"#;
    // SAFETY: Zoals hierboven, een vaste kleine JSON-fixture en één unieke backend.
    let task = unsafe {
        Task::new(1 << 20, |s| -> Result {
            let (mut files, _) =
                slots::Files::open(Files::new(&mut disk, Park(s), "/data/stulp.json")?)?;
            files.save(bytes)?;
            files.save(bytes)?;
            Ok(())
        })
    }
    .unwrap();
    hostnet::block_on(task).unwrap();
    disk.bytes = disk.durable.clone();
    // SAFETY: De voorgaande taak is voltooid; dezelfde korte callstack en backend-grens.
    let task = unsafe {
        Task::new(1 << 20, |s| -> Result<Vec<u8>> {
            let (files, bytes) =
                slots::Files::open(Files::new(&mut disk, Park(s), "/data/stulp.json")?)?;
            if files.path() != Some("/data/stulp.json") {
                return Err(Error::Changed);
            }
            Ok(bytes)
        })
    }
    .unwrap();
    assert_eq!(hostnet::block_on(task).unwrap(), bytes);
    assert_eq!(disk.syncs, 2);
    assert!(disk.reads >= 2);
}
