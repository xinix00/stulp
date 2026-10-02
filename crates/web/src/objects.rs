//! Publieke browserobjecten: appState en opslaggeheimen verlaten de eigenaar niet.
use super::copy;
use stulp_core::{
    Result,
    json::{self, Value},
    store::{Storage, Store},
};

pub(super) fn device<S: Storage>(store: &Store<S>, id: &str) -> Result<Value> {
    let device = store.device(id)?;
    let app = json::text(&device, "appId");
    let mut result = json::object();
    for key in [
        "id",
        "appId",
        "groupId",
        "sortOrder",
        "name",
        "class",
        "data",
        "settings",
        "capabilities",
        "available",
        "unavailableMessage",
    ] {
        json::set(&mut result, key, copy(json::get(&device, key))?)?;
    }
    let data = json::get(&device, "store").unwrap_or(&Value::Null);
    for (key, stored) in super::manager::FIELDS {
        let fallback = match *key {
            "note" => json::string("")?,
            "zone" => json::string("stulp-home")?,
            "hidden" => Value::Bool(false),
            _ => Value::Null,
        };
        json::set(
            &mut result,
            key,
            copy(json::get(data, stored).or(Some(&fallback)))?,
        )?;
    }
    for (key, stored) in [
        ("warningMessage", "__stulp.warning"),
        ("energy", "__stulp.energy"),
    ] {
        json::set(&mut result, key, copy(json::get(data, stored))?)?;
    }
    let mut owner = json::copy("stulp:app:")?;
    owner
        .try_reserve(app.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    owner.push_str(app);
    json::set(&mut result, "ownerUri", json::string(&owner)?)?;
    json::set(&mut result, "repair", Value::Bool(false))?;
    json::set(&mut result, "flags", Value::Array(alloc::vec::Vec::new()))?;
    json::set(
        &mut result,
        "manufacturer",
        json::string(manufacturer(store, &device))?,
    )?;
    let mut driver = json::copy("stulp:app:")?;
    let driver_id = json::text(&device, "driverId");
    driver
        .try_reserve(app.len() + 1 + driver_id.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    driver.push_str(app);
    driver.push(':');
    driver.push_str(driver_id);
    json::set(&mut result, "driverId", json::string(&driver)?)?;
    let hardware = json::get(&device, "store")
        .map(|s| json::text(s, "__stulp.hardwareName"))
        .filter(|s| !s.is_empty())
        .unwrap_or(json::text(&device, "name"));
    json::set(&mut result, "hardwareName", json::string(hardware)?)?;
    let mut capabilities = json::object();
    for capability in json::array(&device, "capabilities")
        .iter()
        .filter_map(Value::as_str)
    {
        let object = super::capability::output(store, &device, capability)?;
        json::set(&mut capabilities, capability, object)?;
    }
    json::set(&mut result, "capabilitiesObj", capabilities)?;
    for key in ["ready", "unpair", "capabilitiesComplete", "detailComplete"] {
        json::set(&mut result, key, Value::Bool(true))?;
    }
    for key in ["settingsObj", "ui"] {
        json::set(&mut result, key, json::object())?;
    }
    Ok(result)
}

pub(super) fn apps<S: Storage>(store: &Store<S>) -> Result<Value> {
    let mut result = json::Object::new();
    for record in store.document().records("apps") {
        let id = json::text(record, "id");
        result.push(id, app(store, id)?)?;
    }
    Ok(Value::Object(result))
}

pub(super) fn app<S: Storage>(store: &Store<S>, id: &str) -> Result<Value> {
    let app = store.document().record("apps", id)?;
    let empty = json::object();
    let manifest = store.manifest(id).unwrap_or(&empty);
    let runtime = store.app_runtime(id).unwrap_or(&empty);
    let name = json::get(manifest, "name")
        .map(|v| stulp_core::manifest::localized(v, store.language()))
        .filter(|s| !s.is_empty())
        .unwrap_or(id);
    let author = match json::get(manifest, "author") {
        Some(value) => copy(Some(value))?,
        None => json::fields(&[("name", json::string("Unknown")?)])?,
    };
    json::fields(&[
        ("id", json::string(id)?),
        ("name", json::string(name)?),
        ("version", json::string(json::text(manifest, "version"))?),
        (
            "compatibility",
            json::string(json::text(manifest, "compatibility"))?,
        ),
        ("permissions", copy(json::get(manifest, "permissions"))?),
        ("author", author),
        ("enabled", Value::Bool(json::boolean(app, "enabled"))),
        ("offered", Value::Bool(json::boolean(app, "offered"))),
        ("state", json::string(store.app_status(id))?),
        ("crashed", Value::Bool(store.app_status(id) == "crashed")),
        (
            "crashedMessage",
            json::string(json::text(runtime, "error"))?,
        ),
        (
            "crashedCount",
            Value::uint(json::uint(runtime, "restartCount")),
        ),
        ("retryAt", json::string(json::text(runtime, "retryAt"))?),
        (
            "settings",
            Value::Bool(
                json::get(manifest, "settings").is_some_and(|v| *v != Value::Null)
                    || json::get(manifest, "ui").is_some_and(|ui| {
                        json::array(ui, "assets")
                            .iter()
                            .any(|p| p.as_str() == Some("settings/index.html"))
                    }),
            ),
        ),
        ("channel", json::string("live")?),
        ("origin", json::string("devkit_install")?),
        ("autoupdate", Value::Bool(false)),
        ("source", copy(json::get(app, "source"))?),
        (
            "usage",
            json::fields(&[("cpu", Value::uint(0)), ("mem", Value::uint(0))])?,
        ),
    ])
}

pub(super) fn drivers<S: Storage>(store: &Store<S>) -> Result<Value> {
    let mut result = json::Object::new();
    for app in store.document().records("apps") {
        let id = json::text(app, "id");
        let Some(manifest) = store.manifest(id) else {
            continue;
        };
        for definition in json::array(manifest, "drivers") {
            let driver = driver(store, id, definition)?;
            result.push(json::text(&driver, "id"), copy(Some(&driver))?)?;
        }
    }
    Ok(Value::Object(result))
}

pub(super) fn driver<S: Storage>(store: &Store<S>, app: &str, definition: &Value) -> Result<Value> {
    let mut owner = json::copy("stulp:app:")?;
    owner
        .try_reserve(app.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    owner.push_str(app);
    let mut id = json::copy(&owner)?;
    let name = json::text(definition, "id");
    id.try_reserve(name.len() + 1)
        .map_err(|_| stulp_core::Error::Memory)?;
    id.push(':');
    id.push_str(name);
    let mut custom = alloc::vec::Vec::new();
    if let Some(manifest) = store.manifest(app) {
        let ui = json::get(manifest, "ui").unwrap_or(&Value::Null);
        for view in json::array(definition, "pair") {
            let view_id = json::text(view, "id");
            if view_id.is_empty() {
                continue;
            }
            let mut path = json::copy("drivers/")?;
            path.try_reserve(name.len() + view_id.len() + 11)
                .map_err(|_| stulp_core::Error::Memory)?;
            path.push_str(name);
            path.push_str("/pair/");
            path.push_str(view_id);
            path.push_str(".html");
            if json::array(ui, "assets")
                .iter()
                .any(|v| v.as_str() == Some(path.as_str()))
            {
                json::push(&mut custom, json::string(view_id)?, 128)?;
            }
        }
    }
    let object = self::app(store, app)?;
    json::fields(&[
        ("id", json::string(&id)?),
        ("ownerUri", json::string(&owner)?),
        ("ownerName", json::string(json::text(&object, "name"))?),
        (
            "name",
            json::string(
                json::get(definition, "name")
                    .map(|v| stulp_core::manifest::localized(v, store.language()))
                    .unwrap_or(name),
            )?,
        ),
        ("class", json::string(json::text(definition, "class"))?),
        ("ready", Value::Bool(store.app_status(app) == "running")),
        (
            "pair",
            Value::Bool(!json::array(definition, "pair").is_empty()),
        ),
        ("repair", Value::Bool(false)),
        ("unpair", Value::Bool(true)),
        ("deprecated", Value::Bool(false)),
        ("connectivity", json::string("local")?),
        ("pairViews", copy(json::get(definition, "pair"))?),
        ("settings", copy(json::get(definition, "settings"))?),
        ("capabilities", copy(json::get(definition, "capabilities"))?),
        ("customPairViews", Value::Array(custom)),
    ])
}

pub(super) fn manufacturer<'a, S: Storage>(store: &'a Store<S>, device: &'a Value) -> &'a str {
    let app = json::text(device, "appId");
    if app == "com.stulp.scene" {
        return "Stulp";
    }
    for key in ["store", "data"] {
        let value = json::get(device, key)
            .map(|v| json::text(v, "manufacturer"))
            .unwrap_or("")
            .trim();
        if !value.is_empty() {
            return value;
        }
    }
    if let Some(m) = store.manifest(app) {
        for key in ["manufacturer", "name"] {
            let value = json::get(m, key)
                .map(|v| stulp_core::manifest::localized(v, store.language()))
                .unwrap_or("");
            if !value.is_empty() {
                return value;
            }
        }
    }
    app
}
