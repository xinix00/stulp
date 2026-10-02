//! Draagbare Stulp-backups met gevalideerde staging en herstelbare publicatie.
mod inflate;
mod zip;
use std::{
    fs::{self, File, OpenOptions},
    io::{self, Read, Seek, Write},
    os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};
use stulp_core::{
    document::Document,
    json::{self, TryClone, Value},
    manifest,
    store::{Storage, Store},
};
use stulp_web::Environment as _;
fn invalid(s: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, s)
}
fn core<T>(v: stulp_core::Result<T>) -> io::Result<T> {
    v.map_err(io::Error::other)
}
fn valid_path(name: &str) -> bool {
    !name.is_empty()
        && !name.contains(['\\', '\0'])
        && name
            .split('/')
            .all(|part| !part.is_empty() && !matches!(part, "." | ".."))
}
fn manifest_at(root: &Path) -> io::Result<Value> {
    let file = File::open(root.join("app.json"))?;
    let mut data = Vec::new();
    file.take((json::MAX_DOCUMENT + 1) as u64)
        .read_to_end(&mut data)?;
    let m = json::parse(&data).map_err(io::Error::other)?;
    core(manifest::validate(&m))?;
    Ok(m)
}
/// Schrijft een consistente documentkopie met alle lokale appbundels naar een stream.
pub fn write(document: &str, output: impl Write) -> io::Result<()> {
    let doc = core(Document::decode(document.as_bytes()))?;
    let mut apps = Vec::new();
    let mut roots = Vec::new();
    for app in doc.records("apps") {
        let id = json::text(app, "id");
        let root = json::text(app, "root");
        let mut entry = core(json::fields(&[("id", core(json::string(id))?)]))?;
        if !root.is_empty() {
            let m = manifest_at(Path::new(root))?;
            if json::text(&m, "id") != id {
                return Err(invalid("app bundle identity differs from document"));
            }
            let prefix = format!("apps/{:03}", roots.len());
            core(json::set(&mut entry, "path", core(json::string(&prefix))?))?;
            roots.try_reserve(1).map_err(io::Error::other)?;
            roots.push((prefix, PathBuf::from(root)));
        }
        core(json::push(&mut apps, entry, 4096))?;
    }
    let metadata = core(json::fields(&[
        ("format", Value::uint(1)),
        ("createdAt", Value::String(core(crate::Environment.now())?)),
        ("apps", Value::Array(apps)),
    ]))?;
    let metadata = json::to_string(&metadata).map_err(io::Error::other)?;
    let mut zip = zip::Writer::new(output);
    zip.add("backup.json", 0o100600, metadata.as_bytes())?;
    zip.add("stulp.json", 0o100600, document.as_bytes())?;
    for (prefix, root) in roots {
        tree(&mut zip, &prefix, &root, 0)?;
    }
    zip.finish()?;
    Ok(())
}
fn tree<W: Write>(
    zip: &mut zip::Writer<W>,
    prefix: &str,
    root: &Path,
    depth: usize,
) -> io::Result<()> {
    if depth > 64 {
        return Err(invalid("app bundle directory depth exceeds 64"));
    }
    let info = fs::symlink_metadata(root)?;
    if info.is_symlink() {
        return Err(invalid("app bundle contains a symbolic link"));
    }
    if info.is_dir() {
        for entry in fs::read_dir(root)? {
            let entry = entry?;
            let name = entry.file_name();
            let name = name
                .to_str()
                .ok_or_else(|| invalid("app filename is not UTF-8"))?;
            if !valid_path(name) {
                return Err(invalid("unsafe app filename"));
            }
            tree(zip, &format!("{prefix}/{name}"), &entry.path(), depth + 1)?;
        }
    } else if info.is_file() {
        zip.add(
            prefix,
            0o100000 | (info.permissions().mode() & 0o777),
            File::open(root)?,
        )?;
    } else {
        return Err(invalid("app bundle contains a special file"));
    }
    Ok(())
}
/// Exclusieve output voorkomt dat een bestaande backup ongemerkt wordt overschreven.
pub fn write_file(document: &str, destination: &Path) -> io::Result<()> {
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(destination)?;
    let result = write(document, &mut file).and_then(|()| file.sync_all());
    if result.is_err() {
        let _ = fs::remove_file(destination);
    }
    result
}
/// Een private stagingmap wordt bij iedere fout of annulering automatisch verwijderd.
pub struct Prepared {
    directory: PathBuf,
    document: String,
    destination: String,
    bundled: bool,
}
impl Drop for Prepared {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.directory);
    }
}
impl Prepared {
    /// Valideert het hele archief voordat de adapter lopende apps onderbreekt.
    pub fn read(input: &mut (impl Read + Seek), destination: &Path) -> io::Result<Self> {
        let entries = zip::directory(input)?;
        let mut names = Vec::new();
        names
            .try_reserve_exact(entries.len())
            .map_err(io::Error::other)?;
        for entry in &entries {
            let name = &entry.name;
            if !valid_path(name)
                || !(matches!(name.as_str(), "backup.json" | "stulp.json" | "apps")
                    || name.starts_with("apps/"))
            {
                return Err(invalid("backup contains an unsafe or unknown path"));
            }
            names.push(name.as_str());
        }
        names.sort_unstable();
        if names.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(invalid("backup contains duplicate paths"));
        }
        let bytes = |name: &str, input: &mut _| {
            entries
                .iter()
                .find(|e| e.name == name)
                .ok_or_else(|| invalid("backup manifest or document missing"))?
                .bytes(
                    input,
                    if name == "stulp.json" {
                        stulp_core::document::MAX_BYTES
                    } else {
                        json::MAX_DOCUMENT
                    },
                )
        };
        let meta = json::parse(&bytes("backup.json", input)?).map_err(io::Error::other)?;
        if json::uint(&meta, "format") != 1 {
            return Err(invalid("unsupported Stulp backup format"));
        }
        let doc = core(Document::decode(&bytes("stulp.json", input)?))?;
        let apps = json::array(&meta, "apps");
        if apps.len() != doc.records("apps").len() {
            return Err(invalid("backup app count differs from document"));
        }
        let mut ids = Vec::new();
        ids.try_reserve_exact(apps.len())
            .map_err(io::Error::other)?;
        let mut roots: Vec<(&str, &str)> = Vec::new();
        for entry in apps {
            let id = json::text(entry, "id");
            let path = json::text(entry, "path");
            if id.is_empty() || ids.contains(&id) {
                return Err(invalid("backup has empty or duplicate app id"));
            }
            ids.push(id);
            let app = core(doc.record("apps", id))?;
            if path.is_empty() != json::text(app, "root").is_empty() {
                return Err(invalid("backup app bundle differs from document"));
            }
            if path.is_empty() {
                continue;
            }
            if !valid_path(path) || !path.starts_with("apps/") {
                return Err(invalid("unsafe app bundle path"));
            }
            for (_, previous) in &roots {
                if path == *previous
                    || path
                        .strip_prefix(previous)
                        .is_some_and(|s| s.starts_with('/'))
                    || previous
                        .strip_prefix(path)
                        .is_some_and(|s| s.starts_with('/'))
                {
                    return Err(invalid("overlapping app bundle paths"));
                }
            }
            roots.try_reserve(1).map_err(io::Error::other)?;
            roots.push((id, path));
        }
        for entry in &entries {
            if entry.name.starts_with("apps/")
                && !roots.iter().any(|(_, root)| {
                    entry.name == *root
                        || entry
                            .name
                            .strip_prefix(root)
                            .is_some_and(|s| s.starts_with('/'))
                })
            {
                return Err(invalid("file outside a declared app bundle"));
            }
        }
        let parent = destination
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        fs::create_dir_all(parent)?;
        let parent = fs::canonicalize(parent)?;
        let destination = parent.join(
            destination
                .file_name()
                .ok_or_else(|| invalid("document filename missing"))?,
        );
        let destination = destination
            .to_str()
            .ok_or_else(|| invalid("document path not UTF-8"))?
            .to_owned();
        let directory = parent.join(format!(".stulp-restore-{}", core(crate::Environment.id())?));
        fs::DirBuilder::new().mode(0o700).create(&directory)?;
        let mut prepared = Self {
            directory,
            document: String::new(),
            destination,
            bundled: !roots.is_empty(),
        };
        fs::create_dir(prepared.directory.join("apps"))?;
        // Ook niet-gebruikte directoryentries krijgen checksum- en lengtecontrole.
        for entry in &entries {
            if !entry.name.starts_with("apps/") {
                entry.copy(input, &mut io::sink())?;
                continue;
            }
            let target = prepared.directory.join(&entry.name);
            if entry.directory() {
                fs::create_dir_all(&target)?;
                entry.copy(input, &mut io::sink())?;
            } else {
                fs::create_dir_all(
                    target
                        .parent()
                        .ok_or_else(|| invalid("entry parent missing"))?,
                )?;
                let mode = if entry.mode & 0o111 != 0 {
                    0o700
                } else {
                    0o600
                };
                let mut file = OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .mode(mode)
                    .open(target)?;
                entry.copy(input, &mut file)?;
                file.sync_all()?;
            }
        }
        let mut records = Vec::new();
        for app in doc.records("apps") {
            let mut app = app.try_clone().map_err(io::Error::other)?;
            if let Some((id, path)) = roots.iter().find(|(id, _)| *id == json::text(&app, "id")) {
                let loaded = manifest_at(&prepared.directory.join(path))?;
                if json::text(&loaded, "id") != *id {
                    return Err(invalid("restored app manifest identity mismatch"));
                }
                core(json::set(
                    &mut app,
                    "root",
                    core(json::string(&format!(
                        "{}.apps/{}",
                        prepared.destination,
                        &path[5..]
                    )))?,
                ))?;
            }
            core(json::push(&mut records, app, 4096))?;
        }
        let mut root = doc.root().try_clone().map_err(io::Error::other)?;
        core(json::set(&mut root, "apps", Value::Array(records)))?;
        let text = json::to_string(&root).map_err(io::Error::other)?;
        // Store::open doet ook de native scene-reconciliatie, zonder fysieke opslag.
        let checked = core(Store::open(text.as_bytes(), stulp_core::store::Memory))?;
        prepared.document = core(checked.document().encode())?;
        Ok(prepared)
    }
    /// Publiceert na stopbevestiging; een mislukte documentsave zet de oude bundels terug.
    pub fn apply<S: Storage>(mut self, store: &mut Store<S>) -> io::Result<Value> {
        if store.path() != Some(self.destination.as_str()) {
            return Err(invalid("restore belongs to another document"));
        }
        let reused = self.reuse_local_bundles(store)?;
        let suffix = format!(".pre-restore-{}", core(crate::Environment.id())?);
        let previous = format!("{}{suffix}", self.destination);
        let old = core(store.document().encode())?;
        let mut backup = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&previous)?;
        backup.write_all(old.as_bytes())?;
        backup.sync_all()?;
        drop(backup);
        let apps = format!("{}.apps", self.destination);
        let old_apps = format!("{apps}{suffix}");
        let mut moved = false;
        if self.bundled {
            match fs::rename(&apps, &old_apps) {
                Ok(()) => moved = true,
                Err(e) if e.kind() == io::ErrorKind::NotFound => (),
                Err(e) => return Err(e),
            }
            if let Err(e) = fs::rename(self.directory.join("apps"), &apps) {
                if moved {
                    fs::rename(&old_apps, &apps)?;
                }
                return Err(e);
            }
        }
        if let Err(error) = store.restore(self.document.as_bytes()) {
            if self.bundled {
                fs::rename(&apps, self.directory.join("apps"))?;
                if moved {
                    fs::rename(&old_apps, &apps)?;
                }
            }
            return Err(io::Error::other(error));
        }
        if let Some(parent) = Path::new(&self.destination).parent()
            && let Err(e) = File::open(parent).and_then(|f| f.sync_all())
        {
            eprintln!("[stulp:restore-directory-sync] {e}");
        }
        eprintln!("[stulp:restore] reused_local_bundles={reused}");
        core(json::fields(&[
            ("restored", Value::Bool(true)),
            ("document", core(json::string(&self.destination))?),
            ("appsRoot", core(json::string(&apps))?),
            ("previousDocument", core(json::string(&previous))?),
            (
                "previousAppsRoot",
                core(json::string(if moved { &old_apps } else { "" }))?,
            ),
        ]))
    }

    /// Een configuratiebackup van externe apps behoudt de lokale installatie op dit systeem.
    fn reuse_local_bundles<S: Storage>(&mut self, store: &Store<S>) -> io::Result<usize> {
        let doc = core(Document::decode(self.document.as_bytes()))?;
        let replaced = self
            .bundled
            .then(|| fs::canonicalize(format!("{}.apps", self.destination)).ok())
            .flatten();
        let mut records = Vec::new();
        let mut reused = 0;
        for app in doc.records("apps") {
            let mut app = app.try_clone().map_err(io::Error::other)?;
            let id = json::text(&app, "id");
            if json::text(&app, "root").is_empty()
                && let Ok(previous) = store.document().record("apps", id)
                && let Ok(root) = fs::canonicalize(json::text(previous, "root"))
                && !replaced.as_ref().is_some_and(|base| root.starts_with(base))
                && let Ok(manifest) = manifest_at(&root)
                && json::text(&manifest, "id") == id
                && root.join(id).is_file()
            {
                // Alleen het installatiepad komt van lokaal; instellingen en enabled uit de backup.
                core(json::set(
                    &mut app,
                    "root",
                    core(json::string(
                        root.to_str()
                            .ok_or_else(|| invalid("app path is not UTF-8"))?,
                    ))?,
                ))?;
                reused += 1;
            }
            core(json::push(&mut records, app, 4096))?;
        }
        let mut root = doc.root().try_clone().map_err(io::Error::other)?;
        core(json::set(&mut root, "apps", Value::Array(records)))?;
        self.document = json::to_string(&root).map_err(io::Error::other)?;
        Ok(reused)
    }
}

