//! Combineert kleur en inschakelen/dimmen zonder capabilityroutes of individuele fouten te verliezen.
use crate::{
    capabilities::{self, Command},
    devices, im,
    interaction::Interaction,
    reports,
};
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Result, Transport, clone,
    util::{field, number},
};
/// Eén wirecommand kan meer dan één gevraagde capability uitvoeren.
pub struct Planned {
    /// Concrete clusteropdracht en eventuele timed handshake.
    pub command: Command,
    /// Aangevraagde waarden die na een geslaagd antwoord gepubliceerd mogen worden.
    pub values: Value,
}
impl Planned {
    /// Verifieert ook het concrete antwoordpad en het aantal resultaten.
    pub async fn invoke<T: Transport>(
        &self,
        im: &mut Interaction<'_>,
        c: &mut Client<T>,
        deadline: u64,
    ) -> Result {
        let response = im
            .invoke(
                c,
                &[self.command.borrow()],
                if self.command.timed { Some(5000) } else { None },
                deadline,
            )
            .await?;
        let mut count = 0;
        for chunk in response.iter() {
            for result in chunk?.results {
                count += 1;
                result.status.result()?;
                if result.path != self.command.path {
                    return Err(Error::Invalid("Matter command response path differs"));
                }
            }
        }
        if count != 1 {
            return Err(Error::Invalid(
                "Matter command was not acknowledged exactly once",
            ));
        }
        Ok(())
    }
    /// Wirevorm voor interoperabiliteitstests en transport-state-machines.
    pub fn encode(&self) -> Result<Vec<u8>> {
        im::invoke(&[self.command.borrow()], self.command.timed)
    }
}
/// Plan bevat zowel verzendbare opdrachten als fouten per gevraagde capability.
pub struct Plan {
    /// Power-on eerst, overige functies daarna, power-off als laatste.
    pub commands: Vec<Planned>,
    /// Alleen ongeldige/niet-schrijfbare verzoeken; netwerkfouten ontstaan pas bij uitvoering.
    pub errors: Value,
}
fn rank(cap: &str, value: &Value) -> u8 {
    if capabilities::base(cap) != "onoff" {
        1
    } else if value.as_bool() == Some(true) {
        0
    } else {
        2
    }
}
fn normalized(cap: &str, value: &Value) -> Result<Value> {
    if matches!(
        capabilities::base(cap),
        "dim" | "light_hue" | "light_saturation"
    ) && let Some(n) = number(value)
    {
        return stulp_sdk::util::float(n);
    }
    clone(value)
}
fn planned(command: Command, caps: &[&str], values: &Value) -> Result<Planned> {
    let mut applied = json::object();
    for cap in caps {
        json::set(&mut applied, cap, normalized(cap, field(values, cap))?)?;
    }
    Ok(Planned {
        command,
        values: applied,
    })
}
fn failure(errors: &mut Value, caps: &[&str], error: Error) -> Result {
    if let Error::Core(e) = error {
        return Err(Error::Core(e));
    }
    let text = stulp_sdk::message(&error)?;
    for cap in caps {
        json::set(errors, cap, json::string(&text)?)?;
    }
    Ok(())
}
/// De planning verstuurt niets; ongeldige deelverzoeken houden hun eigen foutmelding.
pub fn plan(device: &Value, values: &Value) -> Result<Plan> {
    let object = values
        .as_object()
        .ok_or(Error::Invalid("Matter command values must be an object"))?;
    let mut ids = Vec::new();
    for (cap, _) in object.iter() {
        json::push(&mut ids, cap, 256)?;
    }
    ids.sort_unstable_by(|a, b| {
        rank(a, field(values, a))
            .cmp(&rank(b, field(values, b)))
            .then(a.cmp(b))
    });
    let servers = devices::ids(field(field(device, "store"), "matter.serverClusters"))?;
    let mut covered = Vec::<&str>::new();
    let mut out = Plan {
        commands: Vec::new(),
        errors: json::object(),
    };
    for cap in &ids {
        let cap = *cap;
        if covered.contains(&cap) {
            continue;
        }
        let endpoint = devices::endpoint(device, cap);
        let partner = |base| {
            ids.iter()
                .find(|candidate| {
                    capabilities::base(candidate) == base
                        && devices::endpoint(device, candidate) == endpoint
                })
                .copied()
        };
        if capabilities::base(cap) == "onoff"
            && field(values, cap).as_bool() == Some(true)
            && let Some(dim) = partner("dim")
            && number(field(values, dim)).is_some_and(|n| n > 0.0)
            && let Some(mapping) = capabilities::for_capability(&[], &[], "dim")
        {
            match mapping.command(endpoint, field(values, dim)) {
                Ok(command) => {
                    json::push(
                        &mut out.commands,
                        planned(command, &[cap, dim], values)?,
                        256,
                    )?;
                    json::push(&mut covered, cap, 256)?;
                    json::push(&mut covered, dim, 256)?;
                    continue;
                }
                Err(Error::Core(e)) => return Err(Error::Core(e)),
                Err(_) => (),
            }
        }
        if capabilities::base(cap) == "light_hue"
            && let Some(saturation) = partner("light_saturation")
        {
            match Command::hue_saturation(endpoint, field(values, cap), field(values, saturation)) {
                Ok(command) => json::push(
                    &mut out.commands,
                    planned(command, &[cap, saturation], values)?,
                    256,
                )?,
                Err(e) => failure(&mut out.errors, &[cap, saturation], e)?,
            }
            json::push(&mut covered, cap, 256)?;
            json::push(&mut covered, saturation, 256)?;
            continue;
        }
        json::push(&mut covered, cap, 256)?;
        let result =
            capabilities::for_capability(&reports::types(device, endpoint)?, &servers, cap)
                .ok_or(Error::Invalid(
                    "Matter capability is read-only or unsupported",
                ))
                .and_then(|mapping| mapping.command(endpoint, field(values, cap)));
        match result {
            Ok(command) => json::push(&mut out.commands, planned(command, &[cap], values)?, 256)?,
            Err(e) => failure(&mut out.errors, &[cap], e)?,
        }
    }
    Ok(out)
}
/// Namen van alle capabilities die één wirecommand uitvoert, voor responses per capability.
pub fn names(command: &Planned) -> Result<Vec<String>> {
    let mut out = Vec::new();
    for (key, _) in command
        .values
        .as_object()
        .ok_or(Error::Invalid("invalid planned values"))?
        .iter()
    {
        json::push(&mut out, json::copy(key)?, 256)?;
    }
    Ok(out)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn lamp_commands_combine_only_same_endpoint_and_power_off_is_last() -> Result {
        let device = json::fields(&[(
            "store",
            json::fields(&[
                ("matter.endpoint", Value::uint(1)),
                (
                    "matter.serverClusters",
                    Value::Array(alloc::vec![
                        json::string("0x6")?,
                        json::string("0x8")?,
                        json::string("0x300")?
                    ]),
                ),
            ])?,
        )])?;
        let values = json::fields(&[
            ("onoff", Value::Bool(true)),
            ("dim", stulp_sdk::util::float(0.5)?),
            ("light_hue", stulp_sdk::util::float(0.3)?),
            ("light_saturation", stulp_sdk::util::float(0.8)?),
        ])?;
        let combined = plan(&device, &values)?;
        assert_eq!(combined.commands.len(), 2);
        assert_eq!(
            (
                combined.commands[0].command.path.cluster,
                combined.commands[0].command.path.command
            ),
            (8, 4)
        );
        assert_eq!(names(&combined.commands[0])?, ["onoff", "dim"]);
        assert_eq!(
            (
                combined.commands[1].command.path.cluster,
                combined.commands[1].command.path.command
            ),
            (0x300, 6)
        );
        let mut off = values;
        json::set(&mut off, "onoff", Value::Bool(false))?;
        let plans = plan(&device, &off)?;
        assert_eq!(plans.commands.len(), 3);
        assert_eq!(plans.commands[2].command.path.cluster, 6);
        assert_eq!(plans.commands[2].command.path.command, 0);
        let bad = plan(
            &device,
            &json::fields(&[
                ("dim", stulp_sdk::util::float(1.1)?),
                ("onoff", Value::Bool(true)),
            ])?,
        )?;
        assert_eq!(bad.commands.len(), 1);
        assert!(!json::text(&bad.errors, "dim").is_empty());
        Ok(())
    }
}
