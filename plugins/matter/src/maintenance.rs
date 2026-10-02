//! Modelverversing bewaart gebruikersvelden en schrijft alleen gewijzigde hardwarevelden.
use alloc::vec::Vec;
use stulp_core::json::{self, Value};
use stulp_sdk::{Client, Error, Result, Transport, clone, util::field};
/// Pas de bestaande pluginrechten toe: geen autonoom aanmaken of verwijderen van apparaten.
pub(crate) async fn update<T: Transport>(c: &mut Client<T>, updated: &Value) -> Result {
    let id = json::text(updated, "id");
    let old = clone(c.state().device(id)?)?;
    for (method, left, right) in [
        ("capability.add", updated, &old),
        ("capability.remove", &old, updated),
    ] {
        for cap in json::array(left, "capabilities") {
            if !json::array(right, "capabilities")
                .iter()
                .any(|v| json::equal(v, cap))
            {
                c.call(
                    method,
                    &json::fields(&[("deviceId", json::string(id)?), ("capability", clone(cap)?)])?,
                )
                .await?;
            }
        }
    }
    for key in ["state", "settings", "store"] {
        let mut patch = json::object();
        if let Some(object) = field(updated, key).as_object() {
            for (name, value) in object.iter() {
                if !json::equal(field(field(&old, key), name), value) {
                    json::set(&mut patch, name, clone(value)?)?;
                }
            }
        }
        if patch.as_object().is_some_and(|o| o.iter().next().is_some()) {
            c.call(
                "device.merge",
                &json::fields(&[
                    ("deviceId", json::string(id)?),
                    ("field", json::string(key)?),
                    ("patch", patch),
                ])?,
            )
            .await?;
        }
    }
    let class = json::text(updated, "class");
    if !class.is_empty() && class != json::text(&old, "class") {
        c.call(
            "device.set",
            &json::fields(&[
                ("deviceId", json::string(id)?),
                ("field", json::string("class")?),
                ("value", json::string(class)?),
            ])?,
        )
        .await?;
    }
    // Prototypes noemen dit message; bestaande SDK-snapshots unavailableMessage.
    let message = json::get(updated, "message")
        .and_then(Value::as_str)
        .unwrap_or_else(|| json::text(updated, "unavailableMessage"));
    if json::boolean(&old, "available") != json::boolean(updated, "available")
        || json::text(&old, "unavailableMessage") != message
    {
        if json::boolean(updated, "available") {
            c.available(id, true).await?;
        } else {
            c.unavailable(id, message).await?;
        }
    }
    Ok(())
}
pub(crate) fn required(devices: &[Value]) -> bool {
    devices
        .iter()
        .any(|d| json::uint(field(d, "store"), "matter.modelVersion") < crate::model::VERSION)
}
pub(crate) async fn refreshed<T: Transport>(
    c: &mut Client<T>,
    existing: &[Value],
    prototypes: Vec<Value>,
) -> Result {
    let mut used = Vec::new();
    used.try_reserve_exact(existing.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    for prototype in prototypes {
        let found = existing.iter().enumerate().find(|(index, current)| {
            !used.contains(index)
                && crate::devices::bridged(current) == crate::devices::bridged(&prototype)
                && (!crate::devices::bridged(&prototype)
                    || crate::devices::primary(current) == crate::devices::primary(&prototype))
        });
        let Some((index, current)) = found else {
            return Err(Error::Invalid(
                "Nieuw Matter-endpoint gevonden; voeg dit via koppelen toe.",
            ));
        };
        used.push(index);
        update(c, &crate::devices::refresh(current, &prototype)?).await?;
    }
    Ok(())
}
pub(crate) async fn upgrade<T: Transport>(c: &mut Client<T>) -> Result {
    let mut all = Vec::new();
    if let Some(devices) = field(c.state().root(), "devices").as_object() {
        for (_, d) in devices.iter() {
            json::push(&mut all, clone(d)?, 4096)?;
        }
    }
    for mut device in all {
        if crate::devices::upgrade(&mut device)? {
            update(c, &device).await?;
        }
    }
    Ok(())
}
