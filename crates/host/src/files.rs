//! Atomair documentbeheer met private bestanden en een fsync vóór rename.
use std::os::unix::fs::OpenOptionsExt;
use std::{
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
};
use stulp_core::{Error, Result, document::MAX_BYTES, store::Storage};

/// De bestandseigenaar bewaart het pad; geen enkele app krijgt dit handvat.
pub struct Files {
    path: PathBuf,
    serial: u64,
    _lock: File,
}

impl Files {
    /// Resolvet de oudermap zodat een werkdirectorywijziging de opslag niet verplaatst.
    pub fn new(path: &Path) -> std::io::Result<Self> {
        let parent = path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let mut resolved = fs::canonicalize(parent)?;
        resolved.push(path.file_name().ok_or(std::io::ErrorKind::InvalidInput)?);
        if resolved.is_symlink() {
            resolved = fs::canonicalize(resolved)?;
        }
        let mut lock_path = resolved.as_os_str().to_os_string();
        lock_path.push(".lock");
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(lock_path)?;
        lock.try_lock()
            .map_err(|e| std::io::Error::other(format!("document is already open: {e}")))?;
        Ok(Self {
            _lock: lock,
            path: resolved,
            serial: 0,
        })
    }

    /// Leest met een limiet óók als het bestand tijdens de read groeit.
    pub fn read(&self) -> std::io::Result<Vec<u8>> {
        let mut bytes = Vec::new();
        let file = match File::open(&self.path) {
            Ok(file) => file,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(bytes),
            Err(e) => return Err(e),
        };
        file.take((MAX_BYTES + 1) as u64).read_to_end(&mut bytes)?;
        if bytes.len() > MAX_BYTES {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "document exceeds parser limit",
            ));
        }
        Ok(bytes)
    }

    fn write(&mut self, bytes: &[u8]) -> std::io::Result<()> {
        let parent = self.path.parent().ok_or(std::io::ErrorKind::InvalidInput)?;
        self.serial = self
            .serial
            .checked_add(1)
            .ok_or(std::io::ErrorKind::Other)?;
        let path = parent.join(format!(".stulp-{}-{}.tmp", std::process::id(), self.serial));
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&path)?;
        let temporary = Temporary(path);
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temporary.0, &self.path)?;
        // Na rename is de nieuwe waarheid zichtbaar. Een directory-syncfout mag
        // de actor niet terugzetten op de oude versie; meld de degradatie apart.
        if let Err(e) = File::open(parent).and_then(|directory| directory.sync_all()) {
            eprintln!("[stulp:directory-sync] {e}");
        }
        Ok(())
    }
}

impl Storage for Files {
    fn path(&self) -> Option<&str> {
        self.path.to_str()
    }
    fn save(&mut self, bytes: &[u8]) -> Result {
        self.write(bytes).map_err(|e| {
            eprintln!("[stulp:storage] {e}");
            Error::Storage
        })
    }
}

struct Temporary(PathBuf);
impl Drop for Temporary {
    fn drop(&mut self) {
        let _ = fs::remove_file(&self.0);
    }
}
