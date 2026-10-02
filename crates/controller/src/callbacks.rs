//! Browser requests resolved into authenticated plugin callbacks.
use alloc::string::String;
use stulp_core::{
    json,
    store::{Storage, Store},
};
use stulp_web::Request;
/// Resolve device commands, media, autocomplete and plugin API requests.
pub fn callback<S: Storage>(
    store: &Store<S>,
    request: &Request,
    now: u64,
) -> stulp_core::Result<Option<(String, &'static str, json::Value)>> {
    use json::Value;
    if request.method == "GET"
        && let Some(id) = request.path.strip_prefix("/image/")
    {
        let (app, params) = store.image_source(id, now)?;
        return Ok(Some((json::copy(app)?, "video.resolve", params)));
    }
    if request.method == "GET"
        && let Some(rest) = request.path.strip_prefix("/api/stulp/devices/")
        && let Some((id, tail)) = rest.split_once("/media/")
        && let Some(slot) = tail
            .strip_suffix("/stream")
            .filter(|s| !s.is_empty() && !s.contains('/'))
    {
        let device = store.document().record("devices", id)?;
        let query = crate::app_ui::query(&request.query)?;
        let kind = json::text(&query, "kind");
        let media = store.device_media(id)?;
        let selected = media
            .as_array()
            .and_then(|a| {
                a.iter().find(|m| {
                    json::text(m, "slot") == slot
                        && (kind.is_empty() || json::text(m, "kind") == kind)
                })
            })
            .ok_or(stulp_core::Error::Missing("media slot does not exist"))?;
        return Ok(Some((
            json::copy(json::text(device, "appId"))?,
            "video.resolve",
            json::fields(&[
                ("deviceId", json::string(id)?),
                ("slot", json::string(slot)?),
                ("kind", json::string(json::text(selected, "kind"))?),
            ])?,
        )));
    }
    if request.method == "POST" && request.path == "/api/stulp/flow/autocomplete" {
        use json::TryClone;
        let body = json::parse(&request.body)?;
        let app = json::text(&body, "appId");
        if app.is_empty()
            || json::text(&body, "cardId").is_empty()
            || json::text(&body, "argument").is_empty()
        {
            return Err(stulp_core::Error::Invalid(
                "appId, cardId and argument are required",
            ));
        }
        let params = json::fields(&[
            ("kind", json::string(json::text(&body, "cardType"))?),
            ("id", json::string(json::text(&body, "cardId"))?),
            ("argument", json::string(json::text(&body, "argument"))?),
            ("query", json::string(json::text(&body, "query"))?),
            (
                "args",
                json::get(&body, "args")
                    .unwrap_or(&Value::Null)
                    .try_clone()?,
            ),
        ])?;
        return Ok(Some((json::copy(app)?, "flow.autocomplete", params)));
    }
    if let Some(api) = crate::app_ui::api(store, request)? {
        return Ok(Some(api));
    }
    if request.method == "GET"
        && let Some(rest) = request.path.strip_prefix("/api/stulp/apps/")
    {
        if let Some(id) = rest
            .strip_suffix("/registrations")
            .filter(|id| !id.contains('/'))
        {
            store.document().record("apps", id)?;
            return Ok(Some((json::copy(id)?, "registrations", json::object())));
        }
        if let Some((app, rest)) = rest.split_once("/drivers/")
            && let Some(driver) = rest
                .strip_suffix("/pair/devices")
                .filter(|id| !id.is_empty() && !id.contains('/'))
        {
            let manifest = store
                .manifest(app)
                .ok_or(stulp_core::Error::Missing("app manifest unavailable"))?;
            if !json::array(manifest, "drivers")
                .iter()
                .any(|d| json::text(d, "id") == driver)
            {
                return Err(stulp_core::Error::Missing("driver does not exist"));
            }
            return Ok(Some((
                json::copy(app)?,
                "pair.list",
                json::fields(&[("driverId", json::string(driver)?)])?,
            )));
        }
    }
    if request.method == "PUT"
        && let Some(rest) = request.path.strip_prefix("/api/manager/devices/device/")
        && let Some((id, capability)) = rest.split_once("/capability/")
    {
        let device = store.document().record("devices", id)?;
        if !json::array(device, "capabilities")
            .iter()
            .any(|v| v.as_str() == Some(capability))
        {
            return Err(stulp_core::Error::Missing(
                "device capability does not exist",
            ));
        }
        let body = json::parse(&request.body)?;
        let params = json::fields(&[
            ("deviceId", json::string(id)?),
            ("capability", json::string(capability)?),
            (
                "value",
                stulp_web::capability_input(
                    store,
                    device,
                    capability,
                    json::get(&body, "value").unwrap_or(&Value::Null),
                )?,
            ),
            ("options", json::object()),
        ])?;
        return Ok(Some((
            json::copy(json::text(device, "appId"))?,
            "capability.invoke",
            params,
        )));
    }
    Ok(None)
}
