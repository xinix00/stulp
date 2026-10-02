//! Native bundle discovery and filesystem containment for shared browser assets.
pub(crate) use stulp_controller::app_ui::{Asset, prepare};
use stulp_core::{
    Error, Result,
    json::{self, Value},
};
use stulp_sdk::util::field;
const MAX_LOCAL_ASSET: usize = 4 * 1024 * 1024;
fn join(parts: &[&str]) -> Result<String> {
    stulp_sdk::util::join(parts).map_err(|_| Error::Memory)
}
pub(crate) fn discover(root: &str, manifest: &mut Value) -> Result {
    use stulp_core::json::TryClone;
    let mut ui = field(manifest, "ui").try_clone()?;
    if ui.as_object().is_none() {
        ui = json::object();
    }
    let mut assets = Vec::new();
    for asset in json::array(&ui, "assets") {
        json::push(&mut assets, asset.try_clone()?, 1024)?;
    }
    let mut pending = vec!["settings".to_owned(), "locales".to_owned()];
    for driver in json::array(manifest, "drivers") {
        let id = json::text(driver, "id");
        if stulp_sdk::valid_asset(id) && !id.contains('/') {
            json::push(&mut pending, join(&["drivers/", id, "/pair"])?, 1024)?;
        }
    }
    let mut visited = 0;
    while let Some(relative) = pending.pop() {
        let directory = std::path::Path::new(root).join(&relative);
        let entries = match std::fs::read_dir(directory) {
            Ok(v) => v,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
            Err(_) => return Err(Error::Invalid("cannot inspect app UI directory")),
        };
        for entry in entries {
            visited += 1;
            if visited > 2048 {
                return Err(Error::Full);
            }
            let entry = entry.map_err(|_| Error::Invalid("cannot inspect app UI entry"))?;
            let name = entry.file_name();
            let Some(name) = name.to_str() else {
                continue;
            };
            let name = join(&[&relative, "/", name])?;
            if !stulp_sdk::valid_asset(&name) {
                continue;
            }
            let kind = entry
                .file_type()
                .map_err(|_| Error::Invalid("cannot inspect app UI type"))?;
            if kind.is_dir() {
                if name.bytes().filter(|b| *b == b'/').count() >= 16 {
                    return Err(Error::Full);
                }
                json::push(&mut pending, name, 1024)?;
            } else if kind.is_file() && !assets.iter().any(|v| v.as_str() == Some(&name)) {
                json::push(&mut assets, json::string(&name)?, 1024)?;
            }
        }
    }
    json::set(&mut ui, "assets", Value::Array(assets))?;
    json::set(manifest, "ui", ui)
}
pub(crate) fn read_local(root: &str, name: &str) -> Result<Value> {
    use std::{fs::File, io::Read, path::Path};
    if !stulp_sdk::valid_asset(name) {
        return Err(Error::Invalid("invalid asset path"));
    }
    let base = if name.starts_with("drivers/") {
        let mut parts = name.split('/');
        parts.next();
        join(&[
            "drivers/",
            parts.next().ok_or(Error::Invalid("missing driver"))?,
            "/pair",
        ])?
    } else {
        json::copy(name.split('/').next().unwrap_or(""))?
    };
    if !matches!(base.as_str(), "settings" | "locales") && !base.ends_with("/pair") {
        return Err(Error::Invalid("invalid asset root"));
    }
    let missing = || json::fields(&[("found", Value::Bool(false))]);
    let canonical = |path: &Path| -> Result<Option<std::path::PathBuf>> {
        match path.canonicalize() {
            Ok(p) => Ok(Some(p)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(_) => Err(Error::Invalid("cannot resolve app asset")),
        }
    };
    let Some(base) = canonical(&Path::new(root).join(base))? else {
        return missing();
    };
    let Some(target) = canonical(&Path::new(root).join(name))? else {
        return missing();
    };
    if !target.starts_with(base) {
        return missing();
    }
    let file = File::open(target).map_err(|_| Error::Invalid("cannot open app asset"))?;
    if !file
        .metadata()
        .map_err(|_| Error::Invalid("cannot inspect app asset"))?
        .is_file()
    {
        return missing();
    }
    let mut bytes = Vec::new();
    file.take((MAX_LOCAL_ASSET + 1) as u64)
        .read_to_end(&mut bytes)
        .map_err(|_| Error::Invalid("cannot read app asset"))?;
    if bytes.len() > MAX_LOCAL_ASSET {
        return Err(Error::Full);
    }
    json::fields(&[
        ("found", Value::Bool(true)),
        (
            "data",
            json::string(&stulp_protocol::token::base64(&bytes)?)?,
        ),
    ])
}
