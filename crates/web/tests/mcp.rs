//! MCP hergebruikt het huiscontract zonder browsercookie of geheime pluginvelden.
#![allow(clippy::unwrap_used, clippy::panic)]
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_web::{
    Environment, Request, Web,
    mcp::{self, Dispatch},
};
struct Env;
impl Environment for Env {
    fn id(&mut self) -> stulp_core::Result<String> {
        Ok("new".into())
    }
    fn now(&self) -> stulp_core::Result<String> {
        Ok("2026-10-01T12:00:00Z".into())
    }
}
fn request(body: &str) -> Request {
    Request{method:"POST".into(),path:"/mcp/key".into(),query:String::new(),host:"localhost".into(),origin:String::new(),cookie:String::new(),body:body.as_bytes().to_vec(),headers:json::parse(br#"{"content-type":"application/json","accept":"application/json, text/event-stream"}"#).unwrap()}
}

#[test]
fn execution_projection_matches_go_total_value_and_byte_budgets() {
    let project = |value| {
        let raw = json::fields(&[(
            "actions",
            Value::Array(vec![
                json::fields(&[
                    ("appId", json::string("fixture").unwrap()),
                    ("cardId", json::string("test").unwrap()),
                    ("result", value),
                ])
                .unwrap(),
            ]),
        )])
        .unwrap();
        mcp::execution(&raw, true).unwrap()
    };
    // Go mcpStepResultObject uses 512 values / 8 KiB, including container
    // overhead, independently of the larger budget for MCP input arguments.
    for (count, accepted) in [(510, true), (511, false)] {
        let nested = Value::Array(vec![Value::Array(
            (0..count).map(|_| Value::Bool(true)).collect(),
        )]);
        let result = project(nested);
        let action = json::get(&result, "action").unwrap();
        assert_eq!(json::get(action, "result").is_some(), accepted);
        assert_eq!(json::boolean(action, "resultOmitted"), !accepted);
    }
    for (length, accepted) in [(8188, true), (8189, false)] {
        let result = project(Value::String("x".repeat(length)));
        let action = json::get(&result, "action").unwrap();
        assert_eq!(json::get(action, "result").is_some(), accepted);
    }
    // Escaping is transport encoding, not Go's string-byte budget.
    let result = project(Value::String("\"".repeat(5000)));
    assert!(json::get(json::get(&result, "action").unwrap(), "result").is_some());
}
fn response(req: &Request) -> Value {
    let Dispatch::Reply(r) = mcp::decode(req).unwrap() else {
        panic!("expected protocol reply")
    };
    json::parse(r.body.bytes()).unwrap()
}
#[test]
fn handshake_catalog_schema_and_notifications_follow_go_contract() {
    let init = response(&request(
        r#"{"jsonrpc":"2.0","id":"call","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}"#,
    ));
    assert_eq!(json::text(&init, "id"), "call");
    assert_eq!(
        json::text(json::get(&init, "result").unwrap(), "protocolVersion"),
        "2025-06-18"
    );
    let tools = response(&request(
        r#"{"jsonrpc":"2.0","id":3,"method":"tools/list"}"#,
    ));
    assert_eq!(
        json::array(json::get(&tools, "result").unwrap(), "tools").len(),
        17
    );
    let Dispatch::Reply(r) = mcp::decode(&request(
        r#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#,
    ))
    .unwrap() else {
        panic!("missing notification reply")
    };
    assert_eq!(r.status, 202);
    assert!(r.body.bytes().is_empty());
    for args in [
        r#"{"unknown":true}"#,
        r#"{"limit":0}"#,
        r#"{"limit":1.5}"#,
        r#"{"availableOnly":"true"}"#,
    ] {
        let body = format!(
            r#"{{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{{"name":"devices_list","arguments":{args}}}}}"#
        );
        let v = response(&request(&body));
        assert!(json::boolean(json::get(&v, "result").unwrap(), "isError"));
    }
    let allowed=mcp::decode(&request(r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"devices_list","arguments":{"limit":1}}}"#)).unwrap();
    assert!(matches!(allowed, Dispatch::Tool(_)));
}
#[test]
fn key_origin_method_and_accept_are_checked_before_any_mutation() {
    let mut store = Store::open(b"", Memory).unwrap();
    let web = Web::new("key").unwrap();
    let mut req = request(r#"{"jsonrpc":"2.0","id":null,"method":"ping"}"#);
    req.path = "/mcp/wrong".into();
    assert_eq!(web.handle(&mut store, &req, &mut Env).unwrap().status, 404);
    req.path = "/mcp/key".into();
    req.origin = "https://evil.example".into();
    assert_eq!(web.handle(&mut store, &req, &mut Env).unwrap().status, 403);
    req.origin.clear();
    req.method = "GET".into();
    assert_eq!(web.handle(&mut store, &req, &mut Env).unwrap().status, 405);
    req.method = "POST".into();
    for value in [
        "application/json",
        "*/*",
        "application/json, text/event-stream;q=0",
        "application/json, text/event-stream;q=broken",
    ] {
        json::set(&mut req.headers, "accept", json::string(value).unwrap()).unwrap();
        assert_eq!(web.handle(&mut store, &req, &mut Env).unwrap().status, 400);
    }
}
#[test]
fn targeted_devices_redact_secrets_and_use_house_units() {
    let mut store=Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"appSettings":{"a":{"password":"SECRET"}},"devices":[{"id":"d","name":"Room","appId":"a","capabilities":["measure_temperature"],"data":{"credential":"SECRET"},"store":{"password":"SECRET"}}]}"#,Memory).unwrap();
    store
        .observe(
            "a",
            "d",
            json::parse(br#"{"measure_temperature":20}"#).unwrap(),
            true,
            "",
        )
        .unwrap();
    store
        .system(json::parse(r#"{"units":{"temperature":"°F"}}"#.as_bytes()).unwrap())
        .unwrap();
    let req = request(
        r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"devices_list","arguments":{"deviceId":"d"}}}"#,
    );
    let Dispatch::Tool(call) = mcp::decode(&req).unwrap() else {
        panic!("expected call")
    };
    let result = mcp::read(&store, &call, &Value::Null).unwrap().unwrap();
    let encoded = json::to_string(&result).unwrap();
    assert!(!encoded.contains("SECRET"));
    let device = &json::array(&result, "devices")[0];
    let cap = json::get(
        json::get(device, "capabilities").unwrap(),
        "measure_temperature",
    )
    .unwrap();
    assert!(json::equal(
        json::get(cap, "value").unwrap(),
        &Value::uint(68)
    ));
}
