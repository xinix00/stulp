//! A/B-documentopslag bevestigt pas na HopFS-sync; een onzekere write vergiftigt deze eigenaar.
use alloc::{string::String, vec::Vec};
use applib::{appnet::SystemClient, stacktask::Suspender, sys};
use core::future::Future;
use stulp_core::{
    Error, Result,
    document::MAX_BYTES,
    json,
    slots::{Backend, Slot},
};
const CHUNK: usize = 64 * 1024;
/// Een exclusieve async bestandseigenaar, ook bruikbaar met een foutinjectie-adapter.
pub trait Io {
    /// Ontbreken is None; transport- en toegangsproblemen zijn fouten.
    fn size(&mut self, path: &str) -> impl Future<Output = Result<Option<u64>>>;
    /// Leest hoogstens de aangeboden buffer, vanaf de opgegeven offset.
    fn read(
        &mut self,
        path: &str,
        offset: u64,
        dst: &mut [u8],
    ) -> impl Future<Output = Result<usize>>;
    /// Schrijft alle aangeboden bytes of faalt.
    fn write(&mut self, path: &str, offset: u64, bytes: &[u8]) -> impl Future<Output = Result>;
    /// Maakt de inactieve slot leeg voordat zijn nieuwe generatie geschreven wordt.
    fn truncate(&mut self, path: &str) -> impl Future<Output = Result>;
    /// Een echte duurzaamheidsbarrière; een readback geldt nooit als vervanging.
    fn sync(&mut self, path: &str) -> impl Future<Output = Result>;
}
/// Wacht coöperatief op één future, zonder een geneste executor of actief pollen.
pub trait Wait {
    /// Annulering moet een fout opleveren zodat de synchrone stack normaal afwikkelt.
    fn wait<F: Future>(&self, future: F) -> Result<F::Output>;
}
/// De SDK-parkeerfunctie leent uitsluitend de eigenaar van deze stack.
pub struct Park<'a>(pub &'a Suspender);
impl Wait for Park<'_> {
    fn wait<F: Future>(&self, future: F) -> Result<F::Output> {
        self.0.wait(future).map_err(|_| Error::Storage)
    }
}
/// Eén documentpad, één system-client, één parkeerpunt; geen gedeelde mutatiestaat.
pub struct Files<I, W> {
    io: I,
    wait: W,
    path: String,
    poisoned: bool,
}
impl<I: Io, W: Wait> Files<I, W> {
    /// Alleen canonieke absolute paden binnen de aan deze app toegekende opslag.
    pub fn new(io: I, wait: W, path: &str) -> Result<Self> {
        if !valid_path(path) {
            return Err(Error::Invalid("invalid HopOS document path"));
        }
        Ok(Self {
            io,
            wait,
            path: json::copy(path)?,
            poisoned: false,
        })
    }
    fn path(&self, slot: Slot) -> Result<String> {
        if self.poisoned {
            return Err(Error::Storage);
        }
        let mut path = json::copy(&self.path)?;
        path.try_reserve(2).map_err(|_| Error::Memory)?;
        path.push_str(match slot {
            Slot::A => ".a",
            Slot::B => ".b",
            Slot::Legacy => "",
        });
        Ok(path)
    }
}
/// Paths mogen niet buiten de gemounte opslag verwijzen via relatieve componenten.
pub fn valid_path(path: &str) -> bool {
    path.starts_with('/')
        && path.len() <= 480
        && !path.chars().any(|c| c.is_control() || c == '\\')
        && path[1..]
            .split('/')
            .all(|s| !s.is_empty() && s != "." && s != "..")
}
impl<I: Io, W: Wait> Backend for Files<I, W> {
    fn path(&self) -> Option<&str> {
        Some(&self.path)
    }
    fn read(&mut self, slot: Slot) -> Result<Option<Vec<u8>>> {
        let path = self.path(slot)?;
        let result = self
            .wait
            .wait(async {
                let Some(size) = self.io.size(&path).await? else {
                    return Ok(None);
                };
                if size > (MAX_BYTES + 24) as u64 {
                    return Err(Error::Full);
                }
                let size = usize::try_from(size).map_err(|_| Error::Full)?;
                let mut bytes = Vec::new();
                bytes.try_reserve_exact(size).map_err(|_| Error::Memory)?;
                bytes.resize(size, 0);
                let mut at = 0;
                while at < size {
                    let end = size.min(at + CHUNK);
                    let n = self.io.read(&path, at as u64, &mut bytes[at..end]).await?;
                    if n == 0 || n > end - at {
                        return Err(Error::Storage);
                    }
                    at += n;
                }
                Ok(Some(bytes))
            })
            .and_then(core::convert::identity);
        if result.is_err() {
            self.poisoned = true;
        }
        result
    }
    fn write(&mut self, slot: Slot, bytes: &[u8]) -> Result {
        if slot == Slot::Legacy {
            return Err(Error::Invalid("legacy document is read-only"));
        }
        if bytes.len() > MAX_BYTES + 24 {
            return Err(Error::Full);
        }
        let path = self.path(slot)?;
        let result = self
            .wait
            .wait(async {
                self.io.truncate(&path).await?;
                let mut at = 0;
                for chunk in bytes.chunks(CHUNK) {
                    self.io.write(&path, at, chunk).await?;
                    at += chunk.len() as u64;
                }
                self.io.sync(&path).await
            })
            .and_then(core::convert::identity);
        // Nooit een readback laten doorgaan na een verloren sync-antwoord: complete
        // cachebytes bewijzen geen duurzaamheid. Heropenen gebruikt een nieuwe eigenaar.
        if result.is_err() {
            self.poisoned = true;
        }
        result
    }
}
impl Io for SystemClient {
    async fn size(&mut self, path: &str) -> Result<Option<u64>> {
        match self.stat(path).await {
            Ok(n) => Ok(Some(n)),
            Err(sys::Error::NotFound { .. }) => Ok(None),
            Err(_) => Err(Error::Storage),
        }
    }
    async fn read(&mut self, path: &str, offset: u64, dst: &mut [u8]) -> Result<usize> {
        self.read_into(path, offset, dst)
            .await
            .map_err(|_| Error::Storage)
    }
    async fn write(&mut self, path: &str, offset: u64, bytes: &[u8]) -> Result {
        let n = self
            .write_at(path, offset, bytes)
            .await
            .map_err(|_| Error::Storage)?;
        if n != bytes.len() {
            return Err(Error::Storage);
        }
        Ok(())
    }
    async fn truncate(&mut self, path: &str) -> Result {
        self.truncate(path, 0).await.map_err(|_| Error::Storage)
    }
    async fn sync(&mut self, path: &str) -> Result {
        self.sync(path)
            .await
            .map(|_| ())
            .map_err(|_| Error::Storage)
    }
}