pub(crate) fn route<S: Storage>(
    store: &Store<S>,
    request: stulp_web::Request,
) -> stulp_core::Result<stulp_web::Response> {
    use stulp_web::{Body, Response};
    let body = if request.method == "GET" {
        Body::Backup(store.document().encode()?)
    } else {
        let kind = json::text(&request.headers, "content-type")
            .split(';')
            .next()
            .unwrap_or("")
            .trim();
        if !kind.eq_ignore_ascii_case("application/zip")
            && !kind.eq_ignore_ascii_case("application/octet-stream")
        {
            return Response::error(415, "upload a Stulp .zip backup");
        }
        Body::Restore {
            destination: json::copy(
                store
                    .path()
                    .ok_or(stulp_core::Error::Invalid("restore needs file storage"))?,
            )?,
            archive: request.body,
        }
    };
    Ok(Response {
        status: 200,
        content_type: "application/zip",
        body,
        cookie: None,
        headers: Vec::new(),
    })
}

pub(crate) async fn download(
    exchange: &mut leanhttp::Exchange<'_, stulp_transport::Http<'_>>,
    document: &str,
) -> leanhttp::Result<()> {
    exchange
        .header_mut()
        .set("Content-Type", "application/zip")?;
    exchange.header_mut().set(
        "Content-Disposition",
        "attachment; filename=\"stulp-backup.zip\"",
    )?;
    exchange.header_mut().set("Cache-Control", "no-store")?;
    exchange.write_header(200)?;
    struct Sink<'a, 'b, 'tls>(&'a mut leanhttp::Exchange<'b, stulp_transport::Http<'tls>>);
    impl Write for Sink<'_, '_, '_> {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            hostnet::block_on(self.0.write(bytes))
                .map_err(|_| io::Error::other("backup connection closed"))
        }
        fn flush(&mut self) -> io::Result<()> {
            hostnet::block_on(self.0.flush())
                .map_err(|_| io::Error::other("backup connection closed"))
        }
    }
    // Deze reeds bestaande HTTP-werker bezit de stream; de controller blijft vrij.
    if let Err(e) = write(document, Sink(exchange)) {
        eprintln!("[stulp:backup-download] {e}");
        return Err(leanhttp::Error::Connect);
    }
    Ok(())
}

#[cfg(test)]
mod tests;
