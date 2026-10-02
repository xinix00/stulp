//! De oorspronkelijke koppel- en instellingenpagina, met korte lokale snapshots.
use crate::onboarding::Payload;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Error, Result, clone,
    jobs::{Action, Gate},
    util::field,
};
pub(crate) const MANIFEST: &[u8] = include_bytes!("../app.json");
pub(crate) const ASSETS: &[&str] = &[
    "drivers/matter/pair/code.html",
    "settings/index.html",
    "settings/style.css",
    "settings/page.js",
    "settings/mesh.js",
    "settings/vendor/vis-network.min.js",
];
pub(crate) fn asset(path: &str) -> Result<Value> {
    let bytes: &[u8] = match path {
        "drivers/matter/pair/code.html" => {
            include_bytes!("../drivers/matter/pair/code.html")
        }
        "settings/index.html" => include_bytes!("../settings/index.html"),
        "settings/style.css" => include_bytes!("../settings/style.css"),
        "settings/page.js" => include_bytes!("../settings/page.js"),
        "settings/mesh.js" => include_bytes!("../settings/mesh.js"),
        "settings/vendor/vis-network.min.js" => {
            include_bytes!("../settings/vendor/vis-network.min.js")
        }
        _ => return Ok(json::fields(&[("found", Value::Bool(false))])?),
    };
    stulp_sdk::asset(bytes)
}
pub(crate) fn date(unix: u64) -> Result<String> {
    use core::fmt::Write;
    let d = der::DateTime::from_unix_duration(core::time::Duration::from_secs(unix))
        .map_err(|_| Error::Invalid("invalid wall clock"))?;
    let mut s = String::new();
    s.try_reserve(20).map_err(|_| stulp_core::Error::Memory)?;
    write!(
        s,
        "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}Z",
        d.year(),
        d.month(),
        d.day(),
        d.hour(),
        d.minutes(),
        d.seconds()
    )
    .map_err(|_| Error::Invalid("date formatting"))?;
    Ok(s)
}
pub(crate) fn begin(wall: u64) -> Result<Value> {
    Ok(json::fields(&[
        ("running", Value::Bool(true)),
        ("startedAt", json::string(&date(wall)?)?),
    ])?)
}
pub(crate) fn done(state: &mut Value, wall: u64, error: Option<&Error>) -> Result {
    json::set(state, "running", Value::Bool(false))?;
    json::set(state, "finishedAt", json::string(&date(wall)?)?)?;
    if let Some(e) = error {
        json::set(state, "warning", json::string(&stulp_sdk::message(e)?)?)?;
    }
    Ok(())
}
pub(crate) struct Pair {
    pub(crate) id: String,
    pub(crate) state: Value,
    pub(crate) devices: Value,
}
pub(crate) struct Diagnosis {
    pub(crate) id: String,
    pub(crate) state: Value,
}
pub(crate) enum Job {
    Mesh {
        window: u64,
    },
    MeshNode {
        index: usize,
        device: String,
        deadline: u64,
    },
    Diagnose {
        device: String,
    },
    Commission {
        pair: String,
        payload: Payload,
        address: String,
    },
    Scan {
        window: u64,
    },
}
pub(crate) struct Ui {
    pub(crate) pairs: Vec<Pair>,
    pub(crate) diagnoses: Vec<Diagnosis>,
    pub(crate) running: bool,
    pub(crate) pending: Option<Job>,
    pub(crate) active: String,
    pub(crate) found: Value,
    pub(crate) scan: Value,
    pub(crate) mesh: Value,
    pub(crate) map: Option<crate::mesh::Map>,
    pub(crate) topology: Value,
}
impl Default for Ui {
    fn default() -> Self {
        Self {
            pairs: Vec::new(),
            diagnoses: Vec::new(),
            running: false,
            pending: None,
            active: String::new(),
            found: Value::Array(Vec::new()),
            scan: json::object(),
            mesh: json::object(),
            map: None,
            topology: json::object(),
        }
    }
}
impl Gate for Ui {
    fn handle(&mut self, snapshot: &Value, wall: u64, method: &str, p: &Value) -> Result<Action> {
        let value = match method {
            "registrations" => {
                stulp_sdk::registrations(&json::parse(MANIFEST).map_err(stulp_core::Error::from)?)?
            }
            "ui.asset" => asset(json::text(p, "path"))?,
            "pair.list" => {
                if json::text(p, "driverId") != "matter" {
                    return Err(Error::Invalid("Onbekende Matter-driver."));
                }
                clone(&self.found)?
            }
            "pair.start" => {
                let id = json::text(p, "sessionId");
                if json::text(p, "driverId") != "matter"
                    || id.is_empty()
                    || self.pairs.iter().any(|s| s.id == id)
                {
                    return Err(Error::Invalid("Ongeldige Matter-koppelsessie."));
                }
                json::push(
                    &mut self.pairs,
                    Pair {
                        id: json::copy(id)?,
                        state: json::object(),
                        devices: Value::Array(Vec::new()),
                    },
                    32,
                )?;
                json::parse(br#"["commission","commission_state","list_devices","cancel"]"#)
                    .map_err(stulp_core::Error::from)?
            }
            "pair.emit" => {
                let id = json::text(p, "sessionId");
                let pair = self
                    .pairs
                    .iter_mut()
                    .find(|pair| pair.id == id)
                    .ok_or(Error::Invalid("Koppelsessie ontbreekt."))?;
                match json::text(p, "event") {
                    "commission_state" => clone(&pair.state)?,
                    "list_devices" => clone(&pair.devices)?,
                    "commission" => {
                        if json::boolean(&pair.state, "running") {
                            return Ok(Action::Reply(clone(&pair.state)));
                        }
                        if self.running || !self.active.is_empty() || self.pending.is_some() {
                            return Err(Error::Invalid("Er loopt al een Matter-opdracht."));
                        }
                        let data = field(p, "data");
                        let payload = Payload::parse(json::text(data, "code"))?;
                        let address = json::copy(json::text(data, "address").trim())?;
                        self.pending = Some(Job::Commission {
                            pair: json::copy(id)?,
                            payload,
                            address,
                        });
                        self.active = json::copy(id)?;
                        pair.devices = Value::Array(Vec::new());
                        pair.state = begin(wall)?;
                        clone(&pair.state)?
                    }
                    "cancel" => {
                        if self.active == id {
                            self.pending = None;
                            self.active.clear();
                            done(
                                &mut pair.state,
                                wall,
                                Some(&Error::Invalid("Koppelen geannuleerd.")),
                            )?;
                            return Ok(Action::Cancel(Value::Null));
                        }
                        Value::Null
                    }
                    _ => return Err(Error::Invalid("Onbekende Matter-koppelstap.")),
                }
            }
            "pair.close" => {
                let id = json::text(p, "sessionId");
                self.pairs.retain(|p| p.id != id);
                if self.active == id {
                    self.pending = None;
                    self.active.clear();
                    return Ok(Action::Cancel(Value::Null));
                }
                Value::Null
            }
            "api.invoke" => match json::text(p, "handler") {
                "network" => clone(&self.topology)?,
                "diagnostics" | "diagnostics/state" => {
                    let body = field(p, "body");
                    let raw = json::text(body, "deviceId");
                    let id = if raw.is_empty() {
                        json::text(field(p, "query"), "deviceId")
                    } else {
                        raw
                    };
                    if id.is_empty() || json::get(field(snapshot, "devices"), id).is_none() {
                        return Err(Error::Invalid("Matter-apparaat ontbreekt."));
                    }
                    let index = if let Some(i) = self.diagnoses.iter().position(|d| d.id == id) {
                        i
                    } else {
                        json::push(
                            &mut self.diagnoses,
                            Diagnosis {
                                id: json::copy(id)?,
                                state: json::object(),
                            },
                            64,
                        )?;
                        self.diagnoses.len() - 1
                    };
                    if json::text(p, "handler") == "diagnostics"
                        && !json::boolean(&self.diagnoses[index].state, "running")
                    {
                        if self.running || self.pending.is_some() {
                            return Err(Error::Invalid("Er loopt al een Matter-opdracht."));
                        }
                        self.pending = Some(Job::Diagnose {
                            device: json::copy(id)?,
                        });
                        self.diagnoses[index].state = begin(wall)?;
                    }
                    clone(&self.diagnoses[index].state)?
                }

                "mesh/state" => clone(&self.mesh)?,
                "mesh" => {
                    if json::boolean(&self.mesh, "running") {
                        return Ok(Action::Reply(clone(&self.mesh)));
                    }
                    if self.running || self.pending.is_some() {
                        return Err(Error::Invalid("Er loopt al een Matter-opdracht."));
                    }
                    self.mesh = begin(wall)?;
                    let seconds = json::uint(field(p, "body"), "window");
                    let window = if seconds == 0 {
                        4000
                    } else {
                        seconds.clamp(1, 30) * 1000
                    };
                    self.pending = Some(Job::Mesh { window });
                    clone(&self.mesh)?
                }
                "scan/state" => clone(&self.scan)?,
                "scan" => {
                    if json::boolean(&self.scan, "running") {
                        return Ok(Action::Reply(clone(&self.scan)));
                    }
                    if self.running || self.pending.is_some() || !self.active.is_empty() {
                        return Err(Error::Invalid("Er loopt al een Matter-opdracht."));
                    }
                    let seconds = json::uint(field(p, "body"), "window");
                    let window = if seconds == 0 {
                        4000
                    } else {
                        seconds.clamp(1, 30) * 1000
                    };
                    self.pending = Some(Job::Scan { window });
                    self.scan = begin(wall)?;
                    clone(&self.scan)?
                }
                _ => return Ok(Action::Defer),
            },
            _ => return Ok(Action::Defer),
        };
        Ok(Action::Reply(Ok(value)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn pair(ui: &mut Ui, id: &str) -> Result {
        ui.handle(
            &Value::Null,
            1_790_000_000,
            "pair.start",
            &json::fields(&[
                ("sessionId", json::string(id)?),
                ("driverId", json::string("matter")?),
            ])?,
        )?;
        Ok(())
    }
    fn event(ui: &mut Ui, id: &str, name: &str, data: Value) -> Result<Action> {
        ui.handle(
            &Value::Null,
            1_790_000_001,
            "pair.emit",
            &json::fields(&[
                ("sessionId", json::string(id)?),
                ("event", json::string(name)?),
                ("data", data),
            ])?,
        )
    }
    #[test]
    fn pairing_is_isolated_idempotent_and_cancel_does_not_open_a_second_job() -> Result {
        let mut ui = Ui::default();
        pair(&mut ui, "one")?;
        pair(&mut ui, "two")?;
        let payload = Payload {
            passcode: 20202021,
            discriminator: 3840,
            ..Default::default()
        };
        let data = json::fields(&[("code", json::string(&payload.qr()?)?)])?;
        event(&mut ui, "one", "commission", clone(&data)?)?;
        assert!(ui.pending.is_some());
        event(&mut ui, "one", "commission", clone(&data)?)?;
        assert!(event(&mut ui, "two", "commission", clone(&data)?).is_err());
        assert!(matches!(
            event(&mut ui, "two", "cancel", Value::Null)?,
            Action::Reply(Ok(_))
        ));
        assert_eq!(ui.active, "one");
        ui.pending.take();
        ui.running = true;
        assert!(matches!(
            event(&mut ui, "one", "cancel", Value::Null)?,
            Action::Cancel(_)
        ));
        assert!(event(&mut ui, "two", "commission", clone(&data)?).is_err());
        ui.running = false;
        event(&mut ui, "two", "commission", data)?;
        assert_eq!(ui.active, "two");
        assert!(ui.pairs[0].devices.as_array().is_some_and(|v| v.is_empty()));
        Ok(())
    }
}
