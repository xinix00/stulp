//! Mounted-file reads, bounded before allocating and parked by the owner.
use crate::storage::{Io, Wait};
use alloc::{format, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, Value},
};
pub(crate) fn read(path: &str, limit: usize, wait: &impl Wait) -> Result<Option<Vec<u8>>> {
    if !crate::storage::valid_path(path) {
        return Err(Error::Invalid("invalid mounted file path"));
    }
    let mut io = applib::appnet::net().ok_or(Error::Storage)?.system_client();
    wait.wait(async {
        let Some(size) = Io::size(&mut io, path).await? else {
            return Ok(None);
        };
        let size = usize::try_from(size).map_err(|_| Error::Full)?;
        if size > limit {
            return Err(Error::Full);
        }
        let mut bytes = Vec::new();
        bytes.try_reserve_exact(size).map_err(|_| Error::Memory)?;
        bytes.resize(size, 0);
        let mut at = 0;
        while at < size {
            let end = size.min(at + 65536);
            let n = Io::read(&mut io, path, at as u64, &mut bytes[at..end]).await?;
            if n == 0 || n > end - at {
                return Err(Error::Storage);
            }
            at += n;
        }
        Ok(Some(bytes))
    })?
}
pub(crate) fn asset(root: &str, name: &str, wait: &impl Wait) -> Result<Value> {
    if !stulp_sdk::valid_asset(name) {
        return Err(Error::Invalid("invalid asset path"));
    }
    let path = format!("{root}/{name}");
    // HopFS has no symlinks; every component is validated before the file RPC.
    let Some(bytes) = read(&path, 4 << 20, wait)? else {
        return json::fields(&[("found", Value::Bool(false))]);
    };
    json::fields(&[
        ("found", Value::Bool(true)),
        (
            "data",
            json::string(&stulp_protocol::token::base64(&bytes)?)?,
        ),
    ])
}
pub(crate) fn timezone(
    name: &str,
    wait: &impl Wait,
) -> Result<stulp_controller::timezone::Timezone> {
    if name.is_empty() || matches!(name, "UTC" | "Etc/UTC") {
        return Ok(Default::default());
    }
    let path = format!("/data/zoneinfo/{name}");
    match read(&path, 1 << 20, wait) {
        Ok(Some(bytes)) => stulp_controller::timezone::Timezone::decode(&bytes),
        Ok(None) | Err(Error::Storage) => stulp_controller::archive::timezone(name),
        Err(error) => Err(error),
    }
}
/// Open the durable document and enter the full controller, on a parked stack.
pub fn run(app: &'static applib::App, wait: &impl Wait) -> Result {
    use stulp_core::{slots, store::Store};
    let env = crate::environment::Environment::open(app)?;
    // De oude LicheeRV-app kon zonder disk draaien. Alleen expliciet kiezen:
    // een kapotte persistente opslag mag nooit ongemerkt een leeg huis worden.
    if app.env("STULP_STORAGE") == Some("memory") {
        app.log(format_args!("STULP_STORAGE_VOLATILE configuration is lost on restart; export a backup before stopping"));
        return serve(
            app,
            wait,
            env,
            Store::open(b"{}", stulp_core::store::Memory)?,
        );
    }
    if app
        .env("STULP_STORAGE")
        .is_some_and(|s| !s.is_empty() && s != "disk")
    {
        return Err(Error::Invalid("STULP_STORAGE must be disk or memory"));
    }
    let net = applib::appnet::net().ok_or(Error::Storage)?;
    let path = app
        .env("STULP_DOCUMENT")
        .filter(|s| !s.is_empty())
        .unwrap_or("/data/stulp.json");
    let (files, bytes) = slots::Files::open(crate::storage::Files::new(
        net.system_client(),
        Borrowed(wait),
        path,
    )?)?;
    serve(app, wait, env, Store::open(&bytes, files)?)
}
fn serve<S: stulp_core::store::Storage>(
    app: &'static applib::App,
    wait: &impl Wait,
    mut env: crate::environment::Environment,
    mut store: stulp_core::store::Store<S>,
) -> Result {
    use stulp_core::json::TryClone;
    let mut system = json::get(store.document().root(), "system")
        .unwrap_or(&Value::Null)
        .try_clone()?;
    if system.is_null() {
        system = json::object();
    }
    if json::text(&system, "attachSecret").is_empty() {
        let secret = match app.env("STULP_ATTACH_SECRET").filter(|s| !s.is_empty()) {
            Some(s) => json::copy(s)?,
            None => stulp_protocol::token::base64(&env.random())?,
        };
        json::set(&mut system, "attachSecret", json::string(&secret)?)?;
        store.system(system)?;
    }
    let timezone = timezone(store.timezone(), wait)?;
    let token = app
        .env("STULP_TOKEN")
        .filter(|s| !s.is_empty())
        .ok_or(Error::Invalid("STULP_TOKEN is required"))?;
    let web = stulp_web::Web::new(token)?;
    let port = |name, default| -> Result<u16> {
        match app.env(name).filter(|s| !s.is_empty()) {
            Some(s) => s
                .parse::<u16>()
                .ok()
                .filter(|n| *n != 0)
                .ok_or(Error::Invalid("invalid listener port")),
            None => Ok(default),
        }
    };
    let apps = crate::apps::Apps::bind(app, port("ER_PORT_ATTACH", 7000)?)?;
    crate::server::start_http(app, port("ER_PORT_HTTP", 8080)?, &mut env)?;
    app.log(format_args!("STULP_CONTROLLER_READY"));
    crate::server::run(store, web, apps, env, wait, timezone)
}
struct Borrowed<'a, W>(&'a W);
impl<W: Wait> Wait for Borrowed<'_, W> {
    fn wait<F: core::future::Future>(&self, f: F) -> Result<F::Output> {
        self.0.wait(f)
    }
}
