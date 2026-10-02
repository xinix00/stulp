//! Scenewaarden volgen de weergave-eenheden; herstelpunten blijven intern en canoniek.
use super::capability;
use stulp_core::{
    Error, Result,
    json::{self, TryClone, Value},
    store::{Storage, Store},
};
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
pub(super) fn incoming<S: Storage>(
    store: &Store<S>,
    scene: &mut Value,
    previous: Option<&Value>,
) -> Result {
    let mut states = alloc::vec::Vec::new();
    for state in json::array(scene, "states") {
        let mut state = state.try_clone()?;
        let id = json::text(&state, "deviceId");
        let cap = json::text(&state, "capabilityId");
        let d = store.device(id)?;
        if json::text(&d, "appId") == "com.stulp.scene" {
            return Err(Error::Invalid("scene cannot control another scene"));
        }
        if !json::array(&d, "capabilities")
            .iter()
            .any(|v| v.as_str() == Some(cap))
        {
            return Err(Error::Invalid("device does not have scene capability"));
        }
        let definition = capability::definition(store, &d, cap)?;
        if !json::boolean(&definition, "getable")
            || matches!(
                cap.split('.').next(),
                Some("button" | "speaker_prev" | "speaker_next")
            )
        {
            return Err(Error::Invalid("scene requires a persistent device state"));
        }
        let mut value = capability::input(store, &d, cap, field(&state, "value"))?;
        let before = previous
            .and_then(|p| {
                json::array(p, "states").iter().find(|s| {
                    json::text(s, "deviceId") == id && json::text(s, "capabilityId") == cap
                })
            })
            .map(|s| field(s, "value"))
            .unwrap_or_else(|| field(field(&d, "state"), cap));
        if matches!(before, Value::Number(_)) {
            let mut probe = d.try_clone()?;
            json::set(
                &mut probe,
                "state",
                json::fields(&[(cap, before.try_clone()?)])?,
            )?;
            let shown = capability::output(store, &probe, cap)?;
            if let (Value::Number(a), Value::Number(b)) =
                (field(&state, "value"), field(&shown, "value"))
                && (a.as_f64() - b.as_f64()).abs() < 1e-9
            {
                value = before.try_clone()?;
            }
        }
        json::set(&mut state, "value", value)?;
        json::push(&mut states, state, 256)?;
    }
    json::set(scene, "states", Value::Array(states))
}
pub(super) fn output<S: Storage>(store: &Store<S>, scene: &Value) -> Result<Value> {
    let mut scene = scene.try_clone()?;
    let mut states = alloc::vec::Vec::new();
    for state in json::array(&scene, "states") {
        let mut state = state.try_clone()?;
        if let Ok(mut d) = store.device(json::text(&state, "deviceId")) {
            let cap = json::text(&state, "capabilityId");
            json::set(
                &mut d,
                "state",
                json::fields(&[(cap, field(&state, "value").try_clone()?)])?,
            )?;
            let shown = capability::output(store, &d, cap)?;
            json::set(&mut state, "value", field(&shown, "value").try_clone()?)?;
        }
        json::push(&mut states, state, 256)?;
    }
    json::set(&mut scene, "states", Value::Array(states))?;
    json::remove(&mut scene, "previous")?;
    Ok(scene)
}
