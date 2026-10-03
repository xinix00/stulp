//! Draaiende Matter-plugin: UI en protocoltaken hebben afzonderlijke eigenaars.
use crate::{
    engine::Engine,
    ui::{self, Job, Ui},
};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Plugin, Result, Transport, clone,
    jobs::{self, Action, Gate},
    util::field,
};
/// Eén appinstantie per geauthenticeerde attach; fabric staat uitsluitend in appState.
#[derive(Default)]
pub struct Matter {
    ui: Ui,
    engine: Option<Engine>,
    /// Geen achtergrondonderhoud vóór dit moment: zolang de controller
    /// app.init, driver.init en device.init stuurt, is de start bezig.
    quiet_until: u64,
}
/// Zoveel stilte na de laatste startcallback voordat het onderhoud begint.
///
/// Het onderhoud (CASE en abonneren, node na node) draait als taak die elke
/// startcallback afbreekt. Na een restore of herverbinding komen er ~70
/// device.init's achter elkaar; tot 03-10 begon het onderhoud tussen elk
/// tweetal opnieuw en werd het telkens midden in een handshake afgebroken
/// (137 van 173 handshakes op de LicheeRV), zodat geen node ooit verbonden
/// raakte en de apparaten kwamen en gingen.
const STARTUP_QUIET_MS: u64 = 3000;
impl Matter {
    fn engine(&mut self) -> Result<&mut Engine> {
        self.engine
            .as_mut()
            .ok_or(Error::Invalid("Matter-controller is niet gestart."))
    }
    fn topology<T: Transport>(&mut self, c: &Client<T>) -> Result {
        if let Some(e) = &mut self.engine {
            e.sync(c.state().root(), c.now())?;
            self.ui.topology = e.topology(c.state().root(), c.wall_time()?)?;
        }
        Ok(())
    }
}
// Alleen achtergrondwerk wijkt voor callbacks; ook driver.init moet tijdens herstel voorrang krijgen.
struct Background<'a>(&'a mut Ui);
impl Gate for Background<'_> {
    fn handle(
        &mut self,
        snapshot: &Value,
        wall: u64,
        method: &str,
        params: &Value,
    ) -> Result<Action> {
        let action = self.0.handle(snapshot, wall, method, params)?;
        if matches!(action, Action::Defer)
            && matches!(
                method,
                "app.init"
                    | "driver.init"
                    | "capability.invoke"
                    | "capabilities.invoke"
                    | "device.settings"
                    | "device.delete"
                    | "device.init"
            )
        {
            Ok(Action::Preempt)
        } else {
            Ok(action)
        }
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;

    #[test]
    fn every_startup_callback_preempts_network_maintenance() {
        let mut ui = Ui::default();
        let mut gate = Background(&mut ui);
        for method in ["app.init", "driver.init", "device.init"] {
            assert!(matches!(
                gate.handle(&json::object(), 0, method, &json::object())
                    .unwrap(),
                Action::Preempt
            ));
        }
    }
}
impl Plugin for Matter {
    async fn serve<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        jobs::pool::serve(
            c,
            self,
            core::array::from_fn(|_| Commands::default()),
            Nodes,
        )
        .await
    }
    fn manifest(&self) -> &'static [u8] {
        ui::MANIFEST
    }
    fn assets(&self) -> &'static [&'static str] {
        ui::ASSETS
    }
    async fn handle<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        p: &Value,
    ) -> Result<Value> {
        self.topology(c)?;
        if matches!(method, "app.init" | "driver.init" | "device.init") {
            self.quiet_until = c.now().saturating_add(STARTUP_QUIET_MS);
        }
        match self
            .ui
            .handle(c.state().root(), c.wall_time()?, method, p)?
        {
            Action::Reply(r) => return r,
            Action::Cancel(v) => return Ok(v),
            Action::Defer | Action::Preempt => (),
        }
        match method {
            "app.init" => {
                crate::maintenance::upgrade(c).await?;
                if self.engine.is_none() {
                    self.engine = Some(
                        jobs::run(c, &mut self.ui, async |worker| Engine::open(worker).await)
                            .await?,
                    );
                }
                self.topology(c)?;
                Ok(Value::Null)
            }
            "driver.init" => {
                if json::text(p, "driverId") != "matter" {
                    return Err(Error::Invalid("Onbekende Matter-driver."));
                }
                Ok(Value::Null)
            }
            "device.init" => {
                let d = clone(c.state().device(json::text(p, "deviceId"))?)?;
                crate::engine::node_id(&d)?;
                self.engine()?.sync(c.state().root(), c.now())?;
                if json::text(&d, "class") == "other" {
                    let types =
                        crate::devices::ids(field(field(&d, "store"), "matter.deviceTypes"))?;
                    let class = crate::capabilities::class(
                        &types,
                        &crate::devices::ids(field(field(&d, "store"), "matter.serverClusters"))?,
                    );
                    if class != "other" {
                        c.call(
                            "device.set",
                            &json::fields(&[
                                ("deviceId", clone(field(&d, "id"))?),
                                ("field", json::string("class")?),
                                ("value", json::string(class)?),
                            ])?,
                        )
                        .await?;
                    }
                }
                Ok(Value::Null)
            }
            "device.delete" => {
                let engine = self
                    .engine
                    .as_mut()
                    .ok_or(Error::Invalid("Matter-controller is niet gestart."))?;
                jobs::run(c, &mut self.ui, async |worker| {
                    engine.remove(worker, json::text(p, "deviceId")).await
                })
                .await
            }
            "capability.invoke" | "capabilities.invoke" => {
                let id = json::text(p, "deviceId");
                let mut values = json::object();
                if method == "capability.invoke" {
                    json::set(
                        &mut values,
                        json::text(p, "capability"),
                        clone(field(p, "value"))?,
                    )?;
                } else {
                    for command in json::array(p, "commands") {
                        json::set(
                            &mut values,
                            json::text(command, "capability"),
                            clone(field(command, "value"))?,
                        )?;
                    }
                }
                let engine = self
                    .engine
                    .as_mut()
                    .ok_or(Error::Invalid("Matter-controller is niet gestart."))?;
                let errors = jobs::run(c, &mut self.ui, async |worker| {
                    engine.command(worker, id, &values).await
                })
                .await?;
                if method == "capability.invoke" {
                    if let Some(error) =
                        json::get(&errors, json::text(p, "capability")).and_then(Value::as_str)
                    {
                        return Err(Error::Remote(json::copy(error)?));
                    }
                    Ok(Value::Null)
                } else {
                    Ok(errors)
                }
            }
            "device.settings" => {
                let engine = self
                    .engine
                    .as_mut()
                    .ok_or(Error::Invalid("Matter-controller is niet gestart."))?;
                jobs::run(c, &mut self.ui, async |worker| {
                    engine
                        .settings(worker, json::text(p, "deviceId"), field(p, "settings"))
                        .await
                })
                .await
            }
            _ => Err(Error::Invalid("Onbekende Matter-opdracht.")),
        }
    }
    async fn settings<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        id: &str,
        patch: &Value,
    ) -> Result<Value> {
        self.handle(
            c,
            "device.settings",
            &json::fields(&[("deviceId", json::string(id)?), ("settings", clone(patch)?)])?,
        )
        .await
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        let Some(engine) = self.engine.as_mut() else {
            return Ok(());
        };
        if let Some(job) = self.ui.pending.take() {
            self.ui.running = true;
            match job {
                Job::Mesh { window } => {
                    let topology = engine.topology(c.state().root(), c.wall_time()?)?;
                    let fabric = u64::from_be_bytes(engine.fabric.case().compressed_id()?);
                    let result = jobs::run(c, &mut self.ui, async |worker| {
                        engine
                            .browse(
                                worker,
                                &[
                                    crate::discovery::OPERATIONAL,
                                    crate::discovery::BORDER_ROUTER,
                                ],
                                window,
                            )
                            .await
                    })
                    .await;
                    let deadline = c.now().saturating_add(720000);
                    let map = crate::mesh::Map::new(
                        &topology,
                        result.as_deref().unwrap_or(&[]),
                        fabric,
                        deadline,
                        result.as_ref().err(),
                    )?;
                    self.ui.map = Some(map);
                    mesh_step(&mut self.ui, c.wall_time()?)?;
                }
                Job::MeshNode {
                    index,
                    device,
                    deadline,
                } => {
                    let deadline = deadline.min(c.now().saturating_add(10000));
                    let result = if c.now() >= deadline {
                        Err(Error::Timeout)
                    } else {
                        jobs::run(c, &mut self.ui, async |worker| {
                            engine.diagnose(worker, &device, deadline).await
                        })
                        .await
                    };
                    if let Some(map) = &mut self.ui.map {
                        map.apply(index, result)?;
                    }
                    mesh_step(&mut self.ui, c.wall_time()?)?;
                }

                Job::Diagnose { device } => {
                    let result = jobs::run(c, &mut self.ui, async |worker| {
                        let deadline = worker.now().saturating_add(300000);
                        engine.diagnose(worker, &device, deadline).await
                    })
                    .await;
                    if let Some(d) = self.ui.diagnoses.iter_mut().find(|d| d.id == device) {
                        ui::done(&mut d.state, c.wall_time()?, result.as_ref().err())?;
                        if let Ok(value) = result {
                            json::set(&mut d.state, "diagnostics", value)?;
                        }
                    }
                }

                Job::Commission {
                    pair,
                    payload,
                    address,
                } => {
                    let result = jobs::run(c, &mut self.ui, async |worker| {
                        engine.commission(worker, payload, &address).await
                    })
                    .await;
                    if let Some(p) = self.ui.pairs.iter_mut().find(|p| p.id == pair) {
                        ui::done(&mut p.state, c.wall_time()?, result.as_ref().err())?;
                        if let Ok(devices) = result {
                            json::set(&mut p.state, "found", Value::uint(devices.len() as u64))?;
                            p.devices = Value::Array(devices);
                            self.ui.found = clone(&p.devices)?;
                        }
                    }
                    self.ui.active.clear();
                }
                Job::Scan { window } => {
                    let result = jobs::run(c, &mut self.ui, async |worker| {
                        engine
                            .browse(
                                worker,
                                &[
                                    crate::discovery::OPERATIONAL,
                                    crate::discovery::COMMISSIONABLE,
                                    crate::discovery::BORDER_ROUTER,
                                ],
                                window,
                            )
                            .await
                    })
                    .await;
                    ui::done(&mut self.ui.scan, c.wall_time()?, result.as_ref().err())?;
                    if let Ok(found) = result {
                        let mut operational = alloc::vec::Vec::new();
                        let mut commissionable = alloc::vec::Vec::new();
                        let mut routers = alloc::vec::Vec::new();
                        for n in found {
                            let entry = scan_entry(&n)?;
                            let list = if n.kind == "operational" {
                                &mut operational
                            } else if n.kind == "commissionable" {
                                &mut commissionable
                            } else {
                                &mut routers
                            };
                            json::push(list, entry, 128)?;
                        }
                        for (key, values) in [
                            ("operational", operational),
                            ("commissionable", commissionable),
                            ("borderRouters", routers),
                        ] {
                            json::set(&mut self.ui.scan, key, Value::Array(values))?;
                        }
                    }
                }
            }
            self.ui.running = false;
        } else if c.now() >= self.quiet_until && engine.ready(c)? {
            match jobs::run(c, &mut Background(&mut self.ui), async |worker| {
                engine.tick(worker).await
            })
            .await
            {
                Ok(()) | Err(Error::Cancelled) => (),
                Err(e) => return Err(e),
            }
        }
        self.topology(c)
    }
}
pub(crate) fn scan_entry(n: &crate::discovery::Node) -> Result<Value> {
    let mut addresses = alloc::vec::Vec::new();
    for a in &n.addresses {
        json::push(&mut addresses, json::string(a)?, 32)?;
    }
    let mut out = json::fields(&[
        ("instance", json::string(&n.instance)?),
        ("host", json::string(&n.host)?),
        ("port", Value::uint(u64::from(n.port))),
        ("addresses", Value::Array(addresses)),
    ])?;
    if let Some((fabric, node)) = n.operational() {
        json::set(
            &mut out,
            "nodeId",
            json::string(&crate::model::node_id(node)?)?,
        )?;
        json::set(
            &mut out,
            "compressedFabricId",
            json::string(&crate::model::node_id(fabric)?)?,
        )?;
        for (key, source) in [
            ("idleIntervalMs", "SII"),
            ("activeIntervalMs", "SAI"),
            ("activeThresholdMs", "SAT"),
        ] {
            json::set(&mut out, key, json::string(n.text(source).unwrap_or(""))?)?;
        }
    } else if n.kind == "commissionable" {
        json::set(
            &mut out,
            "deviceName",
            json::string(n.text("DN").unwrap_or(""))?,
        )?;
        let (vendor, product) = n
            .text("VP")
            .and_then(|s| s.split_once('+'))
            .unwrap_or(("0", "0"));
        for (key, value) in [
            ("vendorId", vendor),
            ("productId", product),
            ("discriminator", n.text("D").unwrap_or("0")),
            ("commissioningMode", n.text("CM").unwrap_or("0")),
        ] {
            json::set(&mut out, key, Value::uint(value.parse().unwrap_or(0)))?;
        }
    } else {
        for (key, source) in [
            ("networkName", "nn"),
            ("vendor", "vn"),
            ("model", "mn"),
            ("threadVersion", "tv"),
        ] {
            json::set(&mut out, key, json::string(n.text(source).unwrap_or(""))?)?;
        }
        let mut hex = alloc::string::String::new();
        use core::fmt::Write;
        let bytes = n.raw_text("xp").unwrap_or(&[]);
        hex.try_reserve(bytes.len() * 2)
            .map_err(|_| stulp_core::Error::Memory)?;
        for b in bytes {
            write!(hex, "{b:02x}").map_err(|_| Error::Invalid("PAN ID formatting"))?;
        }
        json::set(&mut out, "extendedPanId", json::string(&hex)?)?;
    }
    Ok(out)
}

