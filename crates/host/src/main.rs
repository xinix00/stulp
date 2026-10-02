//! Stulp voor Linux en macOS; gedeelde logica met het HopOS-doel.
#![forbid(unsafe_code)]
use std::{
    net::{SocketAddr, TcpListener},
    path::Path,
    process::ExitCode,
};
use stulp_core::{json, store::Store};
use stulp_host::files::Files;

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("[stulp:error] {e}");
            ExitCode::FAILURE
        }
    }
}

fn run() -> Result<(), String> {
    let _signals = stulp_platform::signals::Signals::install().map_err(|e| e.to_string())?;
    let mut path = String::from("stulp.json");
    let mut listen = String::from("127.0.0.1:8080");
    let mut token = std::env::var("STULP_TOKEN").unwrap_or_default();
    let mut attach = None;
    let mut socket = None;
    let mut plaintext = false;
    let mut tls_cert = None;
    let mut tls_key = None;
    let mut level = stulp_host::logging::Level::Info;
    let mut language = String::from("nl");
    let mut timezone = String::from("UTC");
    let mut command = None;
    let mut operands = Vec::new();
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--document" | "--db" => path = args.next().ok_or("--document requires a path")?,
            "--listen" => listen = args.next().ok_or("--listen requires an address")?,
            "--language" => language = args.next().ok_or("--language requires a value")?,
            "--timezone" => timezone = args.next().ok_or("--timezone requires a value")?,
            "--log-level" => {
                level = stulp_host::logging::Level::parse(
                    &args.next().ok_or("--log-level requires a value")?,
                )
                .map_err(|e| e.to_string())?
            }
            "--token" => token = args.next().ok_or("--token requires a value")?,
            "--attach-port" => {
                attach = Some(args.next().ok_or("--attach-port requires an address")?)
            }
            "--tls-cert" => tls_cert = Some(args.next().ok_or("--tls-cert requires a path")?),
            "--tls-key" => tls_key = Some(args.next().ok_or("--tls-key requires a path")?),
            "--attach-plaintext" => plaintext = true,
            "--attach" => socket = Some(args.next().ok_or("--attach requires a socket path")?),
            "--version" => {
                println!("stulp {}", env!("CARGO_PKG_VERSION"));
                return Ok(());
            }
            "--help" | "-h" => {
                usage();
                return Ok(());
            }
            "serve" | "check" | "apps" | "devices" | "backup" | "restore" | "install"
            | "uninstall" | "attach-token" | "add-device" | "run" | "inspect" | "pair-list"
            | "invoke" | "flow"
                if command.is_none() =>
            {
                command = Some(arg)
            }
            _ if command.is_some() => operands.push(arg),
            _ => return Err(format!("unknown argument: {arg}")),
        }
    }
    let Some(command) = command else {
        usage();
        return Ok(());
    };
    if command == "restore" {
        if operands.len() != 1 {
            return Err("restore requires one archive path".into());
        }
        if let Some(parent) = Path::new(&path)
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
        {
            std::fs::create_dir_all(parent).map_err(|e| e.to_string())?;
        }
    }
    let files = Files::new(Path::new(&path)).map_err(|e| e.to_string())?;
    let bytes = files.read().map_err(|e| e.to_string())?;
    let mut store = Store::open(&bytes, files).map_err(|e| e.to_string())?;
    store
        .configure(&language, &timezone)
        .map_err(|e| e.to_string())?;
    match command.as_str() {
        "backup" | "restore" => {
            if operands.len() != 1 {
                return Err(format!("usage: stulp-host {command} FILE.zip"));
            }
            let file = Path::new(&operands[0]);
            if command == "backup" {
                stulp_host::archive::write_file(
                    &store.document().encode().map_err(|e| e.to_string())?,
                    file,
                )
                .map_err(|e| e.to_string())?;
                println!("backup written to {}", file.display());
            } else {
                let mut input = std::fs::File::open(file).map_err(|e| e.to_string())?;
                let prepared = stulp_host::archive::Prepared::read(
                    &mut input,
                    Path::new(store.path().ok_or("document path missing")?),
                )
                .map_err(|e| e.to_string())?;
                let result = prepared.apply(&mut store).map_err(|e| e.to_string())?;
                println!("{}", json::to_string(&result).map_err(|e| e.to_string())?);
            }
            Ok(())
        }
        "check" => {
            if !operands.is_empty() {
                return Err("check does not accept positional arguments".into());
            }
            println!(
                "[stulp:document] valid; apps={} devices={} flows={} scenes={}",
                store.document().records("apps").len(),
                store.document().records("devices").len(),
                store.document().records("flows").len(),
                store.document().records("scenes").len()
            );
            Ok(())
        }
        "serve" => {
            if !operands.is_empty() {
                return Err("serve does not accept positional arguments".into());
            }
            let address: SocketAddr = listen.parse().map_err(|_| "invalid listen address")?;
            if !address.ip().is_loopback() && token.is_empty() {
                return Err("a non-loopback listener requires --token or STULP_TOKEN".into());
            }
            let identity = match (tls_cert.as_deref(), tls_key.as_deref()) {
                (None, None) => None,
                (Some(cert), Some(key)) => Some(
                    stulp_transport::load_identity(Path::new(cert), Path::new(key))
                        .map_err(|e| e.to_string())?,
                ),
                _ => return Err("--tls-cert and --tls-key belong together".into()),
            };
            let web = stulp_web::Web::new(&token).map_err(|e| e.to_string())?;
            let attach = if let Some(address) = attach {
                if !plaintext && identity.is_none() {
                    return Err("--attach-port requires --tls-cert and --tls-key, or explicit --attach-plaintext".into());
                }
                Some(TcpListener::bind(address).map_err(|e| e.to_string())?)
            } else {
                None
            };
            let listener = TcpListener::bind(address).map_err(|e| e.to_string())?;
            println!(
                "[stulp:listening] {}://{}",
                if identity.is_some() { "https" } else { "http" },
                listener.local_addr().map_err(|e| e.to_string())?
            );
            if let Some(listener) = &attach {
                println!(
                    "[stulp:attach] {}://{}",
                    if !plaintext && identity.is_some() {
                        "tls"
                    } else {
                        "tcp"
                    },
                    listener.local_addr().map_err(|e| e.to_string())?
                );
            }
            let mut apps = match identity.as_ref().filter(|_| !plaintext) {
                Some(identity) => stulp_host::apps::Apps::secure(attach, identity),
                None => stulp_host::apps::Apps::new(attach),
            }
            .map_err(|e| e.to_string())?;
            if let Some(path) = socket {
                apps.attach_local(Path::new(&path))
                    .map_err(|e| e.to_string())?;
                println!("[stulp:attach] unix://{path}");
            }
            stulp_host::server::serve_secure(listener, store, web, apps, level, identity.as_ref())
                .map_err(|e| e.to_string())
        }
        _ => stulp_host::cli::command(&mut store, &command, &operands).map_err(|e| e.to_string()),
    }
}

fn usage() {
    println!(
        "stulp-host [--document PATH] COMMAND\n\
         serve [--listen ADDRESS] [--token KEY] [--language nl] [--timezone UTC] [--tls-cert FILE --tls-key FILE]\n\
               [--attach SOCKET] [--attach-port ADDRESS --attach-plaintext] [--log-level LEVEL]\n\
         check | apps | devices [APP_ID]\n\
         install DIRECTORY | uninstall APP_ID\n\
         attach-token APP_ID | attach-token --rotate\n\
         backup FILE.zip | restore FILE.zip\n\
         add-device APP_ID DRIVER_ID [--name NAME] [--class CLASS] [--data JSON] [--settings JSON]\n\
         run APP_ID [--once] | inspect APP_ID | pair-list APP_ID DRIVER_ID\n\
         invoke APP_ID DEVICE_ID CAPABILITY JSON | flow APP_ID KIND CARD_ID [ARGS_JSON] [STATE_JSON]"
    );
}
