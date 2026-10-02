//! Eenmalige beheercommando's gebruiken dezelfde store en pluginpomp als de server.
use std::time::Duration;
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    manifest,
    store::{Storage, Store},
};
use stulp_web::Environment as _;
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
fn arity(args: &[String], n: usize) -> Result {
    if args.len() != n {
        Err(Error::Invalid("incorrect command argument count"))
    } else {
        Ok(())
    }
}
fn emit(value: &Value) -> Result {
    println!("{}", json::to_string(value)?);
    Ok(())
}
fn metadata<S: Storage>(store: &mut Store<S>, id: &str) -> Result<Value> {
    let record = store.document().record("apps", id)?;
    let m = crate::processes::read_manifest(json::text(record, "root"))?;
    store.announce(id, m.try_clone()?)?;
    Ok(m)
}
/// Voert één volledig geparseerd beheercommando uit; serve en archieven zitten bij de adapter.
pub fn command<S: Storage>(store: &mut Store<S>, command: &str, args: &[String]) -> Result {
    let env = crate::Environment;
    match command {
        "install" => {
            arity(args, 1)?;
            let root = std::fs::canonicalize(&args[0])
                .map_err(|_| Error::Missing("app directory unavailable"))?;
            let root = root
                .to_str()
                .ok_or(Error::Invalid("app path is not UTF-8"))?;
            let m = crate::processes::read_manifest(root)?;
            manifest::validate(&m)?;
            let id = json::text(&m, "id");
            let previous = store.document().record("apps", id).ok();
            let create = previous.is_none();
            let mut app = previous.unwrap_or(&json::object()).try_clone()?;
            for (key, value) in [("id", id), ("root", root)] {
                json::set(&mut app, key, json::string(value)?)?;
            }
            json::set(&mut app, "enabled", Value::Bool(true))?;
            json::set(&mut app, "offered", Value::Bool(false))?;
            store.put("apps", app, create, None, &env.now()?)?;
            store.announce(id, m.try_clone()?)?;
            println!("installed {id}@{} from {root}", json::text(&m, "version"));
            Ok(())
        }
        "uninstall" => {
            arity(args, 1)?;
            let mut processes = crate::processes::Processes::new();
            let mut apps = crate::apps::Apps::new(None)
                .map_err(|_| Error::Invalid("cannot initialize app transport"))?;
            let request = stulp_web::Request {
                method: json::copy("DELETE")?,
                path: format!("/api/manager/apps/app/{}", args[0]),
                query: String::new(),
                cookie: String::new(),
                origin: String::new(),
                host: String::new(),
                headers: json::object(),
                body: Vec::new(),
            };
            let response = processes
                .route(store, &mut apps, &request)?
                .ok_or(Error::Invalid("invalid app id"))?;
            println!(
                "{}",
                std::str::from_utf8(response.body.bytes())
                    .map_err(|_| Error::Invalid("invalid command response"))?
            );
            Ok(())
        }
        "attach-token" => {
            arity(args, 1)?;
            let mut system = field(store.document().root(), "system").try_clone()?;
            if args[0] == "--rotate" {
                json::set(&mut system, "attachSecret", json::string("")?)?;
                store.system(system)?;
                println!("the attach secret is gone; every token that used it stops working");
                return Ok(());
            }
            let id = &args[0];
            if id.is_empty() || id.contains(['\0', '/']) {
                return Err(Error::Invalid("invalid app id"));
            }
            if json::text(&system, "attachSecret").is_empty() {
                let secret = stulp_protocol::token::base64(
                    &hostnet::entropy().map_err(|_| Error::Invalid("OS entropy unavailable"))?
                        [..32],
                )?;
                json::set(&mut system, "attachSecret", json::string(&secret)?)?;
                store.system(system.try_clone()?)?;
            }
            if store.document().record("apps", id).is_err() {
                eprintln!(
                    "note: no app {id:?} is known yet; this token becomes usable when it announces itself"
                );
            }
            println!(
                "{}",
                stulp_protocol::token::token(json::text(&system, "attachSecret"), id)?
            );
            Ok(())
        }
        "apps" => {
            arity(args, 0)?;
            let mut values = Vec::new();
            let mut ids = Vec::new();
            for app in store.document().records("apps") {
                json::push(&mut ids, json::copy(json::text(app, "id"))?, 4096)?;
            }
            for id in ids {
                let mut record = store.document().record("apps", &id)?.try_clone()?;
                if let Ok(m) = metadata(store, &id) {
                    json::set(&mut record, "manifest", m)?;
                }
                json::push(&mut values, record, 4096)?;
            }
            emit(&Value::Array(values))
        }
        "devices" => {
            if args.len() > 1 {
                return Err(Error::Invalid("usage: devices [APP_ID]"));
            }
            let mut devices = Vec::new();
            for record in store.document().records("devices") {
                if args
                    .first()
                    .is_none_or(|a| a == json::text(record, "appId"))
                {
                    json::push(&mut devices, store.device(json::text(record, "id"))?, 4096)?;
                }
            }
            emit(&Value::Array(devices))
        }
        "add-device" => add_device(store, args),
        "run" | "inspect" | "pair-list" | "invoke" | "flow" => execute(store, command, args),
        _ => Err(Error::Invalid("unknown command")),
    }
}
fn add_device<S: Storage>(store: &mut Store<S>, args: &[String]) -> Result {
    let mut input = json::object();
    let mut names = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        if let Some(key) = arg.strip_prefix("--") {
            if !matches!(key, "name" | "class" | "data" | "settings") {
                return Err(Error::Invalid("unknown add-device option"));
            }
            let value = iter
                .next()
                .ok_or(Error::Invalid("missing add-device option value"))?;
            let value = if matches!(key, "data" | "settings") {
                json::parse(value.as_bytes())?
            } else {
                json::string(value)?
            };
            json::set(&mut input, key, value)?;
        } else {
            json::push(&mut names, arg.as_str(), 2)?;
        }
    }
    if names.len() != 2 {
        return Err(Error::Invalid(
            "usage: add-device --name NAME APP_ID DRIVER_ID",
        ));
    }
    let (app, id) = (names[0], names[1]);
    let m = metadata(store, app)?;
    let driver = json::array(&m, "drivers")
        .iter()
        .find(|d| json::text(d, "id") == id)
        .ok_or(Error::Missing("driver does not exist"))?;
    let mut env = crate::Environment;
    let device = crate::pairing::candidate(driver, app, id, &env.id()?, &input)?;
    let id = json::copy(json::text(&device, "id"))?;
    store.put("devices", device, true, None, &env.now()?)?;
    emit(&store.device(&id)?)
}
struct Runner {
    processes: crate::processes::Processes,
    apps: crate::apps::Apps<'static>,
    next: u64,
}
impl Runner {
    fn start<S: Storage>(store: &mut Store<S>, id: &str) -> Result<Self> {
        let record = store.document().record("apps", id)?;
        if json::boolean(record, "offered") {
            return Err(Error::Invalid("app is not installed"));
        }
        let mut runner = Self {
            processes: crate::processes::Processes::for_app(id)?,
            apps: crate::apps::Apps::for_app(id)?,
            next: 0,
        };
        let until = runner.apps.now() + 120_000;
        while !runner.apps.running(id) {
            runner.poll(store)?;
            if stulp_platform::signals::requested() {
                return Err(Error::Invalid("command cancelled"));
            }
            if runner.apps.now() >= until {
                return Err(Error::Invalid("app startup timed out"));
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        Ok(runner)
    }
    fn poll<S: Storage>(&mut self, store: &mut Store<S>) -> Result<Vec<stulp_runtime::Completion>> {
        let completed = self.apps.poll(store)?;
        self.processes.poll(store, &mut self.apps)?;
        Ok(completed)
    }
    fn call<S: Storage>(
        &mut self,
        store: &mut Store<S>,
        app: &str,
        method: &str,
        params: &Value,
    ) -> Result<Value> {
        self.next = self.next.checked_add(1).ok_or(Error::Full)?;
        self.apps.call(app, self.next, method, params)?;
        let until =
            self.apps.now() + stulp_runtime::callback_timeout_for(app, method, params) + 1000;
        loop {
            for result in self.poll(store)? {
                if result.owner != self.next {
                    continue;
                }
                if result.failed {
                    eprintln!(
                        "[stulp:command-callback] {}",
                        json::text(&result.value, "message")
                    );
                    return Err(Error::Invalid("app callback failed"));
                }
                return Ok(result.value);
            }
            if stulp_platform::signals::requested() {
                return Err(Error::Invalid("command cancelled"));
            }
            if self.apps.now() >= until {
                return Err(Error::Invalid("app callback timed out"));
            }
            std::thread::sleep(Duration::from_millis(5));
        }
    }
}
fn execute<S: Storage>(store: &mut Store<S>, command: &str, args: &[String]) -> Result {
    let (once, args) = if command == "run" && args.first().is_some_and(|s| s == "--once") {
        (true, &args[1..])
    } else {
        (false, args)
    };
    match command {
        "run" | "inspect" => arity(args, 1)?,
        "pair-list" => arity(args, 2)?,
        "invoke" => arity(args, 4)?,
        "flow" if (3..=5).contains(&args.len()) => (),
        _ => return Err(Error::Invalid("invalid plugin command arguments")),
    }
    let app = &args[0];
    let mut runner = Runner::start(store, app)?;
    match command {
        "run" => {
            println!("started {app} successfully");
            while !once && !stulp_platform::signals::requested() {
                runner.poll(store)?;
                std::thread::sleep(Duration::from_millis(10));
            }
            Ok(())
        }
        "inspect" => emit(&runner.call(store, app, "registrations", &json::object())?),
        "pair-list" => emit(&runner.call(
            store,
            app,
            "pair.list",
            &json::fields(&[("driverId", json::string(&args[1])?)])?,
        )?),
        "invoke" => {
            let device = &args[1];
            if json::text(store.document().record("devices", device)?, "appId") != app {
                return Err(Error::Invalid("device belongs to another app"));
            }
            runner.call(
                store,
                app,
                "capability.invoke",
                &json::fields(&[
                    ("deviceId", json::string(device)?),
                    ("capability", json::string(&args[2])?),
                    ("value", json::parse(args[3].as_bytes())?),
                    ("options", json::object()),
                ])?,
            )?;
            emit(&store.device(device)?)
        }
        "flow" => {
            let decode = |i: usize| -> Result<Value> {
                let value = match args.get(i) {
                    Some(s) => json::parse(s.as_bytes())?,
                    None => json::object(),
                };
                if value.as_object().is_none() {
                    return Err(Error::Invalid("flow arguments and state must be objects"));
                }
                Ok(value)
            };
            emit(&runner.call(
                store,
                app,
                "flow.run",
                &json::fields(&[
                    ("kind", json::string(&args[1])?),
                    ("id", json::string(&args[2])?),
                    ("args", decode(3)?),
                    ("state", decode(4)?),
                ])?,
            )?)
        }
        _ => Err(Error::Invalid("unknown plugin command")),
    }
}
