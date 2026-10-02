//! De controller bezit lokale kinderen, hun stopbevestiging en begrensde herstartvertraging.
use crate::{Environment, apps::Apps};
use std::{
    os::unix::{
        fs::{DirBuilderExt, PermissionsExt},
        net::UnixListener,
    },
    path::{Path, PathBuf},
    process::{Command, Stdio},
};
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
use stulp_runtime::supervisor::{Mode, State, Supervisor};
use stulp_web::{Environment as _, Request, Response};

pub(crate) struct Local {
    pub(crate) listener: UnixListener,
    pub(crate) path: PathBuf,
    directory: Option<PathBuf>,
}
impl Local {
    pub(crate) fn new() -> Result<Self> {
        let nonce = Environment.id()?;
        let directory = Path::new("/tmp").join(format!("stulp-{}-{nonce}", std::process::id()));
        std::fs::DirBuilder::new()
            .mode(0o700)
            .create(&directory)
            .map_err(|_| Error::Storage)?;
        let path = directory.join("apps.sock");
        let result = UnixListener::bind(&path).and_then(|l| {
            l.set_nonblocking(true)?;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
            Ok(l)
        });
        match result {
            Ok(listener) => Ok(Self {
                listener,
                path,
                directory: Some(directory),
            }),
            Err(_) => {
                let _ = std::fs::remove_file(&path);
                let _ = std::fs::remove_dir(&directory);
                Err(Error::Storage)
            }
        }
    }
    pub(crate) fn at(path: &Path) -> Result<Self> {
        // Bind weigert een bestaand pad; nooit blind een andere socket verwijderen.
        let listener =
            UnixListener::bind(path).map_err(|_| Error::Invalid("cannot bind app socket"))?;
        let result = Self {
            listener,
            path: path.to_path_buf(),
            directory: None,
        };
        result
            .listener
            .set_nonblocking(true)
            .map_err(|_| Error::Storage)?;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
            .map_err(|_| Error::Storage)?;
        Ok(result)
    }
}
impl Drop for Local {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
        if let Some(directory) = &self.directory {
            let _ = std::fs::remove_dir(directory);
        }
    }
}
struct Process {
    id: String,
    root: String,
    child: Option<crate::logging::Running>,
    state: Supervisor,
    generation: u64,
    stopping: bool,
    started: u64,
    level: crate::logging::Level,
}
impl Drop for Process {
    fn drop(&mut self) {
        if let Some(c) = &mut self.child {
            let _ = c.kill();
            let _ = c.wait();
            let _ = c.logs(&self.id, self.level);
        }
    }
}
struct ManifestRead {
    id: String,
    root: String,
    next: u64,
}
pub(crate) struct Processes {
    children: Vec<Process>,
    next: u64,
    manifests: Vec<ManifestRead>,
    only: Option<String>,
    level: crate::logging::Level,
}
impl Processes {
    pub(crate) fn new() -> Self {
        Self {
            children: Vec::new(),
            next: 0,
            manifests: Vec::new(),
            only: None,
            level: crate::logging::Level::Info,
        }
    }
    pub(crate) fn with_level(level: crate::logging::Level) -> Self {
        Self {
            level,
            ..Self::new()
        }
    }
    pub(crate) fn for_app(id: &str) -> Result<Self> {
        let mut result = Self::new();
        result.only = Some(json::copy(id)?);
        Ok(result)
    }
    pub(crate) fn poll<S: Storage>(&mut self, store: &mut Store<S>, apps: &mut Apps<'_>) -> Result {
        let now = apps.now();
        for p in &mut self.children {
            let wanted = store.document().record("apps", &p.id).is_ok_and(|a| {
                (json::boolean(a, "enabled") || self.only.as_deref() == Some(&p.id))
                    && !json::boolean(a, "offered")
                    && json::text(a, "root") == p.root
            });
            if !wanted && !p.stopping {
                p.stopping = true;
                p.state.stop();
                apps.disconnect(&p.id);
                if let Some(child) = &mut p.child {
                    let _ = child.kill();
                }
            }
            if let Some(child) = &mut p.child {
                child.logs(&p.id, self.level)?;
                if child
                    .try_wait()
                    .map_err(|_| Error::Invalid("cannot reap app process"))?
                    .is_some()
                {
                    child.logs(&p.id, self.level)?;
                    p.child = None;
                    apps.disconnect(&p.id);
                    if !p.stopping {
                        p.state.exited(p.generation, now);
                        failure(store, p, now, "app process stopped")?;
                    }
                } else if !p.stopping {
                    if apps.running(&p.id) && p.state.state() == State::Starting {
                        p.state.ready(p.generation)?;
                    }
                    if (p.state.state() == State::Running && !apps.connected(&p.id))
                        || (p.state.state() == State::Starting
                            && now.saturating_sub(p.started) > 120_000)
                    {
                        let _ = child.kill();
                    }
                }
            }
        }
        self.children.retain(|p| !p.stopping || p.child.is_some());
        self.manifests
            .retain(|m| store.document().record("apps", &m.id).is_ok());
        if now < self.next {
            return Ok(());
        }
        self.next = now.saturating_add(250);
        let mut records = Vec::new();
        for record in store.document().records("apps") {
            if self
                .only
                .as_deref()
                .is_some_and(|id| id != json::text(record, "id"))
            {
                continue;
            }
            json::push(&mut records, record.try_clone()?, 4096)?;
        }
        for record in records {
            let id = json::text(&record, "id");
            let root = json::text(&record, "root");
            if root.is_empty() {
                continue;
            }
            let reread =
                self.manifests.iter().find(|m| m.id == id).is_none_or(|m| {
                    m.root != root || (store.manifest(id).is_none() && now >= m.next)
                });
            if reread {
                if let Some(m) = self.manifests.iter_mut().find(|m| m.id == id) {
                    m.root = json::copy(root)?;
                    m.next = now.saturating_add(30_000);
                } else {
                    json::push(
                        &mut self.manifests,
                        ManifestRead {
                            id: json::copy(id)?,
                            root: json::copy(root)?,
                            next: now.saturating_add(30_000),
                        },
                        stulp_core::document::MAX_RECORDS,
                    )?;
                }
                match read_manifest(root).and_then(|m| store.announce(id, m)) {
                    Ok(()) => store.set_app_status(
                        id,
                        if json::boolean(&record, "enabled") {
                            "waiting"
                        } else {
                            "stopped"
                        },
                    )?,
                    Err(e) => {
                        store.set_app_status(id, "crashed")?;
                        store.set_app_runtime(
                            id,
                            json::fields(&[("error", json::string(&e.to_string())?)])?,
                        )?;
                        eprintln!("[stulp:manifest] app={id} error={e}");
                        continue;
                    }
                }
            }
            if store.manifest(id).is_none()
                || (!json::boolean(&record, "enabled") && self.only.as_deref() != Some(id))
                || json::boolean(&record, "offered")
            {
                continue;
            }
            if store
                .manifest(id)
                .is_some_and(|m| json::boolean(m, "external"))
                && self.only.as_deref() != Some(id)
            {
                continue;
            }

            if !self.children.iter().any(|p| p.id == id) {
                if apps.connected(id) {
                    continue;
                }
                json::push(
                    &mut self.children,
                    Process {
                        id: json::copy(id)?,
                        root: json::copy(root)?,
                        child: None,
                        state: Supervisor::new(Mode::Spawned),
                        generation: 0,
                        stopping: false,
                        started: now,
                        level: self.level,
                    },
                    32,
                )?;
            }
            let Some(p) = self.children.iter_mut().find(|p| p.id == id) else {
                continue;
            };
            if p.stopping || p.child.is_some() || apps.connected(id) {
                continue;
            }
            if p.state.state() != State::Stopped && !p.state.retry_due(now) {
                continue;
            }
            p.generation = p.state.start()?;
            p.started = now;
            let result = launch(store, apps, p);
            match result {
                Ok(child) => {
                    p.child = Some(child);
                    store.set_app_status(id, "starting")?;
                }
                Err(e) => {
                    p.state.exited(p.generation, now);
                    failure(store, p, now, &e.to_string())?;
                }
            }
        }
        Ok(())
    }
    pub(crate) fn restart<S: Storage>(
        &mut self,
        store: &Store<S>,
        apps: &mut Apps<'_>,
        id: &str,
    ) -> Result {
        let a = store.document().record("apps", id)?;
        if !json::boolean(a, "enabled") || json::boolean(a, "offered") {
            return Err(Error::Invalid("app is disabled"));
        }
        apps.disconnect(id);
        self.manifests.retain(|m| m.id != id);
        self.next = 0;
        if let Some(p) = self.children.iter_mut().find(|p| p.id == id) {
            p.stopping = true;
            p.state.stop();
            if let Some(c) = &mut p.child {
                c.kill()
                    .map_err(|_| Error::Invalid("cannot stop app process"))?;
            }
        }
        Ok(())
    }
    pub(crate) fn route<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        apps: &mut Apps<'_>,
        r: &Request,
    ) -> Result<Option<Response>> {
        if r.method == "POST"
            && let Some(id) = r
                .path
                .strip_prefix("/api/manager/apps/app/")
                .and_then(|v| v.strip_suffix("/restart"))
                .filter(|v| !v.contains('/'))
        {
            self.restart(store, apps, id)?;
            return Ok(Some(Response::json(200, &Value::Bool(true))?));
        }
        if r.method == "DELETE"
            && let Some(id) = r
                .path
                .strip_prefix("/api/manager/apps/app/")
                .filter(|v| !v.is_empty() && !v.contains('/'))
        {
            let record = store.document().record("apps", id)?;
            let root = json::copy(json::text(record, "root"))?;
            let name = store
                .manifest(id)
                .and_then(|m| json::get(m, "name"))
                .map(|v| stulp_core::manifest::localized(v, store.language()))
                .unwrap_or(id);
            let mut result =
                json::fields(&[("id", json::string(id)?), ("name", json::string(name)?)])?;
            let devices = store
                .document()
                .records("devices")
                .iter()
                .filter(|d| json::text(d, "appId") == id)
                .count();
            let enabled = store
                .document()
                .records("flows")
                .iter()
                .filter(|f| json::boolean(f, "enabled"))
                .count();
            apps.disconnect(id);
            if let Some(p) = self.children.iter_mut().find(|p| p.id == id) {
                p.state.stop();
                p.stopping = true;
                if let Some(child) = &mut p.child
                    && child
                        .try_wait()
                        .map_err(|_| Error::Invalid("cannot inspect uninstalled app"))?
                        .is_none()
                {
                    child
                        .kill()
                        .map_err(|_| Error::Invalid("cannot stop uninstalled app"))?;
                    child
                        .wait()
                        .map_err(|_| Error::Invalid("cannot reap uninstalled app"))?;
                }
                if let Some(child) = &mut p.child {
                    child.logs(&p.id, self.level)?;
                }
                p.child = None;
            }
            store.delete("apps", id)?;
            let remaining = store
                .document()
                .records("flows")
                .iter()
                .filter(|f| json::boolean(f, "enabled"))
                .count();
            json::set(&mut result, "devices", Value::uint(devices as u64))?;
            json::set(
                &mut result,
                "flows",
                Value::uint(enabled.saturating_sub(remaining) as u64),
            )?;
            if !root.is_empty() {
                // Alleen de eigen bundelmap mag weg; een extern bronproject blijft staan.
                let owned = store
                    .path()
                    .and_then(|p| Path::new(&format!("{p}.apps")).canonicalize().ok());
                let bundle = Path::new(&root).canonicalize().ok();
                let removed = match (owned, bundle) {
                    (Some(base), Some(path)) if path.parent() == Some(base.as_path()) => {
                        std::fs::remove_dir_all(path).is_ok()
                    }
                    _ => false,
                };
                if !removed {
                    json::set(
                        &mut result,
                        "warning",
                        json::string("De app is verwijderd; de externe bundelmap is behouden.")?,
                    )?;
                }
            }
            return Ok(Some(Response::json(200, &result)?));
        }
        Ok(None)
    }
}
pub(crate) fn read_manifest(root: &str) -> Result<Value> {
    let path = Path::new(root).join("app.json");
    let size = std::fs::metadata(&path)
        .map_err(|_| Error::Missing("app manifest unavailable"))?
        .len();
    if size > json::MAX_DOCUMENT as u64 {
        return Err(Error::Full);
    }
    let bytes = std::fs::read(path).map_err(|_| Error::Missing("app manifest unavailable"))?;
    let mut manifest = json::parse(&bytes)?;
    crate::app_ui::discover(root, &mut manifest)?;
    Ok(manifest)
}
fn launch<S: Storage>(
    store: &mut Store<S>,
    apps: &mut Apps<'_>,
    p: &Process,
) -> Result<crate::logging::Running> {
    let mut system = json::get(store.document().root(), "system")
        .unwrap_or(&json::object())
        .try_clone()?;
    if json::text(&system, "attachSecret").is_empty() {
        let secret = stulp_protocol::token::base64(
            &hostnet::entropy().map_err(|_| Error::Invalid("OS entropy unavailable"))?[..32],
        )?;
        json::set(&mut system, "attachSecret", json::string(&secret)?)?;
        store.system(system.try_clone()?)?;
    }
    let token = stulp_protocol::token::token(json::text(&system, "attachSecret"), &p.id)?;
    let binary = Path::new(&p.root)
        .join(&p.id)
        .canonicalize()
        .map_err(|_| Error::Missing("app binary unavailable"))?;
    let child = Command::new(binary)
        .current_dir(&p.root)
        .env("STULP_ATTACH", apps.local_path()?)
        .env("STULP_ATTACH_TOKEN", token)
        .env("STULP_MANAGED", "1")
        .env("STULP_APP_DATA", Path::new(&p.root).join(".data"))
        .env_remove("STULP_SOCKET")
        .env_remove("STULP_ATTACH_PLAINTEXT")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|_| Error::Invalid("cannot start app binary"))?;
    crate::logging::Running::capture(child)
}

fn failure<S: Storage>(store: &mut Store<S>, p: &Process, now: u64, message: &str) -> Result {
    store.set_app_status(&p.id, "crashed")?;
    let time = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|_| Error::Invalid("clock predates epoch"))?
        .as_nanos();
    let time = time.saturating_add(u128::from(p.state.retry_at().saturating_sub(now)) * 1_000_000);
    let retry = json::timestamp(u64::try_from(time).map_err(|_| Error::Full)?)?;
    store.set_app_runtime(
        &p.id,
        json::fields(&[
            ("error", json::string(message)?),
            ("restartCount", Value::uint(u64::from(p.state.retries()))),
            ("retryAt", json::string(&retry)?),
        ])?,
    )?;
    eprintln!(
        "[stulp:app-spawn] app={} attempt={} error={message}",
        p.id,
        p.state.retries()
    );
    Ok(())
}
