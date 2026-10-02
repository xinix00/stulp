//! Stateless MCP gebruikt dezelfde store en callbacks als de browser, met publieke projecties.
use crate::{Request, Response};
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, Number, TryClone, Value},
    manifest,
    store::{Storage, Store},
};
mod arguments;
mod read;
mod schema;
mod write;
const VERSIONS: [&str; 3] = ["2025-11-25", "2025-06-18", "2025-03-26"];
fn field<'a>(v: &'a Value, k: &str) -> &'a Value {
    json::get(v, k).unwrap_or(&Value::Null)
}
fn text<'a>(v: &'a Value, k: &str) -> &'a str {
    json::text(v, k).trim()
}
fn metadata() -> Result<Value> {
    Ok(json::parse(include_bytes!("../../data/mcp.json"))?)
}
/// Een gevalideerde toolaanroep; transporthandvatten blijven bij de adapter.
pub struct Call {
    /// De oorspronkelijke JSON-RPC-identiteit, ook null is toegestaan.
    pub id: Value,
    /// Een naam uit tools/list.
    pub name: String,
    /// Begrensde argumenten, gecontroleerd tegen het geadverteerde schema.
    pub args: Value,
}
/// Handshake en protocolfouten antwoorden direct; tools kunnen op apps wachten.
pub enum Dispatch {
    /// Direct JSON-RPC-antwoord of notificatiebevestiging.
    Reply(Response),
    /// Werk voor de controller-eigenaar.
    Tool(Call),
}
/// Valideert het transport en één JSON-RPC-bericht, na toegangscontrole.
pub fn decode(req: &Request) -> Result<Dispatch> {
    let id = Value::Null;
    if !req.origin.is_empty()
        && req
            .origin
            .strip_prefix("https://")
            .or_else(|| req.origin.strip_prefix("http://"))
            != Some(req.host.as_str())
    {
        return Ok(Dispatch::Reply(rpc_error(
            403,
            &id,
            -32600,
            "origin does not match this Stulp host",
        )?));
    }
    if req.method != "POST" {
        let mut reply = rpc_error(
            405,
            &id,
            -32600,
            "this stateless MCP server accepts POST only",
        )?;
        json::push(&mut reply.headers, ("Allow", json::copy("POST")?), 8)?;
        return Ok(Dispatch::Reply(reply));
    }
    if !json::text(&req.headers, "content-type")
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .eq_ignore_ascii_case("application/json")
    {
        return Ok(Dispatch::Reply(rpc_error(
            415,
            &id,
            -32600,
            "MCP requests need Content-Type: application/json",
        )?));
    }
    let accept = json::text(&req.headers, "accept");
    if !accepts(accept, "application/json") || !accepts(accept, "text/event-stream") {
        return Ok(Dispatch::Reply(rpc_error(
            400,
            &id,
            -32600,
            "MCP requests must accept application/json and text/event-stream",
        )?));
    }
    if req.body.len() > 256 * 1024 {
        return Ok(Dispatch::Reply(rpc_error(
            200,
            &id,
            -32700,
            "MCP request exceeds 256 KiB",
        )?));
    }
    let v = match json::parse(&req.body) {
        Ok(v) => v,
        Err(_) => return Ok(Dispatch::Reply(rpc_error(200, &id, -32700, "parse error")?)),
    };
    let id = field(&v, "id");
    if v.as_object().is_none()
        || json::text(&v, "jsonrpc") != "2.0"
        || text(&v, "method").is_empty()
        || !matches!(id, Value::Null | Value::Number(_) | Value::String(_))
    {
        return Ok(Dispatch::Reply(rpc_error(
            200,
            &Value::Null,
            -32600,
            "invalid request",
        )?));
    }
    if json::get(&v, "params").is_some_and(|p| p.as_object().is_none()) {
        return Ok(Dispatch::Reply(rpc_error(
            200,
            id,
            -32602,
            "params must be an object",
        )?));
    }
    if json::get(&v, "id").is_none() {
        return Ok(Dispatch::Reply(Response {
            status: 202,
            content_type: "application/json",
            body: crate::Body::Static(b""),
            cookie: None,
            headers: Vec::new(),
        }));
    }
    let version = json::text(&req.headers, "mcp-protocol-version").trim();
    if !version.is_empty() && !VERSIONS.contains(&version) {
        return Ok(Dispatch::Reply(rpc_error(
            400,
            id,
            -32602,
            "unsupported MCP-Protocol-Version",
        )?));
    }
    let params = field(&v, "params");
    match json::text(&v, "method") {
        "initialize" => {
            let client = field(params, "clientInfo");
            if json::text(params, "protocolVersion").is_empty()
                || field(params, "capabilities").as_object().is_none()
                || json::text(client, "name").is_empty()
                || json::text(client, "version").is_empty()
            {
                return Ok(Dispatch::Reply(rpc_error(
                    200,
                    id,
                    -32602,
                    "protocolVersion, capabilities and clientInfo name/version are required",
                )?));
            }
            let asked = json::text(params, "protocolVersion");
            let version = if VERSIONS.contains(&asked) {
                asked
            } else {
                VERSIONS[0]
            };
            let result = json::fields(&[
                ("protocolVersion", json::string(version)?),
                (
                    "capabilities",
                    json::parse(br#"{"tools":{"listChanged":false}}"#)?,
                ),
                (
                    "serverInfo",
                    json::parse(
                        concat!(
                            r#"{"name":"stulp","version":""#,
                            env!("CARGO_PKG_VERSION"),
                            r#""}"#
                        )
                        .as_bytes(),
                    )?,
                ),
                (
                    "instructions",
                    field(&metadata()?, "instructions").try_clone()?,
                ),
            ])?;
            Ok(Dispatch::Reply(rpc_result(id, result)?))
        }
        "ping" => Ok(Dispatch::Reply(rpc_result(id, json::object())?)),
        "tools/list" => Ok(Dispatch::Reply(rpc_result(
            id,
            json::fields(&[("tools", field(&metadata()?, "tools").try_clone()?)])?,
        )?)),
        "tools/call" => {
            let name = text(params, "name");
            let catalog = metadata()?;
            let Some(tool) = json::array(&catalog, "tools")
                .iter()
                .find(|t| json::text(t, "name") == name)
            else {
                return Ok(Dispatch::Reply(rpc_error(200, id, -32602, "unknown tool")?));
            };
            let args = match json::get(params, "arguments") {
                Some(v) => v.try_clone()?,
                None => json::object(),
            };
            if let Err(error) = schema::validate(&args, field(tool, "inputSchema")) {
                return Ok(Dispatch::Reply(tool_error(id, &error_text(&error)?)?));
            }
            Ok(Dispatch::Tool(Call {
                id: id.try_clone()?,
                name: json::copy(name)?,
                args,
            }))
        }
        _ => Ok(Dispatch::Reply(rpc_error(
            200,
            id,
            -32601,
            "method not found",
        )?)),
    }
}
fn accepts(header: &str, mime: &str) -> bool {
    header.split(',').any(|part| {
        let mut fields = part.trim().split(';');
        fields
            .next()
            .unwrap_or("")
            .trim()
            .eq_ignore_ascii_case(mime)
            && !fields
                .filter_map(|s| s.trim().split_once('='))
                .any(|(k, v)| {
                    k.eq_ignore_ascii_case("q")
                        && !v
                            .trim()
                            .trim_matches('"')
                            .parse::<f64>()
                            .is_ok_and(|q| q > 0.0)
                })
    })
}
fn error_text(e: &Error) -> Result<String> {
    use core::fmt::Write;
    let mut s = String::new();
    s.try_reserve(512).map_err(|_| Error::Memory)?;
    write!(s, "{e}").map_err(|_| Error::Full)?;
    Ok(s)
}
/// Protocolfouten zijn JSON-RPC-fouten, geen toolresultaat.
pub fn rpc_error(status: u16, id: &Value, code: i64, message: &str) -> Result<Response> {
    Response::json(
        status,
        &json::fields(&[
            ("jsonrpc", json::string("2.0")?),
            ("id", id.try_clone()?),
            (
                "error",
                json::fields(&[
                    ("code", Value::Number(Number::Int(code))),
                    ("message", json::string(message)?),
                ])?,
            ),
        ])?,
    )
}
/// Een antwoord bewaart het oorspronkelijke id-type.
pub fn rpc_result(id: &Value, result: Value) -> Result<Response> {
    Response::json(
        200,
        &json::fields(&[
            ("jsonrpc", json::string("2.0")?),
            ("id", id.try_clone()?),
            ("result", result),
        ])?,
    )
}
/// Publieke resultaten staan zowel gestructureerd als in TextContent.
pub fn tool_result(id: &Value, mut value: Value, summary: &str, failed: bool) -> Result<Response> {
    if value.as_object().is_none() {
        value = json::fields(&[("result", value)])?;
    }
    let encoded = json::to_string(&value)?;
    if encoded.len() > 256 * 1024 {
        return tool_error(
            id,
            "tool result exceeds 256 KiB; use filters, pagination or an exact id",
        );
    }
    let mut content = Vec::new();
    json::push(
        &mut content,
        json::fields(&[
            ("type", json::string("text")?),
            ("text", json::string(&encoded)?),
        ])?,
        2,
    )?;
    if !summary.is_empty() {
        json::push(
            &mut content,
            json::fields(&[
                ("type", json::string("text")?),
                ("text", trim(summary, 4096)?),
            ])?,
            2,
        )?;
    }
    rpc_result(
        id,
        json::fields(&[
            ("content", Value::Array(content)),
            ("structuredContent", value),
            ("isError", Value::Bool(failed)),
        ])?,
    )
}
/// Een uitvoeringsfout blijft een geldig MCP-toolantwoord.
pub fn tool_error(id: &Value, error: &str) -> Result<Response> {
    let mut content = Vec::new();
    json::push(
        &mut content,
        json::fields(&[
            ("type", json::string("text")?),
            ("text", trim(error, 4096)?),
        ])?,
        1,
    )?;
    rpc_result(
        id,
        json::fields(&[
            ("content", Value::Array(content)),
            ("isError", Value::Bool(true)),
        ])?,
    )
}
fn trim(text: &str, max: usize) -> Result<Value> {
    let mut end = text.len().min(max);
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    json::string(&text[..end])
}
/// Direct leeswerk wordt uitgevoerd zonder pluginstatus of geheimen mee te kopiëren.
pub fn read<S: Storage>(store: &Store<S>, call: &Call, cards: &Value) -> Result<Option<Value>> {
    read::dispatch(store, &call.name, &call.args, cards)
}

/// Een gevalideerde bewerking die de adapter uitvoert onder dezelfde controller-eigenaar.
#[allow(missing_docs)]
pub enum Work {
    Done(Value),
    Callback {
        app: String,
        method: &'static str,
        params: Value,
        projection: Projection,
    },
    Scene {
        id: String,
        on: bool,
        result: Value,
    },
    Flow {
        definition: Value,
        persist: bool,
    },
    CreateVirtual(String),
}
/// Alleen deze publieke velden mogen van een callback terug naar MCP.
#[allow(missing_docs)]
pub enum Projection {
    Device(Value),
    Autocomplete,
}
/// Mutaties en callbackplanning na authenticatie, schema- en ratecontrole.
pub fn prepare<S: Storage>(
    store: &mut Store<S>,
    call: &Call,
    cards: &Value,
    env: &mut impl crate::Environment,
) -> Result<Work> {
    if let Some(v) = read(store, call, cards)? {
        return Ok(Work::Done(v));
    }
    let a = &call.args;
    match call.name.as_str() {
        "flows_create"
        | "flows_update"
        | "flows_add_cards"
        | "flows_configure_card"
        | "flows_connect_cards"
        | "flows_disconnect_cards"
        | "flows_remove_card"
        | "flows_delete" => Ok(Work::Done(write::edit(store, &call.name, a, cards, env)?)),
        "devices_write" => {
            let id = text(a, "deviceId");
            let cap = text(a, "capabilityId");
            let d = store.device(id)?;
            if !read::has_cap(&d, cap) {
                return Err(Error::Missing("device capability does not exist"));
            }
            let def = crate::capability::output(store, &d, cap)?;
            arguments::capability_value(&def, field(a, "value"))?;
            let canonical = crate::capability::input(store, &d, cap, field(a, "value"))?;
            let mut result = json::fields(&[
                ("accepted", Value::Bool(true)),
                ("deviceId", json::string(id)?),
                ("capabilityId", json::string(cap)?),
                ("requestedValue", field(a, "value").try_clone()?),
            ])?;
            if matches!(
                field(&def, "value"),
                Value::Bool(_) | Value::Number(_) | Value::String(_)
            ) {
                json::set(
                    &mut result,
                    "lastReportedValue",
                    field(&def, "value").try_clone()?,
                )?;
            }
            if let Some(unit) = json::get(&def, "units") {
                json::set(&mut result, "requestedUnits", unit.try_clone()?)?;
            }
            if json::text(&d, "appId") == "com.stulp.scene" {
                return Ok(Work::Scene {
                    id: json::copy(json::text(field(&d, "data"), "sceneId"))?,
                    on: canonical
                        .as_bool()
                        .ok_or(Error::Invalid("scene requires boolean"))?,
                    result,
                });
            }
            Ok(Work::Callback {
                app: json::copy(json::text(&d, "appId"))?,
                method: "capability.invoke",
                params: json::fields(&[
                    ("deviceId", json::string(id)?),
                    ("capability", json::string(cap)?),
                    ("value", canonical),
                    ("options", json::object()),
                ])?,
                projection: Projection::Device(result),
            })
        }
        "flow_card_autocomplete" => {
            let mut node = json::fields(&[("step", a.try_clone()?)])?;
            arguments::normalize(store, &mut node, cards, true, false, true)?;
            let step = field(&node, "step");
            let card = read::card(cards, step).ok_or(Error::Missing("card missing"))?;
            if !json::array(card, "args").iter().any(|v| {
                json::text(v, "name") == text(a, "argument")
                    && json::text(v, "type") == "autocomplete"
            }) {
                return Err(Error::Invalid("card has no such autocomplete argument"));
            }
            Ok(Work::Callback {
                app: json::copy(text(a, "appId"))?,
                method: "flow.autocomplete",
                params: json::fields(&[
                    ("kind", json::string(text(a, "cardType"))?),
                    ("id", json::string(text(a, "cardId"))?),
                    ("argument", json::string(text(a, "argument"))?),
                    ("query", json::string(text(a, "query"))?),
                    ("args", field(step, "args").try_clone()?),
                ])?,
                projection: Projection::Autocomplete,
            })
        }
        "flows_run" => Ok(Work::Flow {
            definition: store
                .document()
                .record("flows", text(a, "flowId"))?
                .try_clone()?,
            persist: true,
        }),
        "flow_action_run" => {
            if arguments::contains_token(field(a, "args")) {
                return Err(Error::Invalid(
                    "flow_action_run has no trigger context; use literal values",
                ));
            }
            let mut step = a.try_clone()?;
            json::set(&mut step, "cardType", json::string("action")?)?;
            let mut node = json::fields(&[("id", json::string("action")?), ("step", step)])?;
            arguments::normalize(store, &mut node, cards, true, false, false)?;
            let mut nodes = Vec::new();
            json::push(&mut nodes,json::parse(br#"{"id":"start","step":{"appId":"stulp","cardId":"manual","cardType":"trigger"}}"#)?,2)?;
            json::push(&mut nodes, node, 2)?;
            let f = json::fields(&[
                ("id", json::string("")?),
                ("name", json::string("MCP action")?),
                ("enabled", Value::Bool(true)),
                ("nodes", Value::Array(nodes)),
                (
                    "edges",
                    json::parse(br#"[{"id":"edge","from":"start","to":"action"}]"#)?,
                ),
            ])?;
            Ok(Work::Flow {
                definition: crate::flow_units::convert(store, &f, true, None)?,
                persist: false,
            })
        }
        "devices_create" => {
            let name = text(a, "name");
            if name.is_empty() {
                return Err(Error::Invalid("virtual device name cannot be empty"));
            }
            Ok(Work::CreateVirtual(json::copy(name)?))
        }
        _ => Err(Error::Invalid("unknown MCP tool")),
    }
}
/// De callbackwaarde wordt begrensd en geprojecteerd voordat hij openbaar wordt.
pub fn projected(projection: Projection, value: &Value) -> Result<Value> {
    match projection {
        Projection::Device(result) => Ok(result),
        Projection::Autocomplete => {
            let mut out = Vec::new();
            for v in value
                .as_array()
                .unwrap_or(&[])
                .iter()
                .filter(|v| v.as_object().is_some())
                .take(50)
            {
                if json::text(v, "id").is_empty() || json::text(v, "name").is_empty() {
                    continue;
                }
                let mut item = json::fields(&[
                    ("id", trim(json::text(v, "id"), 500)?),
                    ("name", trim(json::text(v, "name"), 500)?),
                ])?;
                if !json::text(v, "description").is_empty() {
                    json::set(
                        &mut item,
                        "description",
                        trim(json::text(v, "description"), 2048)?,
                    )?;
                }
                json::push(&mut out, item, 50)?;
            }
            json::fields(&[
                ("total", Value::uint(out.len() as u64)),
                ("values", Value::Array(out)),
            ])
        }
    }
}
/// Het aangemaakte virtuele apparaat wordt na device.init opnieuw gelezen.
pub fn created<S: Storage>(store: &Store<S>, id: &str) -> Result<Value> {
    json::fields(&[
        ("created", Value::Bool(true)),
        ("type", json::string("virtual_switch")?),
        ("device", read::device(store, &store.device(id)?, "", true)?),
    ])
}

/// Alleen beperkte uitvoeringsresultaten verlaten het callbackkanaal.
pub fn execution(raw: &Value, action: bool) -> Result<Value> {
    let mut result = json::object();
    for k in ["flowId", "success", "stopped", "ranAt"] {
        if let Some(v) = json::get(raw, k) {
            json::set(&mut result, k, v.try_clone()?)?;
        }
    }
    if !json::text(raw, "error").is_empty() {
        json::set(&mut result, "error", trim(json::text(raw, "error"), 4096)?)?;
    }
    for kind in ["actions", "conditions"] {
        let mut steps = Vec::new();
        for s in json::array(raw, kind) {
            let mut step = json::object();
            for k in ["appId", "cardId", "cardType", "passed"] {
                if let Some(v) = json::get(s, k) {
                    json::set(&mut step, k, v.try_clone()?)?;
                }
            }
            let v = field(s, "result");
            if !v.is_null() {
                if schema::validate_limit(v, &Value::Null, 512, 8 << 10).is_ok() {
                    json::set(&mut step, "result", v.try_clone()?)?;
                } else {
                    json::set(&mut step, "resultOmitted", Value::Bool(true))?;
                }
            }
            json::push(&mut steps, step, 128)?;
        }
        json::set(&mut result, kind, Value::Array(steps))?;
    }
    if action {
        let step = json::array(&result, "actions")
            .first()
            .unwrap_or(&Value::Null)
            .try_clone()?;
        return json::fields(&[("action", step)]);
    }
    json::fields(&[("execution", result)])
}
/// Sceneherstel kan deels slagen: beide tellers en de begrensde deelresultaten blijven zichtbaar.
pub fn scene(mut result: Value, raw: &Value) -> Result<Value> {
    let mut scene = json::object();
    for k in ["sceneId", "sceneName"] {
        json::set(
            &mut scene,
            k,
            trim(json::text(raw, k), if k == "sceneId" { 256 } else { 500 })?,
        )?;
    }
    for k in [
        "requestedOn",
        "momentary",
        "active",
        "success",
        "attempted",
        "succeeded",
        "failed",
    ] {
        json::set(&mut scene, k, field(raw, k).try_clone()?)?;
    }
    let mut states = Vec::new();
    let mut bytes = 0;
    for (i, s) in json::array(raw, "states").iter().enumerate() {
        let mut p = json::fields(&[
            ("deviceId", trim(json::text(s, "deviceId"), 256)?),
            ("capabilityId", trim(json::text(s, "capabilityId"), 256)?),
            ("success", Value::Bool(json::boolean(s, "success"))),
        ])?;
        if !json::text(s, "error").is_empty() {
            json::set(&mut p, "error", trim(json::text(s, "error"), 4096)?)?;
        }
        if json::boolean(s, "unchanged") {
            json::set(&mut p, "unchanged", Value::Bool(true))?;
        }
        let value = field(s, "value");
        if matches!(value, Value::Bool(_) | Value::Number(_) | Value::String(_)) {
            json::set(
                &mut p,
                "value",
                if let Some(s) = value.as_str() {
                    trim(s, 2048)?
                } else {
                    value.try_clone()?
                },
            )?;
        } else if !value.is_null() {
            json::set(&mut p, "valueOmitted", Value::Bool(true))?;
        }
        bytes += json::to_string(&p)?.len();
        if bytes > 96 * 1024 {
            json::set(
                &mut scene,
                "statesOmitted",
                Value::uint((json::array(raw, "states").len() - i) as u64),
            )?;
            break;
        }
        json::push(&mut states, p, 256)?;
    }
    json::set(&mut scene, "states", Value::Array(states))?;
    json::set(
        &mut result,
        "accepted",
        Value::Bool(json::boolean(raw, "success") || json::uint(raw, "attempted") > 0),
    )?;
    json::set(&mut result, "sceneActivation", scene)?;
    Ok(result)
}
