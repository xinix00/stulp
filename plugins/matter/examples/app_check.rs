//! De echte plugincallbacks tegen een synthetische controller en het onafhankelijke Go-apparaat.
#[path = "../../../crates/plugin-host/src/udp.rs"]
mod udp;
use std::{
    collections::VecDeque,
    io::{BufRead, Write},
    time::{Duration, Instant},
};
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_matter::Matter;
use stulp_protocol::{Frame, Kind};
use stulp_sdk::{
    Client, Error, Event, Plugin, Result, Transport, UdpCommand, UdpEvent, clone, util::field,
};
fn frame(value: Value) -> Result<Frame> {
    Ok(Frame::decode(
        json::to_string(&value)
            .map_err(stulp_core::Error::from)?
            .as_bytes(),
    )?)
}
struct Controller {
    store: Store<Memory>,
    app: stulp_runtime::App,
    input: VecDeque<Frame>,
    udp: udp::Worker,
    clock: Instant,
    published: bool,
    injected: bool,
    progress: usize,
    triggers: usize,
    offline: Option<String>,
    preempted: Option<Instant>,
    finished: bool,
    parallel: bool,
    offline_command: bool,
}
impl Controller {
    fn new() -> Result<Self> {
        let manifest =
            json::parse(Matter::default().manifest()).map_err(stulp_core::Error::from)?;
        Ok(Self {
            store: Store::open(
                br#"{"version":2,"apps":[{"id":"com.stulp.matter","enabled":true}],"devices":[]}"#,
                Memory,
            )?,
            app: stulp_runtime::App::new("com.stulp.matter", manifest)?,
            input: VecDeque::new(),
            udp: udp::Worker::new()?,
            clock: Instant::now(),
            published: false,
            injected: false,
            progress: 0,
            triggers: 0,
            offline: None,
            preempted: None,
            finished: false,
            parallel: std::env::var_os("STULP_TEST_PARALLEL").is_some(),
            offline_command: false,
        })
    }
}
impl Transport for Controller {
    async fn send(&mut self, v: &Value) -> Result {
        let f = frame(clone(v)?)?;
        if std::env::var_os("STULP_TEST_TRACE").is_some() {
            eprintln!("frame {:?} {} {}", f.kind, f.id, f.method());
        }
        if f.kind != Kind::Request {
            if f.id == 900004 {
                return Err(Error::Invalid(
                    "silent node finished before healthy command",
                ));
            }
            if f.id == 900002 {
                if f.kind != Kind::Response
                    || self
                        .preempted
                        .is_none_or(|at| at.elapsed() > Duration::from_secs(5))
                {
                    return Err(Error::Invalid(
                        "offline Matter node blocked foreground command",
                    ));
                }
                if self.parallel {
                    if !self.offline_command {
                        return Err(Error::Invalid("silent command was not in flight"));
                    }
                    self.finished = true;
                    return Err(Error::Transport("synthetic test completed"));
                }
                self.input.push_back(frame(Frame::request(
                    900003,
                    "device.delete",
                    &json::fields(&[("deviceId", json::string("lamp")?)])?,
                )?)?);
            }
            if f.id == 900003 {
                if f.kind != Kind::Response {
                    return Err(Error::Invalid("foreground removal failed"));
                }
                self.finished = true;
                return Err(Error::Transport("synthetic test completed"));
            }
            if f.id == 900001 {
                if f.kind != Kind::Response || !json::boolean(field(v, "r"), "running") {
                    return Err(Error::Invalid("pair progress did not stay responsive"));
                }
                self.progress += 1;
            }
            return Ok(());
        }
        if f.method() == "flow.trigger" {
            if !json::boolean(field(v, "p"), "system")
                || json::text(field(v, "p"), "kind") != "trigger"
            {
                return Err(Error::Invalid("system trigger contract differs"));
            }
            self.triggers += 1;
        }
        if f.method() == "state.set" && !self.published {
            // Uitsluitend nieuw gegenereerde testsleutels naar de Go-kindprocespipe.
            println!(
                "{}",
                json::to_string(field(field(v, "p"), "state")).map_err(stulp_core::Error::from)?
            );
            std::io::stdout()
                .flush()
                .map_err(|_| Error::Transport("test pipe"))?;
            self.published = true;
        }
        let now = self.now();
        let actions = self
            .app
            .receive(&mut self.store, f, now, "2026-10-01T12:00:00Z", "test")?;
        for out in actions.outgoing {
            let f = frame(out)?;
            // In dit voorbeeld roept main de lifecyclecallbacks expliciet aan.
            if f.kind != Kind::Request {
                self.input.push_back(f);
            }
        }
        Ok(())
    }
    async fn next(&mut self) -> Result<Event> {
        if let Some(f) = self.input.pop_front() {
            return Ok(Event::Frame(f));
        }
        std::thread::sleep(Duration::from_millis(2));
        Ok(Event::Tick)
    }
    fn now(&self) -> u64 {
        self.clock.elapsed().as_millis() as u64
    }
    fn wall_time(&self) -> Result<u64> {
        Ok(1_790_000_000 + self.now() / 1000)
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        hostnet::entropy()
            .map_err(|_| Error::Transport("test entropy"))?
            .get(..32)
            .ok_or(Error::Invalid("entropy length"))?
            .try_into()
            .map_err(|_| Error::Invalid("entropy length"))
    }
    fn udp(&mut self, command: UdpCommand) -> Result {
        if std::env::var_os("STULP_TEST_TRACE").is_some() {
            match &command {
                UdpCommand::Bind { id, address } => eprintln!("bind {id} {address}"),
                UdpCommand::Send { id, address, .. } => eprintln!("udp {id} {address}"),
                _ => (),
            }
        }
        if matches!(command, UdpCommand::Send { .. }) && !self.injected {
            self.injected = true;
            self.input.push_back(frame(Frame::request(
                900001,
                "pair.emit",
                &json::fields(&[
                    ("sessionId", json::string("test")?),
                    ("event", json::string("commission_state")?),
                ])?,
            )?)?);
        }
        if let UdpCommand::Send { id: 3, address, .. } = &command
            && self.offline.as_deref() == Some(address.as_str())
        {
            self.offline_command = true;
        }
        if let UdpCommand::Send { address, .. } = &command
            && self.preempted.is_none()
            && self.offline.as_deref() == Some(address.as_str())
        {
            self.preempted = Some(Instant::now());
            if self.parallel {
                self.input.push_back(frame(Frame::request(
                    900004,
                    "capability.invoke",
                    &json::fields(&[
                        ("deviceId", json::string("offline")?),
                        ("capability", json::string("onoff")?),
                        ("value", Value::Bool(true)),
                    ])?,
                )?)?);
            }
            self.input.push_back(frame(Frame::request(
                900002,
                "capability.invoke",
                &json::fields(&[
                    ("deviceId", json::string("lamp")?),
                    ("capability", json::string("onoff")?),
                    ("value", Value::Bool(false)),
                ])?,
            )?)?);
        }
        self.udp.send(command)
    }
    fn poll_udp(&mut self) -> Option<UdpEvent> {
        self.udp.poll()
    }
}
fn main() -> std::result::Result<(), Box<dyn std::error::Error>> {
    let mut address = String::new();
    std::io::stdin().lock().read_line(&mut address)?;
    let address = address.trim();
    if !address.starts_with("127.0.0.1:") {
        return Err("test requires loopback".into());
    }
    hostnet::block_on(async {
        let mut c = Client::new(Controller::new()?);
        c.hello("com.stulp.matter").await?;
        let mut app = Matter::default();
        app.handle(&mut c, "app.init", &Value::Null).await?;
        app.handle(
            &mut c,
            "pair.start",
            &json::fields(&[
                ("driverId", json::string("matter")?),
                ("sessionId", json::string("test")?),
            ])?,
        )
        .await?;
        let payload = stulp_matter::onboarding::Payload {
            passcode: 20202021,
            discriminator: 3840,
            vendor: 0xfff1,
            product: 0x8000,
            ..Default::default()
        };
        app.handle(
            &mut c,
            "pair.emit",
            &json::fields(&[
                ("sessionId", json::string("test")?),
                ("event", json::string("commission")?),
                (
                    "data",
                    json::fields(&[
                        ("code", json::string(&payload.qr()?)?),
                        ("address", json::string(address)?),
                    ])?,
                ),
            ])?,
        )
        .await?;
        app.tick(&mut c).await?;
        let state = app
            .handle(
                &mut c,
                "pair.emit",
                &json::fields(&[
                    ("sessionId", json::string("test")?),
                    ("event", json::string("commission_state")?),
                ])?,
            )
            .await?;
        if json::boolean(&state, "running") || !json::text(&state, "warning").is_empty() {
            return Err(Error::Remote(json::copy(json::text(&state, "warning"))?));
        }
        let candidates = app
            .handle(
                &mut c,
                "pair.emit",
                &json::fields(&[
                    ("sessionId", json::string("test")?),
                    ("event", json::string("list_devices")?),
                ])?,
            )
            .await?;
        let list = candidates
            .as_array()
            .ok_or(Error::Invalid("candidate list"))?;
        if list.len() != 1 || json::text(&list[0], "name") != "Fake Lamp" {
            return Err(Error::Invalid("plugin model differs"));
        }
        let mut device = clone(&list[0])?;
        json::set(&mut device, "id", json::string("lamp")?)?;
        json::set(&mut device, "appId", json::string("com.stulp.matter")?)?;
        // Een bestaand Go-model moet via dezelfde CASE-sessie verversen, zonder
        // opnieuw koppelen of de naam/groep/geheime fabricvelden te vervangen.
        json::set(&mut device, "name", json::string("Hall lamp")?)?;
        let credentials = json::string(json::text(field(&device, "store"), "matter.noc"))?;
        let mut model = clone(field(&device, "store"))?;
        json::set(&mut model, "matter.modelVersion", Value::uint(1))?;
        json::set(&mut device, "store", model)?;
        let mut snapshot = clone(c.state().root())?;
        let mut transport = c.into_transport();
        transport
            .store
            .put("devices", device, true, None, "2026-10-01T12:00:00Z")?;
        json::set(
            &mut snapshot,
            "devices",
            json::fields(&[("lamp", transport.store.device("lamp")?)])?,
        )?;
        let mut c = Client::from_snapshot(transport, snapshot)?;
        let p = json::fields(&[("deviceId", json::string("lamp")?)])?;
        app.handle(&mut c, "device.init", &p).await?;
        for on in [true, false] {
            app.handle(
                &mut c,
                "capability.invoke",
                &json::fields(&[
                    ("deviceId", json::string("lamp")?),
                    ("capability", json::string("onoff")?),
                    ("value", Value::Bool(on)),
                ])?,
            )
            .await?;
            if field(field(c.state().device("lamp")?, "state"), "onoff").as_bool() != Some(on) {
                return Err(Error::Invalid("command state not published"));
            }
        }
        // Laat de echte subscriber en diens peer-initiated event lopen.
        let until = c.now() + 10000;
        loop {
            app.tick(&mut c).await?;
            let n = json::text(
                field(c.state().device("lamp")?, "store"),
                "matter.lastEventNumber",
            );
            if !n.is_empty() {
                break;
            }
            if c.now() >= until {
                return Err(Error::Timeout);
            }
            c.idle().await?;
        }
        let lamp = c.state().device("lamp")?;
        if json::uint(field(lamp, "store"), "matter.modelVersion") != 4
            || json::text(lamp, "name") != "Hall lamp"
            || field(field(lamp, "store"), "matter.noc") != &credentials
        {
            return Err(Error::Invalid(
                "existing Matter model was not refreshed safely",
            ));
        }
        // Keep a second node's UDP port open but silent: no ICMP shortcut, a real
        // forty-second CASE wait unless the foreground callback preempts it.
        let blackhole = std::net::UdpSocket::bind("127.0.0.1:0")
            .map_err(|_| Error::Transport("test UDP bind"))?;
        let offline_address = blackhole
            .local_addr()
            .map_err(|_| Error::Transport("test UDP address"))?
            .to_string();
        let mut fabric = stulp_matter::fabric::Fabric::load(&mut c).await?;
        let offline_node = fabric.allocate(&mut c).await?;
        let public = stulp_matter::certificate::Certificate::from_tlv(&fabric.controller()?)?;
        let noc = fabric.issue(public.public(), offline_node, [5; 16], c.wall_time()?)?;
        let mut offline = clone(c.state().device("lamp")?)?;
        json::set(&mut offline, "id", json::string("offline")?)?;
        let mut data = clone(field(&offline, "data"))?;
        json::set(&mut data, "id", json::string("offline-node")?)?;
        json::set(
            &mut data,
            "nodeId",
            json::string(&stulp_matter::model::node_id(offline_node)?)?,
        )?;
        json::set(&mut offline, "data", data)?;
        let mut fields = clone(field(&offline, "store"))?;
        json::set(
            &mut fields,
            "matter.address",
            json::string(&offline_address)?,
        )?;
        json::set(
            &mut fields,
            "matter.nodeId",
            json::string(&stulp_matter::model::node_id(offline_node)?)?,
        )?;
        json::set(
            &mut fields,
            "matter.noc",
            clone(field(&stulp_sdk::asset(&noc)?, "data"))?,
        )?;
        json::set(&mut offline, "store", fields)?;
        let mut snapshot = clone(c.state().root())?;
        let mut transport = c.into_transport();
        if transport.progress != 1 || transport.triggers != 1 {
            return Err(Error::Invalid("progress or subscription event missing"));
        }
        transport
            .store
            .put("devices", offline, true, None, "2026-10-01T12:00:00Z")?;
        let mut devices = clone(field(&snapshot, "devices"))?;
        json::set(&mut devices, "offline", transport.store.device("offline")?)?;
        json::set(&mut snapshot, "devices", devices)?;
        transport.offline = Some(offline_address);
        let mut c = Client::from_snapshot(transport, snapshot)?;
        app.handle(
            &mut c,
            "device.init",
            &json::fields(&[("deviceId", json::string("offline")?)])?,
        )
        .await?;
        let result = c.serve_initialized(&mut app).await;
        if !matches!(result, Err(Error::Transport("synthetic test completed"))) {
            eprintln!("pool stopped: {result:?}");
            return Err(Error::Invalid("preemption test did not complete"));
        }
        let transport = c.into_transport();
        if !transport.finished || transport.preempted.is_none() {
            return Err(Error::Invalid("offline maintenance was not exercised"));
        }
        drop(blackhole);
        Ok::<_, Error>(())
    })?;
    Ok(())
}