fn mesh_step(ui: &mut Ui, wall: u64) -> Result {
    if let Some(map) = &ui.map {
        if let Some(fields) = map.snapshot()?.as_object() {
            for (key, value) in fields.iter() {
                json::set(&mut ui.mesh, key, clone(value)?)?;
            }
        }
        if let Some((index, id)) = map.next() {
            ui.pending = Some(Job::MeshNode {
                index,
                device: json::copy(id)?,
                deadline: map.deadline,
            });
        } else {
            ui::done(&mut ui.mesh, wall, None)?;
            ui.map = None;
        }
    }
    Ok(())
}

// Device commands have separate session owners; commissioning and fabric allocation
// remain exclusively with Matter. The pool serializes all endpoints of one node.
struct Nodes;
impl jobs::pool::Key for Nodes {
    fn key(&self, snapshot: &Value, method: &str, params: &Value) -> Result<Option<u64>> {
        if !matches!(method, "capability.invoke" | "capabilities.invoke") {
            return Ok(None);
        }
        let device = json::get(field(snapshot, "devices"), json::text(params, "deviceId"))
            .ok_or(Error::Invalid("Matter-apparaat ontbreekt."))?;
        crate::engine::node_id(device).map(Some)
    }
    /// device.init en driver.init raken alleen de node-lijst van het
    /// onderhoud, niet de sessies van de commandowerkers: een klik hoeft daar
    /// niet achter te wachten. Tot 03-10 stonden ze hier wel, en wachtte een
    /// klik na een start achter alle ~70 inits (15 tot 30 s op de LicheeRV).
    fn barrier(&self, method: &str) -> bool {
        matches!(method, "app.init" | "device.settings" | "device.delete")
    }
}
#[derive(Default)]
struct Commands {
    engine: Option<Engine>,
}
impl Plugin for Commands {
    fn manifest(&self) -> &'static [u8] {
        ui::MANIFEST
    }
    async fn handle<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        p: &Value,
    ) -> Result<Value> {
        if !matches!(method, "capability.invoke" | "capabilities.invoke") {
            return Err(Error::Invalid("non-command sent to Matter node worker"));
        }
        if field(field(c.state().root(), "appState"), "fabric").is_null() {
            return Err(Error::Invalid("Matter fabric is not initialized"));
        }
        if self.engine.is_none() {
            self.engine = Some(Engine::open(c).await?);
        }
        let mut values = json::object();
        if method == "capability.invoke" {
            json::set(
                &mut values,
                json::text(p, "capability"),
                clone(field(p, "value"))?,
            )?;
        } else {
            for command in json::array(p, "commands") {
                json::set(
                    &mut values,
                    json::text(command, "capability"),
                    clone(field(command, "value"))?,
                )?;
            }
        }
        let errors = self
            .engine
            .as_mut()
            .ok_or(Error::Invalid("Matter command owner missing"))?
            .command(c, json::text(p, "deviceId"), &values)
            .await?;
        if method == "capability.invoke" {
            if let Some(error) =
                json::get(&errors, json::text(p, "capability")).and_then(Value::as_str)
            {
                return Err(Error::Remote(json::copy(error)?));
            }
            Ok(Value::Null)
        } else {
            Ok(errors)
        }
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        if let Some(engine) = &mut self.engine {
            engine.sync(c.state().root(), c.now())?;
            engine.network.tick(c)?;
        }
        Ok(())
    }
}
